package main

// Phase 2 — local server + live view. `netdebug --serve` runs the Phase 1
// sampler (REUSED unchanged via runSampleQuiet + *Ring) in a goroutine and
// serves a self-contained page on 127.0.0.1:PORT. Three endpoints: GET / (the
// dashboard), GET /data.json (the live ring snapshot), GET /healthz ("ok").
//
// Pure/impure split (house style): validateBindAddr / dataJSON / hostAllowed are
// PURE and table-tested; listenLocal / serveDashboard / the handlers are the thin
// impure wrappers that bind, serve and shut down.
//
// THE invariant is loopback-only bind. It is ENFORCED, not merely asserted:
// listenLocal pins 127.0.0.1 via net.JoinHostPort + an explicit IP host (never
// ":PORT"/":0"/wildcard), runs validateBindAddr pre-bind on the candidate and
// post-bind on the real ln.Addr(), and serveDashboard carries no addr string so
// no caller can smuggle a wildcard into http.ListenAndServe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// defaultTailWindow is the number of trailing samples /data.json returns when no
// ?n= is given. The ring holds defaultRingCap (3600); returning the whole ring
// every tick would re-marshal ~400KB/poll within an hour, so default to a
// bounded tail and let ?n= widen it.
const defaultTailWindow = 300

// LiveData is the /data.json wire shape. As of Phase 4 each Sample may carry a
// `link` object (band/SSID/RSSI/channel, omitted until the sampler stamps one) and
// LiveData carries a top-level Events array (drop/roam/band_change) computed over
// the SAME serialized tail slice. Warm-up samples and the --sample path carry no
// link key, keeping the Phase-2 wire byte-identical for consumers that ignore the
// new fields. History persistence remains a separate wire surface (/history.json).
type LiveData struct {
	Iface    string    `json:"iface"`
	Interval float64   `json:"interval_sec"` // clamped value the sampler runs at
	Samples  []Sample  `json:"samples"`
	Events   []Event   `json:"events"`         // detectEvents over the serialized tail (always an array)
	Meta     *LiveMeta `json:"meta,omitempty"` // Phase 6: serve always sets it; nil off the pure test path

	// Phase 7 reachability (additive; both omitempty). Reach is the latest cycle;
	// ReachSeries is a bounded tail for the sparkline. A nil pointer AND a nil/empty
	// slice are BOTH omitted, so the pure dataJSON test path (Reach nil) emits
	// neither key and the existing "no null / no meta" assertions are unaffected.
	Reach       *ReachSample  `json:"reach,omitempty"`        // latest cycle
	ReachSeries []ReachSample `json:"reach_series,omitempty"` // bounded tail
}

// LiveMeta is the Phase-6 top-level /data.json meta object (additive; consumers
// ignore unknown keys). It is a POINTER on LiveData so a Meta-less LiveData (the
// pure dataJSON unit tests) marshals with NO "meta" key and the existing "no null"
// assertions are unaffected.
type LiveMeta struct {
	// SSIDLabel is ALWAYS the ssidDisplay() output on the serve path (never empty —
	// an unset label becomes the "HIDDEN (macOS-redacted)" sentinel), so the contract
	// is "meta present => ssid_label present"; NO omitempty.
	SSIDLabel string  `json:"ssid_label"`
	UptimeSec float64 `json:"uptime_sec,omitempty"` // server-serve uptime; always >0 on serve, so omitempty only drops the pure-path 0

	// Phase 9 meeting badge (additive; BOTH omitempty => omitted => existing meta
	// assertions unaffected). Computed SERVER-SIDE via the single pure meetingReady
	// from the latest FRESH Phase-7 reach sample; the dashboard JS only maps the
	// status string to a color + renders the label/reason. They live on LiveMeta, NOT
	// on ReachSample (which is raw sample data reused in reach_series — a derived
	// verdict there would bloat every element and conflate raw vs. derived). Absent
	// when no reach sample exists or the latest is STALE/paused (neutral "—" badge).
	MeetingStatus string `json:"meeting_status,omitempty"` // GOOD / RISKY / BAD
	MeetingReason string `json:"meeting_reason,omitempty"` // the driving dimension
}

// meetingStaleFactor matches the dashboard's Phase-6 STALE_FACTOR: a reach sample
// older than this many reach intervals is considered stale (reach probing pauses on
// battery / locked screen), so the meeting fields are OMITTED rather than served
// minutes-old — a stale GOOD glanced at before a call is exactly the false
// reassurance this feature exists to prevent.
const meetingStaleFactor = 3

// validateBindAddr returns an error unless addr is a loopback host:port. It
// PARSES — never prefix/substring-matches — so a hostile host like
// "127.0.0.1.evil.com" is rejected and all of 127.0.0.0/8 (plus ::1 /
// ::ffff:127.0.0.1) is accepted. "localhost:PORT" is rejected on purpose: a
// non-IP host can be pointed at a LAN IP via /etc/hosts or DNS, so only parsed
// loopback IPs pass.
func validateBindAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port: %w", err)
	}
	if port == "" {
		return fmt.Errorf("missing port in %q", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q is not an IP literal (loopback IP required)", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("host %q is not a loopback address", host)
	}
	return nil
}

// dataJSON serializes a ring snapshot + meta into the wire bytes the page
// expects. Pure: takes LiveData, returns []byte, no IO. It normalizes a nil
// Samples slice to an empty slice so the JSON is "samples":[] and never
// "samples":null (Go marshals a nil []Sample to null) — the []-not-null
// guarantee lives here in the pure function, not in Ring.Snapshot's happening to
// return non-nil. Marshal FAILS on NaN/Inf, but deltaRate's minDt guard already
// guarantees finite, non-negative rates.
func dataJSON(d LiveData) ([]byte, error) {
	if d.Samples == nil {
		d.Samples = []Sample{}
	}
	if d.Events == nil { // same []-not-null house rule as Samples — never "events":null
		d.Events = []Event{}
	}
	return json.Marshal(d)
}

// stripReachSeriesTargets nils the per-element Targets on a reach_series snapshot so
// the bounded sparkline tail never ships a targets table per element (the detail rides
// the LATEST reach object only). PURE + RACE-FREE: it is called on reachR.Snapshot()'s
// INDEPENDENT copies, so reassigning the slice-header field touches only the copy, never
// the shared holder/ring sample. Returns the same slice for call-site convenience.
func stripReachSeriesTargets(series []ReachSample) []ReachSample {
	for i := range series {
		series[i].Targets = nil
	}
	return series
}

