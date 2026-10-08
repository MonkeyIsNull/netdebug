package main

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- VERBATIM macOS `/sbin/ping` captures (LC_ALL=C) ----------------------
// Captured on macOS 24.6.0 (Darwin), /sbin/ping mode 555 (unprivileged ICMP).

const pingFull3of3 = `PING 1.1.1.1 (1.1.1.1): 56 data bytes
64 bytes from 1.1.1.1: icmp_seq=0 ttl=53 time=37.419 ms
64 bytes from 1.1.1.1: icmp_seq=1 ttl=53 time=24.585 ms
64 bytes from 1.1.1.1: icmp_seq=2 ttl=53 time=19.150 ms

--- 1.1.1.1 ping statistics ---
3 packets transmitted, 3 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 19.150/27.051/37.419/7.659 ms
`

const pingGateway3of3 = `PING 10.0.0.1 (10.0.0.1): 56 data bytes
64 bytes from 10.0.0.1: icmp_seq=0 ttl=64 time=5.844 ms
64 bytes from 10.0.0.1: icmp_seq=1 ttl=64 time=5.459 ms
64 bytes from 10.0.0.1: icmp_seq=2 ttl=64 time=4.135 ms

--- 10.0.0.1 ping statistics ---
3 packets transmitted, 3 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 4.135/5.146/5.844/0.732 ms
`

// Total loss: "Request timeout" lines only, "100.0% packet loss" summary.
const pingTotalLoss = `PING 192.0.2.1 (192.0.2.1): 56 data bytes
Request timeout for icmp_seq 0
Request timeout for icmp_seq 1

--- 192.0.2.1 ping statistics ---
3 packets transmitted, 0 packets received, 100.0% packet loss
`

// Unknown host (DNS failure before any packet).
const pingUnknownHost = `ping: cannot resolve nonexistent.invalid.example: Unknown host
`

// 1-packet run whose summary stddev is "nan" (must NEVER reach the struct).
const pingNanStddev = `PING 1.1.1.1 (1.1.1.1): 56 data bytes
64 bytes from 1.1.1.1: icmp_seq=0 ttl=53 time=30.109 ms

--- 1.1.1.1 ping statistics ---
1 packets transmitted, 1 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 30.109/30.109/30.109/nan ms
`

// Partial loss (1 of 3 received): one reply + two timeouts, "66.7% packet loss".
const pingPartialLoss = `PING 1.1.1.1 (1.1.1.1): 56 data bytes
Request timeout for icmp_seq 0
64 bytes from 1.1.1.1: icmp_seq=1 ttl=53 time=22.500 ms
Request timeout for icmp_seq 2

--- 1.1.1.1 ping statistics ---
3 packets transmitted, 1 packets received, 66.7% packet loss
round-trip min/avg/max/stddev = 22.500/22.500/22.500/nan ms
`

// A duplicate reply (DUP!) must NOT be counted as an extra RTT.
const pingWithDUP = `PING 10.0.0.1 (10.0.0.1): 56 data bytes
64 bytes from 10.0.0.1: icmp_seq=0 ttl=64 time=5.000 ms
64 bytes from 10.0.0.1: icmp_seq=0 ttl=64 time=5.000 ms (DUP!)
64 bytes from 10.0.0.1: icmp_seq=1 ttl=64 time=6.000 ms

--- 10.0.0.1 ping statistics ---
3 packets transmitted, 2 packets received, +1 duplicates, 33.3% packet loss
round-trip min/avg/max/stddev = 5.000/5.500/6.000/0.500 ms
`

// Replies in NON-ascending printed order (arrival order preserved, not sorted).
const pingReordered = `PING 1.1.1.1 (1.1.1.1): 56 data bytes
64 bytes from 1.1.1.1: icmp_seq=0 ttl=53 time=30.000 ms
64 bytes from 1.1.1.1: icmp_seq=1 ttl=53 time=10.000 ms
64 bytes from 1.1.1.1: icmp_seq=2 ttl=53 time=20.000 ms

--- 1.1.1.1 ping statistics ---
3 packets transmitted, 3 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 10.000/20.000/30.000/8.165 ms
`

