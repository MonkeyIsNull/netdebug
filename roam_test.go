package main

import (
	"testing"
	"time"
)

// roamT is a fixed base instant for the pure detector/cadence tests (wall-clock only).
var roamBase = time.Date(2026, 10, 8, 11, 20, 0, 0, time.Local)

// TestDetectTransition is the table-driven heart of the Phase-14 pure detector. Each
// case feeds a (prev, cur, havePrev, withDrop, afterPause) tuple and asserts whether a
// Transition is emitted and its salient fields.
func TestDetectTransition(t *testing.T) {
	cases := []struct {
		name       string
		prev, cur  RoamLink
		havePrev   bool
		withDrop   bool
		afterPause bool
		wantEmit   bool
		// expectations (only checked when wantEmit)
		fromBand, toBand string
		fromCh, toCh     string
		bssidChanged     bool
		rssi             int
		withDropWant     bool
		afterPauseWant   bool
	}{
		{
			name:     "same-band no-op (identical)",
			prev:     RoamLink{Band: "5 GHz", Channel: "149", RSSI: -55},
			cur:      RoamLink{Band: "5 GHz", Channel: "149", RSSI: -56},
			havePrev: true,
			wantEmit: false,
		},
		{
			name:         "band steer, no drop",
			prev:         RoamLink{Band: "5 GHz", Channel: "149", RSSI: -55},
			cur:          RoamLink{Band: "2.4 GHz", Channel: "6", RSSI: -60},
			havePrev:     true,
			wantEmit:     true,
			fromBand:     "5 GHz",
			toBand:       "2.4 GHz",
			fromCh:       "149",
			toCh:         "6",
			rssi:         -60,
			withDropWant: false,
		},
		{
			name:         "channel change, same band",
			prev:         RoamLink{Band: "5 GHz", Channel: "149", RSSI: -55},
			cur:          RoamLink{Band: "5 GHz", Channel: "157", RSSI: -58},
			havePrev:     true,
			wantEmit:     true,
			fromBand:     "5 GHz",
			toBand:       "5 GHz",
			fromCh:       "149",
			toCh:         "157",
			rssi:         -58,
			withDropWant: false,
		},
		{
			name:         "BSSID roam (same band/channel)",
			prev:         RoamLink{Band: "5 GHz", Channel: "149", BSSID: "aa:bb", RSSI: -55},
			cur:          RoamLink{Band: "5 GHz", Channel: "149", BSSID: "cc:dd", RSSI: -50},
			havePrev:     true,
			wantEmit:     true,
			fromBand:     "5 GHz",
			toBand:       "5 GHz",
			fromCh:       "149",
			toCh:         "149",
			bssidChanged: true,
			rssi:         -50,
		},
		{
			name:         "drop-triggered band change",
			prev:         RoamLink{Band: "5 GHz", Channel: "149", RSSI: -55},
			cur:          RoamLink{Band: "2.4 GHz", Channel: "6", RSSI: -70},
			havePrev:     true,
			withDrop:     true,
			wantEmit:     true,
			fromBand:     "5 GHz",
			toBand:       "2.4 GHz",
			rssi:         -70,
			withDropWant: true,
		},
		{
			name:           "band change straddling a pause (approximate time)",
			prev:           RoamLink{Band: "5 GHz", Channel: "149", RSSI: -55},
			cur:            RoamLink{Band: "2.4 GHz", Channel: "6", RSSI: -62},
			havePrev:       true,
			afterPause:     true,
			wantEmit:       true,
			fromBand:       "5 GHz",
			toBand:         "2.4 GHz",
			afterPauseWant: true,
		},
		{
			name:     "first sample (no prior) is a no-op",
			prev:     RoamLink{},
			cur:      RoamLink{Band: "5 GHz", Channel: "149"},
			havePrev: false,
			wantEmit: false,
		},
		{
			name:     "warm-up phantom guard: empty prev band never fires",
			prev:     RoamLink{Band: "", Channel: ""},
			cur:      RoamLink{Band: "5 GHz", Channel: "149"},
			havePrev: true,
			wantEmit: false,
		},
		{
			name:     "warm-up phantom guard: empty cur band never fires",
			prev:     RoamLink{Band: "5 GHz", Channel: "149"},
			cur:      RoamLink{Band: "", Channel: ""},
			havePrev: true,
			wantEmit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, ok := detectTransition(tc.prev, tc.cur, tc.havePrev, tc.withDrop, tc.afterPause, roamBase)
			if ok != tc.wantEmit {
				t.Fatalf("emit = %v, want %v (tr=%+v)", ok, tc.wantEmit, tr)
			}
			if !tc.wantEmit {
				return
			}
			if !tr.T.Equal(roamBase) {
				t.Errorf("T = %v, want %v", tr.T, roamBase)
			}
			if tr.FromBand != tc.fromBand || tr.ToBand != tc.toBand {
				t.Errorf("band %q->%q, want %q->%q", tr.FromBand, tr.ToBand, tc.fromBand, tc.toBand)
			}
			if tc.fromCh != "" || tc.toCh != "" {
				if tr.FromChannel != tc.fromCh || tr.ToChannel != tc.toCh {
					t.Errorf("ch %q->%q, want %q->%q", tr.FromChannel, tr.ToChannel, tc.fromCh, tc.toCh)
				}
			}
			if tr.BSSIDChanged != tc.bssidChanged {
				t.Errorf("BSSIDChanged = %v, want %v", tr.BSSIDChanged, tc.bssidChanged)
			}
			if tc.rssi != 0 && tr.RSSI != tc.rssi {
				t.Errorf("RSSI = %d, want %d", tr.RSSI, tc.rssi)
			}
			if tr.WithDrop != tc.withDropWant {
				t.Errorf("WithDrop = %v, want %v", tr.WithDrop, tc.withDropWant)
			}
			if tr.AfterPause != tc.afterPauseWant {
				t.Errorf("AfterPause = %v, want %v", tr.AfterPause, tc.afterPauseWant)
			}
		})
	}
}

