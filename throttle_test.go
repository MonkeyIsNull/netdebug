package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestShouldSample(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name    string
		last    time.Time
		spacing time.Duration
		want    bool
	}{
		{"never attempted (actual zero Time)", time.Time{}, 30 * time.Minute, true},
		{"40m ago, spacing 30m => due", now.Add(-40 * time.Minute), 30 * time.Minute, true},
		{"exactly 30m ago, spacing 30m => due", now.Add(-30 * time.Minute), 30 * time.Minute, true},
		{"10m ago, spacing 30m => not due", now.Add(-10 * time.Minute), 30 * time.Minute, false},
		{"backward clock (now < last) => false", now.Add(1 * time.Hour), 30 * time.Minute, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSample(now, tc.last, tc.spacing); got != tc.want {
				t.Errorf("shouldSample(now, %v, %v) = %v, want %v", tc.last, tc.spacing, got, tc.want)
			}
		})
	}
}

func TestJitterSpacingNeverShortensBudget(t *testing.T) {
	const minSpacing = 30 * time.Minute
	const jitterMax = 10 * time.Minute
	// Sweep r across [0,1): effective must ALWAYS be in [min, min+jitterMax] and NEVER
	// below the 30m floor (the budget-floor invariant).
	for i := 0; i <= 100; i++ {
		r := float64(i) / 100.0
		eff := jitterSpacing(minSpacing, jitterMax, r)
		if eff < minSpacing {
			t.Fatalf("jitterSpacing(r=%.2f) = %v < floor %v — budget violated", r, eff, minSpacing)
		}
		if eff > minSpacing+jitterMax {
			t.Fatalf("jitterSpacing(r=%.2f) = %v > max %v", r, eff, minSpacing+jitterMax)
		}
	}
	// r=0 => exactly the floor; out-of-range / negative jitterMax stay at/above floor.
	if got := jitterSpacing(minSpacing, jitterMax, 0); got != minSpacing {
		t.Errorf("jitterSpacing(r=0) = %v, want %v", got, minSpacing)
	}
	if got := jitterSpacing(minSpacing, -1, 0.5); got != minSpacing {
		t.Errorf("negative jitterMax must not shorten: got %v, want %v", got, minSpacing)
	}
	if got := jitterSpacing(minSpacing, jitterMax, -0.5); got < minSpacing {
		t.Errorf("negative r must not shorten below floor: got %v", got)
	}
}

func TestBackoffSpacing(t *testing.T) {
	const minSpacing = 30 * time.Minute
	const maxBackoff = 4 * time.Hour
	tests := []struct {
		fails int
		want  time.Duration
	}{
		{0, 30 * time.Minute},
		{1, 60 * time.Minute},
		{2, 120 * time.Minute},
		{3, 240 * time.Minute},
		{4, maxBackoff},   // 8x=240m but capped at 4h=240m; next would exceed
		{5, maxBackoff},   // capped
		{100, maxBackoff}, // no overflow, stays capped
		{-1, minSpacing},  // nonsensical negative => floor
	}
	for _, tc := range tests {
		if got := backoffSpacing(minSpacing, maxBackoff, tc.fails); got != tc.want {
			t.Errorf("backoffSpacing(fails=%d) = %v, want %v", tc.fails, got, tc.want)
		}
	}
}