// Replies present but the process was killed before the summary line.
const pingRepliesNoSummary = `PING 1.1.1.1 (1.1.1.1): 56 data bytes
64 bytes from 1.1.1.1: icmp_seq=0 ttl=53 time=15.000 ms
64 bytes from 1.1.1.1: icmp_seq=1 ttl=53 time=17.000 ms
`

func approx(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestParsePingDetail(t *testing.T) {
	tests := []struct {
		name     string
		out      string
		count    int
		wantRTTs []float64
		wantAvg  float64
		wantLoss float64
		wantOK   bool
	}{
		{"3/3 replies", pingFull3of3, 3, []float64{37.419, 24.585, 19.150}, 27.0513, 0, true},
		{"gateway 3/3", pingGateway3of3, 3, []float64{5.844, 5.459, 4.135}, 5.146, 0, true},
		{"partial 1/3", pingPartialLoss, 3, []float64{22.500}, 22.500, 66.7, true},
		{"total loss", pingTotalLoss, 3, nil, 0, 100, false},
		{"unknown host", pingUnknownHost, 3, nil, 0, 100, false},
		{"garbage", "total nonsense output\nno numbers here", 3, nil, 0, 100, false},
		{"empty", "", 3, nil, 0, 100, false},
		{"DUP skipped", pingWithDUP, 3, []float64{5.000, 6.000}, 5.500, 33.3, true},
		{"nan stddev finite", pingNanStddev, 1, []float64{30.109}, 30.109, 0, true},
		{"reordered arrival order", pingReordered, 3, []float64{30.000, 10.000, 20.000}, 20.000, 0, true},
		{"replies without summary", pingRepliesNoSummary, 3, []float64{15.000, 17.000}, 16.000, 33.333, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePingDetail(tc.out, tc.count)
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, tc.wantOK)
			}
			if !approx(got.LossPct, tc.wantLoss) {
				t.Errorf("LossPct = %v, want ~%v", got.LossPct, tc.wantLoss)
			}
			if !approx(got.AvgMs, tc.wantAvg) {
				t.Errorf("AvgMs = %v, want ~%v", got.AvgMs, tc.wantAvg)
			}
			if len(got.RTTs) != len(tc.wantRTTs) {
				t.Fatalf("RTTs len = %d (%v), want %d (%v)", len(got.RTTs), got.RTTs, len(tc.wantRTTs), tc.wantRTTs)
			}
			for i := range tc.wantRTTs {
				if !approx(got.RTTs[i], tc.wantRTTs[i]) {
					t.Errorf("RTTs[%d] = %v, want %v (order matters for jitter)", i, got.RTTs[i], tc.wantRTTs[i])
				}
			}
			for _, v := range got.RTTs {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Errorf("non-finite RTT leaked: %v", v)
				}
			}
		})
	}
}

func TestReachClass(t *testing.T) {
	tests := []struct {
		gwOK, inetOK bool
		want         string
	}{
		{true, true, "ok"},
		{true, false, "gateway_only"},
		{false, false, "down"},
		{false, true, "down"}, // gateway bit DOMINATES
	}
	for _, tc := range tests {
		if got := reachClass(tc.gwOK, tc.inetOK); got != tc.want {
			t.Errorf("reachClass(%v,%v) = %q, want %q", tc.gwOK, tc.inetOK, got, tc.want)
		}
	}
}

