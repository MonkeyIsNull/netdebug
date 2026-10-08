package main

// Phase 4 tests — the pure correlation core. Table-driven, no IO: linkSnapFromAirport
// (the sole link mapper), detectEvents (drop/roam/band_change with the precedence
// ladder, redaction-flip and parse-miss guards), bandColorKey (the pinned key set
// shared with the JS palette), clampLinkInterval, modeBand and dominantBand.

import (
	"reflect"
	"strconv"
	"testing"
	"time"
)

// intp returns a pointer to i, for *int want literals (LinkSnap.MCS). A pointer
// field forces reflect.DeepEqual comparison below: `got != want` would compare
// pointer ADDRESSES, so two equal-valued &0 would never compare equal.
func intp(i int) *int { return &i }

func TestLinkSnapFromAirport(t *testing.T) {
	tests := []struct {
		name   string
		s      SignalSample
		ssid   string
		active bool
		want   LinkSnap
	}{
		{
			name:   "associated full modern detail (noise/snr/txrate/mcs/phy all mapped)",
			s:      SignalSample{OK: true, Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, RateMbps: 1200, MCS: 11, MCSPresent: true, PHY: "802.11ax"},
			ssid:   "Home",
			active: true,
			want:   LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, SNR: 49, TxRate: 1200, MCS: intp(11), PHY: "802.11ax", OK: true},
		},
		{
			name:   "associated, PHY present + MCS LINE 0 => pointer preserves a genuine MCS 0",
			s:      SignalSample{OK: true, Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, RateMbps: 600, MCS: 0, MCSPresent: true, PHY: "802.11ax"},
			ssid:   "Home",
			active: true,
			want:   LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, SNR: 49, TxRate: 600, MCS: intp(0), PHY: "802.11ax", OK: true},
		},
		{
			name:   "associated, PHY present + NO MCS line => MCS nil (a value/PHY rule would wrongly emit &0)",
			s:      SignalSample{OK: true, Band: "5 GHz", Channel: "36/80", RSSI: -50, Noise: -90, RateMbps: 300, MCS: 0, MCSPresent: false, PHY: "802.11a"},
			ssid:   "Home",
			active: true,
			want:   LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "36/80", RSSI: -50, Noise: -90, SNR: 40, TxRate: 300, MCS: nil, PHY: "802.11a", OK: true},
		},
		{
			name:   "associated, rssi set but noise 0 => SNR 0 (wire omits snr; page shows —)",
			s:      SignalSample{OK: true, Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: 0, PHY: "802.11ax"},
			ssid:   "Home",
			active: true,
			want:   LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: 0, SNR: 0, PHY: "802.11ax", OK: true},
		},
		{
			name:   "associated but SSID redacted => SSID normalized to empty, detail intact, OK true",
			s:      SignalSample{OK: true, Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92},
			ssid:   "<redacted>",
			active: true,
			want:   LinkSnap{SSID: "", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, SNR: 49, OK: true},
		},
		{
			name:   "associated but SSID empty => SSID empty, OK true",
			s:      SignalSample{OK: true, Band: "6 GHz", Channel: "37/160", RSSI: -55, Noise: -95},
			ssid:   "",
			active: true,
			want:   LinkSnap{SSID: "", Band: "6 GHz", Channel: "37/160", RSSI: -55, Noise: -95, SNR: 40, OK: true},
		},
		{
			name:   "associated parse MISS (s.OK=false) but active => OK true, ALL detail empty/nil, no bogus rate/MCS",
			s:      SignalSample{OK: false}, // empty system_profiler block
			ssid:   "Home",
			active: true,
			want:   LinkSnap{SSID: "Home", OK: true},
		},
		{
			name: "not associated => OK false, empty everything (exec error lands here too)",
			// A not-associated interface has no "Current Network Information" block,
			// so parseAirportSample returns OK=false and the SSID read is empty.
			s:      SignalSample{OK: false},
			ssid:   "",
			active: false,
			want:   LinkSnap{OK: false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := linkSnapFromAirport(tc.s, tc.ssid, tc.active)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("linkSnapFromAirport(%+v, %q, %v) = %+v (MCS=%v), want %+v (MCS=%v)",
					tc.s, tc.ssid, tc.active, got, fmtMCS(got.MCS), tc.want, fmtMCS(tc.want.MCS))
			}
		})
	}
}

// fmtMCS renders a *int for test failure messages (reflect.DeepEqual compares the
// pointees, but %+v on a struct prints the pointer address).
func fmtMCS(p *int) string {
	if p == nil {
		return "nil"
	}
	return "&" + strconv.Itoa(*p)
}

