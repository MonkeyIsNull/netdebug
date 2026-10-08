package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateBindAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		ok   bool
	}{
		{"loopback v4", "127.0.0.1:8099", true},
		{"rest of 127/8", "127.0.0.2:8099", true},
		{"loopback v6", "[::1]:8099", true},
		{"v4-mapped loopback", "[::ffff:127.0.0.1]:8099", true},
		{"all interfaces v4", "0.0.0.0:8099", false},
		{"all interfaces v6 bracketed", "[::]:8099", false},
		{"v4-mapped unspecified", "[::ffff:0.0.0.0]:8099", false},
		{"empty host (all interfaces)", ":8099", false},
		{"LAN IP", "192.168.1.5:8099", false},
		{"hostile prefix host", "127.0.0.1.evil.com:8099", false},
		{"localhost (non-IP, redirectable)", "localhost:8099", false},
		{"empty", "", false},
		{"garbage no port", "garbage", false},
		{"missing port", "127.0.0.1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBindAddr(tc.addr)
			if tc.ok && err != nil {
				t.Errorf("validateBindAddr(%q) = %v, want ok", tc.addr, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateBindAddr(%q) = nil, want error", tc.addr)
			}
		})
	}
}

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:8099", true},
		{"localhost:8099", true},
		{"[::1]:8099", true},
		{"127.0.0.1", true},
		{"localhost", true},
		{"evil.example", false},
		{"evil.example:8099", false},
		{"192.168.1.5:8099", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := hostAllowed(tc.host); got != tc.ok {
				t.Errorf("hostAllowed(%q) = %v, want %v", tc.host, got, tc.ok)
			}
		})
	}
}

func TestDataJSON(t *testing.T) {
	// NIL slice (zero-value LiveData{}) must serialize "samples":[] not null.
	// CRITICAL: a nil slice, not []Sample{} — the empty literal passes trivially.
	b, err := dataJSON(LiveData{Iface: "en0", Interval: 1.5})
	if err != nil {
		t.Fatalf("dataJSON(nil samples) error: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"samples":[]`) {
		t.Errorf("nil samples did not normalize to []: %s", s)
	}
	if strings.Contains(s, "null") {
		t.Errorf("output contains null: %s", s)
	}
	if !strings.Contains(s, `"iface":"en0"`) || !strings.Contains(s, `"interval_sec":1.5`) {
		t.Errorf("meta fields missing/incorrect: %s", s)
	}

	// Populated finite samples: Marshal succeeds (finite-rate wire-safety) and
	// round-trips with rates intact.
	now := time.Now()
	in := LiveData{
		Iface:    "en0",
		Interval: 1,
		Samples: []Sample{
			{T: now, RxBytes: 1000, TxBytes: 2000, DownBytesPerSec: 123456.5, UpBytesPerSec: 7890.25},
			{T: now.Add(time.Second), RxBytes: 3000, TxBytes: 4000, DownBytesPerSec: 0, UpBytesPerSec: 0, Reset: true},
		},
	}
	b, err = dataJSON(in)
	if err != nil {
		t.Fatalf("dataJSON(populated) error: %v", err)
	}
	var back LiveData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if len(back.Samples) != 2 {
		t.Fatalf("round-trip lost samples: %d", len(back.Samples))
	}
	if back.Samples[0].DownBytesPerSec != 123456.5 || back.Samples[0].UpBytesPerSec != 7890.25 {
		t.Errorf("rates not intact: %+v", back.Samples[0])
	}
	if !back.Samples[1].Reset {
		t.Errorf("reset flag lost on second sample")
	}
}

// TestListenLocalLoopbackAndAutoPick proves the bind is loopback-only and that a
// taken port advances to the next free one (bind-or-advance).
func TestListenLocalLoopbackAndAutoPick(t *testing.T) {
	ln1, addr1, err := listenLocal(0, 1) // port 0 => OS picks a free loopback port
	if err != nil {
		t.Fatalf("listenLocal(0,1): %v", err)
	}
	defer ln1.Close()

	tcp, ok := ln1.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr is not *net.TCPAddr: %T", ln1.Addr())
	}
	if tcp.IP == nil || !tcp.IP.IsLoopback() || tcp.IP.IsUnspecified() {
		t.Errorf("bound IP %v is not concrete loopback", tcp.IP)
	}
	if err := validateBindAddr(addr1); err != nil {
		t.Errorf("chosen addr %q failed validateBindAddr: %v", addr1, err)
	}

	// Occupy a concrete port, then ask for it again: auto-pick must advance.
	port := tcp.Port
	ln2, addr2, err := listenLocal(port, 8)
	if err != nil {
		t.Fatalf("listenLocal(%d,8): %v", port, err)
	}
	defer ln2.Close()
	tcp2 := ln2.Addr().(*net.TCPAddr)
	if tcp2.Port == port {
		t.Errorf("expected auto-pick to advance past the occupied port %d", port)
	}
	if !tcp2.IP.IsLoopback() {
		t.Errorf("advanced addr %q is not loopback", addr2)
	}
}

// TestListenLocalUnreachableOffLoopback dials the chosen port on every
// non-loopback IP of the host and asserts the connection is refused/times out —
// proving the socket is unreachable off loopback.
func TestListenLocalUnreachableOffLoopback(t *testing.T) {
	ln, _, err := listenLocal(0, 1)
	if err != nil {
		t.Fatalf("listenLocal: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interface addrs: %v", err)
	}
	tried := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		tried++
		dialAddr := net.JoinHostPort(ipnet.IP.String(), strconv.Itoa(port))
		c, derr := net.DialTimeout("tcp", dialAddr, 300*time.Millisecond)
		if derr == nil {
			c.Close()
			t.Errorf("port %d reachable on non-loopback %s — bind is NOT loopback-only", port, ipnet.IP)
		}
	}
	if tried == 0 {
		t.Log("no non-loopback IPs to probe (ok on an isolated host)")
	}
}

