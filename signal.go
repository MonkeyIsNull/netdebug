package main

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SignalSample is one poll of the joined interface's radio metrics.
type SignalSample struct {
	T          time.Time `json:"t"`
	OK         bool      `json:"ok"` // parse succeeded
	RSSI       int       `json:"rssi"`
	Noise      int       `json:"noise"`
	SNR        int       `json:"snr"`
	RateMbps   float64   `json:"rate_mbps"`
	MCS        int       `json:"mcs"`
	MCSPresent bool      `json:"-"` // an "MCS Index:" line was parsed (so MCS 0 is a real reading, not absent)
	PHY        string    `json:"phy"`
	ChannelNum int       `json:"channel_num"`
	Band       string    `json:"band"`    // normalized "5 GHz"
	Width      string    `json:"width"`   // "80MHz"
	Channel    string    `json:"channel"` // normalized "149/80"
}

// SignalStats is the aggregate over a sampling run.
type SignalStats struct {
	Polls      int            `json:"polls"`   // total polls attempted
	Samples    int            `json:"samples"` // valid samples counted
	RSSIMin    int            `json:"rssi_min"`
	RSSIAvg    int            `json:"rssi_avg"`
	RSSIMax    int            `json:"rssi_max"`
	RSSIJitter float64        `json:"rssi_jitter"`
	SNRMin     int            `json:"snr_min"`
	SNRAvg     int            `json:"snr_avg"`
	SNRMax     int            `json:"snr_max"`
	SNRJitter  float64        `json:"snr_jitter"`
	RateMin    float64        `json:"rate_min"`
	RateAvg    float64        `json:"rate_avg"`
	RateMax    float64        `json:"rate_max"`
	RateJitter float64        `json:"rate_jitter"`
	PHY        string         `json:"phy"`     // from the LAST valid sample
	Channel    string         `json:"channel"` // from the LAST valid sample
	Changed    bool           `json:"phy_channel_changed,omitempty"`
	Raw        []SignalSample `json:"raw,omitempty"`
}

// parseAirportSample extracts the joined interface's radio metrics from
// `system_profiler SPAirPortDataType`. It scopes to the single
// "Current Network Information:" block and STOPS at the next section header
// (e.g. "Other Local Wi-Fi Networks:") or a dedent, so a stronger neighbor AP
// inside "Other Local Wi-Fi Networks:" is never mistaken for the joined
// network. Returns OK=false (zeroed) when that block is absent or has no
// "Signal / Noise" line (not associated / layout change).
func parseAirportSample(out string) SignalSample {
	lines := strings.Split(out, "\n")
	start := -1
	var headerIndent int
	for i, ln := range lines {
		if strings.Contains(ln, "Current Network Information:") {
			start = i
			headerIndent = indentOf(ln)
			break
		}
	}
	if start < 0 {
		return SignalSample{}
	}
	// Collect the block: lines after the header, more-indented than the
	// header, stopping at the next line at header indent or shallower (the
	// next section header, e.g. "Other Local Wi-Fi Networks:").
	var block []string
	for j := start + 1; j < len(lines); j++ {
		ln := lines[j]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if indentOf(ln) <= headerIndent {
			break
		}
		block = append(block, ln)
	}

	var s SignalSample
	sawSignal := false
	for _, ln := range block {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "Signal / Noise:"):
			if rssi, noise, ok := parseSignalNoise(strings.TrimSpace(strings.TrimPrefix(t, "Signal / Noise:"))); ok {
				s.RSSI, s.Noise, s.SNR = rssi, noise, rssi-noise
				sawSignal = true
			}
		case strings.HasPrefix(t, "Transmit Rate:"):
			s.RateMbps = parseRate(strings.TrimSpace(strings.TrimPrefix(t, "Transmit Rate:")))
		case strings.HasPrefix(t, "MCS Index:"):
			s.MCS, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, "MCS Index:")))
			s.MCSPresent = true // an MCS line was seen, so MCS 0 is real (not absent)
		case strings.HasPrefix(t, "PHY Mode:"):
			s.PHY = strings.TrimSpace(strings.TrimPrefix(t, "PHY Mode:"))
		case strings.HasPrefix(t, "Channel:"):
			num, band, width := parseChannelSpec(strings.TrimSpace(strings.TrimPrefix(t, "Channel:")))
			s.ChannelNum, s.Band, s.Width = num, band, width
			// channelLabel (airspace.go) is the ONE source of the "149/80" form,
			// shared with parseNeighborNetworks so the two can never drift.
			s.Channel = channelLabel(num, width)
		}
	}
	if !sawSignal {
		return SignalSample{}
	}
	s.OK = true
	return s
}

