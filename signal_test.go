package main

import (
	"strings"
	"testing"
	"time"
)

const airportCurrent = `      Wi-Fi:

      Software Versions:
          CoreWLAN: 16.0

      Interfaces:
        en0:
          Card Type: Wi-Fi
          Status: Connected
          Current Network Information:
            <redacted>:
              PHY Mode: 802.11ax
              Channel: 149 (5GHz, 80MHz)
              Security: WPA2/WPA3 Personal
              Signal / Noise: -43 dBm / -92 dBm
              Transmit Rate: 1200
              MCS Index: 11
          Other Local Wi-Fi Networks:
            NeighborNet:
              PHY Mode: 802.11ac
              Channel: 36 (5GHz, 80MHz)
              Signal / Noise: -32 dBm / -90 dBm
`

func TestParseAirportSampleCurrent(t *testing.T) {
	s := parseAirportSample(airportCurrent)
	if !s.OK {
		t.Fatal("expected OK sample")
	}
	if s.RSSI != -43 || s.Noise != -92 || s.SNR != 49 {
		t.Errorf("RSSI/Noise/SNR = %d/%d/%d, want -43/-92/49", s.RSSI, s.Noise, s.SNR)
	}
	if s.RateMbps != 1200 {
		t.Errorf("rate = %v, want 1200", s.RateMbps)
	}
	if s.MCS != 11 {
		t.Errorf("MCS = %d, want 11", s.MCS)
	}
	if s.PHY != "802.11ax" {
		t.Errorf("PHY = %q, want 802.11ax", s.PHY)
	}
	if s.ChannelNum != 149 || s.Band != "5 GHz" || s.Width != "80MHz" {
		t.Errorf("channel = %d/%q/%q, want 149/'5 GHz'/80MHz", s.ChannelNum, s.Band, s.Width)
	}
}

// TestParseAirportSampleMCSPresent pins the Phase-6 parse-presence flag that lets
// the pure bridge tell a genuine "MCS Index: 0" from an absent MCS line. The
// PHY-present-but-no-MCS-line row is the case that makes a value- or PHY-based rule
// impossible (s.MCS==0 is identical to a real 0) and MCSPresent necessary.
func TestParseAirportSampleMCSPresent(t *testing.T) {
	block := func(mcsLine string) string {
		return `      Wi-Fi:
        en0:
          Current Network Information:
            <redacted>:
              PHY Mode: 802.11ax
              Channel: 149 (5GHz, 80MHz)
              Signal / Noise: -43 dBm / -92 dBm
` + mcsLine + `          Other Local Wi-Fi Networks:
`
	}
	tests := []struct {
		name        string
		out         string
		wantMCS     int
		wantPresent bool
	}{
		{"MCS Index: 0 present => MCS 0, present true", block("              MCS Index: 0\n"), 0, true},
		{"MCS Index: 7 present => MCS 7, present true", block("              MCS Index: 7\n"), 7, true},
		{"PHY line but NO MCS line => MCS 0, present FALSE", block(""), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := parseAirportSample(tc.out)
			if !s.OK {
				t.Fatalf("expected OK sample (Signal/Noise present)")
			}
			if s.MCS != tc.wantMCS || s.MCSPresent != tc.wantPresent {
				t.Errorf("MCS=%d MCSPresent=%v, want %d/%v", s.MCS, s.MCSPresent, tc.wantMCS, tc.wantPresent)
			}
		})
	}
}

func TestParseAirportSampleIgnoresOtherNetworks(t *testing.T) {
	s := parseAirportSample(airportCurrent)
	if s.RSSI == -32 {
		t.Fatal("picked up the -32 dBm neighbor from Other Local Wi-Fi Networks")
	}
	if s.RSSI != -43 {
		t.Errorf("RSSI = %d, want the current-network -43", s.RSSI)
	}
}