func TestServeHandlers(t *testing.T) {
	ring := NewRing(defaultRingCap)
	mux := newServeMux(ring, "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// /data.json on a ZERO-sample server: 200, "samples":[] (not null), pinned
	// headers. Exercises the pre-first-sample path end-to-end.
	resp, err := http.Get(srv.URL + "/data.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("/data.json status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("/data.json Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/data.json Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(body, `"samples":[]`) || strings.Contains(body, "null") {
		t.Errorf("/data.json empty body wrong: %s", body)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/data.json must NOT set Access-Control-Allow-Origin")
	}

	// After a sample is added, samples grows and is a non-empty array.
	ring.Add(Sample{T: time.Now(), DownBytesPerSec: 100, UpBytesPerSec: 50})
	resp, _ = http.Get(srv.URL + "/data.json")
	body = readAll(t, resp)
	var ld LiveData
	if err := json.Unmarshal([]byte(body), &ld); err != nil {
		t.Fatalf("/data.json not valid JSON: %v", err)
	}
	if len(ld.Samples) != 1 || ld.Iface != "en0" || ld.Interval != 1 {
		t.Errorf("/data.json after Add = %+v", ld)
	}

	// /healthz
	resp, _ = http.Get(srv.URL + "/healthz")
	body = readAll(t, resp)
	if resp.StatusCode != 200 || body != "ok" {
		t.Errorf("/healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("/healthz Content-Type = %q", ct)
	}

	// /favicon.ico => 204
	resp, _ = http.Get(srv.URL + "/favicon.ico")
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("/favicon.ico status = %d, want 204", resp.StatusCode)
	}

	// Unknown path => 404
	resp, _ = http.Get(srv.URL + "/nope")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}

	// Non-GET => 405
	req, _ := http.NewRequest("POST", srv.URL+"/data.json", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST /data.json status = %d, want 405", resp.StatusCode)
	}

	// Foreign Host header => 403 (DNS-rebinding defense).
	req, _ = http.NewRequest("GET", srv.URL+"/data.json", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host status = %d, want 403", resp.StatusCode)
	}
}

// TestServeDashboardLifecycle starts the real serveDashboard on a listenLocal
// listener, cancels its context, and asserts a clean return (exit 0, no panic on
// ErrServerClosed) and that the port is released afterwards.
func TestServeDashboardLifecycle(t *testing.T) {
	ln, addr, err := listenLocal(0, 1)
	if err != nil {
		t.Fatalf("listenLocal: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveDashboard(ctx, "en0", 250*time.Millisecond, time.Second, ln, addr, t.TempDir(), defaultMinuteTTL, defaultHourTTL, "HIDDEN (macOS-redacted)",
			5*time.Second, []string{"1.1.1.1", "8.8.8.8"}, "apple.com", true, defaultOutageStartThresh, defaultOutageEndThresh, defaultOutageTTL,
			defaultProcsInterval, defaultTopTalkersN, true, alertConfig{}, notify,
			false, DefaultConfig(), defaultThrottleSpacing, defaultThrottleJitterMax, defaultThrottleMaxBackoff, defaultThrottleSampleTTL)
	}()

	// Give it a moment to come up, then probe.
	time.Sleep(150 * time.Millisecond)
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz while serving: %v", err)
	}
	if got := readAll(t, resp); got != "ok" {
		t.Errorf("/healthz = %q, want ok", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveDashboard returned error on cancel: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serveDashboard did not return after ctx cancel")
	}

	// Port released: a fresh listenLocal on the same port succeeds.
	ln2, _, err := listenLocal(port, 1)
	if err != nil {
		t.Fatalf("port %d not released after shutdown: %v", port, err)
	}
	ln2.Close()
}

// TestRingConcurrentAddSnapshot locks the Ring's sampler-vs-handler safety under
// -race: Add in one goroutine while many goroutines Snapshot/Len.
func TestRingConcurrentAddSnapshot(t *testing.T) {
	ring := NewRing(256)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				ring.Add(Sample{T: time.Now(), DownBytesPerSec: float64(i)})
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = ring.Snapshot()
					_ = ring.Len()
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestDashboardHTMLHasNoExternalURLs is the REQUIRED offline guard: it runs
// against the EXACT served bytes (the dashboardHTML const) and fails if any
// external-resource substring slips in. Matchers fire only in fetch-triggering
// contexts, so JS "//" comments and the "//" inside an identifier do not false-
// positive.
func TestDashboardHTMLHasNoExternalURLs(t *testing.T) {
	// CASE-FOLD: CSS/HTML are case-insensitive but strings.Contains is not, so
	// lowercase the haystack ONCE and match all (lowercase) banned tokens against it
	// — otherwise URL(, HTTP://, @FONT-FACE, <IMG, .WOFF etc. would sail through.
	low := strings.ToLower(dashboardHTML)
	banned := []string{
		"http://", "https://",
		// protocol-relative PREFIXED forms (NEVER a bare "//": it would match the
		// SVGNS "http"+"://..." concat AND every JS "//" comment).
		`src="//`, `src='//`, `href="//`, `href='//`,
		// BLANKET url( ban: the page uses ZERO url() (scanline is a
		// repeating-linear-gradient, SVGNS is string-concat, area fills are flat
		// polygons), so this catches every form (relative, scheme-less, data:, quoted)
		// with no false positive.
		"url(",
		"@import", "@font-face", "@font",
		"<link", "<iframe", "<img", "<image", "<source", // <img does NOT match <image — need both
		"srcset", "poster=",
		".woff", ".woff2", ".ttf", ".otf", ".eot",
	}
	for _, sub := range banned {
		if strings.Contains(low, sub) {
			t.Errorf("dashboardHTML contains forbidden external-resource token %q", sub)
		}
	}
}

// TestDashboardHTMLHasLayoutToggle is the POSITIVE feature-present guard for the
// TALL<->WIDE layout toggle. The no-external-URL guard above is purely NEGATIVE and
// would stay green even if a future edit silently DELETED the whole toggle, so this
// asserts the required pieces are PRESENT: the root state class, the paired-chart
// wrapper, the control's id, and the two UNICODE glyph entities (checking the glyph
// entities locks the unicode affordance — a swap to raw UTF-8 or a silent drop would
// pass the negative guard but fail here, while a swap to <img> fails the negative one).
func TestDashboardHTMLHasLayoutToggle(t *testing.T) {
	required := []string{
		"layout-wide",       // the root presence class the toggle + CSS pivot on
		"col-pair",          // the REACH|HISTORY pairing wrapper
		`id="layouttoggle"`, // the two-segment control
		"&#9636;",           // U+25A4 TALL glyph (numeric entity, no literal-UTF8 ambiguity)
		"&#8862;",           // U+229E WIDE glyph
		"netdebug.layout",   // the localStorage persistence key
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required layout-toggle token %q", sub)
		}
	}
}

// TestDashboardHTMLWideShortensCharts pins the WIDE single-screen MECHANISM so it
// can't be silently reverted past the green negative (no-external-URL) guard. It pins
// the INTENT (token names + a number-free structural anchor), NOT the tuning magic
// numbers — the pair-floor and tile-min are explicit, sanctioned dials, so pinning
// "minmax(600px" would turn this RED on a legitimate re-tune to 560/640. Pinning both
// LIVE_H and HIST_H proves the heights DIFFER BY MODE (the fix), not merely that the
// redraw plumbing exists as a stub.
func TestDashboardHTMLWideShortensCharts(t *testing.T) {
	required := []string{
		"applyChartDims",          // the layout-aware viewBox-height mechanism / redraw wiring
		"redrawAll",               // redraw-on-toggle from cached payloads
		"LIVE_H",                  // per-mode LIVE viewBox-height table — the shortening LEVER
		"HIST_H",                  // per-mode HISTORY viewBox-height table — the shortening LEVER
		"html.layout-wide .tiles", // the WIDE tile-compaction rule — a stable, number-free anchor
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing WIDE single-screen mechanism token %q", sub)
		}
	}
	// The broken 720 floor (failed to pair at 1440) must be GONE; tolerate any
	// 560/600/640 re-dial by banning only the specific old value.
	if strings.Contains(dashboardHTML, "minmax(720px") {
		t.Errorf("dashboardHTML still has the broken col-pair floor minmax(720px (should pair at 1440)")
	}
}

// TestDashboardHTMLTallChartsUnchanged pins the "TALL byte-behavior-identical"
// mandate: the SERVED markup still ships the three TALL viewBox literals (they appear
// contiguously in the HTML, not via concat). A future edit to a served viewBox or to
// LIVE_H.tall/HIST_H.tall would silently alter TALL and still pass every other guard.
func TestDashboardHTMLTallChartsUnchanged(t *testing.T) {
	required := []string{
		`viewBox="0 0 900 320"`, // LIVE, served/TALL
		`viewBox="0 0 900 260"`, // HISTORY, served/TALL
		`viewBox="0 0 900 150"`, // REACH, served/TALL (unchanged in both modes)
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing served TALL viewBox literal %q (TALL must stay byte-identical)", sub)
		}
	}
}

