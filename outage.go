package main

// Phase 8 — the OUTAGE JOURNAL. The feature that most directly serves the project's
// founding mission: catch the recurring ~31-minute home disconnect, record WHAT it
// was and the radio conditions at the moment, and surface the cadence so the pattern
// is undeniable.
//
// An outage FUSES two existing signals: the per-sample LinkSnap.OK (Phase 4,
// association lost) AND internet-unreachable from Phase 7's ReachSample. Either alone
// is sufficient — bad(o) = !LinkOK || !InternetOK — and the CAUSE classifies which.
// bad() intentionally IGNORES GatewayOK: a router that drops ICMP to itself but
// forwards fine is NOT an outage (that only affects the CAUSE string).
//
// Pure/impure split (house style, same as history.go / reach.go): EVERYTHING in this
// file is PURE and table-tested in outage_test.go — classifyCause / bad / stepOutage /
// stepReset / outageCadence / modeChannel / marshalOutageLine / parseOutageLine /
// parseJSONLOutages / pruneOutages / sortOutagesByStart / clampOutageThreshold /
// outageJSON. The impure layer (durability) is folded INTO historyStore in history.go
// (appendOutage / OutagesSnapshot + the at-open load/sweep) and the detector CLOSURE
// lives in serve.go (built in serveDashboard, threaded into runReachLoop as `observe`).
//
// SCOPE GUARD: Outage has NO slice/map/pointer field — every field is a value type —
// so OutagesSnapshot's append-copy is a genuine deep, race-safe copy (the same
// invariant that keeps Bucket safe; see history.go). A later pointer field (e.g. a
// *int MCS) would silently break that copy — keep Outage ALL-VALUE.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Cause strings (single Go source of truth; parseOutageLine whitelists this set).
const (
	causeLinkDrop     = "link-drop"           // LinkSnap.OK false — the local radio lost association.
	causeGatewayUnrea = "gateway-unreachable" // link OK, gateway ping fails — router/mesh backhaul dead.
	causeInternetDown = "internet-down"       // link + gateway OK, internet ping fails — ISP/uplink.

	// Debounce + retention defaults (retunable via config; clamped at the impure
	// boundary). A single dropped ping is never an outage (2 consecutive bad opens).
	defaultOutageStartThresh = 2
	defaultOutageEndThresh   = 2
	defaultOutageTTL         = 90 * 24 * time.Hour
)

