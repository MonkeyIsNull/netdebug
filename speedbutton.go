package main

// speedbutton.go — ON-DEMAND SPEED-TEST BUTTON (post-roadmap enhancement). A human
// clicks a button in the --serve dashboard and this file runs ONE reduced-load
// saturating speed test (download / upload / latency / jitter) against Cloudflare by
// default OR a user-supplied custom HTTP(S) endpoint, returning the result as JSON.
//
// THE TOP INVARIANT (proven structurally + behaviorally): the test runs ONLY on the
// explicit inbound POST /speedtest — NEVER from a goroutine, ticker, startup, or init.
// A bare --serve / plain run / --sample hits Cloudflare ZERO times. This file is the
// SANCTIONED home of the on-demand Cloudflare machinery (it names speedClient /
// runSpeed), so serve.go references ONLY the speedRunner seam var and stays clean of
// every banned Cloudflare token (TestServePathNoCloudflareSpeedRefs). The sibling
// static guard TestSpeedButtonGoIsSoleCloudflareCaller pins that arrangement.
//
// SECURITY — the custom URL means the SERVER fetches a user-supplied URL, the SSRF
// crux. Two layers, sharing the ONE isGlobalIP predicate (procdetail.go) so they can
// never drift:
//   1. validateSpeedTarget — PURE pre-screen (scheme http/https only; non-empty host;
//      ports 80/443 only; an IP literal is range-checked directly). Table-tested.
//   2. guardedDialContext — the REAL gate: it resolves the host (injectable seam) and
//      REFUSES to connect unless EVERY resolved IP is global, closing DNS-rebind /
//      TOCTOU that a pure validator cannot. Redirects are refused (ErrUseLastResponse).
// The default Cloudflare path keeps the plain fast speedClient (no throughput
// regression); the guarded client is built ONLY when a custom URL is supplied.
//
// Pure/impure split (house style): validateSpeedTarget + speedResultJSON +
// speedBusyJSON + originLoopback are PURE and table-tested; onDemandSpeed /
// guardedDialContext exec network IO and are reached ONLY via the POST handler.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// The button runs a REDUCED-LOAD profile vs. a full --speed run — the user is
	// upload-heavy with scarce upstream, so the uplink saturation is kept as brief as
	// honest results allow (fewer streams, a short per-direction window).
	speedButtonStreams = 4
	speedButtonSeconds = 4

	// speedButtonHardCap is the wall-clock ceiling on the WHOLE run, wrapped around it
	// via context.WithTimeout(r.Context(), …) in the handler so a closed tab, shutdown,
	// or a stalled custom host cannot pin the single-flight slot past the window. It
	// must EXCEED the reduced-profile duration (~2*(1+4)s streams + latency warm-ups ≈
	// 11s) with margin, and be LESS than the handler's write-deadline carve-out.
	speedButtonHardCap = 30 * time.Second

	// speedButtonWriteMargin is added to the hard cap for THIS route's per-request write
	// deadline (the §0.4 carve-out) so a legitimate >15s result is not torn down by the
	// server-wide 15s WriteTimeout before it is written.
	speedButtonWriteMargin = 15 * time.Second

	// speedButtonDialTimeout bounds one TCP connect on the guarded custom path.
	speedButtonDialTimeout = 10 * time.Second
)

// OnDemandSpeedResult is the result of one button-triggered speed test. SpeedResult
// is reused verbatim for the measurements; Target / OK / Err are REQUEST-scoped (not
// measurement data), so they ride a wrapper rather than bloating SpeedResult (the
// procDetailJSON wrapper precedent).
type OnDemandSpeedResult struct {
	Result SpeedResult // reused measurement type (speed.go)
	Target string      // "Cloudflare" or the VALIDATED custom URL
	OK     bool
	Err    string
}

// customTarget carries the VALIDATED custom endpoint(s) for one POST. When isCustom
// is false the run uses the Cloudflare defaults from the serve config. downURL is
// mandatory in custom mode; upURL empty => upload is SKIPPED (download+latency only —
// never POST zeros to a download-only endpoint).
type customTarget struct {
	isCustom bool
	downURL  string
	upURL    string
}

// speedRunner is the INJECTED on-demand speed seam (mirrors throttleSampler): the
// POST handler calls ONLY this var, never onDemandSpeed directly, so a test can swap
// a spy to PROVE a bare --serve invokes it ZERO times AND the handler invokes it
// exactly once per POST. It defaults to the real onDemandSpeed — production is never
// left pointing at nil/stub. onDemandSpeed is the SOLE on-demand Cloudflare caller.
var speedRunner = onDemandSpeed