// TestDataJSONMeta pins the Phase-6 meta object: a nil Meta (the pure test path)
// omits the "meta" key entirely (pointer omitempty) and still normalizes
// samples/events to [] with no "null"; a set Meta carries ssid_label + uptime_sec;
// and the serve-path sentinel (ssidDisplay("") output) is passed through verbatim.
func TestDataJSONMeta(t *testing.T) {
	// nil Meta => no "meta" key, no "null".
	b, err := dataJSON(LiveData{Iface: "en0", Interval: 1})
	if err != nil {
		t.Fatalf("dataJSON(nil Meta): %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"meta"`) {
		t.Errorf("nil Meta must omit the meta key: %s", s)
	}
	if !strings.Contains(s, `"samples":[]`) || !strings.Contains(s, `"events":[]`) || strings.Contains(s, "null") {
		t.Errorf("nil Meta path broke samples/events normalization: %s", s)
	}

	// Set Meta => ssid_label + uptime_sec present.
	b, err = dataJSON(LiveData{Iface: "en0", Interval: 1, Meta: &LiveMeta{SSIDLabel: "Home", UptimeSec: 12.5}})
	if err != nil {
		t.Fatalf("dataJSON(Meta): %v", err)
	}
	s = string(b)
	if !strings.Contains(s, `"meta"`) || !strings.Contains(s, `"ssid_label":"Home"`) || !strings.Contains(s, `"uptime_sec":12.5`) {
		t.Errorf("meta object missing/incorrect: %s", s)
	}

	// SERVE-PATH SENTINEL: the serve path always sends ssidDisplay() output, never
	// empty — so an unset label becomes the honest sentinel in meta.ssid_label.
	b, err = dataJSON(LiveData{Iface: "en0", Interval: 1, Meta: &LiveMeta{SSIDLabel: ssidDisplay(""), UptimeSec: 1}})
	if err != nil {
		t.Fatalf("dataJSON(sentinel Meta): %v", err)
	}
	if !strings.Contains(string(b), `"ssid_label":"HIDDEN (macOS-redacted)"`) {
		t.Errorf("serve-path sentinel not passed through: %s", b)
	}

	// Phase 9 meeting badge additivity. A meta with meeting_status set carries BOTH
	// meeting keys...
	b, err = dataJSON(LiveData{Iface: "en0", Interval: 1,
		Meta: &LiveMeta{SSIDLabel: "Home", MeetingStatus: meetRisky, MeetingReason: "jitter elevated (35.0 ms)"}})
	if err != nil {
		t.Fatalf("dataJSON(meeting Meta): %v", err)
	}
	s = string(b)
	if !strings.Contains(s, `"meeting_status":"RISKY"`) || !strings.Contains(s, `"meeting_reason":"jitter elevated (35.0 ms)"`) {
		t.Errorf("meeting keys missing when set: %s", s)
	}

	// ...and a meta with ONLY ssid_label/uptime_sec OMITS both meeting keys (the
	// omitempty additivity proof — without this the additive-safety claim is unverified).
	b, err = dataJSON(LiveData{Iface: "en0", Interval: 1, Meta: &LiveMeta{SSIDLabel: "Home", UptimeSec: 3}})
	if err != nil {
		t.Fatalf("dataJSON(meeting-less Meta): %v", err)
	}
	s = string(b)
	if strings.Contains(s, "meeting_status") || strings.Contains(s, "meeting_reason") {
		t.Errorf("unset meeting fields must be omitted (omitempty additivity): %s", s)
	}
}

// TestServePathNoCloudflareSpeedRefs is the REQUIRED static guard that the serve/
// sample/default path hits the Cloudflare SPEED endpoint ZERO times: serve.go's
// source must never reference the speed/bufferbloat HTTP machinery. The guarantee is
// structural today (serve.go's goroutines are sampler/link/reach/prune/http only) but
// is one refactor away from silently breaking, so CI pins it.
func TestServePathNoCloudflareSpeedRefs(t *testing.T) {
	b, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	src := string(b)
	for _, banned := range []string{"speedClient", "runSpeed", "runBufferbloat", "measureLatencyDuring", "__up", "__down", "speed.cloudflare.com"} {
		if strings.Contains(src, banned) {
			t.Errorf("serve.go must not reference %q — the serve/sample/default path must generate no saturating load and hit Cloudflare 0 times", banned)
		}
	}
}

// TestCompareGlobIgnoresBufferbloat pins that --compare's nettest-*.json glob matches
// NEITHER a bufferbloat-*.json NOR a speedtest-*.json file dropped in --out, so a
// bufferbloat report is never read as a probe run (and never collides with speedtest).
func TestCompareGlobIgnoresBufferbloat(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("nettest-20260101-000000.json")
	write("bufferbloat-20260101-000001.json")
	write("speedtest-20260101-000002.json")

	entries, err := filepath.Glob(filepath.Join(dir, "nettest-*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 1 || !strings.HasPrefix(filepath.Base(entries[0]), "nettest-") {
		t.Fatalf("nettest-*.json glob must match ONLY the nettest file, got %v", entries)
	}
}

// TestDataJSONEventsNeverNull mirrors the "samples":[] house rule for the Phase-4
// events array: a nil Events slice must marshal "events":[] so the page's
// events.forEach never hits null.
func TestDataJSONEventsNeverNull(t *testing.T) {
	b, err := dataJSON(LiveData{Iface: "en0", Interval: 1}) // Events nil
	if err != nil {
		t.Fatalf("dataJSON: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"events":[]`) {
		t.Errorf("nil Events did not normalize to []: %s", s)
	}
	if strings.Contains(s, `"events":null`) {
		t.Errorf("events must never be null: %s", s)
	}
}

// TestReachWireShape pins the Phase-7 additive wire: a nil Reach omits BOTH the
// "reach" and "reach_series" keys (never ":null"), alongside the existing
// samples/events "[]" + "no meta" normalization; a populated Reach carries the
// object with class + the always-present loss keys even at 0% (the omitempty
// regression guard).
func TestReachWireShape(t *testing.T) {
	// nil Reach => neither key, no "null".
	b, err := dataJSON(LiveData{Iface: "en0", Interval: 1})
	if err != nil {
		t.Fatalf("dataJSON(nil Reach): %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"reach"`) || strings.Contains(s, `"reach_series"`) {
		t.Errorf("nil Reach must omit both reach keys: %s", s)
	}
	if strings.Contains(s, `"reach":null`) || strings.Contains(s, `"reach_series":null`) {
		t.Errorf("reach keys must be ABSENT, never null: %s", s)
	}

	// An empty (non-nil) series also omits via omitempty (warm-up window).
	b, _ = dataJSON(LiveData{Iface: "en0", Interval: 1, ReachSeries: []ReachSample{}})
	if strings.Contains(string(b), `"reach_series"`) {
		t.Errorf("empty reach_series must be omitted: %s", b)
	}

	// Populated Reach: healthy 0% loss => class ok, loss keys present AND 0, RTT/DNS
	// present. The global no-"null" blanket is deliberately NOT applied here.
	reach := ReachSample{
		T: time.Now(), Class: "ok", GatewayOK: true, GatewayRTTMs: 5.1, GatewayLoss: 0,
		InternetOK: true, InternetRTTMs: 12.3, InternetLoss: 0, JitterMs: 2.0,
		DNSMs: 7.5, DNSOK: true, GatewayAddr: "10.0.0.1", InternetAddr: "1.1.1.1",
	}
	series := []ReachSample{reach}
	b, err = dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &reach, ReachSeries: series})
	if err != nil {
		t.Fatalf("dataJSON(populated Reach): %v", err)
	}
	s = string(b)
	for _, want := range []string{
		`"reach":{`, `"class":"ok"`, `"gateway_ok":true`, `"internet_ok":true`,
		`"gateway_loss_pct":0`, `"internet_loss_pct":0`, `"internet_addr":"1.1.1.1"`,
		`"reach_series":[`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("populated reach missing %s: %s", want, s)
		}
	}

	// A gateway_only sample marshals a legitimate internet_ok:false and omits
	// internet_rtt_ms, but internet_loss_pct stays present (100).
	down := ReachSample{T: time.Now(), Class: "gateway_only", GatewayOK: true, GatewayRTTMs: 5,
		GatewayLoss: 0, InternetOK: false, InternetLoss: 100, DNSOK: false}
	b, _ = dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &down})
	s = string(b)
	if strings.Contains(s, `"internet_rtt_ms"`) {
		t.Errorf("internet_rtt_ms must be omitted when internet is down: %s", s)
	}
	if !strings.Contains(s, `"internet_loss_pct":100`) || !strings.Contains(s, `"internet_ok":false`) {
		t.Errorf("gateway_only must carry internet_loss_pct:100 + internet_ok:false: %s", s)
	}
}

// TestReachTargetsWire pins the REACHABILITY drill-down additive wire: a populated
// Targets marshals reach.targets as a non-empty array carrying kind/addr/host/loss_pct/
// ok/best; a ReachSample WITHOUT Targets still OMITS the key (so every existing
// pure/literal reach path stays byte-identical); and the reach_series STRIP guard: the
// per-target table rides the LATEST reach object only, never a reach_series element.
func TestReachTargetsWire(t *testing.T) {
	reach := ReachSample{
		T: time.Now(), Class: "ok", GatewayOK: true, InternetOK: true, InternetAddr: "8.8.8.8",
		Targets: []ReachTarget{
			{Kind: "gateway", Addr: "10.0.0.1", OK: true, RTTMs: 5, LossPct: 0, Recv: 3, Sent: 3},
			{Kind: "internet", Addr: "1.1.1.1", Host: "one.one.one.one", OK: true, RTTMs: 30, LossPct: 0, Recv: 3, Sent: 3},
			{Kind: "internet", Addr: "8.8.8.8", Host: "dns.google", OK: true, RTTMs: 12, LossPct: 0, Recv: 3, Sent: 3, Best: true},
			{Kind: "dns", Addr: "apple.com", OK: true, RTTMs: 7},
		},
	}
	b, err := dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &reach})
	if err != nil {
		t.Fatalf("dataJSON(targets): %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"targets":[`, `"kind":"gateway"`, `"kind":"internet"`, `"kind":"dns"`,
		`"host":"one.one.one.one"`, `"host":"dns.google"`, `"best":true`,
		`"addr":"8.8.8.8"`, `"loss_pct":0`, `"ok":true`, `"recv":3`, `"sent":3`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("populated targets missing %s: %s", want, s)
		}
	}
	// No target tag may false-match the substring assertions elsewhere.
	if strings.Contains(s, `"dns_ms"`) {
		t.Errorf("target rtt_ms tag must not emit dns_ms: %s", s)
	}

	// A ReachSample without Targets omits the key (existing pure paths unaffected).
	bare := ReachSample{T: time.Now(), Class: "ok", GatewayOK: true}
	b2, _ := dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &bare})
	if strings.Contains(string(b2), `"targets"`) {
		t.Errorf("a ReachSample without Targets must omit the key: %s", b2)
	}

	// reach_series STRIP: series elements carry Targets going in; after the strip the
	// "targets" array appears EXACTLY ONCE (the latest reach object), never per element.
	series := stripReachSeriesTargets([]ReachSample{reach, reach, reach})
	for i, e := range series {
		if e.Targets != nil {
			t.Errorf("series[%d].Targets must be nil after strip", i)
		}
	}
	b3, _ := dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &reach, ReachSeries: series})
	if c := strings.Count(string(b3), `"targets":[`); c != 1 {
		t.Errorf(`"targets" must appear exactly once (latest reach only), got %d: %s`, c, b3)
	}
	// The strip must not mutate the shared latest reach object.
	if reach.Targets == nil {
		t.Errorf("strip must not touch the latest reach object's Targets")
	}
}

// TestDashboardHTMLHasReachDrilldown is the POSITIVE feature-present guard for the
// REACHABILITY click-to-expand targets drill-down (sibling of HasProcDrilldown): the
// served bytes CONTAIN the expand state, the client-side render path, the focusable
// toggle with aria-expanded, and textContent-based rendering — and CONTAIN NO
// .innerHTML (reverse-DNS host + raw addr are attacker-ish).
func TestDashboardHTMLHasReachDrilldown(t *testing.T) {
	required := []string{
		"reachExpanded",      // the ephemeral expand state
		"renderReachTargets", // the client-side (fetch-free) render of reach.targets
		"toggleReachTargets", // the click/keyboard expand entry point
		"reachTargetRow",     // the per-row textContent builder
		`id="reachtoggle"`,   // the focusable toggle control
		`aria-expanded`,      // accessible expand affordance
		"createTextNode",     // textContent-based rendering (host/addr are adversarial)
		`id="reachtargetswrap"`,
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required reach drill-down token %q", sub)
		}
	}
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML (reach host/addr render via textContent only)")
	}
}

