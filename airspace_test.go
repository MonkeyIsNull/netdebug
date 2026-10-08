package main

import (
	"strings"
	"testing"
)

// airportPrimaryFixture is a REAL, VERBATIM `system_profiler SPAirPortDataType`
// capture taken on this machine (BCM4378, 0x4378; 2.4/5 GHz only — Supported
// Channels top out at 165, NO 6 GHz radio). It is the authoritative parse oracle:
// the exact indent ladder (en0 at 8, section heads at 10, neighbor "<redacted>:"
// headers at 12, fields at 14; awdl0 at 8), every neighbor name redacted, and the
// trailing awdl0 "Current Network Information:" (headerless, indent 10) that the
// indent-8 stop must EXCLUDE. The joined net is ch157/80; the neighbor list has
// exactly one AP on 149 (-46) and one on 157 (-46).
const airportPrimaryFixture = `Wi-Fi:

      Interfaces:
        en0:
          Card Type: Wi-Fi  (0x14E4, 0x4378)
          Status: Connected
          Current Network Information:
            <redacted>:
              PHY Mode: 802.11ac
              Channel: 157 (5GHz, 80MHz)
              Country Code: US
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -45 dBm / -85 dBm
              Transmit Rate: 866
              MCS Index: 9
          Other Local Wi-Fi Networks:
            <redacted>:
              PHY Mode: 802.11b/g/n/ac
              Channel: 11 (2GHz, 20MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -81 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11g/n
              Channel: 6 (2GHz, 20MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -32 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11a/n/ac
              Channel: 40 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -87 dBm / -93 dBm
            <redacted>:
              PHY Mode: 802.11a/n/ac
              Channel: 157 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -46 dBm / -93 dBm
            <redacted>:
              PHY Mode: 802.11b/g/n/ax
              Channel: 7 (2GHz, 40MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -93 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11b/g/n
              Channel: 1 (2GHz, 20MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -37 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11b/g/n/ac/ax
              Channel: 6 (2GHz, 40MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -95 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11b/g/n/ac/ax
              Channel: 11 (2GHz, 20MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -86 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11a/n/ac
              Channel: 149 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -46 dBm / -93 dBm
        awdl0:
          MAC Address: 02:00:5e:10:00:70
          Current Network Information:
              Network Type: Infrastructure
`

// airportSecondaryFixture is a CLEARLY-AUTHORED, format-faithful fixture (NOT a
// verbatim capture) that exercises what the real capture cannot: a crowded 149/157
// 5 GHz overlap, a HEADERLESS field-block RUN (two blocks under one "<redacted>:"
// header, delimited by a re-appearing "PHY Mode:"), a block missing "Network
// Type:", a neighbor with a Channel but NO Signal/Noise, an UNPARSEABLE channel
// (must be skipped, never ch 0), a "Cafe: Guest:" header (must NOT split on the
// first ':'), and a SYNTHESIZED 6 GHz AP (this BCM4378 has no 6E radio — the 6 GHz
// span math + palette branch are exercised ONLY here). The trailing awdl0 block
// carries its OWN "Other Local Wi-Fi Networks:" with a ch64 AP that the indent-8
// stop + first-match scoping must EXCLUDE.
const airportSecondaryFixture = `Wi-Fi:
      Interfaces:
        en0:
          Current Network Information:
            <redacted>:
              Channel: 157 (5GHz, 80MHz)
              Signal / Noise: -45 dBm / -85 dBm
          Other Local Wi-Fi Networks:
            Cafe: Guest:
              PHY Mode: 802.11ac
              Channel: 149 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -46 dBm / -93 dBm
            <redacted>:
              PHY Mode: 802.11ac
              Channel: 157 (5GHz, 80MHz)
              Security: WPA2 Personal
              Signal / Noise: -46 dBm / -93 dBm
            <redacted>:
              PHY Mode: 802.11ac
              Channel: 149 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -60 dBm / -93 dBm
              PHY Mode: 802.11ac
              Channel: 157 (5GHz, 80MHz)
              Network Type: Infrastructure
              Security: WPA2 Personal
              Signal / Noise: -62 dBm / -93 dBm
            <redacted>:
              PHY Mode: 802.11ax
              Channel: 36 (5GHz, 80MHz)
              Network Type: Infrastructure
            <redacted>:
              PHY Mode: 802.11ax
              Channel: 37 (6GHz, 80MHz)
              Signal / Noise: -55 dBm / -95 dBm
            <redacted>:
              Channel: 6 (2GHz, 20MHz)
              Signal / Noise: -85 dBm / -100 dBm
            <redacted>:
              Channel: 11 (2GHz, 20MHz)
              Signal / Noise: -88 dBm / -100 dBm
            <redacted>:
              Channel: 1 (2GHz, 20MHz)
              Signal / Noise: -90 dBm / -100 dBm
            <redacted>:
              PHY Mode: 802.11ac
              Channel: auto
              Signal / Noise: -70 dBm / -93 dBm
        awdl0:
          Other Local Wi-Fi Networks:
            <redacted>:
              Channel: 64 (5GHz, 80MHz)
              Signal / Noise: -40 dBm / -90 dBm
`