// speedLookupIP is the DNS-resolution seam for the guarded custom client. A test
// injects a fake resolver (e.g. mapping a hostname to 127.0.0.1) to prove the
// dial-time guard refuses a rebinding host with NO real DNS. Default: the Go
// resolver's LookupIPAddr.
var speedLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// validateSpeedTarget is the PURE SSRF pre-screen for a user-supplied custom URL. It
// returns the NORMALIZED URL (for the Target echo) or an error — the handler maps any
// error to a STATIC 400 and NEVER echoes the raw input. It is NECESSARY BUT NOT
// SUFFICIENT: a non-literal hostname is accepted here (a pure function cannot close
// DNS-rebind), and the dial-time guardedDialContext is the real gate.
func validateSpeedTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty speed-test target")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("unparseable URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		// Rejects file:// gopher:// ftp:// data: blank and everything else.
		return "", errors.New("scheme must be http or https")
	}
	host := u.Hostname() // strips port, [] brackets and userinfo (so evil@169.254… is checked as 169.254…)
	if host == "" {
		return "", errors.New("missing host")
	}
	// Restrict custom targets to the standard web ports — a public IP + arbitrary port
	// would let the tool poke public services on odd ports (cheap hardening).
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return "", errors.New("port must be 80 or 443")
	}
	// An IP LITERAL is range-checked directly (no resolution needed); a hostname is
	// accepted here and re-checked at dial time.
	if ip := net.ParseIP(host); ip != nil {
		if !isGlobalIP(ip) {
			return "", errors.New("target IP is not a public address")
		}
	}
	return u.String(), nil
}

// originLoopback reports whether an Origin header value is a loopback literal. A blank
// Origin is NOT this function's concern (the caller only rejects a PRESENT non-loopback
// Origin). PURE.
func originLoopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return hostAllowed(u.Host)
}

// guardedDialContext is the DIAL-TIME SSRF gate for the custom path. It resolves the
// target host via the speedLookupIP seam and REFUSES the connection unless EVERY
// resolved IP is global (isGlobalIP) — checking the ACTUAL addresses, which closes the
// DNS-rebind / TOCTOU hole the pure validator cannot (a hostname public at validate
// time, 127.0.0.1 / 169.254.169.254 at dial time). An IP literal is checked without
// resolution. The TLS handshake still uses the URL hostname for SNI / cert
// verification (the Transport layers TLS over this raw TCP conn).
func guardedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if lit := net.ParseIP(host); lit != nil {
		ips = []net.IP{lit}
	} else {
		ips, err = speedLookupIP(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}
	// REJECT the whole dial if ANY resolved address is non-global: a public host
	// resolving to a private/metadata IP is an attack or misconfiguration, never
	// something a speed test should connect to.
	for _, ip := range ips {
		if !isGlobalIP(ip) {
			return nil, fmt.Errorf("refusing to connect: %s resolves to non-global address %s", host, ip)
		}
	}
	d := net.Dialer{Timeout: speedButtonDialTimeout}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// guardedSpeedClient builds the custom-path client: the throughput-tuned transport
// (HTTP/1.1, per-stream TCP) PLUS the guarded dialer and a redirect refusal
// (ErrUseLastResponse — the probe.go house pattern) so a 302 to a private IP is never
// followed. Built ONLY when a custom URL is supplied, so the default Cloudflare path
// keeps the plain fast speedClient (no throughput regression).
func guardedSpeedClient(streams int) *http.Client {
	tr := &http.Transport{
		DialContext:         guardedDialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: streams + 2,
		IdleConnTimeout:     30 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // force HTTP/1.1
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // refuse custom-target redirects
		},
	}
}

// speedTally records per-request outcomes (atomics; shared across worker goroutines)
// so onDemandSpeed can surface an HONEST ok/non-200 signal: the stock
// downloadWorker/uploadWorker SWALLOW a non-200 (counting zero bytes), which structurally
// yields 0 Mbps on a throughout-429 run — indistinguishable from a genuine dead link.
// The ok-aware wrappers below flip these so a 429-throughout run reports ok:false +
// "rate-limited (HTTP 429)", never a bogus measured 0.
type speedTally struct {
	ok200      int64 // count of 200 responses that yielded body bytes
	non200     int64 // count of non-200 responses (429/403/…)
	lastCode   int64 // last non-200 status observed
	errCount   int64 // count of client.Do/transport errors (request never saw an HTTP status)
	retryAfter int64 // 1 if any non-200 carried a Retry-After header (explicit back-off ask)

	// lastErr holds the text of the most recent transport error (a string, so NOT
	// atomic-safe like the counters). Multiple stream goroutines write it, so it is
	// mutex-guarded; the counters stay plain atomics as before. This is what lets
	// tallyErr distinguish an HONEST "server reset us mid-upload" (reached, likely
	// rate-limited) from a genuine "unreachable" — see tallyErr.
	mu      sync.Mutex
	lastErr string
}