// TestDashboardHTMLHasTileInfo is the POSITIVE feature-present guard for the
// educational stat-tile INFO CARD (sibling of HasReachDrilldown): the served bytes
// CONTAIN the clickable per-tile affordance, the ONE reusable accessible dialog, the
// client-side open/close + live-interpretation wiring, and the per-metric help
// content — and CONTAIN NO .innerHTML (the help is built via createElement/textContent).
// A future edit that silently drops the info card would pass the negative no-external
// guard but fail here.
func TestDashboardHTMLHasTileInfo(t *testing.T) {
	required := []string{
		// clickable affordance on the tiles (a sample across metrics)
		`data-metric="meeting"`,
		`data-metric="rssi"`,
		`data-metric="jitter"`,
		`data-metric="loss"`,
		`aria-haspopup="dialog"`, // tiles advertise the dialog they open
		// the ONE reusable accessible dialog
		`id="infocard"`,
		`role="dialog"`,
		`aria-modal="true"`,
		`id="infocard-title"`,
		`id="infocard-close"`,
		// the open/close/a11y + live-read wiring
		"METRIC_HELP",         // the per-metric content model
		"openInfoCard",        // open entry point
		"closeInfoCard",       // close (Esc / backdrop / × / re-click)
		"icInterp",            // live current-value interpretation
		"infocardClose.focus", // focus moves into the dialog on open
		// per-metric help content keys (distinctive author-written strings)
		"What it is",
		"Measures / units",
		"Good / expected values",
		"-67 dBm is the usual floor",         // RSSI good-values help
		"GOOD / RISKY / BAD",                 // MEETING verdict help
		"the main enemy of calls and gaming", // JITTER what-it-is help
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required tile-info token %q", sub)
		}
	}
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML (info-card help is built via createElement/textContent only)")
	}
}

// TestDashboardHTMLHasReachTableClarity is the POSITIVE guard for the self-explanatory
// REACHABILITY targets table (Part A): the served bytes CONTAIN the per-cell clarifying
// tooltips (ping-packet LOSS, the DNS-is-a-resolve note, the ACTIVE flip explanation)
// and the one-line legend under the table — and CONTAIN NO .innerHTML. A future edit
// that silently drops the clarifying copy would still pass the no-external guard but
// fail here.
func TestDashboardHTMLHasReachTableClarity(t *testing.T) {
	required := []string{
		// LOSS cell: "(recv/sent)" = ping packets this cycle (count read live, not hardcoded)
		"ping replies this cycle — loss = missed replies",
		// DNS row LOSS "—": a resolve, not a ping
		"DNS is a name lookup (resolve time), not a ping",
		// ACTIVE badge: selected best-of internet target feeding INTERNET RTT, flips each cycle
		"its RTT feeds the INTERNET RTT tile",
		"Flips to whichever internet target is healthiest each cycle",
		// the one-line legend/caption under the table
		`class="rtargets-legend"`,
		"the DNS row is a resolve, not a ping",
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required reach-table clarity token %q", sub)
		}
	}
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML (reach-table clarity renders via textContent/attributes only)")
	}
}

// TestDashboardHTMLHasMarkerDetail is the POSITIVE guard for the clickable LIVE-chart
// event-marker detail card (Part B): the served bytes CONTAIN the clickable marker group,
// the once-bound (redraw-surviving) #chart click delegation, the shared-#infocard reuse,
// the per-kind content, the OUTAGE JOURNAL cross-reference, and the honest no-match
// message — and CONTAIN NO .innerHTML (the card is built via createElement/textContent).
func TestDashboardHTMLHasMarkerDetail(t *testing.T) {
	required := []string{
		// clickable marker group + the stashed stable event snapshot
		`"class": "evmarker"`,
		"grp.ndEvent = ev",
		".evmarker { cursor: pointer; }",
		// the delegated (redraw-surviving) chart click + walk-up
		`chart.addEventListener("click"`,
		"markerGroupFrom",
		// reuse of the shared dialog + the open path
		"openMarkerCard",
		"MARKER_CARD_TITLE",
		`drop: "Link drop"`,
		// OUTAGE JOURNAL cross-reference from the already-polled data
		"lastOutages",
		"nearestOutage",
		"onsetContextText",
		"Onset radio context",
		"See the full log in the Outage Journal below",
		// honest no-match message (no fabricated detail)
		"Brief drop — no logged outage",
		// roam / band-change plain-English one-liners
		"A roam = your Mac moved to a different access point",
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required marker-detail token %q", sub)
		}
	}
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML (marker detail card is built via createElement/textContent only)")
	}
}

// TestDashboardHTMLHasRoamJournal is the POSITIVE feature-present guard for the
// Phase-14 BAND / CHANNEL CHANGE JOURNAL: the section heading, the three summary-tile
// ids, the table/poll wiring, and the clean-steer vs with-drop flag — so a silent
// deletion fails HERE even though the negative no-external-URL guard would stay green.
func TestDashboardHTMLHasRoamJournal(t *testing.T) {
	required := []string{
		"// BAND / CHANNEL CHANGE JOURNAL", // section heading
		`id="rcount"`,                      // COUNT TODAY tile
		`id="rlast"`,                       // LAST CHANGE tile
		`id="rmode"`,                       // MOST-STEERED-TO BAND tile
		`id="roambody"`,                    // the table body
		"/roam.json",                       // the polled endpoint
		"pollRoam",                         // the poller
		"renderRoam",                       // the renderer
		"most_steered_to_band",             // the cadence field read
		"with drop",                        // the WITH DROP flag label
		"clean steer",                      // the CLEAN STEER flag label
		"after pause",                      // the approximate-time note
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required roam-journal token %q", sub)
		}
	}
}

// TestRunLinkLoopCadence is the "no 1s system_profiler storm" unit proof: with an
// injected spy readLink and a short interval, the loop calls readLink at the link
// cadence (≈ window/interval), stores the latest snapshot, and exits promptly on
// ctx cancel with NO late call afterward.
func TestRunLinkLoopCadence(t *testing.T) {
	var calls int32
	// Phase 10: the injected reader returns BOTH a LinkSnap and the neighbor list from
	// ONE call, and runLinkLoop must store BOTH holders from that single read.
	spy := func() (LinkSnap, []Neighbor) {
		atomic.AddInt32(&calls, 1)
		return LinkSnap{Band: "5 GHz", Channel: "157/80", OK: true},
			[]Neighbor{{ChannelNum: 149, Band: "5 GHz", Width: "80MHz", RSSI: -46}}
	}
	holder := &linkHolder{}
	airspaceH := &airspaceHolder{}
	interval := 20 * time.Millisecond
	window := 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runLinkLoop(ctx, interval, spy, holder, airspaceH); close(done) }()

	time.Sleep(window)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runLinkLoop did not exit promptly on ctx cancel")
	}

	n := atomic.LoadInt32(&calls)
	// ~10 expected (window/interval). A 1s sampler cadence would yield 0 over 200ms,
	// so a healthy count proves the loop ticks at the injected interval, not 1/sec.
	// One read per tick also proves ONE exec feeds BOTH holders (no second process).
	if n < 4 || n > 25 {
		t.Errorf("read calls = %d, want ~%d at the link cadence", n, int(window/interval))
	}
	if holder.load() == nil {
		t.Error("link holder should hold the latest snapshot after reads")
	}
	// BOTH holders populate from the SAME tick, and MyChannel is carried from that
	// tick's LinkSnap (a self-consistent pair from one read).
	as := airspaceH.load()
	if as == nil {
		t.Fatal("airspace holder should hold a snapshot after reads")
	}
	if as.MyChannel != "157/80" {
		t.Errorf("airspace MyChannel = %q, want %q (carried from the tick's LinkSnap)", as.MyChannel, "157/80")
	}
	if len(as.Neighbors) != 1 || as.Neighbors[0].ChannelNum != 149 {
		t.Errorf("airspace neighbors not stored from the tick: %+v", as.Neighbors)
	}

	// No late call fires after cancel.
	settled := atomic.LoadInt32(&calls)
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != settled {
		t.Errorf("readLink called %d more times after cancel", got-settled)
	}
}

// TestSamplerStampsFromHolderNoExec proves the 1s sampler stamps the holder's
// snapshot onto each real Sample via a plain atomic load (no exec of its own), and
// that a nil linkProvider (the --sample path) leaves Link=nil. It samples lo0,
// which always exists and is up, so readCounters succeeds deterministically
// without depending on Wi-Fi state.
func TestSamplerStampsFromHolderNoExec(t *testing.T) {
	collect := func(lp func() *LinkSnap) []Sample {
		var mu sync.Mutex
		var got []Sample
		ring := NewRing(256)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		runSampleQuiet(ctx, "lo0", 30*time.Millisecond, ring,
			func(s Sample) { mu.Lock(); got = append(got, s); mu.Unlock() }, lp)
		mu.Lock()
		defer mu.Unlock()
		return append([]Sample(nil), got...)
	}

	holder := &linkHolder{}
	holder.store(LinkSnap{Band: "5 GHz", Channel: "149/80", OK: true})
	stamped := collect(func() *LinkSnap { return holder.load() })
	if len(stamped) == 0 {
		t.Fatal("no samples captured from lo0 (expected several)")
	}
	for i, s := range stamped {
		if s.Link == nil || s.Link.Band != "5 GHz" {
			t.Fatalf("sample[%d] not stamped from holder: %+v", i, s.Link)
		}
	}

	// --sample path: linkProvider nil => no Link stamped on any sample.
	plain := collect(nil)
	if len(plain) == 0 {
		t.Fatal("no samples captured on the nil-linkProvider path")
	}
	for i, s := range plain {
		if s.Link != nil {
			t.Fatalf("sample[%d] carries a Link on the nil-provider (--sample) path: %+v", i, s.Link)
		}
	}
}