func findNeighbors(ns []Neighbor, band string, num int) []Neighbor {
	var out []Neighbor
	for _, n := range ns {
		if n.Band == band && n.ChannelNum == num {
			out = append(out, n)
		}
	}
	return out
}

func TestParseNeighborNetworksPrimary(t *testing.T) {
	ns := parseNeighborNetworks(airportPrimaryFixture)
	if len(ns) != 9 {
		t.Fatalf("primary neighbor count = %d, want 9; got %+v", len(ns), ns)
	}
	// Every real neighbor name is macOS-redacted => Name "".
	for i, n := range ns {
		if n.Name != "" {
			t.Errorf("neighbor[%d] Name = %q, want \"\" (redacted)", i, n.Name)
		}
		if n.ChannelNum == 0 {
			t.Errorf("neighbor[%d] has ChannelNum 0 (should have been skipped)", i)
		}
	}
	// Exactly one 149 and one 157, both -46 / 5 GHz / 80MHz. The 157 is -46 (the
	// NEIGHBOR), NOT -45 (the joined "Current Network Information:" net) — proving the
	// "Other Local" header is matched and the joined block is never scooped.
	if got := findNeighbors(ns, "5 GHz", 149); len(got) != 1 || got[0].RSSI != -46 || got[0].Width != "80MHz" || got[0].Channel != "149/80" {
		t.Errorf("ch149 neighbors = %+v, want one {-46, 80MHz, 149/80}", got)
	}
	if got := findNeighbors(ns, "5 GHz", 157); len(got) != 1 || got[0].RSSI != -46 {
		t.Errorf("ch157 neighbors = %+v, want exactly one at -46 (joined -45 must NOT appear)", got)
	}
	// The awdl0 interface (indent 8) terminates en0's block: nothing from awdl0's
	// trailing "Current Network Information:" is scooped (it has no channel anyway,
	// but the indent-8 stop is the guarantee). Count==9 already encodes this.
}

func TestParseNeighborNetworksSecondary(t *testing.T) {
	ns := parseNeighborNetworks(airportSecondaryFixture)
	// 9 kept: Cafe 149, 157, headerless 149 + 157, 36 (no S/N), 6GHz 37, 2.4 {6,11,1}.
	// The "Channel: auto" record is SKIPPED; the awdl0 ch64 is EXCLUDED by scope.
	if len(ns) != 9 {
		t.Fatalf("secondary neighbor count = %d, want 9; got %+v", len(ns), ns)
	}
	if got := findNeighbors(ns, "5 GHz", 64); len(got) != 0 {
		t.Errorf("ch64 (awdl0's own Other Local block) must be EXCLUDED by the indent-8 stop + first-match: %+v", got)
	}
	// "Cafe: Guest:" => Name "Cafe: Guest" (NOT split on the first ':').
	cafe := findNeighbors(ns, "5 GHz", 149)
	foundCafe := false
	for _, n := range cafe {
		if n.Name == "Cafe: Guest" {
			foundCafe = true
		}
	}
	if !foundCafe {
		t.Errorf("expected a 149 neighbor named %q (no first-':' split); got %+v", "Cafe: Guest", cafe)
	}
	// The HEADERLESS run under one header yields BOTH blocks (149 -60 AND 157 -62).
	if got := findNeighbors(ns, "5 GHz", 149); len(got) != 2 {
		t.Errorf("want 2 neighbors on 149 (Cafe + headerless-run); got %d: %+v", len(got), got)
	}
	if got := findNeighbors(ns, "5 GHz", 157); len(got) != 2 {
		t.Errorf("want 2 neighbors on 157 (missing-NetworkType + headerless-run); got %d: %+v", len(got), got)
	}
	// A block MISSING "Network Type:" still parses (the 157 -46 block).
	// A Channel WITHOUT Signal/Noise is KEPT with RSSI 0 (the 36 block).
	if got := findNeighbors(ns, "5 GHz", 36); len(got) != 1 || got[0].RSSI != 0 {
		t.Errorf("ch36 (no Signal/Noise) must be kept with RSSI 0; got %+v", got)
	}
	// The SYNTHESIZED 6 GHz AP parses with band "6 GHz".
	if got := findNeighbors(ns, "6 GHz", 37); len(got) != 1 || got[0].Width != "80MHz" || got[0].RSSI != -55 {
		t.Errorf("synthesized 6 GHz ch37 parse wrong: %+v", got)
	}
}