// recordErr captures a client.Do/transport error so tallyErr can classify the cause
// honestly instead of discarding it (the old upload/download swallowed it, which is
// exactly how the UPLOAD leg mislabeled an early-429 reset as "unreachable").
func (st *speedTally) recordErr(err error) {
	if err == nil {
		return
	}
	atomic.AddInt64(&st.errCount, 1)
	st.mu.Lock()
	st.lastErr = err.Error()
	st.mu.Unlock()
}

// okDownloadWorker mirrors downloadWorker (speed.go) plus tally bookkeeping.
func (st *speedTally) download(ctx context.Context, client *http.Client, url string, counter *int64) {
	buf := make([]byte, 128*1024)
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			// A window-end cancellation (ctx done) is NORMAL termination, not a server
			// failure — never record it, or a healthy stream whose in-flight request is
			// aborted at the window edge would be mislabeled a transport error.
			if ctx.Err() == nil {
				st.recordErr(err)
			}
			sleepCtx(ctx, 100*time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			atomic.AddInt64(&st.non200, 1)
			atomic.StoreInt64(&st.lastCode, int64(resp.StatusCode))
			if resp.Header.Get("Retry-After") != "" {
				atomic.StoreInt64(&st.retryAfter, 1)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			sleepCtx(ctx, 250*time.Millisecond)
			continue
		}
		atomic.AddInt64(&st.ok200, 1)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				atomic.AddInt64(counter, int64(n))
			}
			if rerr != nil || ctx.Err() != nil {
				break
			}
		}
		resp.Body.Close()
	}
}

// okUploadWorker mirrors uploadWorker (speed.go) plus tally bookkeeping. The custom
// path's transfer is bounded by the short reduced-load window + the hard cap; each
// request streams at most speedButtonUploadChunk zeros.
const speedButtonUploadChunk = 50 * 1024 * 1024