func TestSelectInternet(t *testing.T) {
	a := PingResult{AvgMs: 20, LossPct: 0, OK: true}
	b := PingResult{AvgMs: 10, LossPct: 0, OK: true}
	lossy := PingResult{AvgMs: 5, LossPct: 33.3, OK: true}
	dead := PingResult{LossPct: 100, OK: false}

	t.Run("both OK different loss -> lower loss", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{lossy, b}, []string{"1.1.1.1", "8.8.8.8"})
		if !anyOK || addr != "8.8.8.8" || best.LossPct != 0 {
			t.Errorf("got best=%+v addr=%q anyOK=%v, want the 0%%-loss 8.8.8.8", best, addr, anyOK)
		}
	})
	t.Run("both OK equal loss -> lower RTT", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{a, b}, []string{"1.1.1.1", "8.8.8.8"})
		if !anyOK || addr != "8.8.8.8" || best.AvgMs != 10 {
			t.Errorf("got best=%+v addr=%q, want the lower-RTT 8.8.8.8", best, addr)
		}
	})
	t.Run("exactly one OK", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{dead, b}, []string{"1.1.1.1", "8.8.8.8"})
		if !anyOK || addr != "8.8.8.8" || !best.OK {
			t.Errorf("got best=%+v addr=%q anyOK=%v, want the only-OK 8.8.8.8", best, addr, anyOK)
		}
	})
	t.Run("none OK -> anyOK false, best=results[0]", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{dead, dead}, []string{"1.1.1.1", "8.8.8.8"})
		if anyOK || addr != "1.1.1.1" || best.LossPct != 100 {
			t.Errorf("got best=%+v addr=%q anyOK=%v, want results[0] reported, anyOK false", best, addr, anyOK)
		}
	})
	t.Run("exactly 1 target", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{b}, []string{"1.1.1.1"})
		if !anyOK || addr != "1.1.1.1" || best.AvgMs != 10 {
			t.Errorf("single-target got best=%+v addr=%q anyOK=%v", best, addr, anyOK)
		}
	})
	t.Run("0 targets -> zero, empty, false; no panic", func(t *testing.T) {
		best, addr, anyOK := selectInternet(nil, nil)
		if anyOK || addr != "" || best.OK {
			t.Errorf("empty got best=%+v addr=%q anyOK=%v, want zero", best, addr, anyOK)
		}
	})
	t.Run("more results than addrs -> no index panic", func(t *testing.T) {
		best, addr, anyOK := selectInternet([]PingResult{dead, dead}, []string{"1.1.1.1"})
		if anyOK || addr != "1.1.1.1" || best.LossPct != 100 {
			t.Errorf("misaligned got best=%+v addr=%q", best, addr)
		}
	})
}

