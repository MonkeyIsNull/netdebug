package main

// Phase 7 — reachability & latency core. A continuous, sudo-free probe loop that
// every later diagnostics phase (outages, meeting badge, alerts, throttle) reads.
// It measures whether the gateway and the public internet are reachable and how
// healthy the path is (RTT + loss + jitter + DNS-resolve time), then surfaces a
// live `reach` object + an RTT sparkline on the dashboard that distinguishes
// "gateway OK / internet down" from "link down".
//
// Pure/impure split (house style): PingResult/parsePingDetail, reachClass,
// selectInternet, jitterMs, clampReachInterval and buildReachSample are PURE and
// table-tested in reach_test.go; pingDetail/dnsResolveTime/readReach and the
// loop/holder/ring are the thin impure wrappers. The power/presence gate lives in
// reach_gate.go.
//
// TRAFFIC HONESTY: one cycle execs up to 1 gateway + 2 internet pings at count=3
// each (=> up to ~9 tiny ICMP packets) + 1 DNS lookup. This is real network
// activity — NOT "packet-free" — and faintly perturbs the Phase-1 byte counters
// (negligible at the 5s default cadence).
//
// NO SUDO: latency comes from exec'ing the ABSOLUTE /sbin/ping (unprivileged ICMP;
// the binary is mode 555, NOT setuid) — never a raw ICMP socket (which needs root).

import (
	"context"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultReachInterval is the reach-probe cadence applied when a flag/config value
// is absent or <= 0 (see clampReachInterval): deliberately SLOWER than the 1s byte
// sampler and decoupled from the link cadence, so there is no ping storm.
const defaultReachInterval = 5 * time.Second

// Reach probe parameters. count=3 gives two consecutive deltas for jitter;
// timeoutSec>=count is INVARIANT (macOS -t is a TOTAL wall-clock cap and non-root
// ping is ~1s inter-packet, so timeoutSec<count silently sends fewer packets).
const (
	reachPingCount   = 3
	reachPingTimeout = 3 // seconds; >= reachPingCount
	reachDNSTimeout  = 3 * time.Second
	reachRingCap     = 300 // FIXED independent of cadence (bounded per-poll payload)
)

// reReplyTime captures each per-reply "time=<ms>" so jitter can be computed over
// the per-packet series. FindAll in arrival order (NOT sorted by icmp_seq) because
// jitter is order-sensitive.
var reReplyTime = regexp.MustCompile(`time=([\d.]+)`)

// PingResult is the parsed outcome of one `ping -c N` run.
type PingResult struct {
	RTTs    []float64 // per-packet ms, in PRINTED/ARRIVAL order (for jitter)
	AvgMs   float64   // mean of RTTs (0 if none) — SINGLE source of truth
	LossPct float64   // 0..100 (loss over packets actually SENT within -t)
	OK      bool      // at least one reply
}

// parsePingDetail parses `ping -c N -t T <host>` output into a PingResult. It
// extends probe.go's parsePing (loss+avg only) by capturing EACH per-reply
// "time=<ms>" so jitter can be computed. Committee-pinned rules:
//   - RTTs: every per-reply "time=([\d.]+)", in ARRIVAL order (not sorted).
//   - DUP replies: any line containing "(DUP!)" is SKIPPED so a duplicate's time=
//     is never counted as an extra RTT.
//   - NON-FINITE guard: a NaN/Inf time= value is dropped; the summary stddev
//     ("…/nan ms") is never read (dataJSON fails json.Marshal on NaN/Inf).
//   - AvgMs = mean(captured RTTs); the summary avg (rePingRtt) is NOT read here, so
//     RTTs and AvgMs cannot drift under DUP or -t truncation.
//   - LossPct from rePingLoss ("% packet loss") only. Fully-lost / "Request
//     timeout" only / unknown-host / garbage / empty => RTTs nil, LossPct 100,
//     OK false. Never panics.
//   - REPLIES-WITHOUT-SUMMARY edge (ping ctx-killed before stats): RTTs non-empty
//     but no "% packet loss" line => OK true, LossPct derived from
//     100*(count-len(RTTs))/count rather than defaulting to 100.
func parsePingDetail(out string, count int) PingResult {
	var rtts []float64
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "(DUP!)") {
			continue // never count a duplicate reply's time=
		}
		m := reReplyTime.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue // non-finite must never reach the struct
		}
		rtts = append(rtts, v)
	}

	res := PingResult{RTTs: rtts, OK: len(rtts) > 0}
	if len(rtts) > 0 {
		sum := 0.0
		for _, v := range rtts {
			sum += v
		}
		res.AvgMs = sum / float64(len(rtts))
	}

	switch m := rePingLoss.FindStringSubmatch(out); {
	case m != nil:
		res.LossPct, _ = strconv.ParseFloat(m[1], 64)
	case len(rtts) > 0 && count > 0:
		// Replies present but no summary line (killed on ctx-cancel): derive loss
		// from the packets we did get rather than reporting an inconsistent 100%.
		res.LossPct = 100 * float64(count-len(rtts)) / float64(count)
		if res.LossPct < 0 {
			res.LossPct = 0
		}
	default:
		res.LossPct = 100
	}
	return res
}

