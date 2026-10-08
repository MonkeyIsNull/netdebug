package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestValidateSpeedTarget is the PURE SSRF pre-screen table. The adversarial rows
// cover metadata / private / loopback / link-local / unspecified, non-http schemes,
// userinfo-smuggled metadata, odd ports, and a blank input; the accepted rows are a
// public http + https URL (normalized return). The dial-time guard (separate test) is
// what closes DNS-rebind — this pure function is advisory for hostnames.
func TestValidateSpeedTarget(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"metadata ip", "http://169.254.169.254/", true},
		{"metadata via userinfo", "http://evil@169.254.169.254/", true},
		{"ula v6", "http://[fd00::1]/", true},
		{"localhost name resolves nowhere safe but literal 127", "http://127.0.0.1:9200/", true}, // port not 80/443 AND loopback
		{"loopback literal", "http://127.0.0.1/", true},
		{"private 10/8", "http://10.0.0.1/", true},
		{"private 192.168", "http://192.168.1.1/", true},
		{"unspecified", "http://0.0.0.0/", true},
		{"ipv4-mapped loopback", "http://[::ffff:127.0.0.1]/", true},
		{"ipv4-mapped metadata", "http://[::ffff:169.254.169.254]/", true},
		{"ftp scheme", "ftp://host/", true},
		{"gopher scheme", "gopher://host/", true},
		{"file scheme empty host", "file:///etc/passwd", true},
		{"data uri", "data:text/plain,hi", true},
		{"blank", "", true},
		{"whitespace only", "   ", true},
		{"bad port", "http://example.com:9200/", true},
		{"public http accepted", "http://example.com/file", false},
		{"public https accepted", "https://speed.example.net/__down?bytes=50000000", false},
		{"public https port 443", "https://example.com:443/x", false},
		{"public http port 80", "http://example.com:80/x", false},
		// A non-literal hostname that would REBIND is accepted here (pre-screen only) —
		// the dial-time guard refuses it. Prove the pure screen passes a plain name.
		{"hostname accepted (dial-time gates rebind)", "https://totally-legit.example/path", false},
		// The DEFAULT Cloudflare targets (DefaultConfig) MUST pass the pre-screen: these
		// are exactly the URLs the button's default run and the documented custom examples
		// use, so a regression that rejected them would make every default run fail at the
		// validator. (The default run does not even call validateSpeedTarget — only custom
		// mode does — but pinning these keeps the allowlist honest for the custom path too.)
		{"cloudflare default down", "https://speed.cloudflare.com/__down?bytes=50000000", false},
		{"cloudflare default up", "https://speed.cloudflare.com/__up", false},
		{"cloudflare default latency", "https://speed.cloudflare.com/__down?bytes=0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateSpeedTarget(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateSpeedTarget(%q) = %q, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSpeedTarget(%q) unexpected error: %v", tc.raw, err)
			}
			if got == "" {
				t.Errorf("validateSpeedTarget(%q) returned empty normalized URL", tc.raw)
			}
		})
	}
}

