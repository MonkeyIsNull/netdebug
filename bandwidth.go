package main

// Phase 1 — sampling core. Read per-interface cumulative byte counters from
// `netstat -ibn`, compute clean up/down deltas with correct wrap/reset handling,
// keep a rolling in-memory ring, and expose it through a `--sample` mode that
// prints live up/down rates to the terminal. No server, no persistence yet.
//
// Pure/impure split (house style): parseNetstatIB / deltaRate / newSample /
// nextBaseline / humanBps / clampInterval / Ring are PURE and table-tested;
// readCounters / runSample are the thin impure wrappers that exec and loop.

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultRingCap is the rolling buffer size. Not a flag in Phase 1: there is no
// Snapshot() consumer yet (that is Phase 2's /data.json), so a --capacity knob
// would be unobservable. ~1h at the default 1s interval.
const defaultRingCap = 3600

// minSampleInterval is the sample-period floor enforced by clampInterval.
const minSampleInterval = 250 * time.Millisecond

// minDt is the near-zero-dt floor (seconds) for deltaRate: two near-simultaneous
// reads must not divide a real delta into an enormous rate. ~1ms.
const minDt = 0.001

// IfaceCounters is one interface's cumulative byte counters at a point in time.
type IfaceCounters struct {
	Name    string
	RxBytes uint64 // Ibytes
	TxBytes uint64 // Obytes
	OK      bool   // row found & parsed
}

// parseNetstatIB parses `netstat -ibn` output and returns the counters for the
// given interface. It reads the authoritative "<Link#N>" aggregate row and
// ignores the per-address rows (which repeat the same totals). Returns OK=false
// (zero counters) if the iface is absent or the row is malformed.
//
// Column extraction is right-indexed, NOT fixed left-index: the Address(MAC)
// column is optional (address-less ifaces like lo0/utun0/gif0* split into 10
// fields, a MAC-bearing en0 into 11), but the trailing 7 numeric columns are
// always, in order: Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll. So Ibytes is
// fields[n-5] and Obytes is fields[n-2] regardless of the Address.
func parseNetstatIB(out, iface string) IfaceCounters {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 { // too short to be a Link row
			continue
		}
		name := strings.TrimSuffix(fields[0], "*") // de-star inactive ifaces
		if name != iface {                         // EXACT equality only
			continue
		}
		if !strings.HasPrefix(fields[2], "<Link#") { // aggregate row marker
			continue
		}
		n := len(fields)
		rx, errRx := strconv.ParseUint(fields[n-5], 10, 64) // Ibytes
		tx, errTx := strconv.ParseUint(fields[n-2], 10, 64) // Obytes
		if errRx != nil || errTx != nil {
			return IfaceCounters{Name: iface} // OK=false, never a panic
		}
		return IfaceCounters{Name: iface, RxBytes: rx, TxBytes: tx, OK: true}
	}
	return IfaceCounters{Name: iface} // absent => OK=false, zero counters
}

// LinkSnap is the per-sample Wi-Fi radio snapshot (Phase 4, enriched in Phase 6)
// stamped onto a Sample by the serve-mode sampler. It is the single source of
// truth for which radio a throughput sample rode on, and now carries the full
// sudo-free radio detail macOS exposes (noise/SNR/tx-rate/MCS/PHY). Every detail
// field is populated ONLY when the radio read was associated (s.OK) — a parse miss
// while associated yields empty detail with OK still true, never a bogus number.
//
// Presence rules (all display-only; none drives detectEvents):
//   - RSSI / Noise are int+omitempty: an associated RSSI/Noise is ALWAYS negative,
//     so 0 is an unambiguous "absent".
//   - SNR is int+omitempty and comes from snrFrom(RSSI,Noise): 0 means "unknown"
//     (missing rssi/noise), rendered "—".
//   - MCS is a *int: MCS index 0 is a VALID reading, so a pointer disambiguates a
//     genuine "MCS 0" (non-nil &0) from "no MCS line" (nil). Presence is taken from
//     the parse flag SignalSample.MCSPresent, never from a value/PHY heuristic.
//
// SSID is already usableSSID-normalized at construction (see linkSnapFromAirport),
// so "" means redacted/absent, never "<redacted>".
type LinkSnap struct {
	SSID    string  `json:"ssid,omitempty"`         // usableSSID-normalized ("" if redacted/absent)
	Band    string  `json:"band,omitempty"`         // "2.4 GHz" / "5 GHz" / "6 GHz"; "" if unknown
	Channel string  `json:"channel,omitempty"`      // airport form "149/80" (NEVER wdutil "5g149/80")
	RSSI    int     `json:"rssi,omitempty"`         // dBm, negative; 0 => absent
	Noise   int     `json:"noise,omitempty"`        // dBm, negative; 0 => absent
	SNR     int     `json:"snr,omitempty"`          // dB = snrFrom(RSSI,Noise); 0 => UNKNOWN
	TxRate  float64 `json:"tx_rate_mbps,omitempty"` // transmit rate Mb/s; 0 => absent
	MCS     *int    `json:"mcs,omitempty"`          // MCS index; POINTER so a real MCS 0 survives (presence via MCSPresent)
	PHY     string  `json:"phy,omitempty"`          // "802.11ax" etc; "" => absent
	OK      bool    `json:"link_ok"`                // ASSOCIATED at snapshot time (from interfaceActive)
}