func TestClampReachInterval(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, defaultReachInterval},
		{-3 * time.Second, defaultReachInterval},
		{500 * time.Millisecond, time.Second},
		{time.Second, time.Second},
		{3 * time.Second, 3 * time.Second},
		{10 * time.Second, 10 * time.Second},
	}
	for _, tc := range tests {
		if got := clampReachInterval(tc.in); got != tc.want {
			t.Errorf("clampReachInterval(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildReachSample(t *testing.T) {
	now := time.Now()

	t.Run("full healthy: jitter from SELECTED internet target", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5, 6}, AvgMs: 5.5, LossPct: 0, OK: true}
		// Target A: higher RTT; Target B: lower RTT, both 0% loss -> B is selected.
		inetA := PingResult{RTTs: []float64{40, 20, 30}, AvgMs: 30, LossPct: 0, OK: true}
		inetB := PingResult{RTTs: []float64{10, 14, 12}, AvgMs: 12, LossPct: 0, OK: true}
		s := buildReachSample(now, "10.0.0.1", gw,
			[]PingResult{inetA, inetB}, []string{"1.1.1.1", "8.8.8.8"}, 7.5, true)

		if s.Class != "ok" {
			t.Errorf("Class = %q, want ok", s.Class)
		}
		if !s.GatewayOK || !s.InternetOK || !s.DNSOK {
			t.Errorf("OK bits = %v/%v/%v, want all true", s.GatewayOK, s.InternetOK, s.DNSOK)
		}
		if s.InternetAddr != "8.8.8.8" {
			t.Errorf("InternetAddr = %q, want the best (8.8.8.8)", s.InternetAddr)
		}
		if !approx(s.InternetRTTMs, 12) {
			t.Errorf("InternetRTTMs = %v, want 12 (the selected target)", s.InternetRTTMs)
		}
		// jitter must be from B's series {10,14,12} => (|14-10|+|12-14|)/2 = 3, NOT
		// gateway's {5,6} => 1.
		if !approx(s.JitterMs, 3) {
			t.Errorf("JitterMs = %v, want 3 (internet series, not gateway)", s.JitterMs)
		}
		if !approx(s.GatewayRTTMs, 5.5) || !approx(s.DNSMs, 7.5) {
			t.Errorf("gateway/DNS RTT wrong: gw=%v dns=%v", s.GatewayRTTMs, s.DNSMs)
		}
	})

	t.Run("internet down, gateway up -> gateway_only", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, LossPct: 0, OK: true}
		dead := PingResult{LossPct: 100, OK: false}
		s := buildReachSample(now, "10.0.0.1", gw,
			[]PingResult{dead, dead}, []string{"1.1.1.1", "8.8.8.8"}, 0, false)
		if s.Class != "gateway_only" {
			t.Errorf("Class = %q, want gateway_only", s.Class)
		}
		if s.InternetOK {
			t.Error("InternetOK should be false")
		}
		if s.InternetRTTMs != 0 {
			t.Errorf("InternetRTTMs = %v, want 0 (omitted)", s.InternetRTTMs)
		}
		if s.InternetLoss != 100 {
			t.Errorf("InternetLoss = %v, want 100 (PRESENT)", s.InternetLoss)
		}
		if s.GatewayRTTMs != 5 {
			t.Errorf("GatewayRTTMs = %v, want 5", s.GatewayRTTMs)
		}
	})

	t.Run("healthy 0%% loss keys present and 0 on the wire", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, LossPct: 0, OK: true}
		inet := PingResult{RTTs: []float64{10}, AvgMs: 10, LossPct: 0, OK: true}
		s := buildReachSample(now, "10.0.0.1", gw, []PingResult{inet}, []string{"1.1.1.1"}, 7, true)
		b, err := dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &s})
		if err != nil {
			t.Fatalf("dataJSON: %v", err)
		}
		js := string(b)
		if !strings.Contains(js, `"gateway_loss_pct":0`) || !strings.Contains(js, `"internet_loss_pct":0`) {
			t.Errorf("0%% loss keys must be PRESENT and 0 (no omitempty): %s", js)
		}
	})

	t.Run("dnsMs 0 / dnsOK false -> DNSMs omitted", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, LossPct: 0, OK: true}
		inet := PingResult{RTTs: []float64{10}, AvgMs: 10, LossPct: 0, OK: true}
		s := buildReachSample(now, "10.0.0.1", gw, []PingResult{inet}, []string{"1.1.1.1"}, 0, false)
		if s.DNSMs != 0 || s.DNSOK {
			t.Errorf("DNSMs=%v DNSOK=%v, want 0/false", s.DNSMs, s.DNSOK)
		}
		b, _ := dataJSON(LiveData{Iface: "en0", Interval: 1, Reach: &s})
		if strings.Contains(string(b), `"dns_ms"`) {
			t.Errorf("dns_ms must be omitted when not attempted: %s", b)
		}
	})

	t.Run("routeless gateway -> down", func(t *testing.T) {
		s := buildReachSample(now, "", PingResult{}, nil, nil, 0, false)
		if s.Class != "down" || s.GatewayOK || s.InternetOK {
			t.Errorf("routeless should be down/false/false, got %+v", s)
		}
	})
}

// ---- buildReachTargets (the REACHABILITY drill-down per-target assembly) ----

func findTarget(tg []ReachTarget, kind, addr string) *ReachTarget {
	for i := range tg {
		if tg[i].Kind == kind && (addr == "" || tg[i].Addr == addr) {
			return &tg[i]
		}
	}
	return nil
}

func countBest(tg []ReachTarget) int {
	c := 0
	for _, t := range tg {
		if t.Best {
			c++
		}
	}
	return c
}

// targetsFor mirrors readReach's overlay: build the core sample, then thread its
// (single-sourced) InternetAddr/InternetOK into buildReachTargets.
func targetsFor(gwAddr string, gw PingResult, inet []PingResult, addrs []string,
	dnsName string, dnsMs float64, dnsOK bool, names map[string]string) (ReachSample, []ReachTarget) {
	s := buildReachSample(time.Now(), gwAddr, gw, inet, addrs, dnsMs, dnsOK)
	tg := buildReachTargets(gwAddr, gw, inet, addrs, dnsName, dnsMs, dnsOK, s.InternetAddr, s.InternetOK, names)
	return s, tg
}