// TestIsGlobalIPForSSRF locks the shared predicate's SSRF-relevant verdicts (the §2.5
// additions): IPv4-mapped IPv6 normalization, the 169.254.169.254 metadata IP, CGNAT,
// ULA, link-local v6, and NAT64 — all REJECTED; a public v4 and v6 ACCEPTED.
func TestIsGlobalIPForSSRF(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"104.18.32.47", true},
		{"2606:4700::1111", true},
		{"169.254.169.254", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:169.254.169.254", false},
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"192.168.0.1", false},
		{"172.16.5.5", false},
		{"100.64.0.1", false}, // CGNAT 100.64/10
		{"fd00::1", false},    // ULA fc00::/7
		{"fe80::1", false},    // link-local v6
		{"::1", false},
		{"::", false},
		{"0.0.0.0", false},
		{"64:ff9b::7f00:1", false}, // NAT64 embedding 127.0.0.1
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("test IP %q did not parse", tc.ip)
		}
		if got := isGlobalIP(ip); got != tc.want {
			t.Errorf("isGlobalIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestGuardedDialContextRefusesRebind proves the DIAL-TIME gate refuses a connection
// when the (fake-injected) resolver maps a hostname to a non-global IP — with NO real
// DNS and NO real connection — and that a public resolution is allowed to proceed to
// the dial (short-circuited here by a closed listener, which is a CONNECT error, NOT a
// refusal). An IP literal that is private is refused without resolution.
func TestGuardedDialContextRefusesRebind(t *testing.T) {
	origLookup := speedLookupIP
	defer func() { speedLookupIP = origLookup }()

	// Rebind: hostname resolves to loopback => refused by the guard.
	speedLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	_, err := guardedDialContext(context.Background(), "tcp", "rebind.example:443")
	if err == nil || !strings.Contains(err.Error(), "non-global") {
		t.Fatalf("rebind to 127.0.0.1 should be refused, got err=%v", err)
	}

	// Rebind to metadata => refused.
	speedLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	_, err = guardedDialContext(context.Background(), "tcp", "metadata.example:80")
	if err == nil || !strings.Contains(err.Error(), "non-global") {
		t.Fatalf("rebind to metadata should be refused, got err=%v", err)
	}

	// Private IP LITERAL => refused with NO resolution (seam must not be consulted).
	speedLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		t.Fatal("resolver must NOT be called for an IP literal")
		return nil, nil
	}
	_, err = guardedDialContext(context.Background(), "tcp", "10.0.0.5:443")
	if err == nil || !strings.Contains(err.Error(), "non-global") {
		t.Fatalf("private IP literal should be refused, got err=%v", err)
	}

	// Public resolution => the guard PASSES and proceeds to dial a loopback listener we
	// stand up at a public-looking seam result is impossible (loopback is non-global),
	// so instead assert the guard does NOT reject a public IP: it attempts a real dial
	// to a closed port, yielding a CONNECT error (not a "non-global" refusal).
	speedLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.9")}, nil // TEST-NET-3, global per predicate
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = guardedDialContext(ctx, "tcp", "public.example:443")
	if err != nil && strings.Contains(err.Error(), "non-global") {
		t.Fatalf("public IP must NOT be refused by the guard, got %v", err)
	}
}

// TestGuardedSpeedClientRefusesRedirect proves the custom client refuses to FOLLOW a
// redirect (CheckRedirect => ErrUseLastResponse), so a 302 to a private IP is never
// followed. It exercises the client's redirect policy against a loopback httptest
// server (dialing loopback is itself blocked for real custom targets, but the
// CheckRedirect policy is independent and is what we assert here).
func TestGuardedSpeedClientRefusesRedirect(t *testing.T) {
	c := guardedSpeedClient(1)
	if c.CheckRedirect == nil {
		t.Fatal("guarded client must set CheckRedirect")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// TestSpeedResultJSONShape pins the wire builder: a success carries the measured
// numbers + target + ok:true; a failure zeroes the numbers (never a bogus measured 0)
// and carries ok:false + an honest error. busy JSON is ok:false + the running message.
// TestTallyErrClassification pins the HONEST classification of tallyErr: the UPLOAD
// leg used to assert "unreachable" on a mid-upload reset even though the server had
// been reached and merely reset/rejected us. These cases are PURE (no network).
func TestTallyErrClassification(t *testing.T) {
	// (a) ONLY transport errors recorded, reset/broken-pipe/EOF flavor => the honest
	// "reset by server" message, and specifically NOT "unreachable".
	for _, resetErr := range []string{
		"write tcp 10.0.0.1:52345->1.1.1.1:443: write: connection reset by peer",
		"write tcp 10.0.0.1:52345->1.1.1.1:443: write: broken pipe",
		"unexpected EOF",
		"use of closed network connection",
	} {
		var st speedTally
		st.recordErr(errors.New(resetErr))
		msg := tallyErr(&st, "upload")
		if !strings.Contains(msg, "reset by server") {
			t.Errorf("reset error %q: want 'reset by server', got %q", resetErr, msg)
		}
		if strings.Contains(msg, "unreachable") {
			t.Errorf("reset error %q must NOT be labeled unreachable: %q", resetErr, msg)
		}
	}

	// (b) a genuine connect failure => "unreachable" is honest here.
	for _, connErr := range []string{
		"dial tcp: lookup bogus.invalid: no such host",
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"dial tcp 1.2.3.4:443: connect: no route to host",
		"dial tcp 1.2.3.4:443: connect: network is unreachable",
	} {
		var st speedTally
		st.recordErr(errors.New(connErr))
		msg := tallyErr(&st, "upload")
		if !strings.Contains(msg, "unreachable") {
			t.Errorf("connect failure %q: want 'unreachable', got %q", connErr, msg)
		}
	}

	// (b') an unknown/ambiguous transport error (bare i/o timeout) surfaces the real
	// text rather than falsely claiming unreachable.
	{
		var st speedTally
		st.recordErr(errors.New("Post \"https://host/__up\": context deadline exceeded (i/o timeout)"))
		msg := tallyErr(&st, "upload")
		if strings.Contains(msg, "unreachable") {
			t.Errorf("ambiguous timeout must not claim unreachable: %q", msg)
		}
		if !strings.Contains(msg, "i/o timeout") {
			t.Errorf("ambiguous error should surface real text: %q", msg)
		}
	}

	// (c) a non-200 wins over any recorded error: the HTTP-code message. A 429 is named
	// as a rate-limit with a try-again cue (the panel must say WHY, per the error-
	// visibility requirement), not a bare status number.
	{
		var st speedTally
		st.recordErr(errors.New("connection reset by peer")) // also present, but status wins
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusTooManyRequests))
		msg := tallyErr(&st, "upload")
		if !strings.Contains(msg, "429") || !strings.Contains(msg, "rate-limited") || !strings.Contains(msg, "try again") {
			t.Errorf("non200 429: want rate-limited/try-again/429 message, got %q", msg)
		}
	}
	{
		var st speedTally
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusForbidden))
		msg := tallyErr(&st, "upload")
		if !strings.Contains(msg, "403") {
			t.Errorf("non200 403: want HTTP 403 message, got %q", msg)
		}
	}
	// (c') 503 WITH a Retry-After header => treated as an explicit back-off / rate-limit
	// (server busy), distinct from a bare 503 which is a generic server error.
	{
		var st speedTally
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusServiceUnavailable))
		atomic.StoreInt64(&st.retryAfter, 1)
		msg := tallyErr(&st, "download")
		if !strings.Contains(msg, "503") || !strings.Contains(msg, "rate-limited") || !strings.Contains(msg, "try again") {
			t.Errorf("503+Retry-After: want rate-limited/try-again/503 message, got %q", msg)
		}
	}
	{
		var st speedTally // bare 503 (no Retry-After) => generic server error, NOT rate-limit framing
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusServiceUnavailable))
		msg := tallyErr(&st, "download")
		if !strings.Contains(msg, "503") || !strings.Contains(msg, "server error") {
			t.Errorf("bare 503: want generic server-error message, got %q", msg)
		}
		if strings.Contains(msg, "try again") {
			t.Errorf("bare 503 must not use the rate-limit try-again framing: %q", msg)
		}
	}
	{
		var st speedTally // a generic 5xx surfaces as a server error with the code
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusBadGateway))
		msg := tallyErr(&st, "download")
		if !strings.Contains(msg, "502") || !strings.Contains(msg, "server error") {
			t.Errorf("502: want server-error message with code, got %q", msg)
		}
	}

	// (d) at least one 200 => success ("").
	{
		var st speedTally
		atomic.AddInt64(&st.ok200, 1)
		atomic.AddInt64(&st.non200, 1) // ok200 still wins
		if msg := tallyErr(&st, "upload"); msg != "" {
			t.Errorf("a 200 must yield success (\"\"), got %q", msg)
		}
	}

	// (e) truly nothing recorded => a neutral fallback, not a bogus "unreachable".
	{
		var st speedTally
		msg := tallyErr(&st, "upload")
		if strings.Contains(msg, "unreachable") {
			t.Errorf("empty tally must not claim unreachable: %q", msg)
		}
	}
}