func TestParseNeighborNetworksEmptyOutcomes(t *testing.T) {
	// Header ABSENT => nil (distinct from present-but-empty).
	if ns := parseNeighborNetworks("Wi-Fi:\n      Interfaces:\n        en0:\n          Status: Connected\n"); ns != nil {
		t.Errorf("header absent should yield nil, got %+v", ns)
	}
	// Header PRESENT but zero parseable neighbors => empty len-0 slice (non-nil).
	in := "Wi-Fi:\n        en0:\n          Other Local Wi-Fi Networks:\n        awdl0:\n          Status: foo\n"
	ns := parseNeighborNetworks(in)
	if ns == nil || len(ns) != 0 {
		t.Errorf("header present + zero neighbors should yield empty non-nil slice, got %#v", ns)
	}
	// A literal "<redacted>:" header with a channel => Name "".
	red := "        en0:\n          Other Local Wi-Fi Networks:\n            <redacted>:\n              Channel: 149 (5GHz, 80MHz)\n              Signal / Noise: -50 dBm / -90 dBm\n"
	got := parseNeighborNetworks(red)
	if len(got) != 1 || got[0].Name != "" {
		t.Errorf("'<redacted>:' header must map to Name \"\"; got %+v", got)
	}
}

func TestChannelLabel(t *testing.T) {
	cases := []struct {
		num   int
		width string
		want  string
	}{
		{149, "80MHz", "149/80"},
		{6, "20MHz", "6/20"},
		{36, "", "36"},
		{0, "", ""},
	}
	for _, c := range cases {
		if got := channelLabel(c.num, c.width); got != c.want {
			t.Errorf("channelLabel(%d,%q) = %q, want %q", c.num, c.width, got, c.want)
		}
	}
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChannelSpan(t *testing.T) {
	cases := []struct {
		name  string
		num   int
		band  string
		width string
		want  []int
	}{
		{"2.4 ch6/20", 6, "2.4 GHz", "20MHz", []int{4, 5, 6, 7, 8}},
		{"2.4 ch1/20 clamps low", 1, "2.4 GHz", "20MHz", []int{1, 2, 3}},
		{"2.4 ch11/40", 11, "2.4 GHz", "40MHz", []int{7, 8, 9, 10, 11, 12, 13}},
		{"5 ch157/80", 157, "5 GHz", "80MHz", []int{149, 153, 157, 161}},
		{"5 ch149/80 same block", 149, "5 GHz", "80MHz", []int{149, 153, 157, 161}},
		{"5 ch36/160", 36, "5 GHz", "160MHz", []int{36, 40, 44, 48, 52, 56, 60, 64}},
		{"5 ch149/40 pair", 149, "5 GHz", "40MHz", []int{149, 153}},
		{"5 ch157/40 pair", 157, "5 GHz", "40MHz", []int{157, 161}},
		{"5 ch165/80 no block => single", 165, "5 GHz", "80MHz", []int{165}},
		{"5 ch36/20 => single", 36, "5 GHz", "20MHz", []int{36}},
		{"6 ch37/80 arithmetic (SYNTHESIZED)", 37, "6 GHz", "80MHz", []int{33, 37, 41, 45}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := channelSpan(c.num, c.band, c.width); !eqInts(got, c.want) {
				t.Errorf("channelSpan(%d,%q,%q) = %v, want %v", c.num, c.band, c.width, got, c.want)
			}
		})
	}
}