func TestBuildReachTargets(t *testing.T) {
	t.Run("both up: two internet rows, best flips to winner, headline-consistent", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5, 6}, AvgMs: 5.5, LossPct: 0, OK: true}
		// 1.1.1.1 higher RTT, 8.8.8.8 lower RTT (both 0% loss) -> 8.8.8.8 wins.
		inetA := PingResult{RTTs: []float64{40, 20, 30}, AvgMs: 30, LossPct: 0, OK: true}
		inetB := PingResult{RTTs: []float64{10, 14, 12}, AvgMs: 12, LossPct: 0, OK: true}
		s, tg := targetsFor("10.0.0.1", gw, []PingResult{inetA, inetB},
			[]string{"1.1.1.1", "8.8.8.8"}, "apple.com", 7.5, true, nil)

		if len(tg) != 4 { // gateway + 2 internet + dns
			t.Fatalf("want 4 rows (gw+2 inet+dns), got %d: %+v", len(tg), tg)
		}
		gwRow := findTarget(tg, "gateway", "")
		if gwRow == nil || !gwRow.OK || gwRow.Addr != "10.0.0.1" || gwRow.Recv != 2 || gwRow.Sent != reachPingCount {
			t.Errorf("gateway row wrong: %+v", gwRow)
		}
		a := findTarget(tg, "internet", "1.1.1.1")
		b := findTarget(tg, "internet", "8.8.8.8")
		if a == nil || b == nil {
			t.Fatalf("both internet rows must be present (kept separately)")
		}
		if a.Best {
			t.Errorf("1.1.1.1 (slower) must NOT be best")
		}
		if !b.Best {
			t.Errorf("8.8.8.8 (faster) must be best")
		}
		if countBest(tg) != 1 {
			t.Errorf("exactly ONE best row, got %d", countBest(tg))
		}
		if a.Recv != 3 || a.Sent != reachPingCount || b.Recv != 3 || b.Sent != reachPingCount {
			t.Errorf("recv/sent wrong: a=%+v b=%+v", a, b)
		}
		// headline consistency: the best row can never drift from the tile/sparkline.
		if !approx(b.RTTMs, s.InternetRTTMs) || !approx(b.LossPct, s.InternetLoss) {
			t.Errorf("best row (rtt=%v loss=%v) must match headline (rtt=%v loss=%v)",
				b.RTTMs, b.LossPct, s.InternetRTTMs, s.InternetLoss)
		}
		if b.Addr != s.InternetAddr {
			t.Errorf("best Addr %q != headline InternetAddr %q", b.Addr, s.InternetAddr)
		}
	})

	t.Run("one internet down: both rows present, best flips to survivor", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, LossPct: 0, OK: true}
		dead := PingResult{LossPct: 100, OK: false}
		up := PingResult{RTTs: []float64{10, 11, 12}, AvgMs: 11, LossPct: 0, OK: true}
		_, tg := targetsFor("10.0.0.1", gw, []PingResult{dead, up},
			[]string{"1.1.1.1", "8.8.8.8"}, "", 0, false, nil)
		downRow := findTarget(tg, "internet", "1.1.1.1")
		if downRow == nil || downRow.OK || downRow.LossPct != 100 || downRow.Recv != 0 || downRow.Sent != reachPingCount {
			t.Errorf("down internet row wrong (ok=false,loss=100,recv=0,sent=3): %+v", downRow)
		}
		if downRow.Best {
			t.Errorf("down row must never be best")
		}
		surv := findTarget(tg, "internet", "8.8.8.8")
		if surv == nil || !surv.Best {
			t.Errorf("survivor 8.8.8.8 must be best: %+v", surv)
		}
		if countBest(tg) != 1 {
			t.Errorf("exactly one best, got %d", countBest(tg))
		}
		if findTarget(tg, "dns", "") != nil {
			t.Errorf("no dns row when dnsName empty")
		}
	})

	t.Run("all internet down (gateway_only): NO best anywhere, gateway row OK", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, LossPct: 0, OK: true}
		dead := PingResult{LossPct: 100, OK: false}
		s, tg := targetsFor("10.0.0.1", gw, []PingResult{dead, dead},
			[]string{"1.1.1.1", "8.8.8.8"}, "", 0, false, nil)
		if s.Class != "gateway_only" {
			t.Fatalf("class = %q, want gateway_only", s.Class)
		}
		if countBest(tg) != 0 {
			t.Errorf("all-down must flag NO best (honesty gate), got %d", countBest(tg))
		}
		gwRow := findTarget(tg, "gateway", "")
		if gwRow == nil || !gwRow.OK {
			t.Errorf("gateway row must be present + OK: %+v", gwRow)
		}
	})

	t.Run("all down (down): NO best, gateway row OK=false", func(t *testing.T) {
		dead := PingResult{LossPct: 100, OK: false}
		s, tg := targetsFor("10.0.0.1", PingResult{LossPct: 100, OK: false},
			[]PingResult{dead, dead}, []string{"1.1.1.1", "8.8.8.8"}, "", 0, false, nil)
		if s.Class != "down" {
			t.Fatalf("class = %q, want down", s.Class)
		}
		if countBest(tg) != 0 {
			t.Errorf("all-down must flag NO best, got %d", countBest(tg))
		}
		gwRow := findTarget(tg, "gateway", "")
		if gwRow == nil || gwRow.OK {
			t.Errorf("gateway row present with OK=false: %+v", gwRow)
		}
	})

	t.Run("routeless gateway: row still emitted, OK=false, Addr omitted", func(t *testing.T) {
		_, tg := targetsFor("", PingResult{}, nil, nil, "", 0, false, nil)
		gwRow := findTarget(tg, "gateway", "")
		if gwRow == nil || gwRow.OK || gwRow.Addr != "" {
			t.Errorf("routeless gateway row must be present with OK=false and empty Addr: %+v", gwRow)
		}
	})

	t.Run("dns present / failed / skipped", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, OK: true}
		up := PingResult{RTTs: []float64{10}, AvgMs: 10, OK: true}
		// present & ok
		_, tg := targetsFor("10.0.0.1", gw, []PingResult{up}, []string{"1.1.1.1"}, "apple.com", 7, true, nil)
		d := findTarget(tg, "dns", "")
		if d == nil || !d.OK || !approx(d.RTTMs, 7) || d.Sent != 0 || d.Addr != "apple.com" {
			t.Errorf("dns ok row wrong (rtt=7,sent=0,addr=apple.com): %+v", d)
		}
		// failed resolve: row present, OK false
		_, tg = targetsFor("10.0.0.1", gw, []PingResult{up}, []string{"1.1.1.1"}, "apple.com", 0, false, nil)
		d = findTarget(tg, "dns", "")
		if d == nil || d.OK {
			t.Errorf("failed dns must still emit a row with OK=false: %+v", d)
		}
		// skipped: no row
		_, tg = targetsFor("10.0.0.1", gw, []PingResult{up}, []string{"1.1.1.1"}, "", 0, false, nil)
		if findTarget(tg, "dns", "") != nil {
			t.Errorf("empty dnsName must emit NO dns row")
		}
	})

	t.Run("names overlay: internet rows only, miss/nil => empty", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, OK: true}
		up := PingResult{RTTs: []float64{10}, AvgMs: 10, OK: true}
		names := map[string]string{"1.1.1.1": "one.one.one.one"} // 8.8.8.8 missing
		_, tg := targetsFor("10.0.0.1", gw, []PingResult{up, up},
			[]string{"1.1.1.1", "8.8.8.8"}, "apple.com", 7, true, names)
		if r := findTarget(tg, "internet", "1.1.1.1"); r == nil || r.Host != "one.one.one.one" {
			t.Errorf("1.1.1.1 Host must be overlaid: %+v", r)
		}
		if r := findTarget(tg, "internet", "8.8.8.8"); r == nil || r.Host != "" {
			t.Errorf("8.8.8.8 (cache miss) Host must be empty: %+v", r)
		}
		if r := findTarget(tg, "gateway", ""); r == nil || r.Host != "" {
			t.Errorf("gateway must never carry Host: %+v", r)
		}
		if r := findTarget(tg, "dns", ""); r == nil || r.Host != "" {
			t.Errorf("dns must never carry Host: %+v", r)
		}
	})

	t.Run("N != 2 targets (1 and 3) — no hardcoded-two assumption", func(t *testing.T) {
		gw := PingResult{RTTs: []float64{5}, AvgMs: 5, OK: true}
		up := PingResult{RTTs: []float64{10}, AvgMs: 10, OK: true}
		_, tg1 := targetsFor("10.0.0.1", gw, []PingResult{up}, []string{"1.1.1.1"}, "", 0, false, nil)
		if n := len(tg1); n != 2 { // gateway + 1 internet
			t.Errorf("1 internet target => 2 rows, got %d", n)
		}
		_, tg3 := targetsFor("10.0.0.1", gw, []PingResult{up, up, up},
			[]string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}, "", 0, false, nil)
		if n := len(tg3); n != 4 { // gateway + 3 internet
			t.Errorf("3 internet targets => 4 rows, got %d", n)
		}
		if countBest(tg3) != 1 {
			t.Errorf("exactly one best among 3, got %d", countBest(tg3))
		}
	})
}