func TestClampThrottleSpacing(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, defaultThrottleSpacing},
		{-5 * time.Minute, defaultThrottleSpacing},
		{10 * time.Minute, defaultThrottleSpacing}, // below the 30m HARD FLOOR => clamped up
		{30 * time.Minute, 30 * time.Minute},
		{2 * time.Hour, 2 * time.Hour}, // no upper cap
	}
	for _, tc := range tests {
		if got := clampThrottleSpacing(tc.in); got != tc.want {
			t.Errorf("clampThrottleSpacing(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestClampThrottleStreams(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, 1}, {-3, 1}, {1, 1}, {2, 2}, {5, 2}, {100, 2},
	}
	for _, tc := range tests {
		if got := clampThrottleStreams(tc.in); got != tc.want {
			t.Errorf("clampThrottleStreams(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestClampThrottleBytes(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, defaultThrottleBytes},
		{-1, defaultThrottleBytes},
		{2_000_000, throttleMinBytes},
		{10_000_000, 10_000_000},
		{999_000_000, throttleMaxBytes},
		{throttleMinBytes, throttleMinBytes},
		{throttleMaxBytes, throttleMaxBytes},
	}
	for _, tc := range tests {
		if got := clampThrottleBytes(tc.in); got != tc.want {
			t.Errorf("clampThrottleBytes(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestClampThrottleRatio(t *testing.T) {
	tests := []struct{ in, want float64 }{
		{0, defaultThrottleRatio},
		{-1, defaultThrottleRatio},
		{2, defaultThrottleRatio},
		{0.5, 0.5},
		{1, 1},
	}
	for _, tc := range tests {
		if got := clampThrottleRatio(tc.in); got != tc.want {
			t.Errorf("clampThrottleRatio(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMarshalParseSpeedLineRoundTrip(t *testing.T) {
	samples := []SpeedSample{
		{T: time.Date(2026, 10, 6, 9, 15, 0, 0, time.UTC), DownloadMbps: 523.4, LatencyMs: 12.3, OK: true},
		// An OK=false 429 record carries ZERO Mbps and MUST round-trip (the spacing
		// anchor + honest record depend on it) — parseSpeedLine must NOT reject it.
		{T: time.Date(2026, 10, 6, 19, 30, 0, 0, time.UTC), DownloadMbps: 0, OK: false},
	}
	for _, s := range samples {
		line, err := marshalSpeedLine(s)
		if err != nil {
			t.Fatalf("marshalSpeedLine: %v", err)
		}
		got, err := parseSpeedLine(line)
		if err != nil {
			t.Fatalf("parseSpeedLine(%s): %v", line, err)
		}
		if !got.T.Equal(s.T) || got.DownloadMbps != s.DownloadMbps || got.OK != s.OK || got.LatencyMs != s.LatencyMs {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, s)
		}
	}
}

func TestParseSpeedLineRejectsZeroT(t *testing.T) {
	if _, err := parseSpeedLine([]byte(`{"download_mbps":100,"ok":true}`)); err == nil {
		t.Error("parseSpeedLine must reject a zero T")
	}
	if _, err := parseSpeedLine([]byte(`not json`)); err == nil {
		t.Error("parseSpeedLine must reject invalid JSON")
	}
}

func TestParseJSONLSpeedResilience(t *testing.T) {
	ok := SpeedSample{T: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), DownloadMbps: 100, OK: true}
	fail := SpeedSample{T: time.Date(2026, 10, 6, 19, 0, 0, 0, time.UTC), OK: false}
	l1, _ := marshalSpeedLine(ok)
	l2, _ := marshalSpeedLine(fail)
	blob := string(l1) + "\n" + "garbage-middle\n" + "{}\n" + "\n" + string(l2) + "\n" + `{"download_mbps":5`
	got := parseJSONLSpeed([]byte(blob))
	if len(got) != 2 {
		t.Fatalf("parseJSONLSpeed kept %d, want 2 (intact survive, torn/blank/zero-T skipped)", len(got))
	}
	if !got[0].OK || got[1].OK {
		t.Errorf("round-tripped records out of order or wrong: %+v", got)
	}
	// Empty / missing blob => non-nil empty slice (never null on the wire).
	if e := parseJSONLSpeed(nil); e == nil || len(e) != 0 {
		t.Errorf("parseJSONLSpeed(nil) = %v, want non-nil empty", e)
	}
}

func TestPruneSpeed(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	samples := []SpeedSample{
		{T: now.Add(-100 * 24 * time.Hour), OK: true}, // old => dropped
		{T: now.Add(-time.Hour), DownloadMbps: 100, OK: true},
		{T: now.Add(-29 * 24 * time.Hour), OK: false}, // within TTL => kept (even OK=false)
	}
	got := pruneSpeed(samples, now, ttl)
	if len(got) != 2 {
		t.Fatalf("pruneSpeed kept %d, want 2", len(got))
	}
	// Empty-safe.
	if e := pruneSpeed(nil, now, ttl); len(e) != 0 {
		t.Errorf("pruneSpeed(nil) = %v, want empty", e)
	}
}

func TestSpeedByHour(t *testing.T) {
	mk := func(hour int, mbps float64, ok bool) SpeedSample {
		return SpeedSample{T: time.Date(2026, 10, 6, hour, 30, 0, 0, time.Local), DownloadMbps: mbps, OK: ok}
	}
	samples := []SpeedSample{
		mk(9, 800, true), mk(9, 600, true), // hour 9 avg 700, 2 samples
		mk(14, 500, true), mk(14, 300, true), // hour 14 avg 400, 2 samples
		mk(20, 200, true),   // hour 20 avg 200, 1 sample
		mk(20, 9999, false), // OK=false EXCLUDED
	}
	hours := speedByHour(samples)
	if len(hours) != 3 {
		t.Fatalf("speedByHour returned %d hours, want 3: %+v", len(hours), hours)
	}
	want := map[int]struct {
		avg     float64
		samples int
	}{9: {700, 2}, 14: {400, 2}, 20: {200, 1}}
	for _, h := range hours {
		w, ok := want[h.Hour]
		if !ok {
			t.Errorf("unexpected hour %d", h.Hour)
			continue
		}
		if h.DownAvgMbps != w.avg || h.Samples != w.samples {
			t.Errorf("hour %d = avg %.1f/%d samples, want %.1f/%d", h.Hour, h.DownAvgMbps, h.Samples, w.avg, w.samples)
		}
		if h.UpSamples != 0 || h.UpAvgMbps != 0 {
			t.Errorf("download-only: hour %d must carry no upload avg (got up %v/%d)", h.Hour, h.UpAvgMbps, h.UpSamples)
		}
	}
	// Sorted ascending by hour.
	if hours[0].Hour != 9 || hours[1].Hour != 14 || hours[2].Hour != 20 {
		t.Errorf("speedByHour not sorted ascending: %+v", hours)
	}
	// Empty => non-nil [] (never null), no panic.
	if e := speedByHour(nil); e == nil || len(e) != 0 {
		t.Errorf("speedByHour(nil) = %v, want non-nil empty", e)
	}
}

func TestSpeedByHourLocalHourNormalization(t *testing.T) {
	// A sample stamped in a FIXED-OFFSET zone must bucket by its LOCAL wall-clock hour
	// (In(time.Local)), not the raw zone hour — a DST / cross-zone JSON reload must not
	// mis-bucket "peak hours".
	plus5 := time.FixedZone("UTC+5", 5*3600)
	// 20:00 local == (20:00 - localOffset + 5h) in the +5 zone; build it by converting.
	localT := time.Date(2026, 10, 6, 20, 30, 0, 0, time.Local)
	foreign := localT.In(plus5)
	hours := speedByHour([]SpeedSample{{T: foreign, DownloadMbps: 300, OK: true}})
	if len(hours) != 1 || hours[0].Hour != 20 {
		t.Errorf("fixed-offset-zone sample mis-bucketed: %+v (want hour 20 local)", hours)
	}
}

func TestDetectThrottlePeakHourDip(t *testing.T) {
	// Off-peak ~800 Mbps across 8 hours; 19:00-21:00 ~300 Mbps (broad evening dip).
	// The HIGH-PERCENTILE (p75) baseline must stay ~800 (NOT dragged down by the evening
	// hours) so the evening dips are flagged. REQUIRED headline case.
	var hours []HourStat
	for _, h := range []int{8, 9, 10, 11, 12, 13, 14, 15} {
		hours = append(hours, HourStat{Hour: h, DownAvgMbps: 800, Samples: 5})
	}
	for _, h := range []int{19, 20, 21} {
		hours = append(hours, HourStat{Hour: h, DownAvgMbps: 300, Samples: 5})
	}
	flagged, baseline := detectThrottle(hours, 0.6, 2)
	if baseline < 700 {
		t.Errorf("baseline %.1f dragged down by evening hours (want ~800)", baseline)
	}
	wantFlagged := map[int]bool{19: true, 20: true, 21: true}
	if len(flagged) != 3 {
		t.Fatalf("flagged = %v, want [19 20 21]", flagged)
	}
	for _, h := range flagged {
		if !wantFlagged[h] {
			t.Errorf("unexpected flagged hour %d", h)
		}
	}
}

func TestDetectThrottleUniformAndNoiseGuard(t *testing.T) {
	// Uniform speed => nothing flagged.
	var uniform []HourStat
	for h := 0; h < 24; h++ {
		uniform = append(uniform, HourStat{Hour: h, DownAvgMbps: 500, Samples: 4})
	}
	flagged, _ := detectThrottle(uniform, 0.6, 2)
	if len(flagged) != 0 {
		t.Errorf("uniform speed must flag nothing, got %v", flagged)
	}
	// An hour below the ratio but with < minSamples is NOT flagged (noise guard) AND is
	// excluded from the baseline.
	hours := []HourStat{
		{Hour: 8, DownAvgMbps: 800, Samples: 5},
		{Hour: 9, DownAvgMbps: 800, Samples: 5},
		{Hour: 20, DownAvgMbps: 50, Samples: 1}, // way below ratio but only 1 sample
	}
	flagged, _ = detectThrottle(hours, 0.6, 2)
	if len(flagged) != 0 {
		t.Errorf("sub-minSamples hour must not be flagged, got %v", flagged)
	}
	// Empty => non-nil empty slice, zero baseline, no panic.
	if f, b := detectThrottle(nil, 0.6, 2); f == nil || len(f) != 0 || b != 0 {
		t.Errorf("detectThrottle(nil) = %v, %v; want non-nil empty + 0", f, b)
	}
}

func TestRunThrottleSampleFailure(t *testing.T) {
	// A sampler failure (ok=false) yields an OK=false SpeedSample with zero Mbps,
	// timestamped — recorded for the spacing anchor, excluded from stats. We exercise
	// runThrottleSample via sampleDownload against an empty URL (NewRequest/Do fails or a
	// non-200), which must return ok=false WITHOUT hitting Cloudflare.
	cfg := DefaultConfig()
	cfg.SpeedDownURL = "http://127.0.0.1:1/__none" // unroutable loopback port; no Cloudflare
	cfg.SpeedLatencyURL = ""                       // skip the warm-up GET entirely
	s := runThrottleSample(t.Context(), cfg)
	if s.OK {
		t.Errorf("expected OK=false on a failed sample, got %+v", s)
	}
	if s.DownloadMbps != 0 {
		t.Errorf("a failed sample must carry zero Mbps, got %v", s.DownloadMbps)
	}
	if s.T.IsZero() {
		t.Error("a failed sample must still be timestamped (the spacing anchor)")
	}
}

// TestSampleDownloadHonorsStreams proves cfg.ThrottleSampleStreams is WIRED THROUGH
// (the config-contract fix): streams=2 issues exactly 2 measured GETs, each requesting
// a SPLIT of the bounded total (byteCap/streams) so the total transfer stays bounded at
// ~byteCap regardless of stream count. Uses a LOOPBACK httptest server — NO Cloudflare.
func TestSampleDownloadHonorsStreams(t *testing.T) {
	for _, streams := range []int{1, 2} {
		streams := streams
		t.Run(strconv.Itoa(streams)+"streams", func(t *testing.T) {
			var mu sync.Mutex
			reqCount := 0
			var totalRequestedBytes int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
				if n <= 0 {
					n = 1
				}
				mu.Lock()
				reqCount++
				totalRequestedBytes += int64(n)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				w.Write(make([]byte, n))
			}))
			defer srv.Close()

			cfg := DefaultConfig()
			cfg.SpeedDownURL = srv.URL + "/__down"
			cfg.SpeedLatencyURL = "" // skip the warm-up GET so reqCount counts only measured streams
			cfg.ThrottleSampleStreams = streams
			cfg.ThrottleSampleBytes = 6_000_000

			down, _, ok := sampleDownload(context.Background(), cfg)
			if !ok {
				t.Fatalf("sampleDownload ok=false against a 200 loopback server")
			}
			if down <= 0 {
				t.Errorf("expected positive download mbps, got %v", down)
			}
			if reqCount != streams {
				t.Errorf("streams=%d => %d measured GET(s), got %d", streams, streams, reqCount)
			}
			// Total bytes requested across streams stays bounded at ~byteCap (the split
			// invariant — more streams must NOT mean more total bytes).
			if totalRequestedBytes != int64(cfg.ThrottleSampleBytes) {
				t.Errorf("total requested bytes = %d, want %d (byteCap split across streams)", totalRequestedBytes, cfg.ThrottleSampleBytes)
			}
		})
	}
}

// TestSampleDownloadNon200IsOKFalse proves a non-200 (429/403) on ANY stream makes the
// whole sample OK=false — never a bogus slow number. Loopback only, NO Cloudflare.
func TestSampleDownloadNon200IsOKFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // 429
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.SpeedDownURL = srv.URL + "/__down"
	cfg.SpeedLatencyURL = ""
	cfg.ThrottleSampleStreams = 2
	_, _, ok := sampleDownload(context.Background(), cfg)
	if ok {
		t.Error("a 429 must yield ok=false, never a bogus slow result")
	}
}

func TestThrottleDownURLSetsBytes(t *testing.T) {
	got := throttleDownURL("https://speed.cloudflare.com/__down?bytes=50000000", 10_000_000)
	if got != "https://speed.cloudflare.com/__down?bytes=10000000" {
		t.Errorf("throttleDownURL did not swap bytes=: %s", got)
	}
	// No existing query => bytes is appended.
	got = throttleDownURL("https://example.test/dl", 5_000_000)
	if got != "https://example.test/dl?bytes=5000000" {
		t.Errorf("throttleDownURL did not append bytes=: %s", got)
	}
}
