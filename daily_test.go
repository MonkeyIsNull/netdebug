package main

import (
	"strings"
	"testing"
	"time"
)

// day is a fixed LOCAL target day used across the fixtures.
var reportDay = time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)

func localT(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
}

func TestDailyReportDedupMinuteAndRollupHour(t *testing.T) {
	buckets := []Bucket{
		// A minute bucket at 10:05 — its own rollup-hour is 10:00.
		{Start: localT(2026, 10, 6, 10, 5), Span: spanMinute, DownBytes: 1000, UpBytes: 500, Samples: 60, Band: "5 GHz"},
		// The OVERLAPPING rollup-hour at 10:00 — must be counted ONCE (skipped, the
		// minute covers hour 10). Feeding this simulates Snapshot's overlapping hour view.
		{Start: localT(2026, 10, 6, 10, 0), Span: spanHour, DownBytes: 1000, UpBytes: 500, Samples: 60, Band: "5 GHz"},
		// An hour NOT covered by any minute (08:00) — counted.
		{Start: localT(2026, 10, 6, 8, 0), Span: spanHour, DownBytes: 2000, UpBytes: 100, Samples: 3600, Band: "2.4 GHz"},
	}
	r := dailyReport(buckets, nil, nil, reportDay, "day", defaultThrottleRatio, defaultThrottleMinSamples)

	if r.Day != "2026-10-06" || r.Span != "day" {
		t.Errorf("label/span = %q/%q, want 2026-10-06/day", r.Day, r.Span)
	}
	if r.TotalDownBytes != 3000 || r.TotalUpBytes != 600 {
		t.Errorf("totals = down %d / up %d, want 3000 / 600 (hour 10 de-duped)", r.TotalDownBytes, r.TotalUpBytes)
	}
	if r.BandSeconds["5 GHz"] != 60 {
		t.Errorf("BandSeconds[5 GHz] = %v, want 60 (counted once)", r.BandSeconds["5 GHz"])
	}
	if r.BandSeconds["2.4 GHz"] != 3600 {
		t.Errorf("BandSeconds[2.4 GHz] = %v, want 3600", r.BandSeconds["2.4 GHz"])
	}
}

func TestDailyReportOutageCountDayFilter(t *testing.T) {
	outages := []Outage{
		{Start: localT(2026, 10, 6, 9, 0), End: localT(2026, 10, 6, 9, 1), DurationSec: 60, Cause: causeLinkDrop},
		{Start: localT(2026, 10, 6, 23, 59), End: localT(2026, 10, 7, 0, 0), DurationSec: 60, Cause: causeInternetDown},
		// Prior day — EXCLUDED.
		{Start: localT(2026, 10, 5, 9, 0), End: localT(2026, 10, 5, 9, 1), DurationSec: 60, Cause: causeLinkDrop},
	}
	r := dailyReport(nil, outages, nil, reportDay, "day", defaultThrottleRatio, defaultThrottleMinSamples)
	if r.OutageCount != 2 {
		t.Errorf("OutageCount = %d, want 2 (prior day excluded)", r.OutageCount)
	}
}

func TestDailyReportSpeedsAndThrottle(t *testing.T) {
	speeds := []SpeedSample{
		{T: localT(2026, 10, 6, 9, 10), DownloadMbps: 800, LatencyMs: 10, OK: true},
		{T: localT(2026, 10, 6, 9, 40), DownloadMbps: 800, LatencyMs: 15, OK: true},
		{T: localT(2026, 10, 6, 20, 5), DownloadMbps: 300, LatencyMs: 40, OK: true},
		{T: localT(2026, 10, 6, 20, 25), DownloadMbps: 300, LatencyMs: 60, OK: true}, // worst OK latency
		{T: localT(2026, 10, 6, 20, 45), DownloadMbps: 300, LatencyMs: 50, OK: true},
		{T: localT(2026, 10, 6, 20, 55), DownloadMbps: 0, LatencyMs: 9999, OK: false}, // OK=false EXCLUDED
		// Prior day — EXCLUDED from the "day" span.
		{T: localT(2026, 10, 5, 20, 0), DownloadMbps: 10, LatencyMs: 10, OK: true},
	}
	r := dailyReport(nil, nil, speeds, reportDay, "day", defaultThrottleRatio, defaultThrottleMinSamples)

	if len(r.SpeedByHour) != 2 {
		t.Fatalf("SpeedByHour = %+v, want 2 hours (9, 20)", r.SpeedByHour)
	}
	if r.WorstLatencyMs != 60 {
		t.Errorf("WorstLatencyMs = %v, want 60 (max among OK in-day samples; OK=false 9999 ignored)", r.WorstLatencyMs)
	}
	if len(r.FlaggedHours) != 1 || r.FlaggedHours[0] != 20 {
		t.Errorf("FlaggedHours = %v, want [20] (evening dip, baseline not dragged down)", r.FlaggedHours)
	}
	if r.BaselineMbps < 500 {
		t.Errorf("BaselineMbps = %v, want a high-percentile anchor (>500)", r.BaselineMbps)
	}
}