// ---- reverse-DNS names cache: bounded, keep-last-known-good, race-free ----

func TestRefreshReachNamesBound(t *testing.T) {
	orig := lookupAddrFunc
	defer func() { lookupAddrFunc = orig }()
	// A resolver that blocks until ITS ctx is cancelled (never returns) — the bounded
	// refresh must still return, and a concurrent reader must never block.
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	nc := &reachNames{}
	// Concurrent reader hammering the cache while the refresh runs: must never block.
	stop := make(chan struct{})
	var reads int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = nc.lookup("1.1.1.1")
				nc.snapshot()
				atomic.AddInt64(&reads, 1)
			}
		}
	}()

	start := time.Now()
	refreshReachNames(context.Background(), []string{"1.1.1.1", "8.8.8.8"}, nc)
	elapsed := time.Since(start)
	close(stop)

	if elapsed > reachNamesCap+500*time.Millisecond {
		t.Errorf("refreshReachNames took %v, must be bounded by reachNamesCap=%v", elapsed, reachNamesCap)
	}
	if atomic.LoadInt64(&reads) == 0 {
		t.Errorf("concurrent reader made no progress — the cache read blocked")
	}
}

func TestRefreshReachNamesKeepsLastGood(t *testing.T) {
	orig := lookupAddrFunc
	defer func() { lookupAddrFunc = orig }()

	// First refresh: both resolve.
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		return []string{"name-" + ip + "."}, nil
	}
	nc := &reachNames{}
	refreshReachNames(context.Background(), []string{"1.1.1.1", "8.8.8.8"}, nc)
	if nc.lookup("1.1.1.1") != "name-1.1.1.1" || nc.lookup("8.8.8.8") != "name-8.8.8.8" {
		t.Fatalf("initial resolve failed: %v", nc.snapshot())
	}

	// Second refresh: everything FAILS. Copy-on-write merge must keep last-known-good.
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	refreshReachNames(context.Background(), []string{"1.1.1.1", "8.8.8.8"}, nc)
	if nc.lookup("1.1.1.1") != "name-1.1.1.1" || nc.lookup("8.8.8.8") != "name-8.8.8.8" {
		t.Errorf("a resolver blip must NOT blank good names, got %v", nc.snapshot())
	}

	// Private / non-global targets are never looked up (no PTR work, no blanking).
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		t.Errorf("isGlobalIP gate must skip private %s", ip)
		return nil, nil
	}
	refreshReachNames(context.Background(), []string{"10.0.0.1", "192.168.1.1"}, &reachNames{})
}