// reachClass collapses the two reachability bits into one forensic verdict. The
// gateway bit DOMINATES: (gwOK=false, internetOK=true) still => "down".
//
//	internetOK          -> "ok"
//	!internetOK && gwOK  -> "gateway_only" (router fine, uplink/ISP down)
//	!gwOK                -> "down"         (local link/gateway dead)
func reachClass(gwOK, internetOK bool) string {
	switch {
	case !gwOK:
		return "down"
	case internetOK:
		return "ok"
	default:
		return "gateway_only"
	}
}

// selectInternet picks the best answering internet target. PURE and table-tested
// (the most bug-prone logic, so it is extracted from impure readReach). anyOK = any
// result OK (=> InternetOK). best = the OK result with the lowest LossPct, ties
// broken by lowest AvgMs; addr = its index-aligned address. Guards: 0 results =>
// (zero, "", false); 1 result; all-failed => anyOK false and best = results[0] (so
// loss/addr are still reported). Never indexes out of range.
func selectInternet(results []PingResult, addrs []string) (best PingResult, addr string, anyOK bool) {
	if len(results) == 0 {
		return PingResult{}, "", false
	}
	bestIdx := 0 // all-failed fallback: results[0]
	for i, r := range results {
		if !r.OK {
			continue
		}
		if !anyOK {
			bestIdx, anyOK = i, true
			continue
		}
		b := results[bestIdx]
		if r.LossPct < b.LossPct || (r.LossPct == b.LossPct && r.AvgMs < b.AvgMs) {
			bestIdx = i
		}
	}
	if bestIdx < len(addrs) {
		addr = addrs[bestIdx]
	}
	return results[bestIdx], addr, anyOK
}

// jitterMs is the mean absolute consecutive delta over a per-packet RTT series.
// REUSES signal.go's jitter() so the math is single-sourced; a named wrapper for
// clarity. NOTE: jitter() returns 0 for <2 samples, so count>=2 is the jitter
// floor; at the default count=3 that is only 2 consecutive deltas (noisy).
func jitterMs(rtts []float64) float64 { return jitter(rtts) }

// clampReachInterval: d<=0 => default 5s; floor 1s. DISTINCT from clampInterval
// (250ms) and clampLinkInterval (its own 1s-floor function) so a cadence-policy
// change to one never silently moves the others.
func clampReachInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultReachInterval
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

// ReachTarget is one probed endpoint of a reach cycle (the REACHABILITY drill-down
// unit). It rides the LATEST reach object only (stripped from reach_series — see
// serve.go stripReachSeriesTargets). Kind is "gateway" | "internet" | "dns". Addr is
// the raw IP (gateway/internet) or the configured hostname (dns). Host is the
// reverse-DNS PTR (internet targets only; attacker-ish => rendered via textContent).
// Recv/Sent are the per-target packet counts (ping rows only; dns has Sent 0). Best
// flags the selectInternet winner and is set ONLY when internet is up. It has no
// nested pointer/slice/map json field, so a populated target never emits "null".
// LossPct carries NO omitempty (0% is a real healthy reading, mirroring the headline
// loss fields); RTTMs keeps omitempty (an exact 0.0 ms is never a real reading).
type ReachTarget struct {
	Kind    string  `json:"kind"`
	Addr    string  `json:"addr,omitempty"`
	Host    string  `json:"host,omitempty"`   // reverse-DNS PTR; omitted on miss/private/dns
	RTTMs   float64 `json:"rtt_ms,omitempty"` // internet: own avg; dns: resolve time
	LossPct float64 `json:"loss_pct"`         // NO omitempty; N/A for dns (Sent==0)
	Recv    int     `json:"recv,omitempty"`   // packets received (ping rows)
	Sent    int     `json:"sent,omitempty"`   // packets sent (ping rows == reachPingCount); dns => 0/absent
	OK      bool    `json:"ok"`
	Best    bool    `json:"best,omitempty"` // the selected internet winner; ONLY when internet is up
}