// Sample is one throughput observation (rates already computed). This JSON shape
// IS the Phase 2 /data.json wire schema, so the unit (bytes/sec) lives in the
// field names — only humanBps converts to bits for human display.
//
// Link is a POINTER (Phase 4): encoding/json's omitempty NEVER omits a non-pointer
// struct, so a value field would serialize "link":{"link_ok":false} on EVERY
// sample and silently break the Phase-2 wire. A *LinkSnap is nil (and omitted)
// until the serve-mode sampler stamps one, so warm-up samples and the entire
// --sample / bare-run path stay byte-identical. nil = "no snapshot / not serve";
// non-nil with OK=false = "read done, link down". Consumers MUST nil-guard.
type Sample struct {
	T               time.Time `json:"t"`
	RxBytes         uint64    `json:"rx_bytes"` // raw cumulative at T
	TxBytes         uint64    `json:"tx_bytes"`
	DownBytesPerSec float64   `json:"down_bytes_per_sec"` // bytes/sec since prev
	UpBytesPerSec   float64   `json:"up_bytes_per_sec"`
	Reset           bool      `json:"reset,omitempty"` // reset/wrap OR invalid read
	Link            *LinkSnap `json:"link,omitempty"`  // Phase 4 radio snapshot; nil off the serve path
}

// deltaRate computes bytes/sec between two cumulative readings over dt seconds.
// The guard order is mandatory and is what guarantees "never a multi-Gbps
// phantom spike": the cur<prev check runs BEFORE any subtraction, so the uint64
// subtraction (which would wrap to ~1.8e19 on cur<prev) is only ever evaluated
// on a known-nonnegative difference.
func deltaRate(prev, cur uint64, dt float64) (bps float64, reset bool) {
	if cur < prev { // reset/wrap — BEFORE any subtraction
		return 0, true
	}
	if dt < minDt { // div-by-zero / near-zero-dt guard
		return 0, false
	}
	return float64(cur-prev) / dt, false // subtraction only on known-nonneg
}

// newSample builds a Sample from the previous counters and the current reading
// at time now. Rates are ZERO and Reset=true whenever EITHER prev.OK or cur.OK
// is false (first sample, or the iface vanished/recovered), so a recovery
// reading is never diffed against a zeroed baseline. dt uses Go's monotonic
// clock via now.Sub(prevT).
func newSample(prev, cur IfaceCounters, prevT, now time.Time) Sample {
	s := Sample{T: now, RxBytes: cur.RxBytes, TxBytes: cur.TxBytes}
	if !prev.OK || !cur.OK {
		s.Reset = true
		return s
	}
	dt := now.Sub(prevT).Seconds()
	down, rdown := deltaRate(prev.RxBytes, cur.RxBytes, dt)
	up, rup := deltaRate(prev.TxBytes, cur.TxBytes, dt)
	s.DownBytesPerSec = down
	s.UpBytesPerSec = up
	s.Reset = rdown || rup
	return s
}

// nextBaseline decides what counters to carry forward as the next diff base. It
// returns prev unchanged when cur.OK==false (a zeroed/absent reading must NEVER
// become the baseline — that is the iface-recovery phantom-spike bug), and cur
// when cur.OK==true.
func nextBaseline(prev, cur IfaceCounters) IfaceCounters {
	if !cur.OK {
		return prev
	}
	return cur
}

// Ring is a fixed-capacity rolling buffer of Samples (newest-wins overwrite). It
// carries a sync.RWMutex now (not deferred): Snapshot's "safe to hand off" claim
// must be real, and Phase 2's HTTP reader needs locking immediately.
type Ring struct {
	mu   sync.RWMutex
	buf  []Sample
	cap  int
	head int // index of the next write
	size int
}

// NewRing returns a Ring of the given capacity; capacity<=0 is treated as 1 so
// Add never panics on a zero-length backing slice.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring{buf: make([]Sample, capacity), cap: capacity}
}

// Add appends a sample, overwriting the oldest once the ring is full.
func (r *Ring) Add(s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = s
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// Snapshot returns an independent copy of the live samples, oldest->newest.
func (r *Ring) Snapshot() []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Sample, r.size)
	start := (r.head - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(start+i)%r.cap]
	}
	return out
}

// Len returns the number of samples currently stored.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// humanBps formats a BYTES/sec rate for display as bits/sec, decimal (1000),
// with a bps / Kbps / Mbps / Gbps tier. It multiplies by 8 exactly once (the
// only *8 in this file). Negative and NaN/Inf inputs format as "0 bps".
func humanBps(bytesPerSec float64) string {
	if math.IsNaN(bytesPerSec) || math.IsInf(bytesPerSec, 0) || bytesPerSec < 0 {
		return "0 bps"
	}
	bits := bytesPerSec * 8 // the only *8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.1f Gbps", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1f Mbps", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.0f Kbps", bits/1e3)
	default:
		return fmt.Sprintf("%.0f bps", bits)
	}
}

