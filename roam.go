package main

// Phase 14 — the BAND / CHANNEL CHANGE JOURNAL (the "roam" journal). Sibling of the
// Phase-8 OUTAGE JOURNAL (outage.go), built on the SAME pure/impure split and the SAME
// durable store seam.
//
// MOTIVATION: netdebug already KNOWS the band changed — the per-minute history carries
// a Band, and the LIVE chart paints a transient "band change" marker (detectEvents) —
// but neither is a PERSISTENT, TIMESTAMPED record. The LIVE marker only covers the
// ~300-sample live window, so an older steer (e.g. a 5 GHz -> 2.4 GHz band-steer with
// NO outage) scrolls off and leaves no trace. This journal makes "when/why did my band
// change?" a glance on the dashboard, and it SURVIVES RESTART exactly like the outage
// journal.
//
// Pure/impure split (house style, same as outage.go): EVERYTHING in this file is PURE
// and table-tested in roam_test.go — detectTransition / roamCadence / modeToBand /
// marshalRoamLine / parseRoamLine / parseJSONLRoams / pruneRoams / sortRoamsByT /
// roamJSON. The impure layer (durability) is folded INTO historyStore in history.go
// (appendRoam / RoamsSnapshot + the at-open load/sweep) and the detector state lives in
// serve.go's observe closure (threaded into runReachLoop alongside the outage detector).
//
// SCOPE GUARD: Transition has NO slice/map/pointer field — every field is a value type
// — so RoamsSnapshot's append-copy is a genuine deep, race-safe copy (the same
// invariant that keeps Bucket / Outage safe). A later pointer field would silently
// break that copy — keep Transition ALL-VALUE.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Transition is one immutable journal record: a band / channel / BSSID change between
// two consecutive ASSOCIATED reach observations. ALL fields are value types
// (string/int/bool/time) — no slice/map/pointer — so RoamsSnapshot's append-copy is a
// genuine deep, race-safe copy (the Transition SCOPE GUARD).
//
// T is the observe time of the TO sample (the moment the change was first seen). When
// AfterPause is true a probing pause sat between the two samples, so the EXACT change
// time is unknown — T is the RESUME time and AfterPause flags it as approximate rather
// than fabricating a precise instant.
type Transition struct {
	T            time.Time `json:"t"`                        // observe time of the TO sample (resume time when AfterPause)
	FromBand     string    `json:"from_band,omitempty"`      // band BEFORE ("5 GHz"); "" if unknown
	ToBand       string    `json:"to_band,omitempty"`        // band AFTER ("2.4 GHz"); "" if unknown
	FromChannel  string    `json:"from_ch,omitempty"`        // channel BEFORE ("149/80"); "" if unknown
	ToChannel    string    `json:"to_ch,omitempty"`          // channel AFTER ("6"); "" if unknown
	BSSIDChanged bool      `json:"bssid_changed,omitempty"`  // the AP MAC changed (a true roam between radios)
	RSSI         int       `json:"rssi_at_change,omitempty"` // dBm at the TO sample; 0 => absent
	WithDrop     bool      `json:"with_drop,omitempty"`      // a drop/outage coincided (disconnect-driven) vs a CLEAN steer
	AfterPause   bool      `json:"after_pause,omitempty"`    // a probing pause sat between the samples => T is approximate
}

// RoamLink is the band/channel/BSSID/RSSI slice of one ASSOCIATED observation fed to
// detectTransition. (LinkSnap carries no BSSID — macOS redacts it system-wide without
// Location permission — so the live wiring passes BSSID="" and BSSIDChanged can only
// fire from a source that DOES have it; the pure detector supports it for completeness
// and testing.)
type RoamLink struct {
	Band    string
	Channel string
	BSSID   string
	RSSI    int
}

// detectTransition is the PURE band/channel/BSSID change detector. Given the PREVIOUS
// and CURRENT associated observations (and whether a drop or a probing pause sat between
// them), it returns an optional Transition.
//
// RULES (mirrors detectEvents' both-endpoints-non-empty guards EXACTLY so a warm-up /
// parse-miss flap — "5 GHz"->""->"5 GHz" — never fires a phantom):
//
//   - havePrev false (first sample): NO-OP. Nothing to compare against.
//   - A dimension CHANGED only when BOTH endpoints are non-empty and differ:
//     bandChanged  = prev.Band    != "" && cur.Band    != "" && prev.Band    != cur.Band
//     chanChanged  = prev.Channel != "" && cur.Channel != "" && prev.Channel != cur.Channel
//     bssidChanged = prev.BSSID   != "" && cur.BSSID   != "" && prev.BSSID   != cur.BSSID
//   - A Transition is emitted when band OR channel OR BSSID changed; otherwise NO-OP.
//   - WithDrop   = withDrop   (a drop/outage coincided — a disconnect-driven change).
//   - AfterPause = afterPause (a probing pause sat between — T is approximate).
//   - RSSI = cur.RSSI (the reading AT the change, the TO sample).
//
// PURE, no IO. The caller (serve.go's observe) owns the prev/withDrop/afterPause state.
func detectTransition(prev, cur RoamLink, havePrev, withDrop, afterPause bool, t time.Time) (Transition, bool) {
	if !havePrev {
		return Transition{}, false
	}
	bandChanged := prev.Band != "" && cur.Band != "" && prev.Band != cur.Band
	chanChanged := prev.Channel != "" && cur.Channel != "" && prev.Channel != cur.Channel
	bssidChanged := prev.BSSID != "" && cur.BSSID != "" && prev.BSSID != cur.BSSID
	if !bandChanged && !chanChanged && !bssidChanged {
		return Transition{}, false
	}
	return Transition{
		T:            t,
		FromBand:     prev.Band,
		ToBand:       cur.Band,
		FromChannel:  prev.Channel,
		ToChannel:    cur.Channel,
		BSSIDChanged: bssidChanged,
		RSSI:         cur.RSSI,
		WithDrop:     withDrop,
		AfterPause:   afterPause,
	}, true
}