// ReachSample is one reachability cycle (the wire + sparkline unit). Every HEADLINE
// field is scalar, so the headline can never emit "null"; Targets is an additive
// slice carried ONLY on the latest reach object (omitempty => nil omits it on the
// pure/literal dataJSON path; non-empty + present on the serve path, never "[]"-
// normalized since a serve sample always has at least a gateway row).
type ReachSample struct {
	T             time.Time `json:"t"`                        // always stamped now; never zero on the wire path
	Class         string    `json:"class"`                    // reachClass output
	GatewayOK     bool      `json:"gateway_ok"`               //
	GatewayRTTMs  float64   `json:"gateway_rtt_ms,omitempty"` // 0 ms is never a real reading
	GatewayLoss   float64   `json:"gateway_loss_pct"`         // NO omitempty: 0% is a REAL healthy reading
	InternetOK    bool      `json:"internet_ok"`              //
	InternetRTTMs float64   `json:"internet_rtt_ms,omitempty"`
	InternetLoss  float64   `json:"internet_loss_pct"`   // NO omitempty: 0% is a REAL healthy reading
	JitterMs      float64   `json:"jitter_ms,omitempty"` // over the BEST internet target's per-packet RTTs
	DNSMs         float64   `json:"dns_ms,omitempty"`    // resolve time; 0 => not attempted
	DNSOK         bool      `json:"dns_ok"`              //
	GatewayAddr   string    `json:"gateway_addr,omitempty"`
	InternetAddr  string    `json:"internet_addr,omitempty"` // which target answered (selectInternet)

	// Targets is the per-target drill-down (gateway + EACH internet [+ dns]), assembled
	// by the PURE buildReachTargets from the SAME already-measured inputs selectInternet
	// collapses — NO new probe. Latest-reach only; stripped from reach_series.
	Targets []ReachTarget `json:"targets,omitempty"`
}

// buildReachSample assembles a ReachSample from parsed inputs (PURE: no exec). It
// calls selectInternet on the internet slice so Class, InternetAddr,
// InternetRTT/Loss and jitter (from the SELECTED target's RTTs) are single-sourced.
// GatewayLoss/InternetLoss carry NO omitempty so a genuine 0% shows; RTT and DNSMs
// keep omitempty (an exact 0.0 ms RTT is never real; DNSMs 0 => not attempted).
func buildReachSample(now time.Time, gwAddr string, gw PingResult,
	inetResults []PingResult, inetAddrs []string, dnsMs float64, dnsOK bool) ReachSample {
	best, inetAddr, anyOK := selectInternet(inetResults, inetAddrs)
	s := ReachSample{
		T:            now,
		Class:        reachClass(gw.OK, anyOK),
		GatewayOK:    gw.OK,
		GatewayLoss:  gw.LossPct,
		InternetOK:   anyOK,
		InternetLoss: best.LossPct,
		DNSOK:        dnsOK,
		GatewayAddr:  gwAddr,
		InternetAddr: inetAddr,
	}
	if gw.OK {
		s.GatewayRTTMs = gw.AvgMs
	}
	if anyOK {
		s.InternetRTTMs = best.AvgMs
		s.JitterMs = jitterMs(best.RTTs) // jitter is from the INTERNET ping, not gateway
	}
	if dnsOK {
		s.DNSMs = dnsMs
	}
	return s
}

// Reverse-DNS naming cadence + bounds for the STABLE config internet targets. The
// PTRs of config IPs essentially never change, so a slow refresh keeps names fresh
// across battery/locked stretches for negligible traffic; the per-lookup and overall
// caps mirror procdetail.go's revDNS discipline so a wedged resolver can never stall.
const (
	reachNamesRefresh   = 30 * time.Minute       // PTR refresh cadence
	reachNamesCap       = 2 * time.Second        // ONE overall cap over all PTR lookups per refresh
	reachNamesPerLookup = 800 * time.Millisecond // per-lookup child-ctx timeout
)