// TestNewServer asserts the hardened *http.Server carries every timeout and the
// header cap at the Phase-5 values. These are a security surface (slow-loris
// bounds + a header budget), so they are asserted by test, not left to code review.
func TestNewServer(t *testing.T) {
	mux := http.NewServeMux()
	srv := newServer(mux)

	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 15 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == 0 {
				t.Fatalf("%s is zero (unbounded) — slow-loris gap", tc.name)
			}
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	if srv.MaxHeaderBytes != 1<<16 {
		t.Fatalf("MaxHeaderBytes = %d, want %d (64KB)", srv.MaxHeaderBytes, 1<<16)
	}
	if srv.Handler == nil {
		t.Fatal("Handler is nil — the mux was not wired in")
	}
	// The server must carry NO Addr: the loopback bind is listenLocal's job, and an
	// Addr here would let a caller route around it via ListenAndServe.
	if srv.Addr != "" {
		t.Fatalf("Addr = %q, want empty (bind is listenLocal's job only)", srv.Addr)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestOutagesEndpoint pins the Phase-8 /outages.json wire: { outages, cadence } with
// arrays NEVER null, pinned Content-Type + no-store + ZERO CORS, a served record after
// append, and a 403 (no CORS header) on a non-loopback Host (must_fix #6).
func TestOutagesEndpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, store, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Empty journal => {"outages":[],"cadence":{...}} — never null, pinned headers.
	resp, err := http.Get(srv.URL + "/outages.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("/outages.json status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("/outages.json Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/outages.json Cache-Control = %q, want no-store", cc)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/outages.json must NOT set Access-Control-Allow-Origin")
	}
	if !strings.Contains(body, `"outages":[]`) || !strings.Contains(body, `"cadence"`) {
		t.Errorf("/outages.json empty shape wrong: %s", body)
	}
	if strings.Contains(body, "null") {
		t.Errorf("/outages.json must never contain null: %s", body)
	}

	// After a closed outage appends, it is served.
	now := time.Now()
	if err := store.appendOutage(Outage{
		Start: now.Add(-time.Hour), End: now.Add(-time.Hour).Add(90 * time.Second),
		DurationSec: 90, Cause: causeLinkDrop, Channel: "149", RSSI: -55,
	}); err != nil {
		t.Fatalf("appendOutage: %v", err)
	}
	resp, _ = http.Get(srv.URL + "/outages.json")
	body = readAll(t, resp)
	var wire struct {
		Outages []Outage `json:"outages"`
		Cadence Cadence  `json:"cadence"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("/outages.json not valid JSON: %v", err)
	}
	if len(wire.Outages) != 1 || wire.Outages[0].Cause != causeLinkDrop || wire.Outages[0].Channel != "149" {
		t.Errorf("/outages.json did not serve the appended outage: %+v", wire.Outages)
	}
	if wire.Cadence.CountToday != 1 || wire.Cadence.ModeChannel != "149" {
		t.Errorf("/outages.json cadence wrong: %+v", wire.Cadence)
	}

	// Foreign Host => 403 (DNS-rebinding defense) and NO CORS header on the response.
	req, _ := http.NewRequest("GET", srv.URL+"/outages.json", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("403 path must not set Access-Control-Allow-Origin")
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host /outages.json status = %d, want 403", resp.StatusCode)
	}
}

// TestOutagesEndpointNilStore: a handler-only mux (no store, the --sample-less test
// shape) still serves well-formed empty arrays (never null) — proving the endpoint
// needs no store on the default path.
func TestOutagesEndpointNilStore(t *testing.T) {
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/outages.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"outages":[]`) || strings.Contains(body, "null") {
		t.Errorf("nil-store /outages.json shape wrong: %d %s", resp.StatusCode, body)
	}
}

// TestRoamEndpoint pins the Phase-14 /roam.json wire: { roams, cadence } with pinned
// headers, []-not-null, ZERO CORS, a served crafted transition, and the clean-steer vs
// with-drop distinction. Mirrors TestOutagesEndpoint.
func TestRoamEndpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, store, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Empty journal => {"roams":[],"cadence":{...}} — never null, pinned headers.
	resp, err := http.Get(srv.URL + "/roam.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("/roam.json status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("/roam.json Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/roam.json Cache-Control = %q, want no-store", cc)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/roam.json must NOT set Access-Control-Allow-Origin")
	}
	if !strings.Contains(body, `"roams":[]`) || !strings.Contains(body, `"cadence"`) {
		t.Errorf("/roam.json empty shape wrong: %s", body)
	}
	if strings.Contains(body, "null") {
		t.Errorf("/roam.json must never contain null: %s", body)
	}

	// Feed a CLEAN band steer (5 GHz -> 2.4 GHz, no drop) and a DROP-triggered change.
	now := time.Now()
	if err := store.appendRoam(Transition{
		T: now.Add(-10 * time.Minute), FromBand: "5 GHz", ToBand: "2.4 GHz",
		FromChannel: "149", ToChannel: "6", RSSI: -58, WithDrop: false,
	}); err != nil {
		t.Fatalf("appendRoam clean: %v", err)
	}
	if err := store.appendRoam(Transition{
		T: now.Add(-2 * time.Minute), FromBand: "2.4 GHz", ToBand: "5 GHz",
		FromChannel: "6", ToChannel: "149", RSSI: -70, WithDrop: true, AfterPause: true,
	}); err != nil {
		t.Fatalf("appendRoam drop: %v", err)
	}

	resp, _ = http.Get(srv.URL + "/roam.json")
	body = readAll(t, resp)
	var wire struct {
		Roams   []Transition `json:"roams"`
		Cadence RoamCadence  `json:"cadence"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("/roam.json not valid JSON: %v", err)
	}
	if len(wire.Roams) != 2 {
		t.Fatalf("/roam.json served %d roams, want 2: %+v", len(wire.Roams), wire.Roams)
	}
	// Ascending by T: [0] is the clean steer, [1] the drop-triggered change.
	if wire.Roams[0].WithDrop {
		t.Errorf("roam[0] should be a CLEAN steer, got with_drop=true")
	}
	if !wire.Roams[1].WithDrop || !wire.Roams[1].AfterPause {
		t.Errorf("roam[1] should be WITH DROP + after_pause, got %+v", wire.Roams[1])
	}
	if wire.Cadence.CountToday != 2 {
		t.Errorf("/roam.json cadence CountToday = %d, want 2", wire.Cadence.CountToday)
	}

	// Foreign Host => 403 (DNS-rebinding defense) and NO CORS header.
	req, _ := http.NewRequest("GET", srv.URL+"/roam.json", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("403 path must not set Access-Control-Allow-Origin")
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host /roam.json status = %d, want 403", resp.StatusCode)
	}
}

// TestRoamEndpointNilStore: a handler-only mux (no store) still serves well-formed
// empty arrays (never null). Mirrors TestOutagesEndpointNilStore.
func TestRoamEndpointNilStore(t *testing.T) {
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/roam.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"roams":[]`) || strings.Contains(body, "null") {
		t.Errorf("nil-store /roam.json shape wrong: %d %s", resp.StatusCode, body)
	}
}

// TestRoamJournalSurvivesRestart proves the roam journal PERSISTS exactly like the
// outage journal: records appended to one store reload from the SAME dir in a fresh
// store (same JSONL file, same flock seam).
func TestRoamJournalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h1, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open store 1: %v", err)
	}
	base := time.Now().Add(-time.Hour)
	_ = h1.appendRoam(Transition{T: base, FromBand: "5 GHz", ToBand: "2.4 GHz", FromChannel: "149", ToChannel: "6", RSSI: -58})
	_ = h1.appendRoam(Transition{T: base.Add(10 * time.Minute), FromChannel: "6", ToChannel: "11", RSSI: -60})
	before := h1.RoamsSnapshot()
	if len(before) != 2 {
		t.Fatalf("snapshot before = %d, want 2", len(before))
	}
	if err := h1.Close(); err != nil {
		t.Fatalf("close store 1: %v", err)
	}

	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer h2.Close()
	after := h2.RoamsSnapshot()
	if len(after) != 2 {
		t.Errorf("roam journal did not survive restart: got %d records, want 2", len(after))
	}
	if after[0].ToBand != "2.4 GHz" || after[0].FromBand != "5 GHz" {
		t.Errorf("reloaded roam record wrong: %+v", after[0])
	}
}

// TestSamplePathOpensNoStoreNorJournal LOCKS the opt-in guard: the --sample sampler
// loop (sink nil, linkProvider nil — the bare/terminal path) must open NO history store
// and write NO outage journal. With $HOME pointed at a temp dir, the default config dir
// (os.UserConfigDir()/netdebug) must not even be created.
func TestSamplePathOpensNoStoreNorJournal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // darwin: os.UserConfigDir() => $HOME/Library/Application Support

	ring := NewRing(defaultRingCap)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	runSampleLoop(ctx, "lo0", 30*time.Millisecond, ring, false, nil, nil) // --sample shape

	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "netdebug")); !os.IsNotExist(err) {
		t.Errorf("sample path created a netdebug config dir (opt-in violated): stat err = %v", err)
	}
	// Belt-and-suspenders: no outage journal anywhere under the temp HOME.
	var found bool
	_ = filepath.Walk(tmp, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && info.Name() == outageFileName {
			found = true
		}
		return nil
	})
	if found {
		t.Error("sample path wrote an outage journal (opt-in violated)")
	}
}