func TestDailyReportWeekSpanAggregates(t *testing.T) {
	speeds := []SpeedSample{
		{T: localT(2026, 10, 3, 14, 10), DownloadMbps: 400, OK: true}, // 3 days ago, hour 14
		{T: localT(2026, 10, 6, 14, 20), DownloadMbps: 600, OK: true}, // today, hour 14
		{T: localT(2026, 9, 28, 14, 0), DownloadMbps: 100, OK: true},  // 8 days ago => OUT of the trailing 7
	}
	r := dailyReport(nil, nil, speeds, reportDay, "week", defaultThrottleRatio, defaultThrottleMinSamples)
	if r.Span != "week" || !strings.HasPrefix(r.Day, "week of ") {
		t.Errorf("week label = %q/%q", r.Day, r.Span)
	}
	// Trailing 7 LOCAL days [2026-09-30 .. 2026-10-06]; the 09-28 sample is excluded.
	if len(r.SpeedByHour) != 1 || r.SpeedByHour[0].Hour != 14 || r.SpeedByHour[0].Samples != 2 {
		t.Fatalf("week SpeedByHour = %+v, want hour 14 with 2 samples (09-28 excluded)", r.SpeedByHour)
	}
	if got := r.SpeedByHour[0].DownAvgMbps; got != 500 {
		t.Errorf("week hour 14 avg = %v, want 500 ((400+600)/2 across days)", got)
	}
}

func TestDailyReportEmptyInputs(t *testing.T) {
	r := dailyReport(nil, nil, nil, reportDay, "day", defaultThrottleRatio, defaultThrottleMinSamples)
	if r.TotalDownBytes != 0 || r.TotalUpBytes != 0 || r.OutageCount != 0 || r.WorstLatencyMs != 0 {
		t.Errorf("empty inputs must zero the totals: %+v", r)
	}
	if r.BandSeconds == nil || len(r.BandSeconds) != 0 {
		t.Errorf("BandSeconds must be non-nil empty: %v", r.BandSeconds)
	}
	if r.SpeedByHour == nil || len(r.SpeedByHour) != 0 {
		t.Errorf("SpeedByHour must be non-nil empty: %v", r.SpeedByHour)
	}
	if r.FlaggedHours == nil || len(r.FlaggedHours) != 0 {
		t.Errorf("FlaggedHours must be non-nil empty: %v", r.FlaggedHours)
	}
}

func TestDailyReportUnknownSpanDefaultsToDay(t *testing.T) {
	r := dailyReport(nil, nil, nil, reportDay, "fortnight", defaultThrottleRatio, defaultThrottleMinSamples)
	if r.Span != "day" {
		t.Errorf("unknown span must default to day, got %q", r.Span)
	}
}