// buildReachTargets assembles the per-target list from the SAME already-measured
// inputs selectInternet collapses (PURE: no exec, no IO, never panics). inetAddr +
// anyOK are selectInternet's outputs (threaded in from the just-built ReachSample's
// InternetAddr/InternetOK) so Best is SINGLE-SOURCED with the headline and can never
// disagree. names is a nil-safe addr->PTR map (cold cache => "" => Host omitted). A
// GATEWAY row is ALWAYS emitted (private => no PTR; the UI shows "your router"); ONE
// internet row PER result is emitted (BOTH targets kept, each with its OWN RTT/loss),
// Best flagged on the FIRST address-matching row and ONLY when anyOK (no fabricated
// winner on an all-down cycle, where selectInternet returns addrs[0] with anyOK=false);
// the DNS row is emitted ONLY when dnsName != "" (so a SKIP is distinguishable from a
// FAIL, which readReach can tell but buildReachSample cannot) with Sent 0 (=> no loss /
// packet cell — it is a RESOLVE, not a ping).
func buildReachTargets(gwAddr string, gw PingResult, inetResults []PingResult,
	inetAddrs []string, dnsName string, dnsMs float64, dnsOK bool,
	inetAddr string, anyOK bool, names map[string]string) []ReachTarget {
	targets := make([]ReachTarget, 0, len(inetResults)+2)

	// GATEWAY row (always present so the table always has a gateway line).
	gwT := ReachTarget{Kind: "gateway", Addr: gwAddr, OK: gw.OK,
		LossPct: gw.LossPct, Recv: len(gw.RTTs), Sent: reachPingCount}
	if gw.OK {
		gwT.RTTMs = gw.AvgMs
	}
	targets = append(targets, gwT)

	// ONE internet row PER result (keep BOTH 1.1.1.1 and 8.8.8.8 with their own detail).
	bestFlagged := false
	for i, r := range inetResults {
		var addr string
		if i < len(inetAddrs) {
			addr = inetAddrs[i]
		}
		t := ReachTarget{Kind: "internet", Addr: addr, OK: r.OK,
			LossPct: r.LossPct, Recv: len(r.RTTs), Sent: reachPingCount}
		if r.OK {
			t.RTTMs = r.AvgMs
		}
		if addr != "" {
			t.Host = names[addr] // nil-safe: cold cache / miss => ""
		}
		if anyOK && !bestFlagged && addr == inetAddr {
			t.Best = true // honesty gate: never flagged when internet is down
			bestFlagged = true
		}
		targets = append(targets, t)
	}

	// DNS row ONLY when a name was configured (skip => no row); a FAILED resolve still
	// emits the row with OK=false.
	if dnsName != "" {
		d := ReachTarget{Kind: "dns", Addr: dnsName, OK: dnsOK}
		if dnsOK {
			d.RTTMs = dnsMs
		}
		targets = append(targets, d)
	}
	return targets
}

// reachNames is a copy-on-write reverse-DNS cache for the STABLE config internet
// targets. The refresher swaps a whole NEW map via atomic.Pointer (mirrors
// reachHolder/linkHolder); readReach does ONE atomic load + read-only index. It is
// touched by TWO goroutines (the refresher writes, the reach tick reads) so it is
// deliberately NOT the mutex-free reachGate idiom. Both methods are nil-safe and
// NEVER block.
type reachNames struct {
	p atomic.Pointer[map[string]string]
}

// lookup returns the cached PTR for ip, or "" on a nil cache / miss.
func (n *reachNames) lookup(ip string) string {
	if n == nil {
		return ""
	}
	m := n.p.Load()
	if m == nil {
		return ""
	}
	return (*m)[ip]
}

// snapshot returns the current addr->PTR map (or nil). The returned map is the
// PUBLISHED, read-only copy — never mutated in place (the refresher only ever swaps a
// fresh map), so a reader may index it without a lock.
func (n *reachNames) snapshot() map[string]string {
	if n == nil {
		return nil
	}
	m := n.p.Load()
	if m == nil {
		return nil
	}
	return *m
}