// TestSpeedLegErr pins the per-leg outcome that fixes the button: the UPLOAD leg's one
// long streaming POST never completes a full 200 inside the short window, so success
// MUST key off MEASURED throughput, not "saw a 200". A leg that pushed real bytes with
// NO server rejection is a success even with zero recorded 200s; a server rejection
// (429/5xx) still wins and is surfaced; no throughput + no clean response falls back to
// the honest transport-error classification.
func TestSpeedLegErr(t *testing.T) {
	// (a) THE REGRESSION: bytes flowed (mbps>0), but the streaming request never
	// recorded a 200/non-200/error (it was torn down at the window edge). Pre-fix this
	// returned the bogus "no response ..." fallback; it MUST now be success.
	{
		var st speedTally // empty: no ok200, no non200, no errCount
		if msg := speedLegErr(&st, 30.7, "upload"); msg != "" {
			t.Errorf("measured upload with empty tally must be success, got %q", msg)
		}
	}
	// (b) a measured leg that ALSO saw a server rejection surfaces the rejection (a
	// rate-limit must not be masked just because a little data slipped through).
	{
		var st speedTally
		atomic.AddInt64(&st.non200, 1)
		atomic.StoreInt64(&st.lastCode, int64(http.StatusTooManyRequests))
		msg := speedLegErr(&st, 12.0, "upload")
		if !strings.Contains(msg, "429") {
			t.Errorf("measured leg with a 429 must surface the rate-limit, got %q", msg)
		}
	}
	// (c) zero throughput + a connect failure => unreachable (unchanged honesty).
	{
		var st speedTally
		st.recordErr(errors.New("dial tcp 1.2.3.4:443: connect: connection refused"))
		msg := speedLegErr(&st, 0, "upload")
		if !strings.Contains(msg, "unreachable") {
			t.Errorf("no throughput + connect failure => unreachable, got %q", msg)
		}
	}
	// (d) a clean 200 with zero measured mbps (tiny/odd rounding) is still success.
	{
		var st speedTally
		atomic.AddInt64(&st.ok200, 1)
		if msg := speedLegErr(&st, 0, "download"); msg != "" {
			t.Errorf("a clean 200 must be success even at 0 mbps, got %q", msg)
		}
	}
}

