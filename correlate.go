package main

// Phase 4 — band / SSID / RSSI correlation + annotations (the differentiator).
// This file is the PURE core: it maps a radio reading into a per-sample LinkSnap,
// walks a time-ordered []Sample to emit drop/roam/band_change Events, and provides
// the band-color key + dominant-band helpers. No IO lives here (the impure
// system_profiler/ifconfig reader is readRadio in airspace.go); everything below
// is table-tested in correlate_test.go.
//
// Pure/impure split (house style): linkSnapFromAirport / detectEvents /
// bandColorKey / clampLinkInterval / modeBand / dominantBand / bandOf are PURE;
// readRadio (airspace.go) is the thin exec wrapper that feeds them.

import (
	"strings"
	"time"
)

// Band color keys — the SINGLE Go source of the categorical vocabulary the JS
// palette (dashboard.go) keys off. bandColorKey pins these exact strings so the
// Go producer and the JS consumer cannot silently diverge (the table test asserts
// the whole set).
const (
	bandKey24      = "b24"
	bandKey5       = "b5"
	bandKey6       = "b6"
	bandKeyUnknown = "unknown"

	// defaultLinkInterval is the radio resample cadence applied when a flag/config
	// value is absent or <= 0 (see clampLinkInterval): deliberately SLOWER than the
	// 1s byte sampler so system_profiler is not spawned once per second.
	defaultLinkInterval = 5 * time.Second
)

// linkSnapFromAirport is the SOLE link mapper (airport / system_profiler path).
// Pure, no IO. OK is passed in from interfaceActive (NOT from s.OK — see the OK
// SOURCE note in phase4.plan §2): parseAirportSample returns OK=false for both
// "not associated" AND a benign parse miss, so deriving the drop bit from it would
// emit a phantom drop on a single flaky read. Band/Channel/RSSI are taken from s
// ONLY when s.OK, so a parse miss while associated yields empty detail fields with
// OK still true — never a bogus band. SSID is usableSSID-normalized HERE (one
// source of truth) so the stored snapshot is already clean and detectEvents stays
// a plain comparator; SignalSample has no SSID field, so it is supplied separately
// (from parseSystemProfilerSSID over the SAME command output).
func linkSnapFromAirport(s SignalSample, ssid string, active bool) LinkSnap {
	snap := LinkSnap{OK: active}
	if usableSSID(ssid) {
		snap.SSID = ssid
	}
	if s.OK {
		snap.Band, snap.Channel, snap.RSSI = s.Band, s.Channel, s.RSSI
		snap.Noise = s.Noise
		// snrFrom (NOT s.SNR) is the ONE wire source of truth for SNR:
		// parseAirportSample sets s.SNR = rssi-noise UNGUARDED, so it and snrFrom
		// diverge exactly in the rssi==0 / noise==0 cases. Recompute via the guarded
		// helper here; do NOT "simplify" to s.SNR.
		snap.SNR = snrFrom(s.RSSI, s.Noise)
		snap.TxRate = s.RateMbps
		// MCS presence comes from the PARSE flag, not a value/PHY heuristic: s.MCS==0
		// is identical for "MCS Index: 0" and "no MCS line", and a legacy 802.11a/g
		// link reports PHY with NO MCS line — so neither a value test nor a PHY test
		// can tell a real 0 from absent. Only the parse flag can.
		if s.MCSPresent {
			m := s.MCS
			snap.MCS = &m
		}
		snap.PHY = s.PHY
	}
	return snap
}

// snrFrom returns the signal-to-noise ratio in dB. MISSING-NOISE GUARD: noise 0
// means "no Signal/Noise line parsed" (an associated noise floor is always
// negative), so SNR is unknown => return 0. Likewise rssi 0 => 0. A negative
// result is NOT clamped: a genuine rssi<noise is a real (bad) reading, so
// snrFrom(-92,-43) == -49.
func snrFrom(rssi, noise int) int {
	if rssi == 0 || noise == 0 {
		return 0 // unknown, not a real 0
	}
	return rssi - noise // e.g. -43 - (-92) = 49 dB
}

// ssidHiddenLabel is the honest sentinel shown when no user label is supplied:
// macOS redacts the real SSID system-wide without Location permission.
const ssidHiddenLabel = "HIDDEN (macOS-redacted)"

// ssidDisplay resolves what to SHOW for the (redacted) network name: the
// user-supplied label verbatim when set, else the honest sentinel. TrimSpace
// decides PRESENCE only — the returned label is never trimmed/mutated, so " Home "
// is returned verbatim. One source of truth so the Go producer and the page agree.
func ssidDisplay(label string) string {
	if strings.TrimSpace(label) != "" {
		return label // presence test only — do NOT return the trimmed value
	}
	return ssidHiddenLabel
}

// Event is a timeline annotation over the live/serialized sample window.
type Event struct {
	T      time.Time `json:"t"`
	Kind   string    `json:"kind"` // "drop" | "roam" | "band_change"
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Detail string    `json:"detail,omitempty"` // which dimension ("ssid"/"channel") for a roam
}

// Event kinds.
const (
	eventDrop       = "drop"
	eventRoam       = "roam"
	eventBandChange = "band_change"
	roamDetailSSID  = "ssid"
	roamDetailChan  = "channel"
)

