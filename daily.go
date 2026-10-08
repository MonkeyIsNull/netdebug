package main

// Phase 13 (capstone) — THE DAILY/WEEKLY ROLLUP. dailyReport composes the whole
// era's persisted data — Phase-3 history Buckets (total up/down, time-on-each-band),
// Phase-8 Outages (drop count), and Phase-13 SpeedSamples (speed-by-hour + worst
// sample latency + flagged peak-hour dips) — into one "here's your day" summary.
//
// report.go ALREADY exists (the --compare printer); this is a SEPARATE file and does
// not touch it (emptyDash is reused for the text summary).
//
// Pure/impure split (house style): dailyReport / reportJSON are PURE (no IO, no
// time.Now() inside — the caller supplies `day`) so the capstone compute is fully
// fixture-table-tested; printDayReport is a pure-ish string build to stdout.
//
// HONESTY CAVEAT (mandatory, prominent): detectThrottle CANNOT cleanly separate ISP
// throttling from local congestion / self-inflicted upload saturation / a bad
// channel, AND the small-transfer sample is a RELATIVE proxy, not an absolute
// throughput number. Flagged hours are "peak-hour dips observed", cross-referenced
// with the outage journal + top-talkers — NEVER a definitive throttling accusation.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// throttleHonestyCaveat is the single Go source of truth for the mandatory caveat,
// shared by the --report text summary and (as a prominent static block) the /report
// page. Keep the phrase "peak-hour dips observed" — the positive page-token test
// pins it.
const throttleHonestyCaveat = "NOTE: flagged hours are peak-hour dips observed in a small-transfer relative proxy, " +
	"NOT a definitive throttling accusation. A dip can be ISP throttling, local congestion, " +
	"your own upload saturation, or a bad channel. Cross-reference with the outage journal and top-talkers."

// DayReport is the daily/weekly rollup. BandSeconds + SpeedByHour + FlaggedHours are
// FORCED non-nil (see reportJSON) so the wire is []/{} and never null (the
// historyJSON / outageJSON house rule).
type DayReport struct {
	Day            string             `json:"day"`  // "2026-10-05" or "week of 2026-09-30..2026-10-06"
	Span           string             `json:"span"` // "day" | "week"
	TotalDownBytes uint64             `json:"total_down_bytes"`
	TotalUpBytes   uint64             `json:"total_up_bytes"`
	BandSeconds    map[string]float64 `json:"band_seconds"` // ≈ time on each band (APPROXIMATE sample-seconds)
	OutageCount    int                `json:"outage_count"`
	WorstLatencyMs float64            `json:"worst_latency_ms"` // worst among OK SPEED SAMPLES only (labelled honestly)
	SpeedByHour    []HourStat         `json:"speed_by_hour"`
	FlaggedHours   []int              `json:"flagged_hours"` // detectThrottle result (non-nil)
	BaselineMbps   float64            `json:"baseline_mbps"`
	Ratio          float64            `json:"ratio"`       // EFFECTIVE (clamped) dip ratio fed to detectThrottle
	MinSamples     int                `json:"min_samples"` // EFFECTIVE per-hour min-sample noise guard fed to detectThrottle
}

// spanWindow returns the [start, end) LOCAL instant range for span over the target
// `day`. "day" is the single LOCAL calendar day of `day`; anything else ("week" and
// an unknown span alike) is the inclusive trailing-7-days range [day-6 .. day]. It
// also returns the human label.
func spanWindow(day time.Time, span string) (start, end time.Time, label string) {
	d := day.In(time.Local)
	y, mo, dd := d.Date()
	dayStart := time.Date(y, mo, dd, 0, 0, 0, 0, time.Local)
	dayEnd := dayStart.AddDate(0, 0, 1)
	if span == "week" {
		weekStart := dayStart.AddDate(0, 0, -6)
		return weekStart, dayEnd, fmt.Sprintf("week of %s..%s", weekStart.Format("2006-01-02"), dayStart.Format("2006-01-02"))
	}
	return dayStart, dayEnd, dayStart.Format("2006-01-02")
}