// TestNoActiveScanCommands is the AUTOMATED guard for Phase 10's single hard
// constraint (phase10 §0): the airspace/serve radio read is the PASSIVE, cached
// `system_profiler SPAirPortDataType` scan ONLY. An ACTIVE scan — the deprecated
// airport CLI's scan flag, a wdutil air scan, or a networksetup Wi-Fi switch —
// forces the radio off-channel and can DROP the link (the user's exact problem).
// This replaces manual review of the constraint (sibling to
// TestDashboardHTMLHasNoExternalURLs). The pre-existing, user-gated --live switcher
// in wifi.go is deliberately OUT of scope: this guard inspects only the radio
// data-path files, which must exec nothing but system_profiler + ifconfig.
func TestNoActiveScanCommands(t *testing.T) {
	dataPath := []string{"airspace.go", "serve.go", "signal.go"}
	allowedExec := map[string]bool{"system_profiler": true, "ifconfig": true}
	reExec := regexp.MustCompile(`exec\.Command\(\s*"([^"]+)"`)
	for _, f := range dataPath {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)
		for _, m := range reExec.FindAllStringSubmatch(s, -1) {
			if !allowedExec[m[1]] {
				t.Errorf("%s execs %q on the radio data path; only system_profiler/ifconfig are passive-safe", f, m[1])
			}
		}
		low := strings.ToLower(s)
		for _, banned := range []string{"airport -s", "wdutil scan", "-setairportnetwork"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s contains active-scan token %q (forbidden on the passive data path)", f, banned)
			}
		}
	}

	// GLOBAL regression ban: the literal active-scan invocations appear in NO
	// non-test Go source (they never have; this catches a future regression anywhere,
	// not only on the three data-path files). "-setairportnetwork" is NOT globally
	// banned because wifi.go's existing --live switcher legitimately uses it.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		low := strings.ToLower(string(src))
		for _, banned := range []string{"airport -s", "wdutil scan"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s contains active-scan token %q (forbidden)", f, banned)
			}
		}
	}
}