func indentOf(ln string) int {
	return len(ln) - len(strings.TrimLeft(ln, " \t"))
}

// parseSignalNoise splits "-43 dBm / -92 dBm" into RSSI and Noise (dBm).
func parseSignalNoise(s string) (rssi, noise int, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	r, err1 := parseLeadingInt(parts[0])
	n, err2 := parseLeadingInt(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return r, n, true
}

// parseLeadingInt reads a leading (possibly negative) integer, ignoring a unit
// tail like " dBm".
func parseLeadingInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	end := 0
	for i, c := range s {
		if c == '-' || c == '+' || (c >= '0' && c <= '9') {
			end = i + 1
		} else {
			break
		}
	}
	if end == 0 {
		return 0, fmt.Errorf("no integer in %q", s)
	}
	return strconv.Atoi(s[:end])
}

// parseChannelSpec turns "149 (5GHz, 80MHz)" into (149, "5 GHz", "80MHz").
// A bare "149" yields (149, "", ""). Band is normalized with a space ("5 GHz")
// to match the existing linkInfo label.
func parseChannelSpec(s string) (num int, band, width string) {
	s = strings.TrimSpace(s)
	numPart := s
	if i := strings.IndexByte(s, '('); i >= 0 {
		numPart = s[:i]
		inner := s[i+1:]
		if j := strings.IndexByte(inner, ')'); j >= 0 {
			inner = inner[:j]
		}
		for _, f := range strings.Split(inner, ",") {
			f = strings.TrimSpace(f)
			switch {
			case strings.HasSuffix(f, "MHz"):
				width = f
			case strings.HasSuffix(f, "GHz"):
				band = normalizeBand(f)
			}
		}
	}
	num, _ = strconv.Atoi(strings.TrimSpace(numPart))
	return num, band, width
}

// normalizeBand turns "5GHz" into "5 GHz" (space), matching linkInfo's labels.
// macOS system_profiler writes the 2.4 band as "2GHz"; canonicalize that to
// "2.4 GHz" so it matches wifi.go's label, correlate.go's bandColorKey/
// bandPriority, and the dashboard legend (which all key off "2.4 GHz").
func normalizeBand(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "GHz") && !strings.HasSuffix(s, " GHz") {
		s = strings.TrimSuffix(s, "GHz") + " GHz"
	}
	if s == "2 GHz" {
		return "2.4 GHz"
	}
	return s
}

// parseRate leniently reads a transmit rate in Mb/s: "1200", "1200.0",
// "1200 Mbps" all yield 1200.
func parseRate(s string) float64 {
	s = strings.TrimSpace(s)
	end := 0
	for i, c := range s {
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end = i + 1
		} else {
			break
		}
	}
	if end == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(s[:end], 64)
	return v
}