// refreshReachNames resolves the global config targets ONCE (bounded + concurrent) and
// COPY-ON-WRITE MERGEs the result over the last-known-good map, so a transient resolver
// blip never BLANKS a good name. Only global IPs are looked up (gateway is private/
// per-cycle and keyed nowhere; a mis-configured private PublicIP is skipped by the
// isGlobalIP gate). Mirrors resolveConns: one goroutine per IP under ONE overall cap,
// select(done vs cap), NEVER an unbounded wg.Wait (stragglers unwind on their own ctx).
func refreshReachNames(ctx context.Context, ips []string, nc *reachNames) {
	prev := nc.snapshot()
	next := make(map[string]string, len(prev)+len(ips))
	for k, v := range prev {
		next[k] = v // seed last-known-good
	}

	var targets []string
	for _, ip := range ips {
		if isGlobalIP(net.ParseIP(ip)) {
			targets = append(targets, ip)
		}
	}
	if len(targets) > 0 {
		cctx, cancel := context.WithTimeout(ctx, reachNamesCap)
		resolved := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, ip := range targets {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				lctx, lcancel := context.WithTimeout(cctx, reachNamesPerLookup)
				defer lcancel()
				addrs, err := lookupAddrFunc(lctx, ip)
				if err != nil || len(addrs) == 0 {
					return
				}
				mu.Lock()
				resolved[ip] = strings.TrimSuffix(addrs[0], ".")
				mu.Unlock()
			}(ip)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-cctx.Done(): // the overall cap fired: merge whatever resolved so far
		}
		cancel()
		mu.Lock()
		for ip, name := range resolved {
			next[ip] = name // REPLACE only keys that resolved (keep-last-known-good)
		}
		mu.Unlock()
	}
	nc.p.Store(&next)
}

// runReachNamesRefresh is the dedicated ctx-bound goroutine: an INITIAL resolve then a
// slow ticker. Its only blocking wait is the select on ticker-vs-ctx (refreshReachNames
// is itself bounded by reachNamesCap), so it exits promptly on ctx.Done() and wg.Wait()
// never hangs on shutdown. It is SERVE-ONLY (constructed in serveDashboard); a bare
// run / --sample never warms the cache.
func runReachNamesRefresh(ctx context.Context, ips []string, nc *reachNames) {
	refreshReachNames(ctx, ips, nc)
	t := time.NewTicker(reachNamesRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshReachNames(ctx, ips, nc)
		}
	}
}

// ---- impure layer (thin, NOT unit-tested directly) ------------------------

// pingDetail execs the ABSOLUTE "/sbin/ping" (sudo-free unprivileged ICMP; NOT a
// bare "ping", which is PATH-shadowable under launchd's thin PATH) with LC_ALL=C
// (the English regexes depend on the C locale) and CommandContext so shutdown
// cancels an in-flight probe promptly. Returns parsePingDetail(out, count).
func pingDetail(ctx context.Context, host string, count, timeoutSec int) PingResult {
	cmd := exec.CommandContext(ctx, "/sbin/ping", "-c", strconv.Itoa(count), "-t", strconv.Itoa(timeoutSec), host)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, _ := cmd.CombinedOutput()
	return parsePingDetail(string(out), count)
}

// dnsResolveTime times a single lookup of name via the SYSTEM resolver config
// (net.Resolver{PreferGo:true} reads /etc/resolv.conf), returning (ms, ok). It is
// exec-free (the name never reaches any argv) and ctx/timeout-bounded. CAVEAT:
// PreferGo bypasses mDNSResponder, so VPN/split-DNS/per-interface resolvers and the
// mDNSResponder cache are invisible — an INDICATOR, not a benchmark. Failure =>
// (elapsed, false).
func dnsResolveTime(ctx context.Context, name string, timeout time.Duration) (float64, bool) {
	r := &net.Resolver{PreferGo: true}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ips, err := r.LookupIPAddr(cctx, name)
	elapsed := float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil || len(ips) == 0 {
		return elapsed, false
	}
	return elapsed, true
}