// TestRoamCadence checks the TODAY window, count and most-steered-to band (with the
// deterministic lexicographic tie-break).
func TestRoamCadence(t *testing.T) {
	now := roamBase
	roams := []Transition{
		{T: now.Add(-2 * time.Hour), ToBand: "2.4 GHz"},
		{T: now.Add(-1 * time.Hour), ToBand: "2.4 GHz"},
		{T: now.Add(-30 * time.Minute), ToBand: "5 GHz"},
		{T: now.AddDate(0, 0, -1), ToBand: "5 GHz"}, // yesterday: excluded
	}
	c := roamCadence(roams, now)
	if c.CountToday != 3 {
		t.Errorf("CountToday = %d, want 3", c.CountToday)
	}
	if c.MostSteeredToBand != "2.4 GHz" {
		t.Errorf("MostSteeredToBand = %q, want 2.4 GHz", c.MostSteeredToBand)
	}

	// Empty => zero, no panic.
	if z := roamCadence(nil, now); z.CountToday != 0 || z.MostSteeredToBand != "" {
		t.Errorf("empty cadence = %+v, want zero", z)
	}

	// Tie between two to-bands => lexicographically smallest.
	tie := roamCadence([]Transition{{T: now, ToBand: "5 GHz"}, {T: now, ToBand: "2.4 GHz"}}, now)
	if tie.MostSteeredToBand != "2.4 GHz" {
		t.Errorf("tie MostSteeredToBand = %q, want 2.4 GHz (lexicographic)", tie.MostSteeredToBand)
	}
}

// TestRoamLineRoundTripAndReject pins the JSONL round-trip and the reject rules
// (zero time; a non-transition no-op line).
func TestRoamLineRoundTripAndReject(t *testing.T) {
	orig := Transition{
		T: roamBase, FromBand: "5 GHz", ToBand: "2.4 GHz",
		FromChannel: "149", ToChannel: "6", RSSI: -62, WithDrop: true, AfterPause: true,
	}
	line, err := marshalRoamLine(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := parseRoamLine(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.T.Equal(orig.T) || got.FromBand != "5 GHz" || got.ToBand != "2.4 GHz" ||
		got.FromChannel != "149" || got.ToChannel != "6" || got.RSSI != -62 ||
		!got.WithDrop || !got.AfterPause {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Zero time rejected.
	if _, err := parseRoamLine([]byte(`{"from_band":"5 GHz","to_band":"2.4 GHz"}`)); err == nil {
		t.Error("expected reject for zero time")
	}
	// A no-op (no band/channel/BSSID change) rejected.
	noop := Transition{T: roamBase, FromBand: "5 GHz", ToBand: "5 GHz", FromChannel: "149", ToChannel: "149"}
	nl, _ := marshalRoamLine(noop)
	if _, err := parseRoamLine(nl); err == nil {
		t.Error("expected reject for a non-transition line")
	}
}

// TestParseJSONLRoamsSkipsGarbage proves a torn/garbage/blank middle line never aborts
// the whole load.
func TestParseJSONLRoamsSkipsGarbage(t *testing.T) {
	good1, _ := marshalRoamLine(Transition{T: roamBase, FromBand: "5 GHz", ToBand: "2.4 GHz"})
	good2, _ := marshalRoamLine(Transition{T: roamBase.Add(time.Hour), FromChannel: "149", ToChannel: "157"})
	blob := string(good1) + "\n\n{ this is not json }\n" + string(good2) + "\n"
	roams := parseJSONLRoams([]byte(blob))
	if len(roams) != 2 {
		t.Fatalf("parsed %d, want 2 (garbage skipped): %+v", len(roams), roams)
	}
}

// TestPruneRoams drops records older than the TTL horizon.
func TestPruneRoams(t *testing.T) {
	now := roamBase
	roams := []Transition{
		{T: now.Add(-100 * 24 * time.Hour), FromBand: "5 GHz", ToBand: "2.4 GHz"}, // old
		{T: now.Add(-1 * time.Hour), FromBand: "2.4 GHz", ToBand: "5 GHz"},        // kept
	}
	kept := pruneRoams(roams, now, defaultOutageTTL)
	if len(kept) != 1 || !kept[0].T.Equal(now.Add(-1*time.Hour)) {
		t.Errorf("pruneRoams kept %+v, want only the recent record", kept)
	}
}