func TestSNRFrom(t *testing.T) {
	tests := []struct {
		name        string
		rssi, noise int
		want        int
	}{
		{"typical strong", -43, -92, 49},
		{"typical moderate", -70, -90, 20},
		{"missing noise (0) => unknown 0 regardless of rssi", -43, 0, 0},
		{"missing rssi (0) => 0 even if noise set", 0, -92, 0},
		{"both 0 => 0", 0, 0, 0},
		{"NO negative clamp: rssi<noise is a real bad reading", -92, -43, -49},
		{"ACCEPTED EDGE: equal rssi/noise is a real 0 dB but means unknown => 0", -70, -70, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := snrFrom(tc.rssi, tc.noise); got != tc.want {
				t.Errorf("snrFrom(%d, %d) = %d, want %d", tc.rssi, tc.noise, got, tc.want)
			}
		})
	}
}

func TestSSIDDisplay(t *testing.T) {
	tests := []struct {
		name  string
		label string
		want  string
	}{
		{"empty => sentinel", "", "HIDDEN (macOS-redacted)"},
		{"all whitespace => sentinel", "   ", "HIDDEN (macOS-redacted)"},
		{"set => verbatim", "Home-5G", "Home-5G"},
		{"surrounding whitespace PRESERVED verbatim (TrimSpace is presence-only)", " Home ", " Home "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ssidDisplay(tc.label); got != tc.want {
				t.Errorf("ssidDisplay(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

// mkLinkSample builds a Sample at index-time t0+i with a stamped LinkSnap.
func mkLinkSample(base time.Time, i int, snap *LinkSnap) Sample {
	return Sample{T: base.Add(time.Duration(i) * time.Second), Link: snap}
}

func TestDetectEvents(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	ok5 := func() *LinkSnap {
		return &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, OK: true}
	}
	down := func() *LinkSnap { return &LinkSnap{OK: false} }

	type want struct {
		kind, from, to, detail string
	}
	tests := []struct {
		name    string
		samples []Sample
		want    []want
	}{
		{name: "empty => none", samples: nil, want: nil},
		{name: "single => none", samples: []Sample{mkLinkSample(base, 0, ok5())}, want: nil},
		{
			name:    "leading down (nil link) then up => no drop, no event",
			samples: []Sample{mkLinkSample(base, 0, nil), mkLinkSample(base, 1, ok5())},
			want:    nil,
		},
		{
			name:    "steady associated => none",
			samples: []Sample{mkLinkSample(base, 0, ok5()), mkLinkSample(base, 1, ok5()), mkLinkSample(base, 2, ok5())},
			want:    nil,
		},
		{
			name:    "drop: OK true->false => one drop, From=band To empty",
			samples: []Sample{mkLinkSample(base, 0, ok5()), mkLinkSample(base, 1, down())},
			want:    []want{{kind: "drop", from: "5 GHz", to: ""}},
		},
		{
			name: "drop then reconnect => exactly one drop, zero roam/band_change",
			samples: []Sample{
				mkLinkSample(base, 0, ok5()),
				mkLinkSample(base, 1, down()),
				mkLinkSample(base, 2, &LinkSnap{SSID: "Home", Band: "2.4 GHz", Channel: "6", RSSI: -60, OK: true}),
			},
			want: []want{{kind: "drop", from: "5 GHz", to: ""}},
		},
		{
			name: "roam by SSID (band+channel steady)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Guest", Band: "5 GHz", Channel: "149/80", OK: true}),
			},
			want: []want{{kind: "roam", from: "Home", to: "Guest", detail: "ssid"}},
		},
		{
			name: "roam by channel (same band+SSID, both channels non-empty)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "36/80", OK: true}),
			},
			want: []want{{kind: "roam", from: "149/80", to: "36/80", detail: "channel"}},
		},
		{
			name: "channel parse-miss 149/80 -> '' -> 149/80 => ZERO roams (non-empty guard)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "", OK: true}),
				mkLinkSample(base, 2, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
			},
			want: nil,
		},
		{
			name: "REDACTION FLIP: real/''/''/real SSID, band+channel steady => ZERO roams",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 2, &LinkSnap{SSID: "", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 3, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
			},
			want: nil,
		},
		{
			name: "band_change 5->2.4 => exactly one band_change, NOT a roam",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Home", Band: "2.4 GHz", Channel: "6", OK: true}),
			},
			want: []want{{kind: "band_change", from: "5 GHz", to: "2.4 GHz"}},
		},
		{
			name: "simultaneous band+SSID+channel change => one band_change, zero roam (precedence)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Other", Band: "6 GHz", Channel: "37/160", OK: true}),
			},
			want: []want{{kind: "band_change", from: "5 GHz", to: "6 GHz"}},
		},
		{
			name: "RSSI-only change => zero events (RSSI never drives)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -70, OK: true}),
			},
			want: nil,
		},
		{
			// Phase 6 guard: the new display-only fields (Noise/SNR/TxRate/MCS/PHY)
			// must NOT drive detectEvents. A pair identical in Band/SSID/Channel but
			// differing in every new field emits ZERO events — a future refactor that
			// reads a new field in the ladder fails here.
			name: "new-field-only change (noise/snr/txrate/mcs/phy) => zero events",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -92, SNR: 49, TxRate: 1200, MCS: intp(11), PHY: "802.11ax", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{SSID: "Home", Band: "5 GHz", Channel: "149/80", RSSI: -43, Noise: -80, SNR: 37, TxRate: 600, MCS: intp(4), PHY: "802.11ac", OK: true}),
			},
			want: nil,
		},
		{
			name: "flapping A->B->A band => one event per real transition (two events)",
			samples: []Sample{
				mkLinkSample(base, 0, &LinkSnap{Band: "5 GHz", Channel: "149/80", OK: true}),
				mkLinkSample(base, 1, &LinkSnap{Band: "2.4 GHz", Channel: "6", OK: true}),
				mkLinkSample(base, 2, &LinkSnap{Band: "5 GHz", Channel: "149/80", OK: true}),
			},
			want: []want{
				{kind: "band_change", from: "5 GHz", to: "2.4 GHz"},
				{kind: "band_change", from: "2.4 GHz", to: "5 GHz"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectEvents(tc.samples)
			if len(got) != len(tc.want) {
				t.Fatalf("detectEvents got %d events %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.Kind != w.kind || g.From != w.from || g.To != w.to || g.Detail != w.detail {
					t.Errorf("event[%d] = {kind:%q from:%q to:%q detail:%q}, want {kind:%q from:%q to:%q detail:%q}",
						i, g.Kind, g.From, g.To, g.Detail, w.kind, w.from, w.to, w.detail)
				}
			}
		})
	}

	// Event.T is the T of the sample where the NEW snapshot first appears.
	t.Run("Event.T anchors to the transition sample", func(t *testing.T) {
		samples := []Sample{mkLinkSample(base, 0, ok5()), mkLinkSample(base, 1, down())}
		got := detectEvents(samples)
		if len(got) != 1 || !got[0].T.Equal(samples[1].T) {
			t.Fatalf("drop T = %v, want %v", got[0].T, samples[1].T)
		}
	})
}