// TestDefaultConfigSpeedTargetsValidate proves the shipped DefaultConfig speed URLs all
// pass the SSRF pre-screen — a guard against a future config/allowlist drift that would
// make the default button run fail at validation.
func TestDefaultConfigSpeedTargetsValidate(t *testing.T) {
	c := DefaultConfig()
	for _, u := range []string{c.SpeedDownURL, c.SpeedUpURL, c.SpeedLatencyURL} {
		if _, err := validateSpeedTarget(u); err != nil {
			t.Errorf("DefaultConfig speed URL %q must pass validateSpeedTarget, got %v", u, err)
		}
	}
}

func TestSpeedResultJSONShape(t *testing.T) {
	ok := OnDemandSpeedResult{
		Result: SpeedResult{DownloadMbps: 123.4, UploadMbps: 45.6, LatencyMs: 12.3, JitterMs: 1.2},
		Target: "Cloudflare", OK: true,
	}
	b, err := speedResultJSON(ok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"download_mbps":123.4`, `"upload_mbps":45.6`, `"latency_ms":12.3`, `"jitter_ms":1.2`, `"target":"Cloudflare"`, `"ok":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("success wire missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"error"`) {
		t.Errorf("success wire must omit error: %s", s)
	}

	// Failure (429): numbers zeroed, ok:false, honest error echoed, NOT a bogus measured
	// value from any partial Result.
	fail := OnDemandSpeedResult{
		Result: SpeedResult{DownloadMbps: 999, UploadMbps: 999}, // would-be bogus
		Target: "Cloudflare", OK: false, Err: "download: rate-limited by server (HTTP 429)",
	}
	b, _ = speedResultJSON(fail, time.Now())
	s = string(b)
	if !strings.Contains(s, `"ok":false`) || !strings.Contains(s, "HTTP 429") {
		t.Errorf("failure wire wrong: %s", s)
	}
	if !strings.Contains(s, `"download_mbps":0`) || strings.Contains(s, "999") {
		t.Errorf("failure wire must zero measured numbers, got: %s", s)
	}

	busy, err := speedBusyJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(busy), `"ok":false`) || !strings.Contains(string(busy), "already running") {
		t.Errorf("busy wire wrong: %s", busy)
	}
}