// dailyReport composes a DayReport PURELY from already-decoded inputs + the target
// day/now — NO IO, NO time.Now() inside. span "day" filters to the single LOCAL
// calendar day of `day`; "week" is the inclusive trailing-7-days range (LOCAL days),
// aggregating SpeedByHour across all 7 into the 24 hour-of-day buckets. ALL day/hour
// filtering uses .In(time.Local) consistently.
//
// Buckets: de-dup minute vs. rollup-hour by covered-hour set (§0.1 #3) for BOTH the
// byte totals AND BandSeconds — so dailyReport is correct whether fed the DISJOINT
// on-disk tiers (--report) OR Snapshot()'s OVERLAPPING hour view (/report). Outages:
// count those whose Start is in range. Speeds: speedByHour + detectThrottle over
// in-range OK samples; WorstLatencyMs = max LatencyMs over in-range OK samples.
//
// ratio / minSamples are the throttle-detection tunables (config throttle_ratio /
// throttle_min_samples): ratio is clamped here via clampThrottleRatio (the
// impure-value clamp applied pure-ly — a hostile ratio can never widen or disable the
// dip gate), and minSamples is floored at 1. Both EFFECTIVE values are recorded on the
// DayReport so the text summary / wire are self-describing.
func dailyReport(buckets []Bucket, outages []Outage, speeds []SpeedSample, day time.Time, span string, ratio float64, minSamples int) DayReport {
	if span != "day" && span != "week" {
		span = "day"
	}
	ratio = clampThrottleRatio(ratio)
	if minSamples < 1 {
		minSamples = 1
	}
	start, end, label := spanWindow(day, span)
	inRange := func(t time.Time) bool {
		lt := t.In(time.Local)
		return !lt.Before(start) && lt.Before(end)
	}

	r := DayReport{Day: label, Span: span, BandSeconds: map[string]float64{}, SpeedByHour: []HourStat{}, FlaggedHours: []int{}, Ratio: ratio, MinSamples: minSamples}

	// Buckets: sum ALL in-range minute-span buckets; then add an in-range hour-span
	// bucket ONLY when its hour-Start is not already covered by a minute bucket. Apply
	// the IDENTICAL de-dup to BandSeconds. This neutralizes Snapshot's overlapping hour
	// view AND the documented prune-crash residue (a promoted hour transiently in both
	// files).
	coveredHours := make(map[int64]bool)
	for _, b := range buckets {
		if b.Span == spanMinute && inRange(b.Start) {
			coveredHours[bucketStart(b.Start, spanHour).UnixNano()] = true
		}
	}
	addBucket := func(b Bucket) {
		r.TotalDownBytes += b.DownBytes
		r.TotalUpBytes += b.UpBytes
		if b.Band != "" {
			r.BandSeconds[b.Band] += float64(b.Samples)
		}
	}
	for _, b := range buckets {
		if !inRange(b.Start) {
			continue
		}
		switch b.Span {
		case spanMinute:
			addBucket(b)
		case spanHour:
			if !coveredHours[bucketStart(b.Start, spanHour).UnixNano()] {
				addBucket(b)
			}
		}
	}

	// Outages: count those whose Start is in range (Phase 8).
	for _, o := range outages {
		if inRange(o.Start) {
			r.OutageCount++
		}
	}

	// Speeds: speedByHour + detectThrottle over in-range OK samples; WorstLatencyMs =
	// max LatencyMs over in-range OK samples.
	inRangeSpeeds := make([]SpeedSample, 0, len(speeds))
	for _, s := range speeds {
		if inRange(s.T) {
			inRangeSpeeds = append(inRangeSpeeds, s)
			if s.OK && s.LatencyMs > r.WorstLatencyMs {
				r.WorstLatencyMs = s.LatencyMs
			}
		}
	}
	r.SpeedByHour = speedByHour(inRangeSpeeds)
	r.FlaggedHours, r.BaselineMbps = detectThrottle(r.SpeedByHour, ratio, minSamples)
	return r
}