func TestParseAirportSampleNotAssociated(t *testing.T) {
	out := `      Wi-Fi:
        en0:
          Status: Not Connected
          Other Local Wi-Fi Networks:
            SomeNet:
              Signal / Noise: -50 dBm / -90 dBm
`
	s := parseAirportSample(out)
	if s.OK {
		t.Error("expected OK=false with no Current Network Information block")
	}
	if s.RSSI != 0 {
		t.Errorf("expected zeroed sample, got RSSI %d", s.RSSI)
	}
}

func TestParseAirportSampleRedactedSSID(t *testing.T) {
	// SSID name redacted but metrics present — still parseable.
	s := parseAirportSample(airportCurrent)
	if !s.OK || s.RSSI != -43 {
		t.Errorf("redacted-SSID sample should still yield metrics, got OK=%v RSSI=%d", s.OK, s.RSSI)
	}
}

func TestParseSignalNoise(t *testing.T) {
	rssi, noise, ok := parseSignalNoise("-43 dBm / -92 dBm")
	if !ok || rssi != -43 || noise != -92 {
		t.Errorf("= %d/%d/%v, want -43/-92/true", rssi, noise, ok)
	}
	if _, _, ok := parseSignalNoise("garbage"); ok {
		t.Error("expected ok=false for malformed input")
	}
	if _, _, ok := parseSignalNoise("-43 dBm"); ok {
		t.Error("expected ok=false when noise half absent")
	}
}

func TestParseChannelSpec(t *testing.T) {
	num, band, width := parseChannelSpec("149 (5GHz, 80MHz)")
	if num != 149 || band != "5 GHz" || width != "80MHz" {
		t.Errorf("= %d/%q/%q, want 149/'5 GHz'/80MHz", num, band, width)
	}
	num, band, width = parseChannelSpec("6 (2GHz, 20MHz)")
	if num != 6 || band != "2.4 GHz" || width != "20MHz" {
		t.Errorf("= %d/%q/%q, want 6/'2.4 GHz'/20MHz", num, band, width)
	}
	num, band, width = parseChannelSpec("1 (2GHz, 20MHz)")
	if num != 1 || band != "2.4 GHz" || width != "20MHz" {
		t.Errorf("= %d/%q/%q, want 1/'2.4 GHz'/20MHz", num, band, width)
	}
	num, band, width = parseChannelSpec("149")
	if num != 149 || band != "" || width != "" {
		t.Errorf("bare = %d/%q/%q, want 149/''/''", num, band, width)
	}
}