// TestAirspaceJSONNilHolder pins the empty-payload contract the /airspace.json
// handler serves when airspaceH is nil (handler-only test) or holds nil (serve
// warm-up): arrays are [] (never null), my_channel is "", recommendation is null,
// and NO array ever serializes as null.
func TestAirspaceJSONNilHolder(t *testing.T) {
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/airspace.json")
	if err != nil {
		t.Fatalf("GET /airspace.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`"channels":[]`, `"neighbors":[]`, `"my_channel":""`, `"recommendation":null`} {
		if !strings.Contains(s, want) {
			t.Errorf("empty /airspace.json missing %q: %s", want, s)
		}
	}
	// NO array may be null.
	for _, bad := range []string{`"channels":null`, `"neighbors":null`} {
		if strings.Contains(s, bad) {
			t.Errorf("/airspace.json emitted a null array %q (house []-not-null rule): %s", bad, s)
		}
	}
}

// TestAirspaceJSONPopulated pins the handler against a populated airspaceHolder:
// the width-aware occupancy + recommendation are present, my channel is marked
// Mine, and the redacted neighbor name never leaks "<redacted>".
func TestAirspaceJSONPopulated(t *testing.T) {
	airspaceH := &airspaceHolder{}
	airspaceH.store(airspaceSnap{
		MyChannel: "157/80",
		Neighbors: []Neighbor{
			{ChannelNum: 149, Band: "5 GHz", Width: "80MHz", Channel: "149/80", RSSI: -46},
			{ChannelNum: 6, Band: "2.4 GHz", Width: "20MHz", Channel: "6/20", RSSI: -90},
		},
	})
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, airspaceH, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/airspace.json")
	if err != nil {
		t.Fatalf("GET /airspace.json: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if strings.Contains(s, "<redacted>") {
		t.Errorf("/airspace.json leaked the redaction sentinel: %s", s)
	}
	if !strings.Contains(s, `"my_channel":"157/80"`) {
		t.Errorf("/airspace.json missing my_channel 157/80: %s", s)
	}
	if !strings.Contains(s, `"recommendation":{`) {
		t.Errorf("/airspace.json should carry a recommendation with 5 GHz data: %s", s)
	}
	if !strings.Contains(s, `"mine":true`) {
		t.Errorf("/airspace.json should mark my channel Mine: %s", s)
	}
}

// getProcs is a small helper: GET /procs.json off a mux and return the body string.
func getProcs(t *testing.T, mux *http.ServeMux) (int, string, http.Header) {
	t.Helper()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/procs.json")
	if err != nil {
		t.Fatalf("GET /procs.json: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// TestProcsJSONNilHolder pins the Phase-11 unavailable contract for a nil holder
// (handler-only test) AND a holder that is present but holds nil (serve warm-up
// before the first procs tick): both emit {"procs":[],"available":false} with
// []-not-null, never a reassuring blank, and with pinned no-store / Content-Type.
func TestProcsJSONNilHolder(t *testing.T) {
	// nil holder.
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	code, s, hdr := getProcs(t, mux)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(s, `"procs":[]`) || !strings.Contains(s, `"available":false`) {
		t.Errorf("nil-holder /procs.json = %s, want procs:[] available:false", s)
	}
	if strings.Contains(s, `"procs":null`) {
		t.Errorf("/procs.json emitted a null array (house []-not-null rule): %s", s)
	}
	if strings.Contains(s, `"paused"`) {
		t.Errorf("nil holder must not claim paused: %s", s)
	}

	// holder present but holding nil (warm-up).
	ph := &procsHolder{}
	mux2 := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, ph, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	_, s2, _ := getProcs(t, mux2)
	if !strings.Contains(s2, `"procs":[]`) || !strings.Contains(s2, `"available":false`) {
		t.Errorf("warm-up (nil snapshot) /procs.json = %s, want procs:[] available:false", s2)
	}
}

// TestProcsJSONUnavailable: an unavailable snapshot (nettop yielded nothing
// parseable) serves procs:[] available:false, no paused key.
func TestProcsJSONUnavailable(t *testing.T) {
	ph := &procsHolder{}
	ph.store(procsSnap{Available: false, T: time.Now()})
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, ph, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	_, s, _ := getProcs(t, mux)
	if !strings.Contains(s, `"procs":[]`) || !strings.Contains(s, `"available":false`) {
		t.Errorf("unavailable /procs.json = %s", s)
	}
	if strings.Contains(s, `"paused"`) {
		t.Errorf("unavailable (not gated) must not set paused: %s", s)
	}
}

// TestProcsJSONPaused: a gated tick (battery / locked) serves available:false WITH
// paused:true, so the dashboard says "paused" rather than a stale rate.
func TestProcsJSONPaused(t *testing.T) {
	ph := &procsHolder{}
	ph.store(procsSnap{Available: false, Paused: true, T: time.Now()})
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, ph, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	_, s, _ := getProcs(t, mux)
	if !strings.Contains(s, `"available":false`) || !strings.Contains(s, `"paused":true`) {
		t.Errorf("paused /procs.json = %s, want available:false paused:true", s)
	}
	if !strings.Contains(s, `"procs":[]`) {
		t.Errorf("paused /procs.json must still carry procs:[]: %s", s)
	}
}

// TestProcsJSONAvailableEmpty: a real rate snapshot with NO surviving talkers
// (everyone idle) is available:true with procs:[] — distinct from unavailable.
func TestProcsJSONAvailableEmpty(t *testing.T) {
	ph := &procsHolder{}
	ph.store(procsSnap{Procs: []ProcBandwidth{}, Available: true, T: time.Now()})
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, ph, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	_, s, _ := getProcs(t, mux)
	if !strings.Contains(s, `"procs":[]`) || !strings.Contains(s, `"available":true`) {
		t.Errorf("available-empty /procs.json = %s, want procs:[] available:true", s)
	}
}

// TestProcsJSONPopulated: a real rate snapshot serves the talkers with up/down rates
// and the PID, and passes an adversarial process name through JSON-encoded, untouched
// (the dashboard renders it via textContent).
func TestProcsJSONPopulated(t *testing.T) {
	ph := &procsHolder{}
	ph.store(procsSnap{
		Available: true,
		T:         time.Now(),
		Procs: []ProcBandwidth{
			{Name: "2.1.258", PID: 60633, UpBytesPerSec: 28717, DownBytesPerSec: 174.5},
			{Name: "<script>evil", PID: 999, UpBytesPerSec: 100, DownBytesPerSec: 0},
		},
	})
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, ph, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	_, s, _ := getProcs(t, mux)
	if !strings.Contains(s, `"available":true`) {
		t.Errorf("populated /procs.json should be available:true: %s", s)
	}
	if !strings.Contains(s, `"pid":60633`) || !strings.Contains(s, `"up_bytes_per_sec":28717`) {
		t.Errorf("populated /procs.json missing the top talker rate: %s", s)
	}
	// The adversarial name is JSON-encoded (< becomes <), never raw HTML.
	if strings.Contains(s, "<script>") {
		t.Errorf("/procs.json leaked a raw adversarial name (must be JSON-encoded): %s", s)
	}
}

// TestProcsJSONShapePure pins procsJSON directly: []-not-null, paused omitempty.
func TestProcsJSONShapePure(t *testing.T) {
	// nil procs, unavailable, not paused.
	b, err := procsJSON(nil, false, false)
	if err != nil {
		t.Fatalf("procsJSON: %v", err)
	}
	s := string(b)
	if s != `{"procs":[],"available":false}` {
		t.Errorf("procsJSON(nil,false,false) = %s", s)
	}
	// paused present only when true.
	b, _ = procsJSON(nil, false, true)
	if !strings.Contains(string(b), `"paused":true`) {
		t.Errorf("procsJSON paused=true should emit paused:true: %s", string(b))
	}
}

// TestNoNettopOnDefaultPath is the Phase-11 zero-nettop guard (models
// TestNoActiveScanCommands): the ONLY non-test .go file that may name/exec nettop is
// nettop.go. The default / --sample / serve-sampler data path must reference it
// NOWHERE, so a bare run or --sample spawns ZERO nettop.
func TestNoNettopOnDefaultPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "nettop.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)
		// No exec of nettop anywhere but nettop.go.
		if strings.Contains(s, `"nettop"`) || strings.Contains(s, "/usr/bin/nettop") {
			t.Errorf("%s names nettop; only nettop.go may exec it (zero-nettop guard)", f)
		}
	}
	// The default/--sample data path files (bandwidth.go, the sampler) must not
	// reference the top-talkers symbols at all.
	for _, f := range []string{"bandwidth.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "readTopTalkers") || strings.Contains(string(string(src)), "nettop") {
			t.Errorf("%s references the nettop path; the sampler must spawn zero nettop", f)
		}
	}
}

// getProc is a small helper: GET /proc?pid=<raw> off a mux and return the response.
func getProc(t *testing.T, mux *http.ServeMux, rawPID string) (int, string, http.Header) {
	t.Helper()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/proc?pid=" + rawPID)
	if err != nil {
		t.Fatalf("GET /proc?pid=%s: %v", rawPID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// TestProcEndpointPIDValidation is the SECURITY must-have: a table of raw ?pid=
// values each asserted to 400 AND to perform ZERO exec (proven via the
// procDetailReader seam counting calls). A valid pid reaches the reader exactly once.
func TestProcEndpointPIDValidation(t *testing.T) {
	orig := procDetailReader
	defer func() { procDetailReader = orig }()
	var calls int32
	procDetailReader = func(ctx context.Context, pid int, resolveDNS bool) ProcDetail {
		atomic.AddInt32(&calls, 1)
		return ProcDetail{PID: pid, Available: false, Connections: []ProcConn{}}
	}
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())

	bad := []string{
		"", "abc", "-1", "0", "1.5", "0x10", "99999999999999999999",
		"1%202", "1%3Brm%20-rf%20%2F", "%24%28id%29", "..%2F1", "1%0a", "%201", "100000", "999999",
	}
	for _, raw := range bad {
		code, body, _ := getProc(t, mux, raw)
		if code != http.StatusBadRequest {
			t.Errorf("pid=%q status = %d, want 400", raw, code)
		}
		if !strings.Contains(body, "invalid pid") {
			t.Errorf("pid=%q body = %q, want static \"invalid pid\"", raw, body)
		}
		// The raw value must NOT be reflected back (no needless reflection).
		if raw != "" && strings.Contains(body, raw) && raw != "invalid pid" {
			t.Errorf("pid=%q: raw value reflected in body %q", raw, body)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("bad pids triggered %d reader calls, want ZERO exec before validation", n)
	}

	// A valid pid reaches the reader exactly once.
	code, _, _ := getProc(t, mux, "13595")
	if code != http.StatusOK {
		t.Errorf("valid pid status = %d, want 200", code)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("valid pid reader calls = %d, want 1", n)
	}
}

// TestProcEndpointShape pins the /proc wire shape + headers via the reader seam
// (deterministic, no exec): a live pid => available:true with connections; a dead pid
// => available:false, connections:[] (not null), HTTP 200 (never 500); Content-Type
// pinned, no-store, NO CORS. Adversarial strings pass JSON-encoded (textContent on
// the client), never raw HTML.
func TestProcEndpointShape(t *testing.T) {
	orig := procDetailReader
	defer func() { procDetailReader = orig }()

	// Live pid with connections (and an adversarial command + remote_host).
	procDetailReader = func(ctx context.Context, pid int, resolveDNS bool) ProcDetail {
		return ProcDetail{
			PID: pid, Name: "<script>evil", Command: "/x --y", Path: "/x", PPID: 1,
			ParentName: "launchd", User: "adam", Started: "Tue Oct  6 09:45:28 2026",
			Available: true, SnapshotT: time.Now(),
			Connections: []ProcConn{
				{RemoteIP: "104.18.32.47", RemotePort: 443, RemoteHost: "<img>.evil.example"},
			},
		}
	}
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	code, body, hdr := getProc(t, mux, "13595")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/proc must NOT set Access-Control-Allow-Origin")
	}
	if !strings.Contains(body, `"available":true`) || !strings.Contains(body, `"remote_ip":"104.18.32.47"`) {
		t.Errorf("live /proc shape wrong: %s", body)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img>") {
		t.Errorf("/proc leaked a raw adversarial string (must be JSON-encoded): %s", body)
	}

	// Dead pid => available:false, connections:[] (not null), 200.
	procDetailReader = func(ctx context.Context, pid int, resolveDNS bool) ProcDetail {
		return ProcDetail{PID: pid, Available: false, Connections: []ProcConn{}}
	}
	mux2 := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	code, body, _ = getProc(t, mux2, "99998")
	if code != http.StatusOK {
		t.Errorf("dead pid status = %d, want 200 (never 500)", code)
	}
	if !strings.Contains(body, `"available":false`) || !strings.Contains(body, `"connections":[]`) {
		t.Errorf("dead /proc = %s, want available:false connections:[]", body)
	}
	if strings.Contains(body, `"connections":null`) {
		t.Errorf("/proc emitted a null array (house []-not-null rule): %s", body)
	}
}

// TestProcEndpointLoopbackGuard: a request with a non-loopback Host header => 403.
// This is the catch if /proc is ever registered OUTSIDE the guard() closure.
func TestProcEndpointLoopbackGuard(t *testing.T) {
	orig := procDetailReader
	defer func() { procDetailReader = orig }()
	var calls int32
	procDetailReader = func(ctx context.Context, pid int, resolveDNS bool) ProcDetail {
		atomic.AddInt32(&calls, 1)
		return ProcDetail{PID: pid, Connections: []ProcConn{}}
	}
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/proc?pid=13595", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host /proc status = %d, want 403", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("a 403'd /proc must NOT reach the reader (exec); calls = %d", n)
	}

	// Non-GET => 405 (Go 1.22 method matching).
	req, _ = http.NewRequest("POST", srv.URL+"/proc?pid=13595", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST /proc status = %d, want 405", resp.StatusCode)
	}
}

// TestProcDetailJSONShape pins procDetailJSON directly: []-not-null connections.
func TestProcDetailJSONShape(t *testing.T) {
	b, err := procDetailJSON(ProcDetail{PID: 5, Available: false})
	if err != nil {
		t.Fatalf("procDetailJSON: %v", err)
	}
	if !strings.Contains(string(b), `"connections":[]`) {
		t.Errorf("nil connections must normalize to []: %s", b)
	}
	if strings.Contains(string(b), `"connections":null`) {
		t.Errorf("connections must never be null: %s", b)
	}
}

// TestNoPsLsofOnDefaultPath is the MANDATORY zero-ps/lsof guard (mirrors
// TestNoNettopOnDefaultPath): the ONLY non-test .go file that may name /bin/ps or
// /usr/sbin/lsof is procdetail.go, so a bare run / --sample spawns ZERO ps/lsof. The
// default-path files (bandwidth.go + the sampler) must reference neither
// readProcDetail nor those binaries.
func TestNoPsLsofOnDefaultPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "procdetail.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)
		if strings.Contains(s, `"/bin/ps"`) || strings.Contains(s, `"/usr/sbin/lsof"`) || strings.Contains(s, `"lsof"`) {
			t.Errorf("%s names ps/lsof; only procdetail.go may exec them (zero-default guard)", f)
		}
	}
	for _, f := range []string{"bandwidth.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)
		if strings.Contains(s, "readProcDetail") || strings.Contains(s, "lsof") {
			t.Errorf("%s references the ps/lsof drill-down path; the sampler must spawn zero ps/lsof", f)
		}
	}
}

// TestDashboardHTMLHasProcDrilldown is the POSITIVE feature-present guard for the
// process drill-down: the served bytes CONTAIN the "/proc?pid=" fetch literal and the
// textContent-based detail renderer, and CONTAIN NO ".innerHTML" (so no adversarial
// command / PTR string can become self-XSS). Also pins colspan == the TOP TALKERS
// header th-count so a future column change can't silently misalign the detail row.
func TestDashboardHTMLHasProcDrilldown(t *testing.T) {
	required := []string{
		`"/proc?pid=" + encodeURIComponent`, // the string-concat (NOT template-literal) fetch URL
		"toggleProc",                        // the click/keyboard expand entry point
		"createTextNode",                    // textContent-based detail rendering
		"renderDetail",                      // the detail painter
		"expandedPid",                       // the single-open accordion state
		"procFetchGen",                      // the async stale-node generation guard
		`<thead><tr><th>Process</th><th>PID</th><th>Up</th><th>Down</th></tr></thead>`, // 4 header th
		`setAttribute("colspan", "4")`, // detail cell spans all 4 columns (== header th count)
	}
	for _, sub := range required {
		if !strings.Contains(dashboardHTML, sub) {
			t.Errorf("dashboardHTML missing required drill-down token %q", sub)
		}
	}
	// NO innerHTML anywhere — every adversarial string (command/path/parent_name/
	// remote_ip/remote_host) must render via textContent.
	if strings.Contains(dashboardHTML, ".innerHTML") {
		t.Error("dashboardHTML must NOT use .innerHTML (adversarial strings render via textContent only)")
	}
}

// --- Phase 13 report endpoints + throttle watchdog guards -------------------

// TestReportJSONEndpoint: /report.json serves the DayReport ([]-not-null), pinned
// headers, DNS-rebinding guard, and reflects appended speeds + outages.
func TestReportJSONEndpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, store, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Empty => zeroed totals + [] / {} arrays, never null, pinned headers.
	resp, err := http.Get(srv.URL + "/report.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("/report.json status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("/report.json Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/report.json Cache-Control = %q, want no-store", cc)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/report.json must NOT set Access-Control-Allow-Origin")
	}
	if !strings.Contains(body, `"band_seconds":{}`) || !strings.Contains(body, `"speed_by_hour":[]`) || !strings.Contains(body, `"flagged_hours":[]`) {
		t.Errorf("/report.json empty shape wrong: %s", body)
	}
	if strings.Contains(body, "null") {
		t.Errorf("/report.json must never contain null: %s", body)
	}

	// After appends, the DayReport reflects them (using time.Now() so they land in today).
	now := time.Now()
	if err := store.appendSpeed(SpeedSample{T: now, DownloadMbps: 500, LatencyMs: 22, OK: true}); err != nil {
		t.Fatalf("appendSpeed: %v", err)
	}
	if err := store.appendOutage(Outage{Start: now.Add(-time.Minute), End: now, DurationSec: 60, Cause: causeLinkDrop, Channel: "149"}); err != nil {
		t.Fatalf("appendOutage: %v", err)
	}
	resp, _ = http.Get(srv.URL + "/report.json")
	body = readAll(t, resp)
	var wire DayReport
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("/report.json not valid JSON: %v", err)
	}
	if wire.OutageCount != 1 {
		t.Errorf("/report.json OutageCount = %d, want 1", wire.OutageCount)
	}
	if len(wire.SpeedByHour) != 1 || wire.SpeedByHour[0].Samples != 1 {
		t.Errorf("/report.json did not reflect the appended speed sample: %+v", wire.SpeedByHour)
	}

	// Foreign Host => 403 (DNS-rebinding defense), no CORS.
	req, _ := http.NewRequest("GET", srv.URL+"/report.json", nil)
	req.Host = "evil.example"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host /report.json status = %d, want 403", resp.StatusCode)
	}
}