// reportJSON is the PURE /report.json serializer: it re-forces BandSeconds,
// SpeedByHour and FlaggedHours to non-nil values as the FINAL guard before Marshal
// (mirrors historyJSON/outageJSON), so empty data serializes zeroed totals + [] / {}
// and never ":null".
func reportJSON(r DayReport) ([]byte, error) {
	if r.BandSeconds == nil {
		r.BandSeconds = map[string]float64{}
	}
	if r.SpeedByHour == nil {
		r.SpeedByHour = []HourStat{}
	}
	if r.FlaggedHours == nil {
		r.FlaggedHours = []int{}
	}
	return json.Marshal(r)
}

// printDayReport renders the --report text summary. It LEADS with the honesty caveat
// and prints totals / band time / drop count / worst-sample latency / a text
// speed-by-hour list with flagged hours marked "(dip)".
func printDayReport(r DayReport) {
	flagged := make(map[int]bool, len(r.FlaggedHours))
	for _, h := range r.FlaggedHours {
		flagged[h] = true
	}

	fmt.Println("==================== DAILY REPORT ====================")
	fmt.Printf("Span      : %s (%s)\n", emptyDash(r.Day), r.Span)
	fmt.Println(throttleHonestyCaveat)
	fmt.Println("------------------------------------------------------")
	fmt.Printf("Total down: %s\n", humanBytes(r.TotalDownBytes))
	fmt.Printf("Total up  : %s\n", humanBytes(r.TotalUpBytes))
	fmt.Printf("Drops     : %d outage(s)\n", r.OutageCount)
	if r.WorstLatencyMs > 0 {
		fmt.Printf("Worst lat : %.1f ms (worst among speed samples; NOT a full-day worst)\n", r.WorstLatencyMs)
	} else {
		fmt.Printf("Worst lat : - (no speed samples)\n")
	}

	// Band time (≈ sample-seconds on each dominant band), sorted descending by seconds.
	if len(r.BandSeconds) > 0 {
		fmt.Println("Band time (≈):")
		type bt struct {
			band string
			secs float64
		}
		bands := make([]bt, 0, len(r.BandSeconds))
		for b, s := range r.BandSeconds {
			bands = append(bands, bt{b, s})
		}
		sort.Slice(bands, func(i, j int) bool {
			if bands[i].secs != bands[j].secs {
				return bands[i].secs > bands[j].secs
			}
			return bands[i].band < bands[j].band
		})
		for _, b := range bands {
			fmt.Printf("  %-10s ≈ %.0f s\n", b.band, b.secs)
		}
	}

	// Speed-by-hour (OK samples only), flagged hours marked.
	if len(r.SpeedByHour) > 0 {
		fmt.Printf("Speed by hour (baseline p75 ≈ %.1f Mbps; dip = < %.0f%% of baseline, min %d sample%s/hr):\n",
			r.BaselineMbps, r.Ratio*100, r.MinSamples, plural(r.MinSamples))
		for _, h := range r.SpeedByHour {
			mark := ""
			if flagged[h.Hour] {
				mark = "  (dip)"
			}
			fmt.Printf("  %02d:00  %6.1f Mbps  (%d sample%s)%s\n",
				h.Hour, h.DownAvgMbps, h.Samples, plural(h.Samples), mark)
		}
	} else {
		fmt.Println("Speed by hour: - (no speed samples — run --serve --throttle-watch over a day)")
	}
	fmt.Println("======================================================")
}

// plural returns "s" unless n==1 (tiny local helper for the text summary).
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// humanBytes formats a TOTAL byte count (decimal, 1000-base: B/KB/MB/GB/TB) for the
// text summary. Distinct from humanBps (which renders a per-second rate as bits/sec).
func humanBytes(b uint64) string {
	f := float64(b)
	switch {
	case f >= 1e12:
		return fmt.Sprintf("%.2f TB", f/1e12)
	case f >= 1e9:
		return fmt.Sprintf("%.2f GB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.2f MB", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1f KB", f/1e3)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