func loadFor(loads []ChannelLoad, band string, num int) (ChannelLoad, bool) {
	for _, cl := range loads {
		if cl.Band == band && cl.ChannelNum == num {
			return cl, true
		}
	}
	return ChannelLoad{}, false
}

func TestChannelOccupancy(t *testing.T) {
	ns := parseNeighborNetworks(airportSecondaryFixture)
	loads := channelOccupancy(ns, "157/80")

	// Sorted band (2.4 -> 5 -> 6) then NUMERIC channel: the first 2.4 entry must be
	// ch1, and ch6 must precede ch11 (a string sort would invert them).
	var order24 []int
	for _, cl := range loads {
		if cl.Band == "2.4 GHz" {
			order24 = append(order24, cl.ChannelNum)
		}
	}
	if !eqInts(order24, []int{1, 6, 11}) {
		t.Errorf("2.4 GHz channels not numerically sorted: %v", order24)
	}

	// Discrete tally.
	if cl, ok := loadFor(loads, "5 GHz", 149); !ok || cl.Count != 2 || cl.StrongCount != 2 || cl.MaxRSSI != -46 {
		t.Errorf("ch149 discrete = %+v, want Count2 Strong2 MaxRSSI-46", cl)
	}
	if cl, ok := loadFor(loads, "5 GHz", 157); !ok || cl.Count != 2 || !cl.Mine || cl.MaxRSSI != -46 {
		t.Errorf("ch157 discrete = %+v, want Count2 Mine MaxRSSI-46", cl)
	}
	// 2.4 GHz APs are all weaker than -80 => NOT strong (interference ENERGY, not
	// population — the user's 2.4 crowd is harmless).
	for _, num := range []int{1, 6, 11} {
		if cl, _ := loadFor(loads, "2.4 GHz", num); cl.StrongCount != 0 {
			t.Errorf("2.4 ch%d StrongCount = %d, want 0 (all < -80 dBm)", num, cl.StrongCount)
		}
	}
	// ch36 neighbor has NO Signal/Noise => RSSI 0 => StrongCount 0, MaxRSSI 0 (never a
	// bogus -inf from maxing an empty slice).
	if cl, ok := loadFor(loads, "5 GHz", 36); !ok || cl.StrongCount != 0 || cl.MaxRSSI != 0 {
		t.Errorf("ch36 = %+v, want StrongCount0 MaxRSSI0", cl)
	}

	// Width-aware overlap: the 149-161 pile-up (two 149/80 + two 157/80 neighbors +
	// my 157/80) renders as OverlapCount 5 on EACH of 149/153/157/161 — where the
	// discrete Count is only ~2. 153/161 exist as rows (part of MY block) even though
	// no neighbor is centered there.
	for _, num := range []int{149, 153, 157, 161} {
		cl, ok := loadFor(loads, "5 GHz", num)
		if !ok {
			t.Fatalf("ch%d missing from loads (my block must seed all four subchannels)", num)
		}
		if cl.OverlapCount != 5 || cl.OverlapStrong != 5 {
			t.Errorf("ch%d overlap = (%d,%d), want (5,5) — the width-aware pile-up incl. my AP", num, cl.OverlapCount, cl.OverlapStrong)
		}
	}
	if cl, _ := loadFor(loads, "5 GHz", 153); cl.Mine || cl.Count != 0 {
		t.Errorf("ch153 should be a non-Mine, Count-0 row (part of my block, not my center): %+v", cl)
	}
}

func TestChannelOccupancyMyChannelAlone(t *testing.T) {
	// My channel on a band NO neighbor uses => present, Count 0, Mine true,
	// OverlapCount >= 1 (my own AP).
	loads := channelOccupancy(nil, "157/80")
	cl, ok := loadFor(loads, "5 GHz", 157)
	if !ok || !cl.Mine || cl.Count != 0 || cl.OverlapCount < 1 {
		t.Errorf("solo my-channel = %+v, want Mine Count0 OverlapCount>=1", cl)
	}
	// Empty neighbors + empty myChannel => no rows.
	if got := channelOccupancy(nil, ""); len(got) != 0 {
		t.Errorf("empty neighbors + empty myChannel => no rows, got %+v", got)
	}
}