// TestReportJSONEndpointNilStore: a handler-only mux (no store) still serves a
// well-formed zeroed DayReport (never null) — the endpoint needs no store on the
// default path.
func TestReportJSONEndpointNilStore(t *testing.T) {
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/report.json")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"speed_by_hour":[]`) || strings.Contains(body, "null") {
		t.Errorf("nil-store /report.json shape wrong: %d %s", resp.StatusCode, body)
	}
}

// TestReportPageEndpoint: GET /report serves the self-contained HTML page with pinned
// Content-Type and the DNS-rebinding guard.
func TestReportPageEndpoint(t *testing.T) {
	mux := newServeMux(NewRing(defaultRingCap), "en0", time.Second, nil, "HIDDEN (macOS-redacted)", time.Now(), nil, nil, 5*time.Second, nil, nil, true, defaultThrottleRatio, defaultThrottleMinSamples, DefaultConfig())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("/report status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("/report Content-Type = %q", ct)
	}
	if !strings.Contains(body, "daily report") {
		t.Errorf("/report did not serve the report page")
	}
	// Foreign Host => 403.
	req, _ := http.NewRequest("GET", srv.URL+"/report", nil)
	req.Host = "evil.example"
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Errorf("foreign Host /report status = %d, want 403", resp2.StatusCode)
	}
}

// TestReportHTMLHasNoExternalURLs is the REQUIRED offline guard for the report page,
// the sibling of TestDashboardHTMLHasNoExternalURLs: no external-resource token slips
// into reportHTML, and no .innerHTML (every value renders via textContent).
func TestReportHTMLHasNoExternalURLs(t *testing.T) {
	low := strings.ToLower(reportHTML)
	banned := []string{
		"http://", "https://",
		`src="//`, `src='//`, `href="//`, `href='//`,
		"url(",
		"@import", "@font-face", "@font",
		"<link", "<iframe", "<img", "<image", "<source",
		"srcset", "poster=",
		".woff", ".woff2", ".ttf", ".otf", ".eot",
	}
	for _, sub := range banned {
		if strings.Contains(low, sub) {
			t.Errorf("reportHTML contains forbidden external-resource token %q", sub)
		}
	}
	if strings.Contains(reportHTML, ".innerHTML") {
		t.Error("reportHTML must NOT use .innerHTML (adversarial strings render via textContent only)")
	}
}

// TestReportHTMLPositiveTokens pins the report page's REQUIRED pieces so a future edit
// cannot silently delete the chart / caveat and still pass the purely-negative
// no-external-URL guard: the chart group id, the "http"+"://...svg" SVGNS concat
// token, the flagged-hour highlight class, and the mandatory honesty phrase.
func TestReportHTMLPositiveTokens(t *testing.T) {
	required := []string{
		`id="speedchart"`,                   // the speed-by-hour chart group
		`"http" + "://www.w3.org/2000/svg"`, // the split-literal SVGNS (guard-safe)
		"bar-flagged",                       // the flagged peak-hour-dip highlight class
		"peak-hour dips observed",           // the MANDATORY honesty phrase (the detection-honesty requirement)
		"outage journal",                    // cross-reference guidance
		"top-talkers",                       // cross-reference guidance
		"createElementNS",                   // rendering via createElementNS, not innerHTML
	}
	for _, sub := range required {
		if !strings.Contains(reportHTML, sub) {
			t.Errorf("reportHTML missing required token %q", sub)
		}
	}
}

// countingThrottleSpy swaps the throttleSampler seam with a call counter.
type throttleSpyState struct{ calls int }

// TestThrottleWatchOffNeverSamples PROVES a bare --serve (no --throttle-watch) invokes
// the throttle sampler ZERO times: the watchdog goroutine is never spawned, so the
// injected spy is never called. (Fast + structural — no dependence on the Ticker.)
func TestThrottleWatchOffNeverSamples(t *testing.T) {
	spy := &throttleSpyState{}
	orig := throttleSampler
	throttleSampler = func(ctx context.Context, cfg Config) SpeedSample {
		spy.calls++
		return SpeedSample{T: time.Now(), OK: false}
	}
	defer func() { throttleSampler = orig }()

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
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveDashboard did not return promptly on cancel")
	}
	if spy.calls != 0 {
		t.Errorf("throttle sampler called %d times with --throttle-watch OFF, want 0", spy.calls)
	}
}

// TestRunThrottleWatchExitsOnCancel proves the watchdog body joins promptly on ctx
// cancel and fires NO sample within the cancel window (the 1-minute Ticker never ticks
// before cancel), so it never touches Cloudflare spuriously. It also proves the
// injected sampler seam is what the watchdog calls (it is passed the spy).
func TestRunThrottleWatchExitsOnCancel(t *testing.T) {
	dir := t.TempDir()
	store, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	spy := &throttleSpyState{}
	sampler := func(ctx context.Context, cfg Config) SpeedSample {
		spy.calls++
		return SpeedSample{T: time.Now(), OK: true}
	}
	gate := newReachGate(false) // gating off => allow(), no exec
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runThrottleWatch(ctx, DefaultConfig(), store, gate, sampler,
			defaultThrottleSpacing, defaultThrottleJitterMax, defaultThrottleMaxBackoff, time.Time{})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runThrottleWatch did not exit promptly on cancel")
	}
	if spy.calls != 0 {
		t.Errorf("sampler fired %d times before the first tick, want 0", spy.calls)
	}
	if len(store.SpeedSnapshot()) != 0 {
		t.Error("a watchdog with no elapsed tick must append nothing")
	}
}

// TestThrottleGoIsSoleCloudflareCaller is the NEW sibling static guard: the throttle
// Cloudflare machinery (speedClient / __down / __up / speed.cloudflare.com) lives in
// throttle.go (the sanctioned sole new caller), while serve.go references NONE of it
// (TestServePathNoCloudflareSpeedRefs). Pinning that the tokens live THERE catches a
// future refactor that leaks the sampler out of throttle.go.
func TestThrottleGoIsSoleCloudflareCaller(t *testing.T) {
	b, err := os.ReadFile("throttle.go")
	if err != nil {
		t.Fatalf("read throttle.go: %v", err)
	}
	src := string(b)
	// The sampler builds its own speedClient and reads the cfg __down endpoint, so this
	// file is EXPECTED to reference the speed client (proving it is the sanctioned caller).
	if !strings.Contains(src, "speedClient") {
		t.Error("throttle.go should build the speed client (the sanctioned sole Cloudflare caller)")
	}
	// daily.go (the pure rollup) must NOT reference any Cloudflare/speed machinery.
	db, err := os.ReadFile("daily.go")
	if err != nil {
		t.Fatalf("read daily.go: %v", err)
	}
	for _, banned := range []string{"speedClient", "speed.cloudflare.com", "__down", "__up", "http.Get", "http.Client"} {
		if strings.Contains(string(db), banned) {
			t.Errorf("daily.go (pure rollup) must not reference %q", banned)
		}
	}
}