// hostAllowed is the DNS-rebinding defense: a rebinding attack arrives with an
// attacker-controlled Host (e.g. "evil.example" resolved to 127.0.0.1), so only
// requests whose Host hostname is a loopback literal are served. Port-independent
// so it holds for the auto-picked port and for httptest's own port. Pure/tested.
func hostAllowed(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

// listenLocal binds a TCP listener to loopback ONLY and auto-picks the next free
// port starting at `port` (bind-or-advance: the net.Listen SUCCESS is the
// reservation, EADDRINUSE advances, any other error fails immediately). Every
// candidate is pinned to 127.0.0.1 via JoinHostPort + an explicit IP host and
// passes validateBindAddr both PRE-bind (on the candidate) and POST-bind (on the
// real ln.Addr(), which also catches a wildcard mistake at the socket, not just
// in a unit test). No probe-then-bind (TOCTOU), no ":0"/":PORT" free-port
// discovery (momentarily binds all interfaces). Fails loudly if all maxTries
// ports are in use.
func listenLocal(port, maxTries int) (net.Listener, string, error) {
	if maxTries < 1 {
		maxTries = 1
	}
	var lastErr error
	for p := port; p < port+maxTries && p <= 65535; p++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) // never ":%d", never a bare host
		if err := validateBindAddr(addr); err != nil {         // pre-bind, aborts BEFORE net.Listen
			return nil, "", fmt.Errorf("refusing to bind %q: %w", addr, err)
		}
		ln, err := net.Listen("tcp", addr) // explicit IP host constrains the bind; success IS the reservation
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				lastErr = err
				continue // only in-use advances
			}
			return nil, "", fmt.Errorf("listen %q: %w", addr, err) // any other error fails immediately
		}
		bound := ln.Addr().String()
		// POST-bind re-check on the REAL socket: a ":0"/wildcard mistake yields
		// "[::]:N" or "0.0.0.0:N", both of which validateBindAddr rejects.
		if err := validateBindAddr(bound); err != nil {
			ln.Close()
			return nil, "", fmt.Errorf("bound socket %q failed loopback re-check: %w", bound, err)
		}
		tcp, ok := ln.Addr().(*net.TCPAddr)
		if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() || tcp.IP.IsUnspecified() {
			ln.Close()
			return nil, "", fmt.Errorf("bound socket %q is not a concrete loopback address", bound)
		}
		return ln, bound, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no ports attempted")
	}
	return nil, "", fmt.Errorf("no free loopback port in [%d,%d): %w", port, port+maxTries, lastErr)
}