// TestDailyReportHonorsRatio proves throttle_ratio is WIRED THROUGH (the config-contract
// fix): the SAME speeds flag no hours at ratio 0.6 but DO at ratio 0.95, and the
// EFFECTIVE (clamped) ratio is recorded on the report. Baseline p75 ≈ 800; the 700-hour
// dips below 0.95*800=760 but not 0.6*800=480.
func TestDailyReportHonorsRatio(t *testing.T) {
	speeds := []SpeedSample{
		{T: localT(2026, 10, 6, 1, 0), DownloadMbps: 800, OK: true}, {T: localT(2026, 10, 6, 1, 30), DownloadMbps: 800, OK: true},
		{T: localT(2026, 10, 6, 2, 0), DownloadMbps: 800, OK: true}, {T: localT(2026, 10, 6, 2, 30), DownloadMbps: 800, OK: true},
		{T: localT(2026, 10, 6, 3, 0), DownloadMbps: 800, OK: true}, {T: localT(2026, 10, 6, 3, 30), DownloadMbps: 800, OK: true},
		{T: localT(2026, 10, 6, 4, 0), DownloadMbps: 700, OK: true}, {T: localT(2026, 10, 6, 4, 30), DownloadMbps: 700, OK: true},
	}
	loose := dailyReport(nil, nil, speeds, reportDay, "day", 0.6, 2)
	if len(loose.FlaggedHours) != 0 {
		t.Errorf("ratio 0.6 should flag nothing (700 > 480), got %v", loose.FlaggedHours)
	}
	if loose.Ratio != 0.6 {
		t.Errorf("effective Ratio = %v, want 0.6", loose.Ratio)
	}
	strict := dailyReport(nil, nil, speeds, reportDay, "day", 0.95, 2)
	if len(strict.FlaggedHours) != 1 || strict.FlaggedHours[0] != 4 {
		t.Errorf("ratio 0.95 should flag hour 4 (700 < 760), got %v", strict.FlaggedHours)
	}
	if strict.Ratio != 0.95 {
		t.Errorf("effective Ratio = %v, want 0.95", strict.Ratio)
	}
	// A hostile out-of-range ratio is clamped (clampThrottleRatio) to the 0.6 default.
	clamped := dailyReport(nil, nil, speeds, reportDay, "day", 2.0, 2)
	if clamped.Ratio != defaultThrottleRatio {
		t.Errorf("out-of-range ratio must clamp to %v, got %v", defaultThrottleRatio, clamped.Ratio)
	}
	if len(clamped.FlaggedHours) != 0 {
		t.Errorf("clamped-to-0.6 ratio should flag nothing, got %v", clamped.FlaggedHours)
	}
}

// TestDailyReportHonorsMinSamples proves throttle_min_samples is WIRED THROUGH: a
// single-sample dip hour is the noise-guard'd OUT at minSamples 2 but flagged at
// minSamples 1, and the EFFECTIVE value is recorded.
func TestDailyReportHonorsMinSamples(t *testing.T) {
	speeds := []SpeedSample{
		{T: localT(2026, 10, 6, 1, 0), DownloadMbps: 800, OK: true}, {T: localT(2026, 10, 6, 1, 30), DownloadMbps: 800, OK: true},
		{T: localT(2026, 10, 6, 2, 0), DownloadMbps: 800, OK: true}, {T: localT(2026, 10, 6, 2, 30), DownloadMbps: 800, OK: true},
		{T: localT(2026, 10, 6, 3, 0), DownloadMbps: 100, OK: true}, // lone dip sample in hour 3
	}
	guarded := dailyReport(nil, nil, speeds, reportDay, "day", 0.6, 2)
	if len(guarded.FlaggedHours) != 0 {
		t.Errorf("minSamples 2 noise-guards the 1-sample dip hour, got %v", guarded.FlaggedHours)
	}
	if guarded.MinSamples != 2 {
		t.Errorf("effective MinSamples = %d, want 2", guarded.MinSamples)
	}
	sensitive := dailyReport(nil, nil, speeds, reportDay, "day", 0.6, 1)
	if len(sensitive.FlaggedHours) != 1 || sensitive.FlaggedHours[0] != 3 {
		t.Errorf("minSamples 1 should flag the lone dip hour 3, got %v", sensitive.FlaggedHours)
	}
	if sensitive.MinSamples != 1 {
		t.Errorf("effective MinSamples = %d, want 1", sensitive.MinSamples)
	}
}

func TestReportJSONNeverNull(t *testing.T) {
	// Empty data => zeroed totals + [] / {} arrays, NEVER ":null" (mirror
	// TestReachWireShape / TestDataJSONEventsNeverNull).
	b, err := reportJSON(dailyReport(nil, nil, nil, reportDay, "day", defaultThrottleRatio, defaultThrottleMinSamples))
	if err != nil {
		t.Fatalf("reportJSON: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"band_seconds":{}`, `"speed_by_hour":[]`, `"flagged_hours":[]`, `"total_down_bytes":0`} {
		if !strings.Contains(s, want) {
			t.Errorf("reportJSON empty shape missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "null") {
		t.Errorf("reportJSON must never contain null: %s", s)
	}
}