// RoamCadence summarizes TODAY's journal for the dashboard tiles (COUNT TODAY /
// MOST-STEERED-TO BAND). All-value (never marshals null). The LAST CHANGE tile is
// derived client-side from the newest record's T, so it is not duplicated here.
type RoamCadence struct {
	CountToday        int    `json:"count_today"`
	MostSteeredToBand string `json:"most_steered_to_band,omitempty"` // most-frequent TO band today
}

// roamCadence computes RoamCadence over transitions whose T is the SAME LOCAL CALENDAR
// DAY as now (both normalized to time.Local before comparing y/m/d, so a fixed-offset-
// zone reload / DST boundary does not mis-bucket). Mirrors outageCadence. Empty => zero.
func roamCadence(roams []Transition, now time.Time) RoamCadence {
	var c RoamCadence
	ny, nm, nd := now.In(time.Local).Date()
	today := make([]Transition, 0, len(roams))
	for _, r := range roams {
		ly, lm, ld := r.T.In(time.Local).Date()
		if ly == ny && lm == nm && ld == nd {
			today = append(today, r)
		}
	}
	c.CountToday = len(today)
	if len(today) == 0 {
		return c
	}
	c.MostSteeredToBand = modeToBand(today)
	return c
}

// modeToBand returns the most-frequent non-empty TO band among the given transitions.
// Ties are broken DETERMINISTICALLY by the lexicographically smallest band string (Go's
// map iteration is randomized, so an explicit tie-break is mandatory). Mirrors
// modeChannel. "" when none has a to-band.
func modeToBand(roams []Transition) string {
	counts := make(map[string]int)
	for _, r := range roams {
		if r.ToBand != "" {
			counts[r.ToBand]++
		}
	}
	best := ""
	bestCount := 0
	for b, c := range counts {
		if c > bestCount || (c == bestCount && b < best) {
			best, bestCount = b, c
		}
	}
	return best
}

// marshalRoamLine serializes one Transition to one line of JSONL (no trailing newline —
// the caller adds it as part of the single combined write, like marshalOutageLine).
func marshalRoamLine(r Transition) ([]byte, error) {
	return json.Marshal(r)
}

// parseRoamLine parses ONE line of JSONL into a Transition and REJECTS a semantically
// invalid record so a bogus/zero/no-op line is skipped, never admitted: zero T; and a
// record that is not actually a transition (no band, channel OR BSSID change) — a torn
// or fabricated no-op line must never masquerade as a change.
func parseRoamLine(line []byte) (Transition, error) {
	var r Transition
	if err := json.Unmarshal(line, &r); err != nil {
		return Transition{}, err
	}
	if r.T.IsZero() {
		return Transition{}, fmt.Errorf("roam line has zero time")
	}
	bandChanged := r.FromBand != r.ToBand
	chanChanged := r.FromChannel != r.ToChannel
	if !bandChanged && !chanChanged && !r.BSSIDChanged {
		return Transition{}, fmt.Errorf("roam line is not a transition (no band/channel/BSSID change)")
	}
	return r, nil
}

// parseJSONLRoams reads a whole JSONL blob: split on '\n', SKIP EVERY blank or
// unparseable line and continue (a crash-interrupted rewrite or any torn/garbage middle
// line must never abort the load or panic). Missing/empty blob => empty slice. Does NOT
// sort — the caller runs sortRoamsByT. Mirrors parseJSONLOutages.
func parseJSONLRoams(data []byte) []Transition {
	out := []Transition{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		r, err := parseRoamLine(line)
		if err != nil {
			continue // skip torn/garbage/invalid lines; never abort the whole load
		}
		out = append(out, r)
	}
	return out
}

// pruneRoams drops records whose T is older than now-ttl, keyed on T (consistent with
// sorting + cadence). Empty-safe. best-effort under clock skew (same posture as
// pruneOutages).
func pruneRoams(roams []Transition, now time.Time, ttl time.Duration) []Transition {
	cutoff := now.Add(-ttl)
	out := make([]Transition, 0, len(roams))
	for _, r := range roams {
		if r.T.Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortRoamsByT sorts ascending by T using Before (monotonic-strip safe), mirroring
// sortOutagesByStart. Applied at load and in RoamsSnapshot/roamJSON.
func sortRoamsByT(r []Transition) {
	sort.Slice(r, func(i, j int) bool { return r[i].T.Before(r[j].T) })
}

// roamJSON is the PURE /roam.json serializer with an INJECTED now (serve_test needs a
// fixed now; it does NOT call time.Now()). It forces roams to a NON-nil array (never
// "roams":null) and pairs it with today's cadence summary. Mirrors outageJSON.
func roamJSON(roams []Transition, now time.Time) ([]byte, error) {
	if roams == nil {
		roams = []Transition{}
	}
	sortRoamsByT(roams)
	return json.Marshal(struct {
		Roams   []Transition `json:"roams"`
		Cadence RoamCadence  `json:"cadence"`
	}{Roams: roams, Cadence: roamCadence(roams, now)})
}