// clampInterval enforces the sample-period floor: max(d, 250ms); 0/negative =>
// 250ms. Pure + tested rather than an inline `if *x < min` in main.go.
func clampInterval(d time.Duration) time.Duration {
	if d < minSampleInterval {
		return minSampleInterval
	}
	return d
}

// readCounters execs `netstat -ibn` (the -n is REQUIRED: without it netstat does
// reverse-DNS on every address row before emitting output, which can stall each
// tick) and calls parseNetstatIB. LC_ALL=C is cheap locale insurance.
func readCounters(iface string) IfaceCounters {
	cmd := exec.Command("netstat", "-ibn")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return IfaceCounters{Name: iface} // OK=false
	}
	return parseNetstatIB(string(out), iface)
}

// runSample loops: read -> newSample(prev,cur,prevT,now) -> ring.Add -> print.
// The baseline advances (via nextBaseline) ONLY on a valid read; an invalid read
// (cur.OK==false) prints an "iface down/not found" notice to stderr, is NOT added
// to the ring, and leaves prev/prevT untouched. prevT is a time.Time kept in the
// loop (monotonic) — dt is never derived from a Sample.T that has round-tripped
// through JSON. The first valid read is a warm-up (prev.OK==false): it seeds the
// baseline and is not printed. Stops on ctx done (SIGINT or the timeout).
func runSample(ctx context.Context, iface string, interval time.Duration, ring *Ring) {
	runSampleLoop(ctx, iface, interval, ring, true, nil, nil)
}

// runSampleQuiet is the serve-mode sampler loop: identical to runSample but it
// only ring.Add()s — it does NOT fmt.Printf each tick and does NOT emit the
// per-tick "iface down" stderr notice, so serve mode does not flood the terminal
// once per interval (which would scroll the startup URL away). The warm-up
// suppression and baseline logic stay single-sourced in runSampleLoop. The
// linkProvider (Phase 4) is a plain atomic read of the most-recent LinkSnap — it
// does NOT exec; the dedicated link goroutine (serve.go) does the system_profiler
// read at its own slower cadence.
func runSampleQuiet(ctx context.Context, iface string, interval time.Duration, ring *Ring, sink func(Sample), linkProvider func() *LinkSnap) {
	runSampleLoop(ctx, iface, interval, ring, false, sink, linkProvider)
}

// runSampleLoop is the shared sampler loop behind runSample (verbose=true,
// sink=nil) and runSampleQuiet (verbose=false, sink from serve mode). Only the
// terminal output is conditional on verbose; the read/newSample/ring.Add/baseline
// logic is identical for both so serve mode and --sample mode can never drift
// apart. sink is an optional per-sample hook (serve mode's history store ingest):
// it is called with each REAL sample at the SAME point as ring.Add (post-warm-up),
// is nil-safe, and is nil for --sample so a terminal-only run creates NO files.
// linkProvider (Phase 4) is an optional per-sample radio-snapshot hook: when
// non-nil the loop stamps its return (which may be nil during link warm-up) onto
// the Sample BEFORE ring.Add/sink. It is a plain atomic READ — NEVER an exec — so
// the 1s sampler does not spawn system_profiler; it is nil for --sample so that
// path stays byte-identical and execs nothing new.
func runSampleLoop(ctx context.Context, iface string, interval time.Duration, ring *Ring, verbose bool, sink func(Sample), linkProvider func() *LinkSnap) {
	var prev IfaceCounters
	var prevT time.Time

	tick := func() {
		now := time.Now()
		cur := readCounters(iface)
		if !cur.OK {
			if verbose {
				fmt.Fprintf(os.Stderr, "netdebug: interface %q down or not found — skipping sample\n", iface)
			}
			return // not added to the ring; prev/prevT untouched
		}
		s := newSample(prev, cur, prevT, now)
		if linkProvider != nil { // Phase 4: stamp the latest radio snapshot (may be nil)
			s.Link = linkProvider()
		}
		if prev.OK { // suppress the warm-up (first valid) sample
			ring.Add(s)
			if sink != nil {
				sink(s)
			}
			if verbose {
				fmt.Printf("%s  down %-10s up %-10s\n",
					now.Format("15:04:05"), humanBps(s.DownBytesPerSec), humanBps(s.UpBytesPerSec))
			}
		}
		prev = nextBaseline(prev, cur)
		prevT = now
	}

	tick() // seed the baseline immediately so the first printed line is a real rate
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// sampleStop builds the stop context for --sample: SIGINT/SIGTERM always stop it,
// and seconds>0 adds a timeout. The returned cancel must be deferred by the caller.
func sampleStop(seconds int) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	if seconds > 0 {
		tctx, tcancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		return tctx, func() { tcancel(); cancel() }
	}
	return ctx, cancel
}