// readReach runs ONE reachability cycle: defaultRoute() for the gateway, then
// pingDetail(gw), pingDetail(each internetIPs[i]), dnsResolveTime(dnsName), then
// buildReachSample. Every target is net.ParseIP-validated BEFORE exec (skip/
// mark-down otherwise) so a hostile/malformed value (e.g. a leading-dash "-S…")
// can never be consumed by ping as a FLAG. ctx is threaded into every exec/lookup
// so shutdown is bounded.
//
// names is the serve-only reverse-DNS cache (nil on the --status one-shot path). It is
// read with ONE non-blocking atomic snapshot() on the hot path — NO lookup, NO IO — and
// overlaid onto the additive Targets list AFTER the (unchanged) core sample is built, so
// a cold/empty cache simply yields raw addrs and never stalls a reach tick.
func readReach(ctx context.Context, internetIPs []string, dnsName string, names *reachNames) ReachSample {
	now := time.Now()

	gwAddr, _ := defaultRoute()
	var gw PingResult // zero value => OK false (routeless link => Class "down")
	if net.ParseIP(gwAddr) != nil {
		gw = pingDetail(ctx, gwAddr, reachPingCount, reachPingTimeout)
	}

	var results []PingResult
	var addrs []string
	for _, ip := range internetIPs {
		addrs = append(addrs, ip)
		if net.ParseIP(ip) == nil {
			results = append(results, PingResult{LossPct: 100}) // mark down, never pass to argv
			continue
		}
		results = append(results, pingDetail(ctx, ip, reachPingCount, reachPingTimeout))
	}

	var dnsMs float64
	var dnsOK bool
	if dnsName != "" { // empty => skip DNS probe (DNSOK false, DNSMs omitted)
		dnsMs, dnsOK = dnsResolveTime(ctx, dnsName, reachDNSTimeout)
	}

	s := buildReachSample(now, gwAddr, gw, results, addrs, dnsMs, dnsOK)
	s.Targets = buildReachTargets(gwAddr, gw, results, addrs, dnsName, dnsMs, dnsOK,
		s.InternetAddr, s.InternetOK, names.snapshot())
	return s
}

// reachHolder mirrors linkHolder: a lock-free atomic.Pointer[ReachSample] the
// /data.json handler reads without blocking on a slow ping.
type reachHolder struct{ p atomic.Pointer[ReachSample] }

func (h *reachHolder) store(s ReachSample) { h.p.Store(&s) }
func (h *reachHolder) load() *ReachSample  { return h.p.Load() }

// reachRing is a small fixed ring of recent ReachSamples for the sparkline (same
// bounded-alloc discipline as the byte Ring; capacity FIXED at reachRingCap
// INDEPENDENT of reach_interval so lowering the cadence cannot grow the per-poll
// payload unbounded). Snapshot returns an independent copy, oldest->newest.
type reachRing struct {
	mu   sync.Mutex
	buf  []ReachSample
	head int
	size int
}

func newReachRing() *reachRing {
	return &reachRing{buf: make([]ReachSample, reachRingCap)}
}

func (r *reachRing) add(s ReachSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = s
	r.head = (r.head + 1) % reachRingCap
	if r.size < reachRingCap {
		r.size++
	}
}

func (r *reachRing) Snapshot() []ReachSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReachSample, r.size)
	start := (r.head - r.size + reachRingCap) % reachRingCap
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(start+i)%reachRingCap]
	}
	return out
}

// runReachLoop is the dedicated goroutine (runLinkLoop shape): a SINGLE
// non-reentrant Ticker. Each tick: evaluate the cached gate; if shouldProbe is
// false, store NOTHING (holder + ring unchanged — the Phase-6 stale-dot handles the
// ageing display) and continue; else read()->store+ring. Exits promptly on ctx
// cancel; because read()'s execs ARE ctx-aware, a cancel aborts an in-flight probe
// so wg.Wait() returns within a few seconds, not the full ping -t x2 + DNS timeout.
//
// Phase 8: `observe` is a nil-safe INJECTED callback (seam b) so the loop stays
// AGNOSTIC of outages/link/store. On a PROBED tick it fires observe(s, true) AFTER
// store+ring; on a GATED/paused tick it fires observe(ReachSample{}, false) BEFORE the
// `continue` (so the detector's pause-reset hook runs). observe == nil => no-op (keeps
// reach.go reusable; the reach_gate_test callsites pass nil).
func runReachLoop(ctx context.Context, interval time.Duration,
	read func(context.Context) ReachSample, gate *reachGate,
	holder *reachHolder, ring *reachRing, observe func(ReachSample, bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if gate != nil && !gate.allow() {
				if observe != nil {
					observe(ReachSample{}, false) // paused: fire the reset hook, store nothing
				}
				continue // paused: on battery or screen locked (holder keeps last)
			}
			s := read(ctx)
			holder.store(s)
			ring.add(s)
			if observe != nil {
				observe(s, true)
			}
		}
	}
}