func (st *speedTally) upload(ctx context.Context, client *http.Client, url string, counter *int64) {
	for ctx.Err() == nil {
		body := &zeroCounter{ctx: ctx, counter: counter, remaining: speedButtonUploadChunk}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		// ROOT-CAUSE NOTE (deeper fix deliberately left out — see tallyErr): this streams
		// a large (speedButtonUploadChunk) body, so when the server answers early with a
		// non-200 (429/413/403) and closes the connection MID-upload, Go's http.Transport
		// surfaces the body-WRITE error here (connection reset / broken pipe) and DISCARDS
		// the response — the 429 status never reaches the resp.StatusCode branch below.
		// A clean status-preserving fix would use an "Expect: 100-continue" handshake
		// (Transport.ExpectContinueTimeout > 0) so the server's early non-200 arrives
		// BEFORE the body is sent, but that adds a round-trip per request and would have
		// to change the SHARED throughput transport (speedClient + guardedSpeedClient),
		// risking the throughput measurement. Instead, recordErr captures the transport
		// error and tallyErr classifies a mid-request reset HONESTLY (reached + likely
		// rate-limited), never as "unreachable".
		resp, err := client.Do(req)
		if err != nil {
			// A window-end cancellation (ctx done) is NORMAL termination for the long
			// streaming upload, not a server failure — never record it. The whole reason
			// the button's upload leg looked broken was that this ONE streaming POST never
			// completes a 200 inside the short window; it is torn down by the window-end
			// cancel, and that benign error must not pollute the tally (onDemandSpeed keys
			// upload success off MEASURED throughput, see speedLegErr).
			if ctx.Err() == nil {
				st.recordErr(err)
			}
			sleepCtx(ctx, 100*time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			atomic.AddInt64(&st.non200, 1)
			atomic.StoreInt64(&st.lastCode, int64(resp.StatusCode))
			if resp.Header.Get("Retry-After") != "" {
				atomic.StoreInt64(&st.retryAfter, 1)
			}
		} else {
			atomic.AddInt64(&st.ok200, 1)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// transportErrIsReset reports whether a transport error text indicates the server was
// REACHED and then dropped/reset the connection (reset / broken pipe / EOF / closed)
// rather than never answering. On the upload leg this is Cloudflare's signature for an
// early non-200 (429/413/403) whose status Go's Transport discards in favor of the
// body-write error — so it means "reached, likely rate-limited", the OPPOSITE of
// unreachable. PURE (string classification).
func transportErrIsReset(errText string) bool {
	t := strings.ToLower(errText)
	for _, s := range []string{"reset", "broken pipe", "eof", "connection closed", "closed network connection", "closed connection"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// transportErrIsConnectFailure reports whether a transport error text indicates the
// server was NEVER reached (DNS failure / connection refused / no route / network
// unreachable / host down) — the only case in which "target unreachable" is honest.
// PURE (string classification).
func transportErrIsConnectFailure(errText string) bool {
	t := strings.ToLower(errText)
	for _, s := range []string{"no such host", "refused", "no route", "network is unreachable", "host is down", "host unreachable"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// tallyErr maps a tally with zero good responses to an HONEST message, or "" when at
// least one 200 landed. The ordering is evidence-first: a seen HTTP status (429/other)
// wins, then a recorded transport error is classified by cause — a mid-request
// reset/broken-pipe/EOF means the host ANSWERED and dropped us (never "unreachable"),
// a genuine connect failure is the only thing that earns "unreachable", and any other
// transport error surfaces its real text rather than a blanket guess. A tally with no
// status and no error at all gets a neutral fallback.
func tallyErr(st *speedTally, what string) string {
	if atomic.LoadInt64(&st.ok200) > 0 {
		return ""
	}
	if atomic.LoadInt64(&st.non200) > 0 {
		code := atomic.LoadInt64(&st.lastCode)
		retryAfter := atomic.LoadInt64(&st.retryAfter) > 0
		switch {
		case code == http.StatusTooManyRequests:
			// The signature rate-limit — name it explicitly so the panel tells the user
			// WHY the run failed and that retrying later is the remedy.
			return what + ": rate-limited by server (HTTP 429) — try again in a bit"
		case code == http.StatusServiceUnavailable && retryAfter:
			// 503 + Retry-After is an explicit "I'm overloaded, back off" — a rate-limit
			// in spirit, so it gets the same try-again framing, distinct from a bare 503.
			return what + ": server busy / rate-limited (HTTP 503) — try again in a bit"
		case code >= 500:
			return fmt.Sprintf("%s: server error (HTTP %d)", what, code)
		default:
			return fmt.Sprintf("%s: server returned HTTP %d", what, code)
		}
	}
	if atomic.LoadInt64(&st.errCount) > 0 {
		st.mu.Lock()
		errText := st.lastErr
		st.mu.Unlock()
		switch {
		case transportErrIsReset(errText):
			return what + ": connection reset by server (likely rate-limited or rejected) — try again in a bit"
		case transportErrIsConnectFailure(errText):
			return what + ": no data received (target unreachable)"
		default:
			// Unknown transport error (e.g. a bare i/o timeout): surface the real cause
			// rather than asserting "unreachable" without evidence.
			return fmt.Sprintf("%s: request failed (%s)", what, errText)
		}
	}
	return what + ": no data received (no response and no transport error recorded)"
}

// speedLegErr decides one leg's (download/upload) honest outcome, folding in the
// MEASURED throughput. The UPLOAD leg's single long streaming POST does NOT complete a
// full 200 inside the short reduced-load window — it is torn down by the window-end
// cancel — so "did we see a 200?" is the WRONG success test for upload (it is why the
// button's upload leg always reported failure). The right test is: did real bytes flow
// AND did the server not reject us? If so, it is a legitimate measurement, window-edge
// cancellation notwithstanding. Any server rejection (non-200: 429/5xx/…) still wins, so
// a rate-limit is surfaced even when a little data slipped through. With no throughput
// and no clean 200, fall back to tallyErr's evidence-first classification (HTTP status,
// then transport-error cause). PURE given the tally + a measured number.
func speedLegErr(st *speedTally, measuredMbps float64, what string) string {
	if measuredMbps > 0 && atomic.LoadInt64(&st.non200) == 0 {
		return "" // real throughput, no server rejection => success
	}
	return tallyErr(st, what)
}

// onDemandSpeed is the production on-demand runner (the speedRunner seam default) and
// the SOLE on-demand Cloudflare caller. It copies cfg, applies the reduced-load
// profile, selects the plain (Cloudflare) or guarded (custom) client, threads ctx
// through the ctx-aware primitives, and surfaces an HONEST OK/Err. Reached ONLY via
// the POST handler (spy-proven).
func onDemandSpeed(ctx context.Context, cfg Config, custom customTarget) OnDemandSpeedResult {
	prof := cfg
	prof.SpeedStreams = speedButtonStreams
	if cfg.SpeedStreams > 0 && cfg.SpeedStreams < speedButtonStreams {
		prof.SpeedStreams = cfg.SpeedStreams
	}
	prof.SpeedSeconds = speedButtonSeconds

	target := "Cloudflare"
	downURL := prof.SpeedDownURL
	upURL := prof.SpeedUpURL
	latURL := prof.SpeedLatencyURL
	doUpload := true
	var client *http.Client

	if custom.isCustom {
		target = custom.downURL
		downURL = custom.downURL
		latURL = custom.downURL // cfServerTimingRTT falls back to wall-clock on a non-CF host
		if custom.upURL != "" {
			upURL = custom.upURL
		} else {
			doUpload = false // download-only endpoint: NEVER POST zeros to it
		}
		client = guardedSpeedClient(prof.SpeedStreams)
	} else {
		client = speedClient(prof.SpeedStreams)
	}

	r := SpeedResult{Streams: prof.SpeedStreams, Seconds: prof.SpeedSeconds}
	r.LatencyMs, r.JitterMs = measureLatency(ctx, client, latURL, 8)

	var dt speedTally
	r.DownloadMbps, r.DownMin, r.DownMax, r.DownSamples =
		measureStream(ctx, client, prof.SpeedStreams, prof.SpeedSeconds, dt.download, downURL)

	var ut speedTally
	if doUpload {
		r.UploadMbps, r.UpMin, r.UpMax, r.UpSamples =
			measureStream(ctx, client, prof.SpeedStreams, prof.SpeedSeconds, ut.upload, upURL)
	}

	// If the whole run was cancelled (disconnect / shutdown / hard cap), report that
	// honestly rather than a partial/zero number.
	if ctx.Err() != nil {
		return OnDemandSpeedResult{Result: r, Target: target, OK: false, Err: "speed test aborted (timed out or cancelled)"}
	}

	if msg := speedLegErr(&dt, r.DownloadMbps, "download"); msg != "" {
		return OnDemandSpeedResult{Result: r, Target: target, OK: false, Err: msg}
	}
	if doUpload {
		if msg := speedLegErr(&ut, r.UploadMbps, "upload"); msg != "" {
			return OnDemandSpeedResult{Result: r, Target: target, OK: false, Err: msg}
		}
	}
	return OnDemandSpeedResult{Result: r, Target: target, OK: true}
}

// speedWire is the POST /speedtest 200 response shape. download/upload/latency/jitter
// are the measurements; target echoes "Cloudflare" or the VALIDATED URL (rendered via
// textContent on the client — safe); ok + error carry honest degradation (a 429 /
// error => ok:false + message, zeroed numbers NOT reported as measured); t is the
// request instant.
type speedWire struct {
	DownloadMbps float64   `json:"download_mbps"`
	UploadMbps   float64   `json:"upload_mbps"`
	LatencyMs    float64   `json:"latency_ms"`
	JitterMs     float64   `json:"jitter_ms"`
	Target       string    `json:"target"`
	OK           bool      `json:"ok"`
	Err          string    `json:"error,omitempty"`
	T            time.Time `json:"t"`
}

// speedResultJSON is the PURE wire builder for a finished (or honestly-failed) run.
// time.Now() is injected (not called inside) so the test path is deterministic.
func speedResultJSON(res OnDemandSpeedResult, now time.Time) ([]byte, error) {
	w := speedWire{
		Target: res.Target,
		OK:     res.OK,
		Err:    res.Err,
		T:      now,
	}
	// Only report measured numbers when the run succeeded — never a bogus 0 dressed as
	// a measurement on a 429/error.
	if res.OK {
		w.DownloadMbps = res.Result.DownloadMbps
		w.UploadMbps = res.Result.UploadMbps
		w.LatencyMs = res.Result.LatencyMs
		w.JitterMs = res.Result.JitterMs
	}
	return json.Marshal(w)
}

// speedBusyJSON is the PURE 409 body: a second trigger while one test runs. ok:false +
// an honest message, no measurement fields.
func speedBusyJSON() ([]byte, error) {
	return json.Marshal(struct {
		OK  bool   `json:"ok"`
		Err string `json:"error"`
	}{OK: false, Err: "a speed test is already running"})
}