func TestBandColorKey(t *testing.T) {
	tests := []struct {
		band string
		want string
	}{
		{"2.4 GHz", "b24"},
		{"5 GHz", "b5"},
		{"6 GHz", "b6"},
		{"", "unknown"},
		{"x", "unknown"},
		{"7 GHz", "unknown"},
	}
	for _, tc := range tests {
		if got := bandColorKey(tc.band); got != tc.want {
			t.Errorf("bandColorKey(%q) = %q, want %q", tc.band, got, tc.want)
		}
	}
}

func TestClampLinkInterval(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 5 * time.Second},
		{-3 * time.Second, 5 * time.Second},
		{500 * time.Millisecond, time.Second}, // floor
		{time.Second, time.Second},
		{3 * time.Second, 3 * time.Second},
		{10 * time.Second, 10 * time.Second},
	}
	for _, tc := range tests {
		if got := clampLinkInterval(tc.in); got != tc.want {
			t.Errorf("clampLinkInterval(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestModeBandAndDominantBand(t *testing.T) {
	t.Run("mostly 5 GHz + one 2.4 => 5 GHz", func(t *testing.T) {
		if got := modeBand(map[string]int{"5 GHz": 10, "2.4 GHz": 1}); got != "5 GHz" {
			t.Errorf("got %q, want 5 GHz", got)
		}
	})
	t.Run("all empty => empty", func(t *testing.T) {
		if got := modeBand(map[string]int{"": 7}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("mostly empty + one 5 GHz => 5 GHz (real beats unknown)", func(t *testing.T) {
		if got := modeBand(map[string]int{"": 59, "5 GHz": 1}); got != "5 GHz" {
			t.Errorf("got %q, want 5 GHz", got)
		}
	})
	t.Run("tie 5 vs 2.4 => 5 GHz (priority)", func(t *testing.T) {
		if got := modeBand(map[string]int{"5 GHz": 3, "2.4 GHz": 3}); got != "5 GHz" {
			t.Errorf("got %q, want 5 GHz", got)
		}
	})
	t.Run("tie 6 vs 5 => 6 GHz (priority)", func(t *testing.T) {
		if got := modeBand(map[string]int{"6 GHz": 4, "5 GHz": 4}); got != "6 GHz" {
			t.Errorf("got %q, want 6 GHz", got)
		}
	})
	t.Run("empty map => empty", func(t *testing.T) {
		if got := modeBand(map[string]int{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("dominantBand nil-safe over nil Link and OK=false links", func(t *testing.T) {
		samples := []Sample{
			{}, // nil Link
			{Link: &LinkSnap{OK: false}},
			{Link: &LinkSnap{Band: "5 GHz", OK: true}},
			{Link: &LinkSnap{Band: "5 GHz", OK: true}},
			{Link: &LinkSnap{Band: "2.4 GHz", OK: true}},
		}
		if got := dominantBand(samples); got != "5 GHz" {
			t.Errorf("dominantBand = %q, want 5 GHz", got)
		}
	})
	t.Run("dominantBand all nil => empty", func(t *testing.T) {
		if got := dominantBand([]Sample{{}, {}}); got != "" {
			t.Errorf("dominantBand = %q, want empty", got)
		}
	})
}