// newServeMux builds the HTTP routes for serve mode. Factored out so the
// httptest in serve_test.go exercises the EXACT handlers the real server runs.
// Go 1.22+ method+anchor patterns: GET /{$} anchors EXACTLY "/" (not a
// catch-all), unknown paths 404 and non-GET auto-405. Content-Types are pinned
// (a bare w.Write would sniff JSON to text/plain). NO CORS header is ever set on
// any endpoint — a reflexive Access-Control-Allow-Origin:* would let any site the
// user visits read their throughput stream from loopback; the same-origin page
// needs none.
func newServeMux(ring *Ring, iface string, interval time.Duration, store TimeSeriesStore, ssidLabel string, startedAt time.Time, reachH *reachHolder, reachR *reachRing, reachInterval time.Duration, airspaceH *airspaceHolder, procsH *procsHolder, resolveDNS bool, throttleRatio float64, throttleMinSamples int, speedCfg Config) *http.ServeMux {
	mux := http.NewServeMux()
	intervalSec := interval.Seconds()

	// guard wraps a handler with the DNS-rebinding Host check.
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !hostAllowed(r.Host) {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}

	// speedBusy is the SERVE-WIDE single-flight flag for POST /speedtest: exactly ONE
	// on-demand speed test at a time, never a second concurrent saturation. It is
	// CLOSURE state (per-server, test-isolated) — NOT a package global, so two httptest
	// servers in one run never share it. A second trigger while one runs returns 409.
	var speedBusy atomic.Bool

	mux.HandleFunc("GET /{$}", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	}))

	mux.HandleFunc("GET /data.json", guard(func(w http.ResponseWriter, r *http.Request) {
		samples := ring.Snapshot()
		n := defaultTailWindow
		if q := r.URL.Query().Get("n"); q != "" {
			if parsed, err := strconv.Atoi(q); err == nil && parsed > 0 {
				n = parsed
			}
		}
		if n > defaultRingCap { // the window can never exceed the ring (bounded allocation)
			n = defaultRingCap
		}
		if n < len(samples) { // tail window: slice the snapshot BEFORE building LiveData
			samples = samples[len(samples)-n:]
		}
		// Detect events over the SAME tail slice that is serialized, so every
		// Event.T anchors to a sample the page actually received. A transition
		// exactly at the window's first sample has no predecessor in the slice and
		// is therefore not annotated (documented tradeoff).
		events := detectEvents(samples)
		// Phase 6 meta: ssidLabel is ALREADY ssidDisplay()'d by main (never empty here);
		// uptime is sampled per poll from the serve-entry startedAt (no client ticking).
		meta := LiveMeta{SSIDLabel: ssidLabel, UptimeSec: time.Since(startedAt).Seconds()}
		// Phase 7: load the latest reach cycle + the bounded sparkline tail. Both are
		// loaded without blocking on a slow ping (atomic holder + mutex ring snapshot)
		// and are independently guarded by omitempty: nil holder / empty ring => key
		// omitted (warm-up window / handler-only test with no reach loop).
		var reach *ReachSample
		var reachSeries []ReachSample
		if reachH != nil {
			reach = reachH.load()
		}
		if reachR != nil {
			// STRIP the per-target drill-down from the sparkline tail: Targets rides the
			// LATEST reach object only. Snapshot returns independent copies, so nilling the
			// slice-header field touches only the copy (race-free; the shared holder/ring
			// sample is never mutated) and the up-to-300-element series never ships ~300
			// copies of a 4-row targets table the sparkline (internet_rtt_ms only) never uses.
			reachSeries = stripReachSeriesTargets(reachR.Snapshot())
		}
		// Phase 9 meeting badge (server-side, single-source meetingReady). Computed
		// ONLY when a reach sample exists AND is FRESH; a stale/paused sample (probing
		// paused on battery / locked screen) omits both fields so the badge renders a
		// neutral "—". meetingReady is fed InternetLoss (NOT GatewayLoss, which is ~0%
		// even on a lossy uplink and would report GOOD during an internet-path loss).
		if reach != nil && reachInterval > 0 &&
			time.Since(reach.T) <= time.Duration(meetingStaleFactor)*reachInterval {
			meta.MeetingStatus, meta.MeetingReason = meetingReady(reach.JitterMs, reach.InternetLoss)
		}
		b, err := dataJSON(LiveData{Iface: iface, Interval: intervalSec, Samples: samples, Events: events, Meta: &meta,
			Reach: reach, ReachSeries: reachSeries})
		if err != nil {
			// Should not happen: deltaRate guarantees finite rates. Never panic.
			fmt.Printf("netdebug: /data.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /history.json serves the persisted + derived history buckets. It is wrapped
	// by the SAME guard as every other route and, like /data.json, is no-store (a
	// stale cached history would hide a just-closed minute). store may be nil in a
	// handler-only test that does not exercise history (then it returns empty
	// arrays), but serve mode always passes a real store.
	mux.HandleFunc("GET /history.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var minute, hour []Bucket
		if store != nil {
			minute, hour = store.Snapshot()
		}
		b, err := historyJSON(minute, hour)
		if err != nil {
			fmt.Printf("netdebug: /history.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /outages.json serves the Phase-8 outage journal + today's cadence summary. Same
	// guard / no-store / pinned Content-Type / ZERO CORS as every other endpoint
	// (must_fix #6). store may be nil in a handler-only test (then empty arrays, never
	// null); serve mode always passes a real store. The pure outageJSON injects
	// time.Now() HERE (not inside the pure function) so the cadence "today" window is
	// the request instant.
	mux.HandleFunc("GET /outages.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var outages []Outage
		if store != nil {
			outages = store.OutagesSnapshot()
		}
		b, err := outageJSON(outages, time.Now())
		if err != nil {
			fmt.Printf("netdebug: /outages.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /roam.json serves the Phase-14 BAND / CHANNEL CHANGE journal + today's cadence
	// summary. MIRRORS /outages.json byte-for-byte in posture: same guard / no-store /
	// pinned Content-Type / ZERO CORS. store may be nil in a handler-only test (then
	// empty arrays, never null); serve mode always passes a real store. The pure
	// roamJSON injects time.Now() HERE (not inside the pure function) so the cadence
	// "today" window is the request instant.
	mux.HandleFunc("GET /roam.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var roams []Transition
		if store != nil {
			roams = store.RoamsSnapshot()
		}
		b, err := roamJSON(roams, time.Now())
		if err != nil {
			fmt.Printf("netdebug: /roam.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /report serves the Phase-13 self-contained daily rollup page (speed-by-hour chart
	// with flagged peak-hour dips + totals/band-time/drops/worst-sample latency + the
	// mandatory honesty caveat). Same guard as every endpoint. It NEVER invokes the
	// throttle sampler — it is a PURE read of already-persisted data.
	mux.HandleFunc("GET /report", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(reportHTML))
	}))

	// /report.json serves the DayReport ([]-not-null). It composes store.Snapshot() +
	// store.OutagesSnapshot() + store.SpeedSnapshot() into dailyReport(..., time.Now(),
	// "day") — dailyReport's internal de-dup handles Snapshot's OVERLAPPING hour view
	// (§0.1 #3). The configured (clamped) throttleRatio/throttleMinSamples are threaded
	// in so /report honors throttle_ratio/throttle_min_samples exactly like --report.
	// store==nil (handler-only test) => a zeroed DayReport with [] / {} arrays. NEVER
	// invokes the sampler. Same guard / no-store / pinned CT / ZERO CORS.
	mux.HandleFunc("GET /report.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var rep DayReport
		if store != nil {
			minute, hour := store.Snapshot()
			buckets := append(append([]Bucket(nil), minute...), hour...)
			rep = dailyReport(buckets, store.OutagesSnapshot(), store.SpeedSnapshot(), time.Now(), "day", throttleRatio, throttleMinSamples)
		} else {
			rep = dailyReport(nil, nil, nil, time.Now(), "day", throttleRatio, throttleMinSamples)
		}
		b, err := reportJSON(rep)
		if err != nil {
			fmt.Printf("netdebug: /report.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /airspace.json serves the Phase-10 channel-occupancy forensic (the passive
	// neighbor list + width-aware per-channel load + the clearest-80MHz-block
	// recommendation). Same guard / no-store / pinned Content-Type / ZERO CORS as
	// every other endpoint. airspaceH may be nil (handler-only test) or hold nil
	// (serve warm-up before the first link tick) — both emit the empty payload with
	// []-not-null arrays and recommendation:null, never a reassuring "airspace clear".
	mux.HandleFunc("GET /airspace.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		var err error
		var snap *airspaceSnap
		if airspaceH != nil {
			snap = airspaceH.load()
		}
		if snap == nil {
			b, err = airspaceJSON(nil, nil, "", BlockRec{})
		} else {
			loads := channelOccupancy(snap.Neighbors, snap.MyChannel)
			rec := clearestBlock(loads)
			b, err = airspaceJSON(loads, snap.Neighbors, snap.MyChannel, rec)
		}
		if err != nil {
			fmt.Printf("netdebug: /airspace.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /procs.json serves the Phase-11 per-process top-UPSTREAM-talkers rates. Same
	// guard / no-store / pinned Content-Type / ZERO CORS as every other endpoint.
	// procsH may be nil (handler-only test) or hold nil (serve warm-up before the
	// first procs tick), OR hold an unavailable / paused snapshot — ALL of which emit
	// {"procs":[],"available":false[,"paused":true]} with []-not-null, never a
	// reassuring blank "nobody is uploading". available:true only with a real rate
	// snapshot. Process NAMES are passed JSON-encoded, untouched (the dashboard
	// renders them via textContent — names are adversarial, any app names itself).
	mux.HandleFunc("GET /procs.json", guard(func(w http.ResponseWriter, r *http.Request) {
		var procs []ProcBandwidth
		var available, paused bool
		if procsH != nil {
			if snap := procsH.load(); snap != nil {
				procs = snap.Procs
				available = snap.Available
				paused = snap.Paused
			}
		}
		b, err := procsJSON(procs, available, paused)
		if err != nil {
			fmt.Printf("netdebug: /procs.json marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /proc?pid=<n> is the PROCESS DRILL-DOWN endpoint (post-Phase-11). REQUEST-DRIVEN
	// (no holder, no goroutine, no new bind): it execs ps + lsof ON the click. Same
	// guard / no-store / pinned Content-Type / ZERO CORS as every other route, and
	// registered INSIDE guard() (a slip that drops guard() would expose it without the
	// DNS-rebinding Host check — the serve_test non-loopback=>403 test is the catch).
	//
	// SECURITY: the pid is BROWSER-SUPPLIED untrusted input, validated as a positive
	// integer (validProcPID: strconv.Atoi + >0 + <=procPIDCeiling) and rejected with a
	// STATIC 400 body BEFORE any exec — the raw ?pid= is NEVER echoed back. r.URL.Query
	// .Get returns only the FIRST value, so ?pid=1&pid=2;rm cannot smuggle a second
	// value past the guard. A dead/unidentifiable pid => {available:false} at HTTP 200
	// (never 500/crash). r.Context() threads through so a client disconnect / shutdown
	// aborts the in-flight ps/lsof/PTR.
	mux.HandleFunc("GET /proc", guard(func(w http.ResponseWriter, r *http.Request) {
		n, ok := validProcPID(r.URL.Query().Get("pid"))
		if !ok {
			http.Error(w, "invalid pid", http.StatusBadRequest) // static body, BEFORE any exec
			return
		}
		detail := procDetailReader(r.Context(), n, resolveDNS)
		b, err := procDetailJSON(detail)
		if err != nil {
			fmt.Printf("netdebug: /proc marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// POST /speedtest is the ON-DEMAND SPEED-TEST endpoint (post-roadmap). It runs ONE
	// reduced-load saturating test ON THE EXPLICIT HUMAN CLICK and blocks until done,
	// then writes the result JSON. It is the SOLE trigger of the speed test — nothing
	// automatic (no goroutine/ticker/startup) ever reaches speedRunner, so a bare
	// --serve hits Cloudflare ZERO times (spy-proven). Registered INSIDE guard() like
	// every sibling (non-loopback Host => 403); Go 1.22 method patterns auto-405 a GET.
	//
	// SECURITY: POST-only (an expensive saturating action must not be drive-by/prefetch-
	// triggerable); a CSRF gate (a required non-simple X-Netdebug-SpeedTest header forces
	// a CORS preflight the zero-CORS server fails for any cross-origin caller, plus a
	// present-non-loopback Origin => 403); a ~4KB MaxBytesReader body cap; first-value
	// parse (a duplicated field cannot smuggle a second unvalidated URL); a custom URL is
	// hard-validated (validateSpeedTarget) BEFORE any fetch and the actual connection is
	// SSRF-gated at dial time (guardedDialContext). Single-flight (409 when busy). The
	// whole run is wrapped in a hard-cap WithTimeout off r.Context() so disconnect /
	// shutdown / cap all abort the saturation AND free the single-flight slot. This
	// handler references ONLY the speedRunner seam + the pure wire builders, so serve.go
	// names no Cloudflare token (TestServePathNoCloudflareSpeedRefs stays green).
	mux.HandleFunc("POST /speedtest", guard(func(w http.ResponseWriter, r *http.Request) {
		// CSRF / drive-by gate. A present Origin must be a loopback literal; the custom
		// header is mandatory (its non-simple name forces a cross-origin preflight the
		// zero-CORS server cannot satisfy, while the same-origin page sets it freely).
		if o := r.Header.Get("Origin"); o != "" && !originLoopback(o) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Netdebug-SpeedTest") != "1" {
			http.Error(w, "missing required header", http.StatusForbidden)
			return
		}

		// Body cap (this is the FIRST serve endpoint that reads a body; newServer sets no
		// cap). Oversize => static 400 before any URL is trusted.
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// First-value parse (PostForm.Get returns only the first value per field).
		var custom customTarget
		if r.PostForm.Get("mode") == "custom" {
			down, err := validateSpeedTarget(r.PostForm.Get("download_url"))
			if err != nil {
				http.Error(w, "invalid target URL", http.StatusBadRequest) // never echo the raw URL
				return
			}
			custom.isCustom = true
			custom.downURL = down
			if raw := strings.TrimSpace(r.PostForm.Get("upload_url")); raw != "" {
				up, err := validateSpeedTarget(raw)
				if err != nil {
					http.Error(w, "invalid upload URL", http.StatusBadRequest)
					return
				}
				custom.upURL = up
			}
		}

		// Single-flight: acquire BEFORE launching; a second trigger => 409, never a
		// second saturation. Released in a defer tied to the ctx-cancellable run
		// returning (so a timed-out/disconnected request frees the slot promptly).
		if !speedBusy.CompareAndSwap(false, true) {
			b, _ := speedBusyJSON()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusConflict)
			w.Write(b)
			return
		}
		defer speedBusy.Store(false)

		// Write-deadline carve-out: the run can legitimately exceed the server-wide 15s
		// WriteTimeout, which would tear the connection down before the result is written
		// (false error on success). Extend THIS request's write deadline only — the
		// sanctioned per-route carve-out the newServer WriteTimeout comment anticipated.
		// Bounded by single-flight (at most one such request) + the single buffered write.
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetWriteDeadline(time.Now().Add(speedButtonHardCap + speedButtonWriteMargin))
		}

		// Hard wall-clock cap off r.Context(): a closed tab, Ctrl-C, Shutdown, or the cap
		// all stop the saturation promptly and free the single-flight slot.
		rctx, cancel := context.WithTimeout(r.Context(), speedButtonHardCap)
		defer cancel()

		res := speedRunner(rctx, speedCfg, custom)
		b, err := speedResultJSON(res, time.Now())
		if err != nil {
			fmt.Printf("netdebug: /speedtest marshal error: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	}))

	// /healthz is LIVENESS-only: "ok" even if the iface is down and no samples
	// flow. It signals "server up", not "sampling healthy".
	mux.HandleFunc("GET /healthz", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
	}))

	// 204 to suppress the browser's same-origin favicon 404 on each load.
	mux.HandleFunc("GET /favicon.ico", guard(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	return mux
}

// newServer builds the hardened *http.Server for serve mode. Factored out of
// serveDashboard (where the server was built inline and untestable) so a table test
// can assert every timeout/cap field. Every field has a concrete rationale:
//   - ReadHeaderTimeout 5s: bounds a slow header dribble.
//   - ReadTimeout 10s: bounds a slowly-dribbled body on keep-alive — GET handlers
//     never read r.Body but net/http must still drain it.
//   - WriteTimeout 15s: THE slow-loris close. A client reading the response one byte
//     at a time otherwise pins a goroutine forever. SAFE here because every endpoint
//     writes one complete buffered body (no streaming/SSE/hijack) and the full-ring
//     /data.json over loopback is instant. NOTE: a future push/SSE dashboard (Phase
//     6) must revisit WriteTimeout (per-handler http.TimeoutHandler or a carve-out).
//   - IdleTimeout 60s: caps idle keep-alive connections.
//   - MaxHeaderBytes 64KB: the only client is the self-hosted same-origin page; the
//     one input (?n=) is in the URL, inside the header budget.
//
// No http.MaxBytesReader / body cap: these are GET-only endpoints that never read a
// body.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64KB
	}
}

// linkHolder holds the most-recent radio snapshot, lock-free, for the 1s sampler
// to read without blocking on the (slow) system_profiler read. The pointer is nil
// until the first link read returns, so early samples stamp Link=nil (byte-
// compatible warm-up).
type linkHolder struct{ p atomic.Pointer[LinkSnap] }

func (h *linkHolder) store(s LinkSnap) { h.p.Store(&s) }
func (h *linkHolder) load() *LinkSnap  { return h.p.Load() }

// airspaceSnap is the neighbor-occupancy snapshot written by the SAME link tick
// that writes the LinkSnap (Phase 10). MyChannel is carried from that tick's
// LinkSnap so /airspace.json reads a SELF-CONSISTENT (neighbors, my channel) pair
// from ONE holder — no torn read across two atomics.
type airspaceSnap struct {
	Neighbors []Neighbor
	MyChannel string
}

// airspaceHolder is the lock-free holder for the latest airspaceSnap (same shape
// as linkHolder; nil until the first link tick returns).
type airspaceHolder struct{ p atomic.Pointer[airspaceSnap] }

func (h *airspaceHolder) store(s airspaceSnap) { h.p.Store(&s) }
func (h *airspaceHolder) load() *airspaceSnap  { return h.p.Load() }

// procsSnap is the Phase-11 per-process top-talkers snapshot, carried lock-free (the
// airspaceSnap precedent). Available AND Paused are carried EXPLICITLY in ONE atomic
// store so the handler never has to guess nil-vs-empty or suffer a torn read, and a
// PAUSED/stale snapshot is never rendered as a live rate. T stamps the store.
type procsSnap struct {
	Procs     []ProcBandwidth
	Available bool
	Paused    bool // gated this tick (battery / locked screen)
	T         time.Time
}

// procsHolder is the lock-free holder for the latest procsSnap (same shape as
// linkHolder/airspaceHolder; nil until the first procs tick stores one).
type procsHolder struct{ p atomic.Pointer[procsSnap] }

func (h *procsHolder) store(s procsSnap) { h.p.Store(&s) }
func (h *procsHolder) load() *procsSnap  { return h.p.Load() }

// uploadStreakSnap is the Phase-12 high-upload counter state. The streak is
// incremented in the 1s sampler goroutine but READ in the reach goroutine, so it is
// carried lock-free through an atomic.Pointer (a plain shared int would be a data race
// -race flags). upBps is the rate at the last sample, carried alongside so the reach
// tick can name the sustained rate in the notification body WITHOUT a second cross-
// goroutine read.
type uploadStreakSnap struct {
	streak int
	upBps  float64
}

// uploadStreakHolder is the lock-free holder for the latest uploadStreakSnap. The
// sampler is the SOLE writer (load-then-store is therefore safe); the reach tick does
// an atomic LOAD only.
type uploadStreakHolder struct {
	p atomic.Pointer[uploadStreakSnap]
}

func (h *uploadStreakHolder) store(s uploadStreakSnap) { h.p.Store(&s) }
func (h *uploadStreakHolder) load() uploadStreakSnap {
	if v := h.p.Load(); v != nil {
		return *v
	}
	return uploadStreakSnap{}
}

// alertConfig carries the Phase-12 alert knobs (already clamped at the impure boundary
// in main) into serveDashboard. enabled is the --alerts opt-in: when false the whole
// alert path is dormant (the sampler sink stays the bare store.ingest, the observe
// closure never builds AlertInputs, and notify is never called).
type alertConfig struct {
	enabled       bool
	cooldown      time.Duration
	thresholdBps  float64 // upstream threshold in BYTES/sec (thresholdBps(mbps))
	streakSamples int     // N consecutive samples that count as "sustained"
}

// procDetailReader is the /proc drill-down read seam: the handler calls it instead
// of readProcDetail directly so a test can swap in a call-COUNTING stub to PROVE the
// pid-validation 400 path performs ZERO exec (and that a valid pid reaches the
// reader exactly once). Defaults to the real exec'ing readProcDetail.
var procDetailReader = readProcDetail

// throttleSampler is the INJECTED Phase-13 throttle-sample seam: the --throttle-watch
// watchdog calls it (never runThrottleSample directly) so a test can swap in a SPY and
// PROVE a bare --serve (no --throttle-watch) invokes it ZERO times. It defaults to the
// real runThrottleSample — the SOLE new Cloudflare caller, which lives in throttle.go
// (so serve.go references no speed/Cloudflare token and TestServePathNoCloudflareSpeedRefs
// stays green). serve.go's watchdog body names only this var + store.appendSpeed.
var throttleSampler = runThrottleSample

// procDetailJSON serializes the /proc wire shape. Pure + []-not-null: a nil
// Connections slice normalizes to "connections":[] (never null), the same house rule
// as procsJSON/dataJSON. available:false carries an honest empty detail.
func procDetailJSON(d ProcDetail) ([]byte, error) {
	if d.Connections == nil {
		d.Connections = []ProcConn{}
	}
	return json.Marshal(d)
}

// procsJSON serializes the /procs.json wire shape. Pure + []-not-null: a nil procs
// slice normalizes to "procs":[] (never null), available is carried explicitly, and
// paused is omitempty (present only when a tick was gated). available:true only ever
// accompanies a real rate snapshot.
func procsJSON(procs []ProcBandwidth, available, paused bool) ([]byte, error) {
	if procs == nil {
		procs = []ProcBandwidth{}
	}
	return json.Marshal(struct {
		Procs     []ProcBandwidth `json:"procs"`
		Available bool            `json:"available"`
		Paused    bool            `json:"paused,omitempty"`
	}{Procs: procs, Available: available, Paused: paused})
}

// runProcsLoop is the Phase-11 per-process top-talkers loop, on the runReachLoop
// SHAPE (NOT runLinkLoop — the link loop never pauses). A SINGLE non-reentrant
// Ticker at procs_interval; each tick consults its OWN gate instance g (a DEDICATED
// newReachGate, NEVER runReachLoop's reachG — reachGate is mutex-free by design, so
// sharing one across two goroutines would be a data race -race catches). A PAUSED
// tick stores an explicit paused/unavailable snapshot (never leaves a stale rate
// rendered as live); an allowed tick execs read (readTopTalkers, ctx-aware so
// shutdown aborts an in-flight nettop), ranks upstream-first, caps at topN, and
// stores it. Exits promptly on ctx cancel without a late store.
func runProcsLoop(ctx context.Context, interval time.Duration,
	read func(context.Context) ([]ProcBandwidth, bool), g *reachGate, h *procsHolder, topN int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !g.allow() {
				h.store(procsSnap{Available: false, Paused: true, T: time.Now()})
				continue
			}
			procs, available := read(ctx)
			h.store(procsSnap{Procs: topTalkers(procs, topN, false), Available: available, T: time.Now()})
		}
	}
}

// runLinkLoop resamples the radio at the link cadence and stores each snapshot. A
// SINGLE goroutine + Ticker: naturally non-reentrant (a hung read drops ticks, no
// pile-up => no 1s system_profiler storm). read is injected (readRadio in serve
// mode, a spy in tests) and returns BOTH the LinkSnap and the neighbor list from
// ONE exec; each tick stores BOTH holders from that single read, so the two holders
// never diverge in time and no extra process is spawned. Exits promptly on ctx
// cancel without a late store.
func runLinkLoop(ctx context.Context, interval time.Duration, read func() (LinkSnap, []Neighbor), linkH *linkHolder, airspaceH *airspaceHolder) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ls, nb := read()
			linkH.store(ls)
			airspaceH.store(airspaceSnap{Neighbors: nb, MyChannel: ls.Channel})
		}
	}
}

// serveDashboard runs the sampler goroutine + HTTP server and blocks until ctx is
// cancelled, then shuts down cleanly and returns nil (exit 0). It is
// CONTEXT-DRIVEN (signal wiring lives in main) so a test can cancel ctx and
// assert a clean return. The listener is created ONLY by listenLocal — this
// signature carries NO addr string, so main cannot build "127.0.0.1:8099" (or
// ":8099") and hand it to http.ListenAndServe. REUSES *Ring as-is (its RWMutex
// already makes concurrent Add and Snapshot safe — no second mutex).
//
// Lifecycle: srv has NO Addr field; srv.Serve(ln) only (ListenAndServe/srv.Addr
// forbidden). On ctx.Done the shutdown uses a FRESH 5s context (never the
// cancelled ctx, which would make Shutdown cut connections immediately), then the
// sampler ctx is cancelled and its goroutine JOINED before returning, so no late
// ring.Add fires after teardown.
func serveDashboard(ctx context.Context, iface string, interval, linkInterval time.Duration, ln net.Listener,
	chosenAddr, historyDir string, minuteTTL, hourTTL time.Duration, ssidLabel string,
	reachInterval time.Duration, internetIPs []string, dnsName string, powerGating bool,
	outageStartThresh, outageEndThresh int, outageTTL time.Duration,
	procsInterval time.Duration, topTalkersN int, procDetailResolveDNS bool,
	ac alertConfig, notifyFn func(context.Context, Alert) error,
	throttleWatch bool, throttleCfg Config,
	throttleMinSpacing, throttleJitterMax, throttleMaxBackoff, throttleTTL time.Duration) error {
	// startedAt is captured at serve entry so meta.uptime_sec is server-serve uptime
	// (sampled per poll in the handler), not a serveDashboard param — fewer call sites.
	startedAt := time.Now()
	ring := NewRing(defaultRingCap)

	// Open the history store FIRST — a flock failure (another --serve on the same
	// dir) or an unreadable dir must fail loudly before we start serving. The outage
	// TTL drives the ONE-SHOT journal sweep inside openHistoryStoreTTL.
	// Construction builds the concrete *historyStore; everything downstream holds it
	// through the TimeSeriesStore seam (the sampler sink, the handlers, the throttle
	// watch) so the backend could be swapped without touching those consumers.
	hist, err := openHistoryStoreTTL(historyDir, outageTTL, throttleTTL)
	if err != nil {
		return err
	}
	var store TimeSeriesStore = hist

	// Phase 4: the radio-snapshot holder (lock-free, nil until the first link read
	// returns). The sampler reads it with a plain atomic load — never an exec.
	holder := &linkHolder{}

	// Phase 10: the neighbor-occupancy holder, written by the SAME link tick from the
	// SAME system_profiler output (no extra process). nil until the first link read.
	airspaceH := &airspaceHolder{}

	// Phase 7: the reach holder + ring + power/presence gate. The gate is LOCAL to
	// the reach goroutine (no mutex); it is constructed ONLY here on the --serve path
	// so a bare run / --sample never reaches the ping/DNS/pmset/ioreg execs.
	reachH := &reachHolder{}
	reachR := newReachRing()
	reachG := newReachGate(powerGating)

	// REACHABILITY drill-down (post-roadmap): the reverse-DNS NAME cache for the stable
	// config internet targets. Populated OFF the reach tick path by a dedicated ctx-bound
	// goroutine (runReachNamesRefresh) and read with a single non-blocking atomic snapshot
	// inside readReach. UNGATED on purpose: a 30-min PTR pair is negligible traffic and
	// keeps names fresh across battery/locked stretches. Serve-only (never a bare run).
	reachN := &reachNames{}

	// Phase 11: the per-process top-talkers holder + its OWN DEDICATED gate instance.
	// It is a NEW newReachGate (same type, same power/presence policy, NEW instance) —
	// NEVER reachG above: reachGate is mutex-free by design (local to one goroutine),
	// so sharing it across the reach and procs goroutines would be a data race. The
	// 3s TTL gives no dedup benefit at the 10s procs cadence (every tick re-execs
	// pmset+ioreg; harmless — two light execs / 10s); do NOT "fix" that by sharing.
	procsH := &procsHolder{}
	procsG := newReachGate(powerGating)

	// Phase 12: the high-upload streak holder (lock-free; written by the 1s sampler,
	// read by the reach tick). Created unconditionally (cheap, nil until first store)
	// but FED only when --alerts is on, so the plain --serve path carries zero added
	// per-sample work.
	streakH := &uploadStreakHolder{}

	// Sampler + link + prune goroutines share one cancel + WaitGroup so teardown
	// waits for ALL of them (no leak, no late Add / late system_profiler exec).
	sampleCtx, sampleCancel := context.WithCancel(context.Background())
	defer sampleCancel()
	var wg sync.WaitGroup

	// Sampler goroutine. The per-sample sink is store.ingest (folds into the open
	// minute, persists a closed minute); the linkProvider is a plain atomic load of
	// the latest snapshot (nil during warm-up) — NO exec in the 1s tick. When --alerts
	// is on the sink is COMPOSED to also step the high-upload streak (the sampler is
	// the SOLE writer of streakH, so load-then-store is safe); when off it stays the
	// bare store.ingest — ZERO added overhead on the plain --serve path.
	sink := store.ingest
	if ac.enabled {
		thBps := ac.thresholdBps
		sink = func(s Sample) {
			store.ingest(s)
			prev := streakH.load().streak
			streakH.store(uploadStreakSnap{
				streak: stepUploadStreak(prev, s.UpBytesPerSec, thBps),
				upBps:  s.UpBytesPerSec,
			})
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		runSampleQuiet(sampleCtx, iface, interval, ring, sink,
			func() *LinkSnap { return holder.load() })
	}()

	// Dedicated link goroutine: execs system_profiler + ifconfig at the SLOWER
	// link cadence and stores BOTH the link snapshot AND the neighbor occupancy from
	// ONE system_profiler read (readRadio). A single Ticker-driven loop is naturally
	// non-reentrant — a hung ~1-3s system_profiler cannot pile up ticks (the Ticker
	// drops ticks while the receiver is busy), so there is no 1s process storm.
	// JOINED on shutdown like the others.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runLinkLoop(sampleCtx, linkInterval, func() (LinkSnap, []Neighbor) { return readRadio(iface) }, holder, airspaceH)
	}()

	// Phase 8 OUTAGE DETECTOR (seam b): a loop-local pure outageState advanced on each
	// reach observation. The closure owns outSt EXCLUSIVELY — it is only ever called
	// from the single reach goroutine (same discipline as reachGate), so no mutex. On a
	// PROBED tick it fuses the FRESH ReachSample `s` (GatewayOK/InternetOK) with the
	// FAIL-OPEN link bit (nil holder => LinkOK true; must_fix #2) into one Obs and runs
	// stepOutage; on close it durably appends the record. On a GATED/paused tick it runs
	// stepReset (must_fix #5) and persists nothing. Obs.T strips the monotonic reading
	// (Round(0)) so a prod duration equals the wall-clock pure-test arithmetic.
	var outSt outageState
	// Phase 12 alert state: loop-local closure state exactly like outSt (owned
	// EXCLUSIVELY by the single reach goroutine, so no mutex). alertSt holds the pure
	// cooldown stamps + level latches; lastBand is the band-change comparator's prior
	// reading. Both are PRESERVED across a gated/paused tick (a battery/screen-lock
	// pause must never manufacture, re-fire, or re-arm an alert).
	var alertSt alertState
	var lastBand string
	// Phase 14 roam-detector state (owned EXCLUSIVELY by the single reach goroutine,
	// like outSt — no mutex). roamPrev is the LAST ASSOCIATED observation; roamDropped /
	// roamPaused record whether a drop (an inactive probed tick) or a probing pause (a
	// gated tick) sat between it and the next associated sample, so detectTransition can
	// flag with_drop / after_pause. onsetCtx is the last-known-good link captured AT the
	// moment an outage OPENS — used to BACKFILL the outage onset band/ch/RSSI when the
	// pure machine's (channel-keyed) lastGoodLink came up empty (e.g. a link that
	// reported a band but no channel), so the OUTAGE JOURNAL's "Onset Band / CH" column
	// populates instead of showing "—".
	var roamPrev RoamLink
	var haveRoamPrev, roamDropped, roamPaused bool
	var lastGoodCtx, onsetCtx LinkSnap
	observe := func(s ReachSample, probed bool) {
		if !probed {
			outSt = stepReset(outSt)
			roamPaused = true // a pause straddles the next active sample => approximate time
			return            // alertSt + lastBand + roam state preserved; decideAlerts NOT called
		}
		linkOK := true // fail-open: unknown (warm-up nil) is NOT down
		var link LinkSnap
		linkActive := false
		if lp := holder.load(); lp != nil {
			link = *lp
			linkOK = lp.OK
			linkActive = lp.OK
		}
		// Track the freshest GOOD link (band OR channel present) for the onset backfill.
		if linkActive && (link.Band != "" || link.Channel != "") {
			lastGoodCtx = link
		}
		o := Obs{
			T:          time.Now().Round(0),
			LinkOK:     linkOK,
			GatewayOK:  s.GatewayOK,
			InternetOK: s.InternetOK,
			Link:       link,
		}
		wasOpen := outSt.open // BEFORE stepOutage (the drop signal is the OPEN edge)
		var closed Outage
		var didClose bool
		outSt, closed, didClose = stepOutage(outSt, o, outageStartThresh, outageEndThresh)
		if !wasOpen && outSt.open { // OPEN edge: snapshot the onset context for the backfill
			onsetCtx = lastGoodCtx
		}
		if didClose {
			// Backfill the onset radio context if the pure machine recorded none (band
			// and channel both empty) — the live last-known-good link at onset is the
			// honest source. Only fills when empty, so a correctly-captured record is
			// byte-identical.
			if closed.Band == "" && closed.Channel == "" {
				closed.Band = onsetCtx.Band
				closed.Channel = onsetCtx.Channel
				closed.RSSI = onsetCtx.RSSI
				closed.Noise = onsetCtx.Noise
				closed.SNR = onsetCtx.SNR
				if closed.SSID == "" {
					closed.SSID = onsetCtx.SSID
				}
			}
			if err := store.appendOutage(closed); err != nil {
				fmt.Printf("netdebug: outage append failed: %v\n", err)
			}
		}

		// Phase 14 roam detection: compare consecutive ASSOCIATED observations. An
		// inactive probed tick (link down) is NOT a comparison point — it only records
		// that a drop sat between the surrounding associated samples (roamDropped). On
		// each associated tick, detect a band/channel/BSSID change vs roamPrev and, if
		// present, durably append it; then advance roamPrev and clear the gap flags.
		if !linkActive {
			roamDropped = true
		} else {
			cur := RoamLink{Band: link.Band, Channel: link.Channel, RSSI: link.RSSI} // BSSID: macOS-redacted => always ""
			if tr, ok := detectTransition(roamPrev, cur, haveRoamPrev, roamDropped, roamPaused, time.Now().Round(0)); ok {
				if err := store.appendRoam(tr); err != nil {
					fmt.Printf("netdebug: roam append failed: %v\n", err)
				}
			}
			roamPrev = cur
			haveRoamPrev = true
			roamDropped = false
			roamPaused = false
		}

		if !ac.enabled {
			return // opt-in: no AlertInputs built, no decide, no notify
		}

		// DROP = the outage OPEN edge (not the CLOSE/reconnect): true for exactly one
		// tick, so cooldown is only a flap guard. Cause/SSID are stamped at onset.
		openedNow := !wasOpen && outSt.open

		// BAND_CHANGE via a dedicated loop-local comparator (NOT detectEvents — the
		// reach tick has no Sample ring). MIRROR detectEvents' both-non-empty guard
		// EXACTLY so a warm-up / parse-miss flap ("5 GHz"->""->"5 GHz") never fires a
		// phantom. lastBand is updated to curBand on every PROBED tick (below).
		curBand := link.Band
		bandChanged := lastBand != "" && curBand != "" && lastBand != curBand

		// MEETING_BAD from the FRESH probed sample (only BAD fires; the once-per-
		// episode latch lives in decideAlerts, so staying BAD does not re-notify).
		meetingStatus, _ := meetingReady(s.JitterMs, s.InternetLoss)

		// HIGH_UPLOAD streak: atomic LOAD of the sampler-written counter + rate.
		us := streakH.load()

		in := AlertInputs{
			NewOutage:                 openedNow,
			OutageCause:               outSt.pending.Cause,
			OutageSSID:                outSt.pending.SSID,
			BandChanged:               bandChanged,
			BandFrom:                  lastBand,
			BandTo:                    curBand,
			MeetingStatus:             meetingStatus,
			HighUploadStreak:          us.streak,
			HighUploadThresholdStreak: ac.streakSamples,
			HighUploadBody:            fmt.Sprintf("↑%s sustained (%d samples)", humanBps(us.upBps), us.streak),
		}
		alertSt = fireAlerts(sampleCtx, ac.enabled, in, alertSt, time.Now(), ac.cooldown, notifyFn)
		lastBand = curBand // update each PROBED tick (and only on probed ticks)
	}

	// Reach goroutine (Phase 7): a SINGLE non-reentrant Ticker at the (decoupled)
	// reach cadence. Each tick consults the power/presence gate; a paused tick stores
	// nothing. read() is ctx-aware so shutdown aborts an in-flight probe. JOINED on
	// the same WaitGroup as the others. The Phase-8 detector rides along as `observe`.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runReachLoop(sampleCtx, reachInterval,
			func(c context.Context) ReachSample { return readReach(c, internetIPs, dnsName, reachN) },
			reachG, reachH, reachR, observe)
	}()

	// REACHABILITY drill-down: the reverse-DNS names refresher. Ctx-bound + JOINED on the
	// same WaitGroup so shutdown waits for it; it is internally bounded (reachNamesCap) so
	// wg.Wait() never hangs. Non-blocking w.r.t. the reach tick (atomic cache).
	wg.Add(1)
	go func() {
		defer wg.Done()
		runReachNamesRefresh(sampleCtx, internetIPs, reachN)
	}()

	// Prune goroutine: its OWN 1-minute ticker, observes the sampler cancel, JOINED
	// before return. Never runs inline in the 1s sampler tick.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-t.C:
				_ = store.prune(time.Now(), minuteTTL, hourTTL)
			}
		}
	}()

	// Phase 11 procs goroutine: a SINGLE non-reentrant Ticker at procs_interval on the
	// runReachLoop shape, pausing on battery / locked screen via its OWN gate
	// (procsG). readTopTalkers is ctx-aware so sampleCancel aborts an in-flight nettop
	// and wg.Wait() returns in moments. JOINED on the SAME WaitGroup as the others.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runProcsLoop(sampleCtx, procsInterval, readTopTalkers, procsG, procsH, topTalkersN)
	}()

	// Phase 13 THROTTLE WATCHDOG (OPT-IN, default OFF): spawned ONLY when
	// throttleWatch==true, so a bare --serve calls Cloudflare ZERO times. It gets its
	// OWN newReachGate(powerGating) — a FOURTH gate instance, NEVER shared with reachG/
	// procsG (reachGate is mutex-free by design; sharing would be a -race data race) —
	// and so pauses on battery/locked like the other loops. lastAttemptT is seeded from
	// the newest persisted SpeedSnapshot T so spacing SURVIVES a restart. JOINED on the
	// same WaitGroup; exits promptly on ctx cancel. The body names ONLY throttleSampler
	// + store.appendSpeed (keeps TestServePathNoCloudflareSpeedRefs green).
	if throttleWatch {
		throttleG := newReachGate(powerGating)
		var seedLast time.Time
		if snap := store.SpeedSnapshot(); len(snap) > 0 {
			seedLast = snap[len(snap)-1].T // SpeedSnapshot is T-sorted ascending
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			runThrottleWatch(sampleCtx, throttleCfg, store, throttleG, throttleSampler,
				throttleMinSpacing, throttleJitterMax, throttleMaxBackoff, seedLast)
		}()
	}

	srv := newServer(newServeMux(ring, iface, interval, store, ssidLabel, startedAt, reachH, reachR, reachInterval, airspaceH, procsH, procDetailResolveDNS, throttleCfg.ThrottleRatio, throttleCfg.ThrottleMinSamples, throttleCfg))

	serveErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is the normal exit after Shutdown, not an error.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		err := srv.Shutdown(shutCtx)
		sampleCancel()
		wg.Wait()             // joins BOTH sampler and prune
		<-serveErr            // let the Serve goroutine finish (it returns ErrServerClosed)
		_ = store.FlushOpen() // partial minute — reachable because it is store state
		store.Close()         // release flock
		return err
	case err := <-serveErr:
		// Serve failed before any signal (e.g. listener closed unexpectedly).
		sampleCancel()
		wg.Wait()
		_ = store.FlushOpen()
		store.Close()
		return err
	}
}