// linkOf returns a sample's LinkSnap, nil-guarded: a nil Link reads as OK=false
// with empty fields (the warm-up / --sample / link-down semantics).
func linkOf(s Sample) LinkSnap {
	if s.Link == nil {
		return LinkSnap{}
	}
	return *s.Link
}

// detectEvents walks a time-ordered []Sample and emits drop/roam/band_change
// Events. Pure, single-pass O(n), allocation-light, and NEVER panics on an empty /
// single / leading-down slice. It is run over the SAME slice /data.json serializes
// (serve.go), so Event.T always matches a sample in that slice.
//
//	drop        : prev.OK && !cur.OK (ASSOCIATION lost). From = last Band, or SSID
//	              if Band "". To = "".
//	roam / band_change: computed ONLY when prev.OK && cur.OK are BOTH true, via a
//	              PRECEDENCE LADDER (a band change always coincides with a channel
//	              change, so a flat scheme would double-count):
//	                band_change (both Bands non-empty and differ) takes precedence,
//	                else roam-by-SSID (both SSIDs non-empty and differ),
//	                else roam-by-channel (both Channels non-empty and differ).
//
// The both-endpoints-non-empty guards stop a transient parse miss
// ("149/80"->""->"149/80") or a redaction flip from masquerading as a roam (SSID
// is already normalized to "" at store time, so "<redacted>" never reaches here).
// RSSI NEVER drives an event. Only consecutive OK pairs are compared, so a change
// that happened DURING an outage is not re-emitted at reconnect — there is
// deliberately no "reassociate" event (scope is drop/roam/band_change).
func detectEvents(samples []Sample) []Event {
	events := []Event{}
	for i := 1; i < len(samples); i++ {
		prev := linkOf(samples[i-1])
		cur := linkOf(samples[i])

		if prev.OK && !cur.OK { // association lost
			from := prev.Band
			if from == "" {
				from = prev.SSID
			}
			events = append(events, Event{T: samples[i].T, Kind: eventDrop, From: from})
			continue
		}
		if !prev.OK || !cur.OK { // across/into an outage, or still down => nothing
			continue
		}

		switch {
		case prev.Band != "" && cur.Band != "" && prev.Band != cur.Band:
			events = append(events, Event{T: samples[i].T, Kind: eventBandChange, From: prev.Band, To: cur.Band})
		case prev.SSID != "" && cur.SSID != "" && prev.SSID != cur.SSID:
			events = append(events, Event{T: samples[i].T, Kind: eventRoam, From: prev.SSID, To: cur.SSID, Detail: roamDetailSSID})
		case prev.Channel != "" && cur.Channel != "" && prev.Channel != cur.Channel:
			events = append(events, Event{T: samples[i].T, Kind: eventRoam, From: prev.Channel, To: cur.Channel, Detail: roamDetailChan})
		}
	}
	return events
}

// bandColorKey returns a stable categorical key per band for client coloring. The
// table test PINS the exact key set so the Go producer and the JS palette (which
// keys off these exact strings) cannot drift.
func bandColorKey(band string) string {
	switch band {
	case "2.4 GHz":
		return bandKey24
	case "5 GHz":
		return bandKey5
	case "6 GHz":
		return bandKey6
	default:
		return bandKeyUnknown
	}
}

// clampLinkInterval is the link-cadence clamp: d <= 0 => default 5s; floor 1s.
// DISTINCT from clampInterval (bandwidth.go, 250ms floor) — the radio cadence floor
// is 1s, so do NOT reuse clampInterval.
func clampLinkInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultLinkInterval
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

// bandPriority is the fixed, documented tie-break order among REAL bands so
// modeBand is deterministic under Go's random map iteration (6 GHz > 5 GHz > 2.4
// GHz). An unlisted real band gets priority 0 (still beats "", never a tie-winner
// against a listed band).
var bandPriority = map[string]int{"6 GHz": 3, "5 GHz": 2, "2.4 GHz": 1}

// modeBand picks a window's dominant band from per-band counts. It prefers the
// most-common REAL band and returns "" ONLY when the window has no real-band
// sample (so a mostly-unknown minute that saw one real band colors as that band —
// a failed read never suppresses a real band). Ties among real bands break by
// bandPriority.
func modeBand(counts map[string]int) string {
	best := ""
	bestCount := 0
	for band, c := range counts {
		if band == "" { // unknown never wins
			continue
		}
		if c > bestCount || (c == bestCount && bandPriority[band] > bandPriority[best]) {
			best = band
			bestCount = c
		}
	}
	return best
}

// bandOf returns a sample's band, nil-guarding Link ("" when nil/unknown).
func bandOf(s Sample) string {
	if s.Link == nil {
		return ""
	}
	return s.Link.Band
}

// dominantBand counts bands over a []Sample (nil-guarding Link) and returns
// modeBand(counts). The bulk/reload-path helper (bucketize et al.).
func dominantBand(samples []Sample) string {
	counts := make(map[string]int)
	for _, s := range samples {
		counts[bandOf(s)]++
	}
	return modeBand(counts)
}