func TestClearestBlock(t *testing.T) {
	ns := parseNeighborNetworks(airportSecondaryFixture)
	loads := channelOccupancy(ns, "157/80")
	rec := clearestBlock(loads)
	// The 149-161 block is piled up (OverlapStrong 20); the clearest non-DFS block is
	// 36/80 (only ch36, OverlapStrong 0). It must NOT recommend the congested block
	// nor a DFS block over an equally-clear non-DFS one.
	if rec.Label != "36/80" {
		t.Errorf("clearestBlock = %q, want 36/80 (least-loaded non-DFS)", rec.Label)
	}
	if rec.DFS {
		t.Errorf("36/80 must be non-DFS")
	}
	if rec.Band != "5 GHz" || !eqInts(rec.Channels, []int{36, 40, 44, 48}) {
		t.Errorf("recommendation block wrong: %+v", rec)
	}
	// DFS flag correctness across the range.
	for _, tc := range []struct {
		num int
		dfs bool
	}{{36, false}, {48, false}, {52, true}, {100, true}, {144, true}, {149, false}, {161, false}} {
		if got := is5GHzDFS(tc.num); got != tc.dfs {
			t.Errorf("is5GHzDFS(%d) = %v, want %v", tc.num, got, tc.dfs)
		}
	}
	// No 5 GHz data at all => empty BlockRec (no bogus "move to 36/80").
	only24 := []ChannelLoad{{ChannelNum: 6, Band: "2.4 GHz", Count: 1}}
	if empty := clearestBlock(only24); len(empty.Channels) != 0 {
		t.Errorf("clearestBlock with no 5 GHz data should be empty, got %+v", empty)
	}
}

func TestAirspaceJSONNormalization(t *testing.T) {
	// nil slices => [] arrays; empty recommendation => null.
	b, err := airspaceJSON(nil, nil, "", BlockRec{})
	if err != nil {
		t.Fatalf("airspaceJSON(nil...): %v", err)
	}
	s := string(b)
	for _, want := range []string{`"channels":[]`, `"neighbors":[]`, `"my_channel":""`, `"recommendation":null`} {
		if !strings.Contains(s, want) {
			t.Errorf("airspaceJSON empty missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"channels":null`) || strings.Contains(s, `"neighbors":null`) {
		t.Errorf("airspaceJSON must never emit a null array: %s", s)
	}

	// Populated recommendation => an object, redaction sentinel never leaks.
	ns := parseNeighborNetworks(airportSecondaryFixture)
	loads := channelOccupancy(ns, "157/80")
	rec := clearestBlock(loads)
	b, err = airspaceJSON(loads, ns, "157/80", rec)
	if err != nil {
		t.Fatalf("airspaceJSON(populated): %v", err)
	}
	s = string(b)
	if strings.Contains(s, "<redacted>") {
		t.Errorf("airspaceJSON leaked the redaction sentinel: %s", s)
	}
	if !strings.Contains(s, `"recommendation":{`) || !strings.Contains(s, `"label":"36/80"`) {
		t.Errorf("airspaceJSON missing populated recommendation: %s", s)
	}
}

func TestFormatAirspace(t *testing.T) {
	ns := parseNeighborNetworks(airportSecondaryFixture)
	loads := channelOccupancy(ns, "157/80")
	rec := clearestBlock(loads)
	out := formatAirspace(loads, ns, "157/80", rec)
	for _, want := range []string{"AIRSPACE", "Your channel: 157/80", "[YOU]", "Clearest 80MHz block: 36/80", "no active scan performed"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatAirspace output missing %q:\n%s", want, out)
		}
	}
	// Redacted names render as "(hidden)", never the raw sentinel.
	if strings.Contains(out, "<redacted>") {
		t.Errorf("formatAirspace leaked the redaction sentinel:\n%s", out)
	}
	if !strings.Contains(out, "(hidden)") {
		t.Errorf("formatAirspace should render redacted names as (hidden):\n%s", out)
	}

	// Empty neighbors => an HONEST "no data" note, never a reassuring "airspace clear".
	empty := formatAirspace(nil, nil, "", BlockRec{})
	if !strings.Contains(empty, "No ambient scan data") {
		t.Errorf("empty formatAirspace should say no data:\n%s", empty)
	}
}