func TestReachNamesConcurrentRefreshAndRead(t *testing.T) {
	orig := lookupAddrFunc
	defer func() { lookupAddrFunc = orig }()
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		return []string{"name-" + ip + "."}, nil
	}
	nc := &reachNames{}
	var wg sync.WaitGroup
	// Writers: refresh in a tight loop (whole-map atomic swaps).
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				refreshReachNames(context.Background(), []string{"1.1.1.1", "8.8.8.8"}, nc)
			}
		}()
	}
	// Readers: lookup + snapshot concurrently (go test -race proves no data race).
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = nc.lookup("1.1.1.1")
				_ = nc.snapshot()["8.8.8.8"]
			}
		}()
	}
	wg.Wait()
}

func TestJitterMsWrapper(t *testing.T) {
	// Single-sourced with signal.go's jitter(): {10,14,12} -> (4+2)/2 = 3.
	if got := jitterMs([]float64{10, 14, 12}); !approx(got, 3) {
		t.Errorf("jitterMs = %v, want 3", got)
	}
	// <2 samples floors to 0 (documented).
	if got := jitterMs([]float64{10}); got != 0 {
		t.Errorf("jitterMs(1 sample) = %v, want 0", got)
	}
	if got := jitterMs(nil); got != 0 {
		t.Errorf("jitterMs(nil) = %v, want 0", got)
	}
}