// summarize aggregates valid samples into SignalStats. It skips OK==false
// samples so a failed poll never pollutes min/avg. Integer averages use
// round-half-away-from-zero. Jitter is the mean absolute consecutive delta
// (mean(|xi - xi-1|)) over valid samples in order; 0 or 1 valid sample => 0.
// PHY/Channel come from the last valid sample; Changed is set if any valid
// sample's PHY or normalized Channel differs from another. Empty / all-invalid
// input yields zeroed stats without panicking.
func summarize(samples []SignalSample) SignalStats {
	var st SignalStats
	st.Polls = len(samples)

	var valid []SignalSample
	for _, s := range samples {
		if s.OK {
			valid = append(valid, s)
		}
	}
	st.Samples = len(valid)
	if len(valid) == 0 {
		return st
	}

	rssis := make([]float64, len(valid))
	snrs := make([]float64, len(valid))
	rates := make([]float64, len(valid))
	for i, s := range valid {
		rssis[i] = float64(s.RSSI)
		snrs[i] = float64(s.SNR)
		rates[i] = s.RateMbps
	}

	st.RSSIMin, st.RSSIMax, st.RSSIAvg = intMin(rssis), intMax(rssis), roundHalfAway(mean(rssis))
	st.RSSIJitter = jitter(rssis)
	st.SNRMin, st.SNRMax, st.SNRAvg = intMin(snrs), intMax(snrs), roundHalfAway(mean(snrs))
	st.SNRJitter = jitter(snrs)
	st.RateMin, st.RateMax, st.RateAvg = fMin(rates), fMax(rates), mean(rates)
	st.RateJitter = jitter(rates)

	last := valid[len(valid)-1]
	st.PHY, st.Channel = last.PHY, last.Channel
	for _, s := range valid {
		if s.PHY != last.PHY || s.Channel != last.Channel {
			st.Changed = true
			break
		}
	}
	return st
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func intMin(xs []float64) int { return int(fMin(xs)) }
func intMax(xs []float64) int { return int(fMax(xs)) }

func fMin(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func fMax(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

// jitter is the mean absolute consecutive delta over the series.
func jitter(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	sum := 0.0
	for i := 1; i < len(xs); i++ {
		sum += math.Abs(xs[i] - xs[i-1])
	}
	return sum / float64(len(xs)-1)
}

// roundHalfAway rounds to the nearest integer, ties away from zero.
func roundHalfAway(f float64) int {
	if f < 0 {
		return int(f - 0.5)
	}
	return int(f + 0.5)
}

// formatSignalCSV returns a header row plus one row per VALID sample. Channel
// is decomposed into channel_num/band/width columns so the comma inside
// "149 (5GHz, 80MHz)" never breaks the CSV. Uses encoding/csv for quoting.
func formatSignalCSV(samples []SignalSample) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"t", "ok", "rssi", "noise", "snr", "rate_mbps", "mcs", "phy", "channel_num", "band", "width"})
	for _, s := range samples {
		if !s.OK {
			continue
		}
		_ = w.Write([]string{
			s.T.Format(time.RFC3339),
			strconv.FormatBool(s.OK),
			strconv.Itoa(s.RSSI),
			strconv.Itoa(s.Noise),
			strconv.Itoa(s.SNR),
			strconv.FormatFloat(s.RateMbps, 'f', -1, 64),
			strconv.Itoa(s.MCS),
			s.PHY,
			strconv.Itoa(s.ChannelNum),
			s.Band,
			s.Width,
		})
	}
	w.Flush()
	return b.String()
}

// ---- impure sampling layer -------------------------------------------------

// sampleSignal polls system_profiler once per second n times and aggregates.
// system_profiler is packet-free, so n never emits traffic.
func sampleSignal(n int) []SignalSample {
	fmt.Fprintf(os.Stderr, "netdebug: sampling signal for ~%ds...\n", n)
	var s []SignalSample
	for i := 0; i < n; i++ {
		out, _ := exec.Command("system_profiler", "SPAirPortDataType").Output()
		smp := parseAirportSample(string(out))
		smp.T = time.Now()
		s = append(s, smp)
		if i < n-1 {
			time.Sleep(time.Second)
		}
	}
	return s
}

// runSignalSection performs the --signal poll, fills r.Signal, and appends a
// soft advisory check. Never feeds diagnose().
func runSignalSection(r *NetResult, iface string, opt Options) {
	raw := sampleSignal(opt.Samples)
	stats := summarize(raw)

	if opt.SignalCSV != "" {
		if err := os.WriteFile(opt.SignalCSV, []byte(formatSignalCSV(raw)), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "netdebug: could not write --signal-csv %q: %v\n", opt.SignalCSV, err)
		}
		// Raw rows live in the CSV; keep the JSON report small.
	} else {
		stats.Raw = raw
	}

	if stats.Samples == 0 {
		// No valid samples: emit no signal check (avoids a spurious WARN),
		// and no r.Signal stanza.
		return
	}

	// Backfill the core Radio line only under --signal, only when empty.
	if r.RSSI == 0 && stats.RSSIAvg != 0 {
		r.RSSI = stats.RSSIAvg
	}
	if r.Channel == "" && stats.Channel != "" {
		r.Channel = stats.Channel
	}
	if r.Band == "" {
		for _, s := range raw {
			if s.OK && s.Band != "" {
				r.Band = s.Band
				break
			}
		}
	}

	r.addKind("advisory", "signal", stats.SNRAvg >= 20,
		"SNR avg %d dB, RSSI avg %d dBm", stats.SNRAvg, stats.RSSIAvg)
	r.Signal = &stats
}