// ---- endpoint tests (httptest + injected spy speedRunner; NO real network) --------

// speedSpy records calls + returns a canned result. When gate != nil, ONLY the FIRST
// call blocks until a value is received on the gate (later calls return immediately) —
// so exactly one goroutine ever waits on the gate, avoiding any receiver-routing
// ambiguity when two concurrency tests share the single speedRunner seam.
type speedSpy struct {
	calls int32
	gate  chan struct{}
	res   OnDemandSpeedResult
}

func (s *speedSpy) run(ctx context.Context, cfg Config, custom customTarget) OnDemandSpeedResult {
	n := atomic.AddInt32(&s.calls, 1)
	if s.gate != nil && n == 1 {
		select {
		case <-s.gate:
		case <-ctx.Done():
		}
	}
	return s.res
}

func postSpeed(t *testing.T, srv *httptest.Server, body string, host string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+"/speedtest", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Netdebug-SpeedTest", "1")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /speedtest: %v", err)
	}
	defer resp.Body.Close()
	b, _ := readAllBytes(resp)
	return resp.StatusCode, string(b), resp.Header
}

func readAllBytes(resp *http.Response) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

func newSpeedMux() *http.ServeMux {
	return newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
}

// TestSpeedTestEndpointShape: one valid POST => exactly ONE runner call, 200, pinned
// Content-Type + no-store + NO CORS, correct wire JSON; an ok:false canned result =>
// 200 with ok:false + honest error (no bogus 0 dressed as measured).
func TestSpeedTestEndpointShape(t *testing.T) {
	orig := speedRunner
	defer func() { speedRunner = orig }()
	spy := &speedSpy{res: OnDemandSpeedResult{
		Result: SpeedResult{DownloadMbps: 88.8, UploadMbps: 22.2, LatencyMs: 9.9, JitterMs: 0.5},
		Target: "Cloudflare", OK: true,
	}}
	speedRunner = spy.run

	srv := httptest.NewServer(newSpeedMux())
	defer srv.Close()

	code, body, hdr := postSpeed(t, srv, "mode=cloudflare", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if n := atomic.LoadInt32(&spy.calls); n != 1 {
		t.Fatalf("runner called %d times, want 1", n)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/speedtest must NOT set Access-Control-Allow-Origin")
	}
	for _, want := range []string{`"download_mbps":88.8`, `"upload_mbps":22.2`, `"ok":true`, `"target":"Cloudflare"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}

	// ok:false canned => 200 with honest error, numbers zeroed.
	spy2 := &speedSpy{res: OnDemandSpeedResult{Target: "Cloudflare", OK: false, Err: "download: rate-limited by server (HTTP 429)"}}
	speedRunner = spy2.run
	srv2 := httptest.NewServer(newSpeedMux())
	defer srv2.Close()
	code, body, _ = postSpeed(t, srv2, "mode=cloudflare", "", nil)
	if code != http.StatusOK {
		t.Fatalf("ok:false status = %d, want 200", code)
	}
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "HTTP 429") {
		t.Errorf("ok:false body wrong: %s", body)
	}
	if strings.Contains(body, `"download_mbps":`) && !strings.Contains(body, `"download_mbps":0`) {
		t.Errorf("ok:false must not report a measured download: %s", body)
	}
}

// TestSpeedTestEndpointGuards: every rejection path. non-loopback Host => 403; GET =>
// 405; missing header => 403; cross-origin Origin => 403; oversize body => 400;
// rejected custom URL => 400 that does NOT echo the raw URL. The runner is NEVER called
// on any rejection.
func TestSpeedTestEndpointGuards(t *testing.T) {
	orig := speedRunner
	defer func() { speedRunner = orig }()
	spy := &speedSpy{res: OnDemandSpeedResult{Target: "Cloudflare", OK: true}}
	speedRunner = spy.run

	srv := httptest.NewServer(newSpeedMux())
	defer srv.Close()

	// Non-loopback Host => 403 (DNS-rebinding defense; catches a drop of guard()).
	code, _, _ := postSpeed(t, srv, "mode=cloudflare", "evil.example", nil)
	if code != http.StatusForbidden {
		t.Errorf("non-loopback Host status = %d, want 403", code)
	}

	// Missing required header => 403.
	code, _, _ = postSpeed(t, srv, "mode=cloudflare", "", map[string]string{"X-Netdebug-SpeedTest": ""})
	if code != http.StatusForbidden {
		t.Errorf("missing header status = %d, want 403", code)
	}

	// Cross-origin Origin => 403.
	code, _, _ = postSpeed(t, srv, "mode=cloudflare", "", map[string]string{"Origin": "http://evil.example"})
	if code != http.StatusForbidden {
		t.Errorf("cross-origin Origin status = %d, want 403", code)
	}

	// A loopback Origin is allowed.
	code, _, _ = postSpeed(t, srv, "mode=cloudflare", "", map[string]string{"Origin": "http://127.0.0.1:12345"})
	if code != http.StatusOK {
		t.Errorf("loopback Origin status = %d, want 200", code)
	}

	// Oversize body => 400 before any trust.
	big := "mode=custom&download_url=" + strings.Repeat("a", 5000)
	code, _, _ = postSpeed(t, srv, big, "", nil)
	if code != http.StatusBadRequest {
		t.Errorf("oversize body status = %d, want 400", code)
	}

	// Rejected custom URL => 400 that does NOT echo the raw URL.
	raw := "http://169.254.169.254/latest/meta-data/"
	code, body, _ := postSpeed(t, srv, "mode=custom&download_url="+raw, "", nil)
	if code != http.StatusBadRequest {
		t.Errorf("metadata custom URL status = %d, want 400", code)
	}
	if strings.Contains(body, "169.254.169.254") {
		t.Errorf("400 body echoed the raw rejected URL: %s", body)
	}

	// GET /speedtest => 405 (method pattern auto-405).
	resp, err := http.Get(srv.URL + "/speedtest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /speedtest status = %d, want 405", resp.StatusCode)
	}

	// Only the ONE legitimate POST above reached the runner (the loopback-Origin
	// request); every rejection short-circuited before it.
	if n := atomic.LoadInt32(&spy.calls); n != 1 {
		t.Errorf("runner reached %d times, want exactly 1 (the single accepted POST)", n)
	}
}

// TestSpeedTestSingleFlight: with a blocking spy runner, two simultaneous POSTs => one
// runs + one 409; after the first completes, a third POST succeeds (slot released).
func TestSpeedTestSingleFlight(t *testing.T) {
	orig := speedRunner
	defer func() { speedRunner = orig }()
	spy := &speedSpy{gate: make(chan struct{}), res: OnDemandSpeedResult{Target: "Cloudflare", OK: true}}
	speedRunner = spy.run

	srv := httptest.NewServer(newSpeedMux())
	defer srv.Close()

	// Fire the first POST; it will block in the runner on the gate.
	type result struct {
		code int
		body string
	}
	first := make(chan result, 1)
	go func() {
		code, body, _ := postSpeed(t, srv, "mode=cloudflare", "", nil)
		first <- result{code, body}
	}()

	// Wait until the first request is actually inside the runner (single-flight held).
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&spy.calls) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("first POST never entered the runner")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second POST while the first is in-flight => 409 BUSY, runner NOT entered again.
	code, body, hdr := postSpeed(t, srv, "mode=cloudflare", "", nil)
	if code != http.StatusConflict {
		t.Fatalf("concurrent POST status = %d, want 409; body=%s", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("409 Content-Type = %q", ct)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("409 must be no-store")
	}
	if !strings.Contains(body, "already running") {
		t.Errorf("409 body = %s, want 'already running'", body)
	}
	if n := atomic.LoadInt32(&spy.calls); n != 1 {
		t.Fatalf("runner entered %d times during single-flight, want 1", n)
	}

	// Release the first; it completes 200.
	spy.gate <- struct{}{}
	select {
	case r := <-first:
		if r.code != http.StatusOK {
			t.Fatalf("first POST final status = %d, want 200", r.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first POST never completed after release")
	}

	// Slot released: a third POST now succeeds (it is the 2nd RUN, so it does not block).
	code, body, _ = postSpeed(t, srv, "mode=cloudflare", "", nil)
	if code != http.StatusOK {
		t.Fatalf("post-release POST status = %d, want 200; body=%s", code, body)
	}
	if n := atomic.LoadInt32(&spy.calls); n != 2 {
		t.Errorf("runner total calls = %d, want 2 (first + third; the 409 never ran)", n)
	}
}

// TestSpeedTestPerServerSingleFlight: the flag is CLOSURE state, so two independent
// muxes/servers do NOT share it — a busy test on one never 409s the other.
func TestSpeedTestPerServerSingleFlight(t *testing.T) {
	orig := speedRunner
	defer func() { speedRunner = orig }()
	spy := &speedSpy{gate: make(chan struct{}), res: OnDemandSpeedResult{Target: "Cloudflare", OK: true}}
	speedRunner = spy.run

	srvA := httptest.NewServer(newSpeedMux())
	defer srvA.Close()
	srvB := httptest.NewServer(newSpeedMux())
	defer srvB.Close()

	done := make(chan struct{})
	go func() {
		postSpeed(t, srvA, "mode=cloudflare", "", nil)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&spy.calls) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("srvA POST never entered the runner")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// srvB must NOT be busy (different closure flag). srvB's run is the 2nd call, so the
	// spy does not block it — it returns 200 immediately while srvA is still held.
	code, body, _ := postSpeed(t, srvB, "mode=cloudflare", "", nil)
	if code != http.StatusOK {
		t.Fatalf("srvB status = %d while srvA busy, want 200 (per-server flag); body=%s", code, body)
	}
	// Release srvA.
	spy.gate <- struct{}{}
	<-done
}

// TestSpeedButtonServeSpyZeroThenOne is the BEHAVIORAL zero-auto-Cloudflare proof: a
// bare --serve (no POST) invokes the speed seam ZERO times (no startup/goroutine/
// ticker/init path calls it), AND a single POST invokes it exactly once (the handler is
// actually wired). It also asserts the seam's production default is the real fn.
func TestSpeedButtonServeSpyZeroThenOne(t *testing.T) {
	// The production default must be the real on-demand runner (never nil/stub).
	if speedRunner == nil {
		t.Fatal("speedRunner seam default is nil")
	}

	var calls int32
	orig := speedRunner
	speedRunner = func(ctx context.Context, cfg Config, custom customTarget) OnDemandSpeedResult {
		atomic.AddInt32(&calls, 1)
		return OnDemandSpeedResult{Target: "Cloudflare", OK: true}
	}
	defer func() { speedRunner = orig }()

	ln, addr, err := listenLocal(0, 1)
	if err != nil {
		t.Fatalf("listenLocal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveDashboard(ctx, "en0", 250*time.Millisecond, time.Second, ln, addr, t.TempDir(), defaultMinuteTTL, defaultHourTTL, "HIDDEN (macOS-redacted)",
			5*time.Second, []string{"1.1.1.1", "8.8.8.8"}, "apple.com", true, defaultOutageStartThresh, defaultOutageEndThresh, defaultOutageTTL,
			defaultProcsInterval, defaultTopTalkersN, true, alertConfig{}, notify,
			false, DefaultConfig(), defaultThrottleSpacing, defaultThrottleJitterMax, defaultThrottleMaxBackoff, defaultThrottleSampleTTL)
	}()

	// Let the server + all its goroutines run WITHOUT any POST: the seam stays at 0.
	time.Sleep(400 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("bare --serve invoked the speed seam %d times, want 0", n)
	}

	// One explicit POST => exactly one call.
	req, _ := http.NewRequest("POST", "http://"+addr+"/speedtest", strings.NewReader("mode=cloudflare"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Netdebug-SpeedTest", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("after one POST the seam was called %d times, want 1", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveDashboard did not return promptly on cancel")
	}
}

// TestSpeedButtonGoIsSoleCloudflareCaller mirrors TestThrottleGoIsSoleCloudflareCaller:
// speedbutton.go is the SANCTIONED home of the on-demand Cloudflare machinery (it builds
// the speed client), while serve.go references NONE of the banned tokens (that is
// TestServePathNoCloudflareSpeedRefs). Pinning the arrangement catches a refactor that
// leaks the on-demand sampler out of speedbutton.go.
func TestSpeedButtonGoIsSoleCloudflareCaller(t *testing.T) {
	b, err := os.ReadFile("speedbutton.go")
	if err != nil {
		t.Fatalf("read speedbutton.go: %v", err)
	}
	if !strings.Contains(string(b), "speedClient") {
		t.Error("speedbutton.go should build the speed client (the sanctioned on-demand Cloudflare caller)")
	}
	// serve.go must reference ONLY the speedRunner seam, never the machinery.
	s, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	if !strings.Contains(string(s), "speedRunner") {
		t.Error("serve.go should call the speedRunner seam")
	}
	for _, banned := range []string{"speedClient", "runSpeed", "onDemandSpeed", "guardedSpeedClient", "__up", "__down", "speed.cloudflare.com"} {
		if strings.Contains(string(s), banned) {
			t.Errorf("serve.go must not reference %q (the on-demand machinery lives in speedbutton.go)", banned)
		}
	}
	// main.go must NOT build the mux or touch the seam (the serve path is the sole home).
	m, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, banned := range []string{"newServeMux", "speedRunner", "onDemandSpeed"} {
		if strings.Contains(string(m), banned) {
			t.Errorf("main.go must not reference %q (serve.go owns the serve mux + speed seam)", banned)
		}
	}
}

// TestDashboardHTMLHasSpeedTest is the POSITIVE feature-present guard + the client-side
// zero-auto proof: the speed POST appears ONLY in the button click handler and in NO
// self-starting loop. The no-external-URL guard stays negative, so this pins the pieces
// are PRESENT (a silent delete would pass that guard but fail here).
func TestDashboardHTMLHasSpeedTest(t *testing.T) {
	required := []string{
		`id="speedbtn"`,        // the button
		`id="speedresult"`,     // the result readout
		`id="speedcustom"`,     // the custom-URL field group
		`name="sttarget"`,      // the Cloudflare|Custom target selector
		"Custom endpoint",      // the custom-mode label
		"runSpeedTest",         // the trigger fn (the SOLE POST site)
		"renderSpeedResult",    // the result-render fn
		"createTextNode",       // result text is textContent-built, never innerHTML
		`method: "POST"`,       // POST method token (not a drive-by GET)
		"X-Netdebug-SpeedTest", // the CSRF header
		"SATURATES",            // the honest warning (duration + saturation + target)
		"SENDS test data",      // the custom-mode data-exfil warning
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required speed-test token %q", sub)
		}
	}
	// CLIENT-SIDE zero-auto guard: the server spy can never catch a self-starting
	// pollSpeed(), so assert the POST path appears EXACTLY ONCE and is NOT wired to a
	// timer/interval/DOMContentLoaded auto-run.
	if n := strings.Count(dashboardHTML, `fetch("/speedtest"`); n != 1 {
		t.Errorf("fetch(\"/speedtest\") appears %d times, want exactly 1 (only the click handler)", n)
	}
	if strings.Count(dashboardHTML, `"/speedtest"`) != 1 {
		t.Errorf("the /speedtest URL literal must appear exactly once (no second caller)")
	}
	// The sole trigger is a click listener, never a self-starting loop.
	if !strings.Contains(dashboardHTML, `speedBtn.addEventListener("click", runSpeedTest)`) {
		t.Error("the speed test must be triggered by the button click handler")
	}
	// runSpeedTest is referenced EXACTLY twice: its definition + the click listener. A
	// third reference (a self-starting call / timer / IIFE) would push this past 2.
	if n := strings.Count(dashboardHTML, "runSpeedTest"); n != 2 {
		t.Errorf("runSpeedTest referenced %d times, want exactly 2 (definition + click listener)", n)
	}
	for _, bad := range []string{"setInterval(runSpeedTest", "setTimeout(runSpeedTest", "runSpeedTest();"} {
		if strings.Contains(dashboardHTML, bad) {
			t.Errorf("speed test must not self-start via %q", bad)
		}
	}
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML")
	}
}