func TestNormalizeBand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2GHz", "2.4 GHz"},
		{"2 GHz", "2.4 GHz"},
		{"2.4 GHz", "2.4 GHz"},
		{"5GHz", "5 GHz"},
		{"5 GHz", "5 GHz"},
		{"6GHz", "6 GHz"},
		{"6 GHz", "6 GHz"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeBand(c.in); got != c.want {
			t.Errorf("normalizeBand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRate(t *testing.T) {
	for _, in := range []string{"1200", "1200.0", "1200 Mbps"} {
		if got := parseRate(in); got != 1200 {
			t.Errorf("parseRate(%q) = %v, want 1200", in, got)
		}
	}
}

func TestSummarize(t *testing.T) {
	// RSSI values -44,-43,-41 => mean -42.67 => round-half-away -43.
	// jitter RSSI = (|-43 - -44| + |-41 - -43|)/2 = (1+2)/2 = 1.5
	samples := []SignalSample{
		{OK: true, RSSI: -44, SNR: 48, RateMbps: 960, PHY: "802.11ax", Channel: "149/80"},
		{OK: true, RSSI: -43, SNR: 49, RateMbps: 1150, PHY: "802.11ax", Channel: "149/80"},
		{OK: true, RSSI: -41, SNR: 50, RateMbps: 1200, PHY: "802.11ax", Channel: "149/80"},
	}
	st := summarize(samples)
	if st.Polls != 3 || st.Samples != 3 {
		t.Errorf("polls/samples = %d/%d, want 3/3", st.Polls, st.Samples)
	}
	if st.RSSIMin != -44 || st.RSSIMax != -41 || st.RSSIAvg != -43 {
		t.Errorf("RSSI min/avg/max = %d/%d/%d, want -44/-43/-41", st.RSSIMin, st.RSSIAvg, st.RSSIMax)
	}
	if st.RSSIJitter != 1.5 {
		t.Errorf("RSSI jitter = %v, want 1.5", st.RSSIJitter)
	}
	if st.SNRMin != 48 || st.SNRMax != 50 || st.SNRAvg != 49 {
		t.Errorf("SNR min/avg/max = %d/%d/%d, want 48/49/50", st.SNRMin, st.SNRAvg, st.SNRMax)
	}
	if st.RateMin != 960 || st.RateMax != 1200 {
		t.Errorf("rate min/max = %v/%v, want 960/1200", st.RateMin, st.RateMax)
	}
	if st.Changed {
		t.Error("did not expect Changed for constant PHY/channel")
	}
}

func TestSummarizeSkipsInvalid(t *testing.T) {
	samples := []SignalSample{
		{OK: false}, // failed poll — must be ignored
		{OK: true, RSSI: -50, SNR: 40},
		{OK: false, RSSI: 0}, // zero must not become the "strongest"
		{OK: true, RSSI: -48, SNR: 42},
	}
	st := summarize(samples)
	if st.Polls != 4 || st.Samples != 2 {
		t.Errorf("polls/samples = %d/%d, want 4/2", st.Polls, st.Samples)
	}
	if st.RSSIMax != -48 || st.RSSIMin != -50 {
		t.Errorf("RSSI min/max = %d/%d, want -50/-48 (0 must be skipped)", st.RSSIMin, st.RSSIMax)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	st := summarize(nil)
	if st.Samples != 0 || st.RSSIJitter != 0 {
		t.Errorf("empty summarize = %+v, want zeroed", st)
	}
	one := summarize([]SignalSample{{OK: true, RSSI: -50, SNR: 40}})
	if one.Samples != 1 || one.RSSIJitter != 0 {
		t.Errorf("single-sample jitter = %v, want 0", one.RSSIJitter)
	}
}

func TestSummarizeChanged(t *testing.T) {
	samples := []SignalSample{
		{OK: true, RSSI: -50, PHY: "802.11ax", Channel: "149/80"},
		{OK: true, RSSI: -55, PHY: "802.11ac", Channel: "36/80"}, // roamed
	}
	if !summarize(samples).Changed {
		t.Error("expected Changed=true when PHY/channel differ across samples")
	}
}

func TestFormatSignalCSV(t *testing.T) {
	tm := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	samples := []SignalSample{
		{OK: true, T: tm, RSSI: -43, Noise: -92, SNR: 49, RateMbps: 1200, MCS: 11, PHY: "802.11ax", ChannelNum: 149, Band: "5 GHz", Width: "80MHz"},
		{OK: false, T: tm}, // must be skipped
	}
	csv := formatSignalCSV(samples)
	if !strings.Contains(csv, "t,ok,rssi,noise,snr,rate_mbps,mcs,phy,channel_num,band,width") {
		t.Errorf("missing header row: %q", csv)
	}
	if !strings.Contains(csv, "2026-09-18T12:00:00Z") {
		t.Errorf("t not RFC3339: %q", csv)
	}
	rows := strings.Count(strings.TrimSpace(csv), "\n") // header + 1 data row = 1 newline
	if rows != 1 {
		t.Errorf("expected only the valid row written, got %d newlines in %q", rows, csv)
	}
	if !strings.Contains(csv, "802.11ax") || !strings.Contains(csv, "5 GHz") {
		t.Errorf("PHY/band columns missing: %q", csv)
	}
}