// Outage is one immutable journal record. ALL fields are value types (string/int/
// float/time) — no slice/map/pointer — so OutagesSnapshot's append-copy is a genuine
// deep, race-safe copy (the Bucket SCOPE GUARD invariant). A persisted outage is
// ALWAYS closed (End set, DurationSec >= 0).
type Outage struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`          // always set once persisted (closed)
	DurationSec float64   `json:"duration_sec"` // End-Start, wall-clock
	Cause       string    `json:"cause"`        // link-drop | gateway-unreachable | internet-down
	// Radio context AT ONSET, from the LAST-KNOWN-GOOD LinkSnap (must_fix #4): a
	// link-drop's onset snapshot is already disassociated and BLANK, so the context
	// comes from the last associated link, not the onset snap.
	Band    string `json:"band,omitempty"`
	Channel string `json:"channel,omitempty"`
	RSSI    int    `json:"rssi,omitempty"`
	Noise   int    `json:"noise,omitempty"`
	SNR     int    `json:"snr,omitempty"`
	SSID    string `json:"ssid,omitempty"` // usableSSID-normalized (already clean on LinkSnap)
}

// classifyCause maps the three health bits (AT ONSET) to a cause string. Precedence:
// !linkOK => link-drop; else !gatewayOK => gateway-unreachable; else internet-down.
// PURE; called ONLY at onset on a bad obs. Mapping from a ReachSample: gatewayOK =
// s.GatewayOK, internetOK = s.InternetOK — so a gateway_only class (gwOK true, inetOK
// false, linkOK true) classifies internet-down, and a "down" class (gwOK false, linkOK
// true) classifies gateway-unreachable.
func classifyCause(linkOK, gatewayOK, internetOK bool) string {
	switch {
	case !linkOK:
		return causeLinkDrop
	case !gatewayOK:
		return causeGatewayUnrea
	default:
		return causeInternetDown
	}
}

// Obs is one fused observation fed to the state machine (one per PROBED reach cycle).
// LinkOK is the FAIL-OPEN bit the caller computes (nil link holder => true; must_fix
// #2 "unknown is not down"). Link is the raw snapshot, used ONLY to update lastGoodLink
// and as the onset-context fallback — never for bad() (that reads LinkOK).
type Obs struct {
	T          time.Time // time.Now().Round(0): monotonic STRIPPED so prod duration == the wall-clock pure test
	LinkOK     bool
	GatewayOK  bool
	InternetOK bool
	Link       LinkSnap
}

// bad reports whether an observation counts toward an outage: link lost OR internet
// unreachable. GatewayOK is intentionally NOT consulted here (see the file header).
func bad(o Obs) bool { return !o.LinkOK || !o.InternetOK }

// outageState is the debounce machine's state (unexported, pure). lastGoodLink
// SURVIVES a close (so the next outage still has context); open/badStreak/goodStreak/
// pending are zeroed on close (must_fix #3).
type outageState struct {
	open         bool
	badStreak    int
	goodStreak   int
	pending      Outage   // onset stamped on badStreak 0->1; End re-stamped on each goodStreak 0->1
	lastGoodLink LinkSnap // most recent Obs.Link with OK && Channel != ""
}

// stepOutage advances the machine with one Obs + thresholds. Returns the next state
// and, when an outage just CLOSED, the finalized Outage (ok=true). PURE, no IO — this
// is where every missed/duplicate/mis-timed-outage bug would live, so it is table-
// tested exhaustively. EXACT RULES (committee-pinned, §1a):
//
//  0. if o.Link.OK && o.Link.Channel != "" { st.lastGoodLink = o.Link }
//  1. b := bad(o)
//  2. NOT open:
//     bad:  if badStreak==0 (0->1): stamp pending.Start=o.T, pending.Cause=
//     classifyCause(...), radio context from (lastGoodLink if it has a
//     Channel else o.Link); THEN badStreak++; THEN if badStreak>=startThresh
//     open=true. goodStreak stays 0. [capture BEFORE increment so startThresh=1
//     still records onset on the opening obs — must_fix #3]
//     good: badStreak=0.
//  3. open:
//     good: if goodStreak==0 (0->1): pending.End=o.T (stamp/RE-stamp — must_fix #1).
//     THEN goodStreak++. if goodStreak>=endThresh: out:=pending;
//     out.DurationSec=out.End.Sub(out.Start).Seconds(); reset st to
//     {lastGoodLink: st.lastGoodLink}; return st,out,true.
//     bad:  goodStreak=0 (next fresh good re-stamps End).
//
// Returns (st, Outage{}, false) in every non-closing path.
func stepOutage(st outageState, o Obs, startThresh, endThresh int) (outageState, Outage, bool) {
	if o.Link.OK && o.Link.Channel != "" {
		st.lastGoodLink = o.Link
	}
	b := bad(o)

	if !st.open {
		if b {
			if st.badStreak == 0 { // 0->1 transition: capture onset (immutable for the record)
				st.pending = Outage{
					Start: o.T,
					Cause: classifyCause(o.LinkOK, o.GatewayOK, o.InternetOK),
				}
				ctx := st.lastGoodLink // last associated link — the "what channel was I on" record
				if ctx.Channel == "" { // no good link ever seen: fall back to the onset snap
					ctx = o.Link
				}
				st.pending.Band = ctx.Band
				st.pending.Channel = ctx.Channel
				st.pending.RSSI = ctx.RSSI
				st.pending.Noise = ctx.Noise
				st.pending.SNR = ctx.SNR
				st.pending.SSID = ctx.SSID
			}
			st.badStreak++
			if st.badStreak >= startThresh {
				st.open = true
			}
		} else {
			st.badStreak = 0
		}
		return st, Outage{}, false
	}

	// open
	if !b {
		if st.goodStreak == 0 { // 0->1 transition: stamp/RE-stamp End to the FIRST good of this run
			st.pending.End = o.T
		}
		st.goodStreak++
		if st.goodStreak >= endThresh {
			out := st.pending
			out.DurationSec = out.End.Sub(out.Start).Seconds()
			st = outageState{lastGoodLink: st.lastGoodLink} // RESET; keep only lastGoodLink (must_fix #3)
			return st, out, true
		}
	} else {
		st.goodStreak = 0 // a bad mid-good-run drops the streak; next fresh good re-stamps End
	}
	return st, Outage{}, false
}

// stepReset returns a zeroed outageState (keeps nothing of the open/pending outage) —
// the gated/paused-tick hook (must_fix #5). An outage straddling a battery/screen-lock
// pause is DISCARDED, never persisted (unknown duration — identical to the still-open-
// at-shutdown rule). lastGoodLink is KEPT (a pause does not invalidate "the last
// channel you were on"). Idempotent: zero stays zero.
func stepReset(st outageState) outageState {
	return outageState{lastGoodLink: st.lastGoodLink}
}

// Cadence summarizes TODAY's journal for the "is it the 31-minute thing" readout
// (must_fix #7). All-value (never marshals null).
type Cadence struct {
	CountToday   int     `json:"count_today"`
	MeanGapMin   float64 `json:"mean_gap_min"`           // mean of today's inter-start gaps (minutes)
	MedianGapMin float64 `json:"median_gap_min"`         // median (even count => mean of the two middle)
	LastGapMin   float64 `json:"last_gap_min"`           // the most recent inter-start gap (lastStart-prevStart), NOT now-lastStart
	ModeChannel  string  `json:"mode_channel,omitempty"` // most-frequent onset Channel today — the 149/157 proof
}

// outageCadence computes Cadence over outages whose Start is the SAME LOCAL CALENDAR
// DAY as now (both normalized to time.Local before comparing y/m/d, so a fixed-offset-
// zone reload / DST boundary does not mis-bucket). Gaps are between consecutive today
// Starts (sorted), clamped to >=0 (a backward NTP step cannot manufacture a negative
// gap). Empty/single => zero-ish Cadence, no panic, no div-by-zero. PURE + tested.
func outageCadence(outages []Outage, now time.Time) Cadence {
	var c Cadence
	ny, nm, nd := now.In(time.Local).Date()
	today := make([]Outage, 0, len(outages))
	for _, o := range outages {
		ly, lm, ld := o.Start.In(time.Local).Date()
		if ly == ny && lm == nm && ld == nd {
			today = append(today, o)
		}
	}
	c.CountToday = len(today)
	if len(today) == 0 {
		return c
	}
	sortOutagesByStart(today)
	c.ModeChannel = modeChannel(today)
	if len(today) < 2 {
		return c // no inter-start gaps with a single outage
	}
	gaps := make([]float64, 0, len(today)-1)
	var sum float64
	for i := 1; i < len(today); i++ {
		g := today[i].Start.Sub(today[i-1].Start).Minutes()
		if g < 0 { // clamp a backward clock step to a non-negative gap
			g = 0
		}
		gaps = append(gaps, g)
		sum += g
	}
	c.MeanGapMin = sum / float64(len(gaps))
	c.LastGapMin = gaps[len(gaps)-1] // lastStart - previousStart, independent of now
	c.MedianGapMin = medianFloat(gaps)
	return c
}

// medianFloat is the median of xs (even count => mean of the two middle values). Pure;
// sorts a COPY so the caller's slice order is untouched. Empty => 0.
func medianFloat(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// modeChannel returns the most-frequent non-empty onset Channel among the given
// outages (mirrors modeBand's shape). Ties are broken DETERMINISTICALLY by the
// lexicographically smallest channel string (Go's map iteration is randomized, so an
// explicit tie-break is mandatory). "" when none has a channel.
func modeChannel(outages []Outage) string {
	counts := make(map[string]int)
	for _, o := range outages {
		if o.Channel != "" {
			counts[o.Channel]++
		}
	}
	best := ""
	bestCount := 0
	for ch, c := range counts {
		if c > bestCount || (c == bestCount && ch < best) {
			best, bestCount = ch, c
		}
	}
	return best
}

// marshalOutageLine serializes one Outage to one line of JSONL (no trailing newline —
// the caller adds it as part of the single combined write, like marshalBucketLine).
func marshalOutageLine(o Outage) ([]byte, error) {
	return json.Marshal(o)
}

// parseOutageLine parses ONE line of JSONL into an Outage and REJECTS a semantically
// invalid record so a bogus epoch/zero/negative line is skipped, never admitted:
// zero Start; empty/non-whitelisted Cause; zero End (a persisted outage is ALWAYS
// closed); End.Before(Start); negative DurationSec.
func parseOutageLine(line []byte) (Outage, error) {
	var o Outage
	if err := json.Unmarshal(line, &o); err != nil {
		return Outage{}, err
	}
	if o.Start.IsZero() {
		return Outage{}, fmt.Errorf("outage line has zero start")
	}
	switch o.Cause {
	case causeLinkDrop, causeGatewayUnrea, causeInternetDown:
	default:
		return Outage{}, fmt.Errorf("outage line has invalid cause %q", o.Cause)
	}
	if o.End.IsZero() {
		return Outage{}, fmt.Errorf("outage line has zero end (a persisted outage is always closed)")
	}
	if o.End.Before(o.Start) {
		return Outage{}, fmt.Errorf("outage line has end before start")
	}
	if o.DurationSec < 0 {
		return Outage{}, fmt.Errorf("outage line has negative duration")
	}
	return o, nil
}

// parseJSONLOutages reads a whole JSONL blob: split on '\n', SKIP EVERY blank or
// unparseable line and continue (a crash-interrupted rewrite or any torn/garbage middle
// line must never abort the load or panic). Missing/empty blob => empty slice. Does NOT
// sort — the caller runs sortOutagesByStart.
func parseJSONLOutages(data []byte) []Outage {
	out := []Outage{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		o, err := parseOutageLine(line)
		if err != nil {
			continue // skip torn/garbage/invalid lines; never abort the whole load
		}
		out = append(out, o)
	}
	return out
}

// pruneOutages drops records whose Start is older than now-ttl (default 90d), keyed on
// Start (consistent with sorting + cadence). Empty-safe. best-effort under clock skew
// (same posture as pruneBuckets).
func pruneOutages(outages []Outage, now time.Time, ttl time.Duration) []Outage {
	cutoff := now.Add(-ttl)
	out := make([]Outage, 0, len(outages))
	for _, o := range outages {
		if o.Start.Before(cutoff) {
			continue
		}
		out = append(out, o)
	}
	return out
}

// sortOutagesByStart sorts ascending by Start using Before (monotonic-strip safe),
// mirroring sortBucketsByStart. Applied at load and in OutagesSnapshot/outageJSON so
// cadence's consecutive-Start gaps are over true Start order.
func sortOutagesByStart(o []Outage) {
	sort.Slice(o, func(i, j int) bool { return o[i].Start.Before(o[j].Start) })
}

// clampOutageThreshold clamps a start/end threshold to >=1 (0/negative => default 2),
// at the IMPURE boundary (analogous to clampTTL) so a stray 0 in config cannot make the
// pure machine open/close on the first obs. stepOutage itself stays threshold-agnostic.
func clampOutageThreshold(n int) int {
	if n < 1 {
		return defaultOutageStartThresh
	}
	return n
}

// outageJSON is the PURE /outages.json serializer with an INJECTED now (serve_test needs
// a fixed now; it does NOT call time.Now()). It forces outages to a NON-nil array
// (historyJSON house rule — never "outages":null) and pairs it with the cadence summary.
func outageJSON(outages []Outage, now time.Time) ([]byte, error) {
	if outages == nil {
		outages = []Outage{}
	}
	sortOutagesByStart(outages)
	return json.Marshal(struct {
		Outages []Outage `json:"outages"`
		Cadence Cadence  `json:"cadence"`
	}{Outages: outages, Cadence: outageCadence(outages, now)})
}
