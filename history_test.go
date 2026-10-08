package main

// Phase 3 tests — the history persistence & aggregation surface. Pure functions
// are table-driven (bucketStart / sampleBytes / foldSample / bucketize /
// advanceBucket / rollup / mergeBucketsByStart / marshal+parse / parseJSONLBuckets
// / pruneBuckets / clampTTL / historyJSON); the impure historyStore layer
// (openHistoryStore / ingest / appendLine / Snapshot / FlushOpen / prune and the
// writeTempFile / commit atomic-rewrite primitives) is exercised against a
// t.TempDir(). The whole file is meant to pass under `go test -race`.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// mkSample builds a Sample at wall-clock t with the given cumulative counters and
// per-sample rates (bytes/sec). reset flags a counter reset/invalid read.
func mkSample(t time.Time, rx, tx uint64, downBps, upBps float64, reset bool) Sample {
	return Sample{T: t, RxBytes: rx, TxBytes: tx, DownBytesPerSec: downBps, UpBytesPerSec: upBps, Reset: reset}
}

func TestBucketStart(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata") // +05:30 half-hour offset
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	tests := []struct {
		name string
		in   time.Time
		span string
		want time.Time
	}{
		{
			name: "minute truncates seconds/nanos, TZ preserved",
			in:   time.Date(2026, 10, 2, 12, 34, 56, 700000000, time.UTC),
			span: spanMinute,
			want: time.Date(2026, 10, 2, 12, 34, 0, 0, time.UTC),
		},
		{
			name: "hour truncates minutes down, TZ preserved",
			in:   time.Date(2026, 10, 2, 12, 34, 56, 0, time.UTC),
			span: spanHour,
			want: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		},
		{
			// The half-hour-offset case: an hour bucket in +05:30 must land on the
			// LOCAL :00, not a :30 bucket (which time.Truncate would produce because
			// it rounds relative to the UTC zero instant).
			name: "half-hour-offset zone hour-bucket lands on local :00 not :30",
			in:   time.Date(2026, 10, 2, 12, 34, 56, 0, kolkata),
			span: spanHour,
			want: time.Date(2026, 10, 2, 12, 0, 0, 0, kolkata),
		},
		{
			name: "half-hour-offset zone minute-bucket preserves local minute",
			in:   time.Date(2026, 10, 2, 12, 34, 56, 0, kolkata),
			span: spanMinute,
			want: time.Date(2026, 10, 2, 12, 34, 0, 0, kolkata),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bucketStart(tc.in, tc.span)
			if !got.Equal(tc.want) {
				t.Errorf("bucketStart(%v,%q) = %v, want %v", tc.in, tc.span, got, tc.want)
			}
			// The offset must be the SAME as the input (zone preserved), not forced
			// to UTC — that is what makes the half-hour case meaningful.
			_, wantOff := tc.want.Zone()
			_, gotOff := got.Zone()
			if gotOff != wantOff {
				t.Errorf("bucketStart offset = %d, want %d (zone not preserved)", gotOff, wantOff)
			}
		})
	}
}

func TestSampleBytes(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		lastRx, lastTx   uint64
		haveLast         bool
		s                Sample
		wantDown, wantUp uint64
	}{
		{
			name:     "no predecessor seeds baseline, contributes 0",
			haveLast: false,
			s:        mkSample(now, 1000, 2000, 0, 0, false),
			wantDown: 0, wantUp: 0,
		},
		{
			name:   "exact cumulative delta from predecessor",
			lastRx: 1000, lastTx: 2000, haveLast: true,
			s:        mkSample(now, 1500, 2300, 0, 0, false),
			wantDown: 500, wantUp: 300,
		},
		{
			name:   "reset sample contributes 0 even with a predecessor",
			lastRx: 1000, lastTx: 2000, haveLast: true,
			s:        mkSample(now, 9999, 9999, 0, 0, true),
			wantDown: 0, wantUp: 0,
		},
		{
			name:   "counter went backward (wrap) contributes 0 (compare before subtract)",
			lastRx: 5000, lastTx: 5000, haveLast: true,
			s:        mkSample(now, 10, 20, 0, 0, false),
			wantDown: 0, wantUp: 0,
		},
		{
			name:   "down advanced, up went backward => only down counts",
			lastRx: 1000, lastTx: 5000, haveLast: true,
			s:        mkSample(now, 1200, 10, 0, 0, false),
			wantDown: 200, wantUp: 0,
		},
		{
			name:   "large gap (skipped ticks) still exact via cumulative delta",
			lastRx: 1000, lastTx: 1000, haveLast: true,
			s:        mkSample(now, 1000000, 1000000, 0, 0, false),
			wantDown: 999000, wantUp: 999000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			down, up := sampleBytes(tc.lastRx, tc.lastTx, tc.haveLast, tc.s)
			if down != tc.wantDown || up != tc.wantUp {
				t.Errorf("sampleBytes = (%d,%d), want (%d,%d)", down, up, tc.wantDown, tc.wantUp)
			}
		})
	}
}

func TestFoldSample(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	b := freshBucket(now, spanMinute)
	b = foldSample(b, mkSample(now, 0, 0, 100, 50, false), 500, 300)
	b = foldSample(b, mkSample(now, 0, 0, 80, 70, false), 200, 100)
	if b.DownBytes != 700 || b.UpBytes != 400 {
		t.Errorf("bytes = (%d,%d), want (700,400)", b.DownBytes, b.UpBytes)
	}
	if b.DownPeakBps != 100 || b.UpPeakBps != 70 { // max of each rate stream
		t.Errorf("peaks = (%v,%v), want (100,70)", b.DownPeakBps, b.UpPeakBps)
	}
	if b.Samples != 2 {
		t.Errorf("Samples = %d, want 2", b.Samples)
	}
}

func TestBucketize(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	t.Run("several samples in one minute => one bucket, exact delta + max peak", func(t *testing.T) {
		samples := []Sample{
			mkSample(base, 1000, 500, 0, 0, false), // seed, 0 bytes
			mkSample(base.Add(1*time.Second), 1500, 700, 500, 200, false),
			mkSample(base.Add(2*time.Second), 2200, 900, 700, 200, false),
		}
		got := bucketize(samples, spanMinute)
		if len(got) != 1 {
			t.Fatalf("got %d buckets, want 1", len(got))
		}
		b := got[0]
		// exact cumulative delta: last.Rx - first.Rx = 2200-1000 = 1200; Tx 900-500=400
		if b.DownBytes != 1200 || b.UpBytes != 400 {
			t.Errorf("bytes = (%d,%d), want (1200,400)", b.DownBytes, b.UpBytes)
		}
		if b.DownPeakBps != 700 || b.UpPeakBps != 200 {
			t.Errorf("peaks = (%v,%v), want (700,200)", b.DownPeakBps, b.UpPeakBps)
		}
		if b.Samples != 3 {
			t.Errorf("Samples = %d, want 3", b.Samples)
		}
	})

	t.Run("samples spanning two minutes => two ordered buckets partitioned by T", func(t *testing.T) {
		min2 := base.Add(1 * time.Minute)
		samples := []Sample{
			mkSample(base, 1000, 0, 0, 0, false),                      // seed
			mkSample(base.Add(1*time.Second), 1500, 0, 500, 0, false), // +500 in min1
			mkSample(min2, 2000, 0, 500, 0, false),                    // +500 in min2
		}
		got := bucketize(samples, spanMinute)
		if len(got) != 2 {
			t.Fatalf("got %d buckets, want 2", len(got))
		}
		if !got[0].Start.Equal(base) || !got[1].Start.Equal(min2) {
			t.Errorf("starts = %v,%v; want %v,%v", got[0].Start, got[1].Start, base, min2)
		}
		if got[0].DownBytes != 500 || got[1].DownBytes != 500 {
			t.Errorf("down bytes = %d,%d; want 500,500", got[0].DownBytes, got[1].DownBytes)
		}
	})

	t.Run("out-of-order T coalesces into one bucket per Start", func(t *testing.T) {
		samples := []Sample{
			mkSample(base.Add(2*time.Second), 1000, 0, 0, 0, false),   // later T first
			mkSample(base.Add(1*time.Second), 1500, 0, 500, 0, false), // earlier T second
		}
		got := bucketize(samples, spanMinute)
		if len(got) != 1 {
			t.Fatalf("got %d buckets, want 1 (same minute coalesced)", len(got))
		}
	})

	t.Run("empty input => empty slice", func(t *testing.T) {
		got := bucketize(nil, spanMinute)
		if len(got) != 0 {
			t.Errorf("got %d buckets, want 0", len(got))
		}
	})

	t.Run("reset sample adds 0 bytes but counts and re-seeds baseline", func(t *testing.T) {
		samples := []Sample{
			mkSample(base, 1000, 0, 0, 0, false),                      // seed
			mkSample(base.Add(1*time.Second), 5000, 0, 0, 0, true),    // reset: 0 bytes, reseed to 5000
			mkSample(base.Add(2*time.Second), 5300, 0, 300, 0, false), // +300 from the reseeded 5000
		}
		got := bucketize(samples, spanMinute)
		if len(got) != 1 {
			t.Fatalf("got %d buckets, want 1", len(got))
		}
		if got[0].DownBytes != 300 {
			t.Errorf("DownBytes = %d, want 300 (reset added 0, reseeded baseline)", got[0].DownBytes)
		}
		if got[0].Samples != 3 {
			t.Errorf("Samples = %d, want 3 (reset still counts)", got[0].Samples)
		}
	})
}

func TestAdvanceBucket(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	t.Run("first sample opens a bucket, no close", func(t *testing.T) {
		closed, didClose, next := advanceBucket(Bucket{}, false, mkSample(base, 0, 0, 0, 0, false), 0, 0, spanMinute)
		if didClose {
			t.Errorf("didClose = true, want false on first sample")
		}
		_ = closed
		if !next.Start.Equal(bucketStart(base, spanMinute)) {
			t.Errorf("next.Start = %v, want %v", next.Start, bucketStart(base, spanMinute))
		}
		if next.Samples != 1 {
			t.Errorf("next.Samples = %d, want 1", next.Samples)
		}
	})

	t.Run("two same-minute samples => no close, one growing bucket", func(t *testing.T) {
		_, _, open := advanceBucket(Bucket{}, false, mkSample(base, 0, 0, 0, 0, false), 0, 0, spanMinute)
		closed, didClose, next := advanceBucket(open, true, mkSample(base.Add(1*time.Second), 0, 0, 0, 0, false), 100, 0, spanMinute)
		if didClose {
			t.Errorf("didClose = true, want false (same minute)")
		}
		_ = closed
		if next.Samples != 2 || next.DownBytes != 100 {
			t.Errorf("next = {Samples:%d, DownBytes:%d}, want {2,100}", next.Samples, next.DownBytes)
		}
	})

	t.Run("next-minute boundary emits prev bucket and opens new", func(t *testing.T) {
		_, _, open := advanceBucket(Bucket{}, false, mkSample(base, 0, 0, 0, 0, false), 0, 0, spanMinute)
		closed, didClose, next := advanceBucket(open, true, mkSample(base.Add(1*time.Minute), 0, 0, 0, 0, false), 50, 0, spanMinute)
		if !didClose {
			t.Fatalf("didClose = false, want true at minute boundary")
		}
		if !closed.Start.Equal(bucketStart(base, spanMinute)) {
			t.Errorf("closed.Start = %v, want %v", closed.Start, bucketStart(base, spanMinute))
		}
		if !next.Start.Equal(bucketStart(base.Add(1*time.Minute), spanMinute)) {
			t.Errorf("next.Start = %v, want the new minute", next.Start)
		}
	})

	t.Run("multi-minute gap emits exactly one bucket and jumps (no empty buckets)", func(t *testing.T) {
		_, _, open := advanceBucket(Bucket{}, false, mkSample(base, 0, 0, 0, 0, false), 0, 0, spanMinute)
		// jump 10 minutes ahead (sleep-wake)
		jump := base.Add(10 * time.Minute)
		closed, didClose, next := advanceBucket(open, true, mkSample(jump, 0, 0, 0, 0, false), 0, 0, spanMinute)
		if !didClose {
			t.Fatalf("didClose = false, want true across a gap")
		}
		if !closed.Start.Equal(bucketStart(base, spanMinute)) {
			t.Errorf("closed.Start = %v, want original minute (exactly one bucket emitted)", closed.Start)
		}
		if !next.Start.Equal(bucketStart(jump, spanMinute)) {
			t.Errorf("next.Start = %v, want the jumped-to minute (no synthesized empties)", next.Start)
		}
	})
}

func TestRollup(t *testing.T) {
	hourBase := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	t.Run("60 minutes in one hour => one hour bucket, summed + max peak", func(t *testing.T) {
		var minutes []Bucket
		for i := 0; i < 60; i++ {
			minutes = append(minutes, Bucket{
				Start:       hourBase.Add(time.Duration(i) * time.Minute),
				Span:        spanMinute,
				DownBytes:   100,
				UpBytes:     10,
				DownPeakBps: float64(i), // max will be 59
				UpPeakBps:   1,
				Samples:     60,
			})
		}
		hrs := rollup(minutes)
		if len(hrs) != 1 {
			t.Fatalf("got %d hours, want 1", len(hrs))
		}
		h := hrs[0]
		if h.DownBytes != 6000 || h.UpBytes != 600 {
			t.Errorf("bytes = (%d,%d), want (6000,600)", h.DownBytes, h.UpBytes)
		}
		if h.DownPeakBps != 59 {
			t.Errorf("DownPeakBps = %v, want 59 (max)", h.DownPeakBps)
		}
		if h.Samples != 3600 {
			t.Errorf("Samples = %d, want 3600", h.Samples)
		}
		if h.Span != spanHour {
			t.Errorf("Span = %q, want hour", h.Span)
		}
	})

	t.Run("minutes crossing an hour boundary => two hours, no minute split", func(t *testing.T) {
		minutes := []Bucket{
			{Start: hourBase.Add(58 * time.Minute), Span: spanMinute, DownBytes: 100},
			{Start: hourBase.Add(59 * time.Minute), Span: spanMinute, DownBytes: 100},
			{Start: hourBase.Add(60 * time.Minute), Span: spanMinute, DownBytes: 100}, // next hour
			{Start: hourBase.Add(61 * time.Minute), Span: spanMinute, DownBytes: 100},
		}
		hrs := rollup(minutes)
		if len(hrs) != 2 {
			t.Fatalf("got %d hours, want 2", len(hrs))
		}
		if hrs[0].DownBytes != 200 || hrs[1].DownBytes != 200 {
			t.Errorf("hour bytes = %d,%d; want 200,200", hrs[0].DownBytes, hrs[1].DownBytes)
		}
	})

	t.Run("idempotent by hour Start", func(t *testing.T) {
		minutes := []Bucket{
			{Start: hourBase, Span: spanMinute, DownBytes: 100},
			{Start: hourBase.Add(time.Minute), Span: spanMinute, DownBytes: 100},
		}
		once := rollup(minutes)
		twice := rollup(append(minutes, minutes...)) // re-including same minutes
		// rollup over duplicated minutes doubles (it sums its literal input); the
		// IDEMPOTENCE that matters is by hour Start — one hour bucket, not two.
		if len(once) != 1 || len(twice) != 1 {
			t.Fatalf("hour count once=%d twice=%d, want 1 and 1 (one Start)", len(once), len(twice))
		}
	})
}

func TestMergeBucketsByStart(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	t.Run("duplicate Start => last write wins", func(t *testing.T) {
		in := []Bucket{
			{Start: base, Span: spanMinute, DownBytes: 100},
			{Start: base, Span: spanMinute, DownBytes: 999}, // later line wins
		}
		got := mergeBucketsByStart(in)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].DownBytes != 999 {
			t.Errorf("DownBytes = %d, want 999 (last-write-wins, never summed)", got[0].DownBytes)
		}
	})

	t.Run("mixed order => sorted by Start", func(t *testing.T) {
		in := []Bucket{
			{Start: base.Add(2 * time.Minute), Span: spanMinute},
			{Start: base, Span: spanMinute},
			{Start: base.Add(1 * time.Minute), Span: spanMinute},
		}
		got := mergeBucketsByStart(in)
		if len(got) != 3 {
			t.Fatalf("got %d, want 3", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].Start.Before(got[i-1].Start) {
				t.Errorf("not sorted at %d: %v before %v", i, got[i].Start, got[i-1].Start)
			}
		}
	})
}

func TestMarshalParseBucketLineRoundTrip(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 34, 0, 0, time.UTC)
	orig := Bucket{
		Start:       base,
		Span:        spanMinute,
		DownBytes:   123456,
		UpBytes:     7890,
		DownPeakBps: 1000.5,
		UpPeakBps:   42.25,
		Samples:     60,
	}
	line, err := marshalBucketLine(orig)
	if err != nil {
		t.Fatalf("marshalBucketLine: %v", err)
	}
	if strings.Contains(string(line), "\n") {
		t.Errorf("marshalBucketLine must not embed a newline: %q", line)
	}
	got, err := parseBucketLine(line)
	if err != nil {
		t.Fatalf("parseBucketLine: %v", err)
	}
	if !got.Start.Equal(orig.Start) { // Equal, never reflect.DeepEqual (monotonic/zone)
		t.Errorf("Start = %v, want %v", got.Start, orig.Start)
	}
	if got.Span != orig.Span || got.DownBytes != orig.DownBytes || got.UpBytes != orig.UpBytes ||
		got.DownPeakBps != orig.DownPeakBps || got.UpPeakBps != orig.UpPeakBps || got.Samples != orig.Samples {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, orig)
	}
}

func TestParseBucketLineRejects(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		line string
	}{
		{"empty object", `{}`},
		{"zero start with valid span", `{"span":"minute"}`},
		{"invalid span", `{"start":"2026-10-02T12:00:00Z","span":"second"}`},
		{"missing span", `{"start":"2026-10-02T12:00:00Z"}`},
		{"not json", `not json at all`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBucketLine([]byte(tc.line)); err == nil {
				t.Errorf("parseBucketLine(%q) = nil error, want rejection", tc.line)
			}
		})
	}
	// Control: a well-formed line IS accepted.
	good, _ := marshalBucketLine(Bucket{Start: base, Span: spanMinute})
	if _, err := parseBucketLine(good); err != nil {
		t.Errorf("well-formed line rejected: %v", err)
	}
}

func TestParseJSONLBucketsResilience(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	b1 := Bucket{Start: base, Span: spanMinute, DownBytes: 100}
	b2 := Bucket{Start: base.Add(time.Minute), Span: spanMinute, DownBytes: 200}
	l1, _ := marshalBucketLine(b1)
	l2, _ := marshalBucketLine(b2)

	assertB1B2 := func(t *testing.T, got []Bucket) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("got %d buckets, want 2", len(got))
		}
		if !got[0].Start.Equal(b1.Start) || got[0].DownBytes != 100 {
			t.Errorf("bucket[0] = %+v, want b1", got[0])
		}
		if !got[1].Start.Equal(b2.Start) || got[1].DownBytes != 200 {
			t.Errorf("bucket[1] = %+v, want b2", got[1])
		}
	}

	t.Run("truncated trailing line (no close brace, no newline)", func(t *testing.T) {
		blob := string(l1) + "\n" + string(l2) + "\n" + `{"start":"2026-10-02T12:00`
		assertB1B2(t, parseJSONLBuckets([]byte(blob)))
	})
	t.Run("trailing blank line", func(t *testing.T) {
		blob := string(l1) + "\n" + string(l2) + "\n\n"
		assertB1B2(t, parseJSONLBuckets([]byte(blob)))
	})
	t.Run("garbage middle line is skipped, not just trailing", func(t *testing.T) {
		blob := string(l1) + "\n" + "not json\n" + string(l2) + "\n"
		assertB1B2(t, parseJSONLBuckets([]byte(blob)))
	})
	t.Run("empty object middle line is rejected", func(t *testing.T) {
		blob := string(l1) + "\n" + "{}\n" + string(l2) + "\n"
		assertB1B2(t, parseJSONLBuckets([]byte(blob)))
	})
	t.Run("all-garbage blob => empty slice, no panic", func(t *testing.T) {
		got := parseJSONLBuckets([]byte("garbage\n{}\nnope\n"))
		if len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})
	t.Run("empty blob => empty slice", func(t *testing.T) {
		if got := parseJSONLBuckets(nil); len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})
}

func TestPruneBuckets(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	buckets := []Bucket{
		{Start: now.Add(-1 * time.Hour), Span: spanMinute},      // within 48h minute TTL
		{Start: now.Add(-50 * time.Hour), Span: spanMinute},     // past 48h minute TTL => drop
		{Start: now.Add(-10 * 24 * time.Hour), Span: spanHour},  // within 90d hour TTL
		{Start: now.Add(-100 * 24 * time.Hour), Span: spanHour}, // past 90d hour TTL => drop
	}

	t.Run("each span uses its own TTL", func(t *testing.T) {
		got := pruneBuckets(buckets, now, defaultMinuteTTL, defaultHourTTL)
		if len(got) != 2 {
			t.Fatalf("got %d, want 2 (one minute + one hour kept)", len(got))
		}
	})

	t.Run("empty input => empty slice", func(t *testing.T) {
		if got := pruneBuckets(nil, now, defaultMinuteTTL, defaultHourTTL); len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})

	t.Run("all expired => empty slice", func(t *testing.T) {
		old := []Bucket{
			{Start: now.Add(-1000 * time.Hour), Span: spanMinute},
			{Start: now.Add(-1000 * 24 * time.Hour), Span: spanHour},
		}
		if got := pruneBuckets(old, now, defaultMinuteTTL, defaultHourTTL); len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})
}

func TestClampTTL(t *testing.T) {
	tests := []struct {
		name string
		n    int
		unit time.Duration
		def  time.Duration
		want time.Duration
	}{
		{"positive minutes-hours", 24, time.Hour, defaultMinuteTTL, 24 * time.Hour},
		{"positive hour-days", 30, 24 * time.Hour, defaultHourTTL, 30 * 24 * time.Hour},
		{"zero clamps to default (not drop-everything)", 0, time.Hour, defaultMinuteTTL, defaultMinuteTTL},
		{"negative clamps to default", -5, 24 * time.Hour, defaultHourTTL, defaultHourTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTTL(tc.n, tc.unit, tc.def); got != tc.want {
				t.Errorf("clampTTL(%d) = %v, want %v", tc.n, got, tc.want)
			}
		})
	}
}

func TestHistoryJSONNeverNull(t *testing.T) {
	t.Run("nil both => empty arrays not null", func(t *testing.T) {
		b, err := historyJSON(nil, nil)
		if err != nil {
			t.Fatalf("historyJSON: %v", err)
		}
		s := string(b)
		if strings.Contains(s, "null") {
			t.Errorf("history JSON contains null: %s", s)
		}
		if !strings.Contains(s, `"minute":[]`) || !strings.Contains(s, `"hour":[]`) {
			t.Errorf("want empty arrays, got %s", s)
		}
	})
	t.Run("populated => both arrays present and parseable", func(t *testing.T) {
		base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
		min := []Bucket{{Start: base, Span: spanMinute, DownBytes: 100}}
		hr := []Bucket{{Start: base, Span: spanHour, DownBytes: 100}}
		b, err := historyJSON(min, hr)
		if err != nil {
			t.Fatalf("historyJSON: %v", err)
		}
		var parsed struct {
			Minute []Bucket `json:"minute"`
			Hour   []Bucket `json:"hour"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(parsed.Minute) != 1 || len(parsed.Hour) != 1 {
			t.Errorf("counts = %d,%d; want 1,1", len(parsed.Minute), len(parsed.Hour))
		}
	})
}

func TestResolveHistoryDir(t *testing.T) {
	t.Run("flag wins over config and default", func(t *testing.T) {
		got, err := resolveHistoryDir("/flag/dir", "/config/dir")
		if err != nil || got != "/flag/dir" {
			t.Errorf("got (%q,%v), want /flag/dir", got, err)
		}
	})
	t.Run("config used when no flag", func(t *testing.T) {
		got, err := resolveHistoryDir("", "/config/dir")
		if err != nil || got != "/config/dir" {
			t.Errorf("got (%q,%v), want /config/dir", got, err)
		}
	})
	t.Run("default ends in netdebug/history under a space-containing path", func(t *testing.T) {
		got, err := resolveHistoryDir("", "")
		if err != nil {
			t.Skipf("UserConfigDir unavailable: %v", err)
		}
		want := filepath.Join("netdebug", "history")
		if !strings.HasSuffix(got, want) {
			t.Errorf("default %q does not end in %q", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Impure historyStore layer — exercised against t.TempDir(). Run under -race.
// ---------------------------------------------------------------------------

func TestOpenHistoryStoreEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "history") // not yet existing
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("openHistoryStore: %v", err)
	}
	defer h.Close()
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		t.Errorf("dir not created: stat err %v", serr)
	}
	min, hr := h.Snapshot()
	if len(min) != 0 || len(hr) != 0 {
		t.Errorf("fresh store Snapshot = %d,%d; want 0,0", len(min), len(hr))
	}
}

func TestOpenHistoryStoreFlockSingleInstance(t *testing.T) {
	dir := t.TempDir()
	h1, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer h1.Close()
	h2, err := openHistoryStore(dir)
	if err == nil {
		h2.Close()
		t.Fatalf("second open on same dir succeeded, want flock failure")
	}
	// After the first closes, a new open must succeed (lock released).
	h1.Close()
	h3, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen after close failed: %v", err)
	}
	h3.Close()
}

func TestIngestCrossMinuteBoundaryPersistsAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	// Two samples in minute 12:00, then one in 12:01 => minute 12:00 closes.
	h.ingest(mkSample(base, 1000, 500, 0, 0, false))                        // seed
	h.ingest(mkSample(base.Add(1*time.Second), 1500, 600, 500, 100, false)) // +500/+100
	h.ingest(mkSample(base.Add(1*time.Minute), 2000, 700, 500, 100, false)) // crosses => closes 12:00

	min, hr := h.Snapshot()
	if len(min) != 1 {
		t.Fatalf("Snapshot minutes = %d, want 1 (closed 12:00; open 12:01 excluded)", len(min))
	}
	if !min[0].Start.Equal(bucketStart(base, spanMinute)) {
		t.Errorf("closed minute Start = %v, want 12:00", min[0].Start)
	}
	// 12:00 captured 1000->1500 = 500 down, 500->600 = 100 up.
	if min[0].DownBytes != 500 || min[0].UpBytes != 100 {
		t.Errorf("closed minute bytes = (%d,%d), want (500,100)", min[0].DownBytes, min[0].UpBytes)
	}
	// Hour view derived immediately (not empty).
	if len(hr) != 1 || hr[0].DownBytes != 500 {
		t.Fatalf("hour snapshot = %+v, want one derived hour with 500 down", hr)
	}

	// Durable on disk: exactly one physical JSONL line, terminator-ended, one object.
	data, rerr := os.ReadFile(filepath.Join(dir, minuteFileName))
	if rerr != nil {
		t.Fatalf("read minute file: %v", rerr)
	}
	assertOnePhysicalLine(t, data)
}

func TestFlushOpenPersistsPartialMinute(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// One open, never-closed partial minute.
	h.ingest(mkSample(base, 1000, 0, 0, 0, false))
	h.ingest(mkSample(base.Add(1*time.Second), 1400, 0, 400, 0, false))
	if min, _ := h.Snapshot(); len(min) != 0 {
		t.Fatalf("Snapshot shows %d minutes before close; the open partial must be hidden", len(min))
	}
	if err := h.FlushOpen(); err != nil {
		t.Fatalf("FlushOpen: %v", err)
	}
	h.Close()

	// A fresh store sees the flushed partial minute.
	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	min, _ := h2.Snapshot()
	if len(min) != 1 {
		t.Fatalf("after reload, minutes = %d, want 1 (flushed partial survived)", len(min))
	}
	if min[0].DownBytes != 400 {
		t.Errorf("flushed partial DownBytes = %d, want 400", min[0].DownBytes)
	}
}

// TestHistorySurvivesRestart is the headline requirement: buckets written before a
// restart are returned immediately after reopening the same dir.
func TestHistorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	h1, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Close two full minutes.
	h1.ingest(mkSample(base, 0, 0, 0, 0, false))
	h1.ingest(mkSample(base.Add(30*time.Second), 1000, 0, 0, 0, false))
	h1.ingest(mkSample(base.Add(1*time.Minute), 2000, 0, 0, 0, false))  // closes 12:00
	h1.ingest(mkSample(base.Add(90*time.Second), 3000, 0, 0, 0, false)) // in 12:01
	h1.ingest(mkSample(base.Add(2*time.Minute), 4000, 0, 0, 0, false))  // closes 12:01
	before, _ := h1.Snapshot()
	h1.Close()

	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	after, _ := h2.Snapshot()
	if len(after) != len(before) || len(after) != 2 {
		t.Fatalf("restart lost buckets: before=%d after=%d, want 2", len(before), len(after))
	}
	for i := range after {
		if !after[i].Start.Equal(before[i].Start) || after[i].DownBytes != before[i].DownBytes {
			t.Errorf("bucket %d changed across restart: %+v vs %+v", i, after[i], before[i])
		}
	}
}

// TestLoaderToleratesCorruptTrailingLine injects a truncated trailing line and a
// garbage middle line into the real on-disk file, then reopens.
func TestLoaderToleratesCorruptTrailingLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	b1 := Bucket{Start: base, Span: spanMinute, DownBytes: 100}
	b2 := Bucket{Start: base.Add(time.Minute), Span: spanMinute, DownBytes: 200}
	l1, _ := marshalBucketLine(b1)
	l2, _ := marshalBucketLine(b2)
	blob := string(l1) + "\n" + "garbage-middle\n" + string(l2) + "\n" + `{"start":"2026-10-02T12:0`
	if err := os.WriteFile(filepath.Join(dir, minuteFileName), []byte(blob), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open with corrupt file must not abort: %v", err)
	}
	defer h.Close()
	min, _ := h.Snapshot()
	if len(min) != 2 {
		t.Fatalf("loaded %d buckets, want 2 (intact ones survive, corrupt skipped)", len(min))
	}
}

// --- Atomic-rewrite invariant tests (durability proven by invariant) ---

func TestWriteTempFileInDir(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	buckets := []Bucket{{Start: base, Span: spanMinute, DownBytes: 100}}
	tmpPath, err := writeTempFile(dir, minuteFileName, buckets)
	if err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	if filepath.Dir(tmpPath) != dir {
		t.Errorf("temp %q not in target dir %q (cross-volume rename risk)", tmpPath, dir)
	}
	if !strings.HasSuffix(tmpPath, ".tmp") {
		t.Errorf("temp %q lacks .tmp suffix", tmpPath)
	}
	os.Remove(tmpPath)
}

func TestWriteTempFileWithoutCommitLeavesOriginalIntactAndTmpIgnored(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	orig := Bucket{Start: base, Span: spanMinute, DownBytes: 100}
	l1, _ := marshalBucketLine(orig)
	origBytes := []byte(string(l1) + "\n")
	if err := os.WriteFile(filepath.Join(dir, minuteFileName), origBytes, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulated crash mid-rewrite: write the temp, but do NOT commit.
	stray := Bucket{Start: base.Add(time.Minute), Span: spanMinute, DownBytes: 999}
	if _, err := writeTempFile(dir, minuteFileName, []Bucket{stray}); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}

	// Original is byte-intact.
	got, err := os.ReadFile(filepath.Join(dir, minuteFileName))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(got) != string(origBytes) {
		t.Errorf("original mutated by an uncommitted rewrite: %q != %q", got, origBytes)
	}

	// The loader ignores the stray *.tmp (reads only the fixed name) and sweeps it.
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()
	min, _ := h.Snapshot()
	if len(min) != 1 || min[0].DownBytes != 100 {
		t.Errorf("loader admitted the uncommitted temp: got %+v", min)
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("stray *.tmp not swept: %v", tmps)
	}
}

func TestCommitChangesInode(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	target := filepath.Join(dir, minuteFileName)

	ino := func() uint64 {
		fi, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no syscall.Stat_t on this platform")
		}
		return st.Ino
	}

	// Initial write via temp+commit.
	tmp1, _ := writeTempFile(dir, minuteFileName, []Bucket{{Start: base, Span: spanMinute, DownBytes: 1}})
	if err := commit(tmp1, target, dir); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	before := ino()

	// A full rewrite must swap the inode (rename-swap, not truncate-in-place).
	tmp2, _ := writeTempFile(dir, minuteFileName, []Bucket{{Start: base, Span: spanMinute, DownBytes: 2}})
	if err := commit(tmp2, target, dir); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	after := ino()
	if before == after {
		t.Errorf("inode unchanged across rewrite (%d) — looks like truncate-in-place, not atomic rename", before)
	}
}

func TestAppendLineOnePhysicalLinePerCall(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := h.appendLine(Bucket{Start: base.Add(time.Duration(i) * time.Minute), Span: spanMinute, DownBytes: uint64(i)}); err != nil {
			t.Fatalf("appendLine %d: %v", i, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, minuteFileName))
	// Exactly 3 newline-terminated lines, each a single parseable object.
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d physical lines, want 3: %q", len(lines), data)
	}
	for i, ln := range lines {
		if _, perr := parseBucketLine([]byte(ln)); perr != nil {
			t.Errorf("line %d not a single valid object (concatenated?): %q: %v", i, ln, perr)
		}
	}
}

func assertOnePhysicalLine(t *testing.T, data []byte) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d physical lines, want 1: %q", len(lines), data)
	}
	if _, err := parseBucketLine([]byte(lines[0])); err != nil {
		t.Errorf("single line not a valid object: %q: %v", lines[0], err)
	}
}

// TestPrunePersistsAtomicRewriteBothFiles exercises promotion: minutes older than
// a (tiny) minuteTTL whose whole hour has expired are promoted into the hour file
// and dropped from the minute file, both rewritten atomically and shrunk on disk.
func TestPrunePromotesAndShrinksFiles(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	// Seed an OLD hour's worth of minutes (2 minutes is enough) directly on disk
	// and in memory, plus a recent minute that must be kept.
	now := time.Now()
	oldHour := now.Add(-3 * time.Hour).Truncate(time.Hour)
	oldMins := []Bucket{
		{Start: oldHour, Span: spanMinute, DownBytes: 100, Samples: 60},
		{Start: oldHour.Add(time.Minute), Span: spanMinute, DownBytes: 200, Samples: 60},
	}
	recent := Bucket{Start: now.Truncate(time.Minute), Span: spanMinute, DownBytes: 50, Samples: 10}
	for _, b := range append(append([]Bucket(nil), oldMins...), recent) {
		if err := h.appendLine(b); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	h.dataMu.Lock()
	h.minutes = append(append([]Bucket(nil), oldMins...), recent)
	h.dataMu.Unlock()

	minBefore, _ := os.ReadFile(filepath.Join(dir, minuteFileName))

	// Prune with a 1h minute TTL: the old hour (3h ago) fully expired => promote;
	// recent minute kept. Hour TTL generous.
	if err := h.prune(now, time.Hour, defaultHourTTL); err != nil {
		t.Fatalf("prune: %v", err)
	}

	min, hr := h.Snapshot()
	if len(min) != 1 || !min[0].Start.Equal(recent.Start) {
		t.Fatalf("after prune, minutes = %+v, want only the recent one", min)
	}
	// The promoted hour is in promotedHours (persisted) — its bytes sum the old mins.
	var promotedHour *Bucket
	for i := range hr {
		if hr[i].Start.Equal(bucketStart(oldHour, spanHour)) {
			promotedHour = &hr[i]
		}
	}
	if promotedHour == nil {
		t.Fatalf("promoted hour missing from snapshot hour view: %+v", hr)
	}
	if promotedHour.DownBytes != 300 {
		t.Errorf("promoted hour DownBytes = %d, want 300 (100+200)", promotedHour.DownBytes)
	}
	// No two hour buckets share a Start.
	seen := map[int64]bool{}
	for _, b := range hr {
		k := b.Start.UnixNano()
		if seen[k] {
			t.Errorf("duplicate hour Start %v (double-count)", b.Start)
		}
		seen[k] = true
	}

	// On-disk minute file shrank (old minutes removed) and hour file now exists.
	minAfter, _ := os.ReadFile(filepath.Join(dir, minuteFileName))
	if len(minAfter) >= len(minBefore) {
		t.Errorf("minute file did not shrink: before=%d after=%d bytes", len(minBefore), len(minAfter))
	}
	hourData, herr := os.ReadFile(filepath.Join(dir, hourFileName))
	if herr != nil || len(hourData) == 0 {
		t.Errorf("hour file not written on promotion: %v len=%d", herr, len(hourData))
	}
	// Promotion persisted: a fresh store sees the promoted hour and only the recent minute.
	h.Close()
	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	min2, hr2 := h2.Snapshot()
	if len(min2) != 1 {
		t.Errorf("reload minutes = %d, want 1", len(min2))
	}
	foundHour := false
	for _, b := range hr2 {
		if b.Start.Equal(bucketStart(oldHour, spanHour)) && b.DownBytes == 300 {
			foundHour = true
		}
	}
	if !foundHour {
		t.Errorf("promoted hour did not survive restart: %+v", hr2)
	}
}

func TestPruneClampGuardDoesNotNukeTier(t *testing.T) {
	// A minuteTTL clamped from a <=0 config must never nuke the whole tier. The
	// clamp lives in clampTTL (tested above); here we assert that prune with the
	// CLAMPED default retains fresh minutes rather than promoting/dropping them.
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()
	now := time.Now()
	recent := Bucket{Start: now.Truncate(time.Minute), Span: spanMinute, DownBytes: 50}
	if err := h.appendLine(recent); err != nil {
		t.Fatalf("append: %v", err)
	}
	h.dataMu.Lock()
	h.minutes = []Bucket{recent}
	h.dataMu.Unlock()

	// clampTTL(0,...) => defaultMinuteTTL (48h); a fresh minute is well within it.
	if err := h.prune(now, clampTTL(0, time.Hour, defaultMinuteTTL), clampTTL(0, 24*time.Hour, defaultHourTTL)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	min, _ := h.Snapshot()
	if len(min) != 1 {
		t.Errorf("clamp-guarded prune nuked the tier: minutes = %d, want 1", len(min))
	}
}

// TestConcurrentIngestSnapshotPrune drives ingest, Snapshot and prune concurrently
// so `go test -race` can prove the locking is sound.
func TestConcurrentIngestSnapshotPrune(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup

	// Ingest a stream crossing many minute boundaries.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var rx uint64
		for i := 0; i < 300; i++ {
			rx += 1000
			// advance ~2s per sample so minutes close frequently
			h.ingest(mkSample(base.Add(time.Duration(i)*2*time.Second), rx, rx/2, 1000, 500, false))
		}
	}()

	// Concurrent snapshots.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			min, hr := h.Snapshot()
			_ = min
			_ = hr
		}
	}()

	// Concurrent prune cycles.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = h.prune(time.Now(), defaultMinuteTTL, defaultHourTTL)
		}
	}()

	wg.Wait()
	// Sanity: final snapshot is coherent (no panic, buckets parse).
	min, _ := h.Snapshot()
	for _, b := range min {
		if b.Span != spanMinute {
			t.Errorf("minute snapshot carries non-minute span %q", b.Span)
		}
	}
}

// --- Phase 4: dominant-band tracking + backward compat ---------------------

// mkBandSample builds a Sample at t with a stamped LinkSnap carrying band (OK=true).
func mkBandSample(t time.Time, rx, tx uint64, band string) Sample {
	s := mkSample(t, rx, tx, 0, 0, false)
	s.Link = &LinkSnap{Band: band, OK: true}
	return s
}

// TestIngestDominantBand drives a minute of mostly-5 GHz samples (plus one 2.4
// GHz) across a close and asserts the CLOSED minute's Band is the dominant "5 GHz"
// — proving the store's openBandCounts tally (not a Bucket map field) feeds
// modeBand at close.
func TestIngestDominantBand(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	h.ingest(mkBandSample(base, 1000, 0, "5 GHz"))                      // seed, 5 GHz
	h.ingest(mkBandSample(base.Add(1*time.Second), 1500, 0, "5 GHz"))   // 5 GHz
	h.ingest(mkBandSample(base.Add(2*time.Second), 2000, 0, "2.4 GHz")) // 2.4 GHz (minority)
	h.ingest(mkBandSample(base.Add(3*time.Second), 2500, 0, "5 GHz"))   // 5 GHz
	h.ingest(mkBandSample(base.Add(1*time.Minute), 3000, 0, "2.4 GHz")) // crosses => closes 12:00

	min, _ := h.Snapshot()
	if len(min) != 1 {
		t.Fatalf("minutes = %d, want 1 closed", len(min))
	}
	if min[0].Band != "5 GHz" {
		t.Errorf("closed minute Band = %q, want 5 GHz (dominant)", min[0].Band)
	}
}

// TestFlushOpenDominantBand asserts FlushOpen finalizes the partial minute's Band.
func TestFlushOpenDominantBand(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	h.ingest(mkBandSample(base, 1000, 0, "6 GHz"))
	h.ingest(mkBandSample(base.Add(1*time.Second), 1500, 0, "6 GHz"))
	if err := h.FlushOpen(); err != nil {
		t.Fatalf("FlushOpen: %v", err)
	}
	h.Close()

	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	min, _ := h2.Snapshot()
	if len(min) != 1 || min[0].Band != "6 GHz" {
		t.Fatalf("flushed partial Band = %+v, want one bucket with Band 6 GHz", min)
	}
}

// TestRollupDominantBand pins the hour-rollup band weighting: 50 minutes of 5 GHz
// and 10 of 2.4 GHz within one hour (weighted by each minute's Samples) => hour
// Band "5 GHz".
func TestRollupDominantBand(t *testing.T) {
	hourBase := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var minutes []Bucket
	for i := 0; i < 50; i++ {
		minutes = append(minutes, Bucket{Start: hourBase.Add(time.Duration(i) * time.Minute), Span: spanMinute, Samples: 60, Band: "5 GHz"})
	}
	for i := 50; i < 60; i++ {
		minutes = append(minutes, Bucket{Start: hourBase.Add(time.Duration(i) * time.Minute), Span: spanMinute, Samples: 60, Band: "2.4 GHz"})
	}
	hrs := rollup(minutes)
	if len(hrs) != 1 {
		t.Fatalf("rollup => %d hours, want 1", len(hrs))
	}
	if hrs[0].Band != "5 GHz" {
		t.Errorf("hour Band = %q, want 5 GHz (Samples-weighted dominant)", hrs[0].Band)
	}
}

// TestBucketizeDominantBand covers the bulk/reload path: a minute of mostly-5 GHz
// samples folds to a bucket with Band "5 GHz".
func TestBucketizeDominantBand(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	samples := []Sample{
		mkBandSample(base, 1000, 0, "5 GHz"),
		mkBandSample(base.Add(1*time.Second), 1500, 0, "5 GHz"),
		mkBandSample(base.Add(2*time.Second), 2000, 0, "2.4 GHz"),
	}
	got := bucketize(samples, spanMinute)
	if len(got) != 1 || got[0].Band != "5 GHz" {
		t.Fatalf("bucketize Band = %+v, want one bucket with Band 5 GHz", got)
	}
}

// TestParseBucketLineBackwardCompat is the headline compat guard: a Phase-3 JSONL
// line with NO "band" field still parses, defaulting Band to "" (no loader change
// needed; omitempty + json-absent-is-zero).
func TestParseBucketLineBackwardCompat(t *testing.T) {
	phase3 := []byte(`{"start":"2026-10-02T12:00:00Z","span":"minute","down_bytes":1000,"up_bytes":500,"down_peak_bps":100,"up_peak_bps":50,"samples":60}`)
	b, err := parseBucketLine(phase3)
	if err != nil {
		t.Fatalf("Phase-3 line (no band) must still parse: %v", err)
	}
	if b.Band != "" {
		t.Errorf("absent band must default to \"\", got %q", b.Band)
	}
	if b.DownBytes != 1000 || b.Samples != 60 {
		t.Errorf("Phase-3 fields lost: %+v", b)
	}

	// And a whole old-format blob loads via the resilient reader.
	buckets := parseJSONLBuckets(phase3)
	if len(buckets) != 1 || buckets[0].Band != "" {
		t.Fatalf("parseJSONLBuckets of a Phase-3 line = %+v, want one bucket Band \"\"", buckets)
	}
}

// ---------------------------------------------------------------------------
// Phase 8 — outage journal store layer (folded into historyStore, seam a).
// Exercised against t.TempDir(); meant to pass under -race.
// ---------------------------------------------------------------------------

// mkStoredOutage builds a valid CLOSED outage record.
func mkStoredOutage(start time.Time, cause, ch string) Outage {
	return Outage{Start: start, End: start.Add(90 * time.Second), DurationSec: 90, Cause: cause, Channel: ch, RSSI: -55}
}

// TestAppendOutageDurableAndSnapshot: appendOutage writes a durable JSONL line AND
// mirrors into OutagesSnapshot; the snapshot is sorted and an independent copy.
func TestAppendOutageDurableAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// Append out of order to prove OutagesSnapshot sorts.
	if err := h.appendOutage(mkStoredOutage(base.Add(time.Hour), causeInternetDown, "157")); err != nil {
		t.Fatalf("appendOutage: %v", err)
	}
	if err := h.appendOutage(mkStoredOutage(base, causeLinkDrop, "149")); err != nil {
		t.Fatalf("appendOutage: %v", err)
	}

	snap := h.OutagesSnapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if !snap[0].Start.Equal(base) || snap[0].Cause != causeLinkDrop {
		t.Errorf("snapshot not sorted ascending by Start: %+v", snap)
	}

	// The durable file carries both lines and reloads via parseJSONLOutages.
	data, rerr := os.ReadFile(filepath.Join(dir, outageFileName))
	if rerr != nil {
		t.Fatalf("read journal: %v", rerr)
	}
	if got := parseJSONLOutages(data); len(got) != 2 {
		t.Fatalf("durable journal has %d lines, want 2", len(got))
	}

	// Snapshot is an independent copy: mutating it must not touch the store.
	snap[0].Channel = "MUTATED"
	if h.OutagesSnapshot()[0].Channel == "MUTATED" {
		t.Error("OutagesSnapshot must return an independent (deep) copy")
	}
}

// TestOutageJournalSurvivesRestart: a closed outage persists across a store reopen,
// sorted by Start (the restart/reload proof).
func TestOutageJournalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	h1, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = h1.appendOutage(mkStoredOutage(base, causeLinkDrop, "149"))
	_ = h1.appendOutage(mkStoredOutage(base.Add(31*time.Minute), causeInternetDown, "149"))
	before := h1.OutagesSnapshot()
	h1.Close()

	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	after := h2.OutagesSnapshot()
	if len(after) != len(before) || len(after) != 2 {
		t.Fatalf("restart lost outages: before=%d after=%d, want 2", len(before), len(after))
	}
	for i := range after {
		if !after[i].Start.Equal(before[i].Start) || after[i].Cause != before[i].Cause || after[i].Channel != before[i].Channel {
			t.Errorf("outage %d changed across restart: %+v vs %+v", i, after[i], before[i])
		}
	}
}

// TestOutageJournalLoaderToleratesCorruptLine: a torn/garbage middle line is skipped
// on load (never aborts), mirroring the bucket loader.
func TestOutageJournalLoaderToleratesCorruptLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	o1 := mkStoredOutage(base, causeLinkDrop, "149")
	o2 := mkStoredOutage(base.Add(time.Hour), causeInternetDown, "157")
	l1, _ := marshalOutageLine(o1)
	l2, _ := marshalOutageLine(o2)
	blob := string(l1) + "\n" + "garbage-middle\n" + "{}\n" + string(l2) + "\n" + `{"start":"2026-10-0`
	if err := os.WriteFile(filepath.Join(dir, outageFileName), []byte(blob), 0o644); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open with corrupt journal must not abort: %v", err)
	}
	defer h.Close()
	if got := h.OutagesSnapshot(); len(got) != 2 {
		t.Fatalf("loaded %d outages, want 2 (intact survive, corrupt skipped)", len(got))
	}
}

// TestOutageJournalOneShotTTLSweep: records older than the TTL are swept ONCE at open
// and the file is rewritten (must_fix #8/#9), while recent records are kept.
func TestOutageJournalOneShotTTLSweep(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldRec := mkStoredOutage(now.Add(-100*24*time.Hour), causeLinkDrop, "149")
	recent := mkStoredOutage(now.Add(-time.Hour), causeInternetDown, "157")
	l1, _ := marshalOutageLine(oldRec)
	l2, _ := marshalOutageLine(recent)
	if err := os.WriteFile(filepath.Join(dir, outageFileName), []byte(string(l1)+"\n"+string(l2)+"\n"), 0o644); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	h, err := openHistoryStoreTTL(dir, 90*24*time.Hour, defaultThrottleSampleTTL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	snap := h.OutagesSnapshot()
	if len(snap) != 1 || !snap[0].Start.Equal(recent.Start) {
		t.Fatalf("in-memory after sweep = %+v, want only the recent record", snap)
	}
	// The on-disk file was rewritten ONCE: it now holds only the surviving record.
	data, _ := os.ReadFile(filepath.Join(dir, outageFileName))
	if got := parseJSONLOutages(data); len(got) != 1 || !got[0].Start.Equal(recent.Start) {
		t.Fatalf("on-disk after sweep = %+v, want only the recent record (file rewritten)", got)
	}
}

// TestWriteOutageTempFileInDir: the Outage temp lands in the TARGET dir with a .tmp
// suffix (atomic rename stays on one volume; the *.tmp glob sweeps it).
func TestWriteOutageTempFileInDir(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	outs := []Outage{mkStoredOutage(base, causeLinkDrop, "149")}
	tmpPath, err := writeOutageTempFile(dir, outageFileName, outs)
	if err != nil {
		t.Fatalf("writeOutageTempFile: %v", err)
	}
	if filepath.Dir(tmpPath) != dir {
		t.Errorf("temp %q not in target dir %q (cross-volume rename risk)", tmpPath, dir)
	}
	if !strings.HasSuffix(tmpPath, ".tmp") {
		t.Errorf("temp %q lacks .tmp suffix", tmpPath)
	}
	os.Remove(tmpPath)
}

// --- Phase 13 speed-samples surface (4th JSONL surface) ---------------------

func mkSpeedSample(t time.Time, mbps float64, ok bool) SpeedSample {
	return SpeedSample{T: t, DownloadMbps: mbps, LatencyMs: 12, OK: ok}
}

// TestAppendSpeedDurableAndSnapshot mirrors TestAppendOutageDurableAndSnapshot: a
// sample is durable (reloads via parseJSONLSpeed), SpeedSnapshot sorts by T and is an
// independent deep copy.
func TestAppendSpeedDurableAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	base := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	// Append out of order to prove SpeedSnapshot sorts by T.
	if err := h.appendSpeed(mkSpeedSample(base.Add(time.Hour), 300, true)); err != nil {
		t.Fatalf("appendSpeed: %v", err)
	}
	if err := h.appendSpeed(mkSpeedSample(base, 800, true)); err != nil {
		t.Fatalf("appendSpeed: %v", err)
	}
	// An OK=false 429 record carries zero Mbps and MUST persist.
	if err := h.appendSpeed(SpeedSample{T: base.Add(2 * time.Hour), OK: false}); err != nil {
		t.Fatalf("appendSpeed(OK=false): %v", err)
	}

	snap := h.SpeedSnapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	if !snap[0].T.Equal(base) || snap[0].DownloadMbps != 800 {
		t.Errorf("snapshot not sorted ascending by T: %+v", snap)
	}
	if snap[2].OK || snap[2].DownloadMbps != 0 {
		t.Errorf("OK=false zero-Mbps record did not persist/sort last: %+v", snap[2])
	}

	data, rerr := os.ReadFile(filepath.Join(dir, speedFileName))
	if rerr != nil {
		t.Fatalf("read speed journal: %v", rerr)
	}
	if got := parseJSONLSpeed(data); len(got) != 3 {
		t.Fatalf("durable speed journal has %d lines, want 3", len(got))
	}

	// Independent copy: mutating the snapshot must not touch the store.
	snap[0].DownloadMbps = -1
	if h.SpeedSnapshot()[0].DownloadMbps == -1 {
		t.Error("SpeedSnapshot must return an independent (deep) copy")
	}
}

// TestSpeedJournalSurvivesRestart: speed samples persist across a store reopen, sorted
// by T (the restart/reload proof that seeds the watchdog's lastAttemptT).
func TestSpeedJournalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	h1, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = h1.appendSpeed(mkSpeedSample(base, 800, true))
	_ = h1.appendSpeed(mkSpeedSample(base.Add(30*time.Minute), 790, true))
	before := h1.SpeedSnapshot()
	h1.Close()

	h2, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	after := h2.SpeedSnapshot()
	if len(after) != len(before) || len(after) != 2 {
		t.Fatalf("restart lost speed samples: before=%d after=%d, want 2", len(before), len(after))
	}
	// The newest T (the watchdog seed) survives.
	if !after[len(after)-1].T.Equal(base.Add(30 * time.Minute)) {
		t.Errorf("newest persisted T not preserved across restart: %+v", after[len(after)-1])
	}
}

// TestSpeedJournalOneShotTTLSweep: samples older than the TTL are swept ONCE at open
// and the file is rewritten, while recent samples (incl. OK=false) are kept.
func TestSpeedJournalOneShotTTLSweep(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := mkSpeedSample(now.Add(-100*24*time.Hour), 500, true)
	recent := SpeedSample{T: now.Add(-time.Hour), OK: false} // recent 429 kept
	l1, _ := marshalSpeedLine(old)
	l2, _ := marshalSpeedLine(recent)
	if err := os.WriteFile(filepath.Join(dir, speedFileName), []byte(string(l1)+"\n"+string(l2)+"\n"), 0o644); err != nil {
		t.Fatalf("seed speed journal: %v", err)
	}

	h, err := openHistoryStoreTTL(dir, defaultOutageTTL, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	snap := h.SpeedSnapshot()
	if len(snap) != 1 || !snap[0].T.Equal(recent.T) {
		t.Fatalf("in-memory after sweep = %+v, want only the recent record", snap)
	}
	data, _ := os.ReadFile(filepath.Join(dir, speedFileName))
	if got := parseJSONLSpeed(data); len(got) != 1 || !got[0].T.Equal(recent.T) {
		t.Fatalf("on-disk after sweep = %+v, want only the recent record (file rewritten)", got)
	}
}

// TestSpeedJournalLoaderToleratesCorruptLine: a torn/garbage middle line is skipped on
// load (never aborts), mirroring the bucket/outage loaders.
func TestSpeedJournalLoaderToleratesCorruptLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	l1, _ := marshalSpeedLine(mkSpeedSample(base, 800, true))
	l2, _ := marshalSpeedLine(mkSpeedSample(base.Add(time.Hour), 300, true))
	blob := string(l1) + "\n" + "garbage-middle\n" + "{}\n" + string(l2) + "\n" + `{"download_mbps":5`
	if err := os.WriteFile(filepath.Join(dir, speedFileName), []byte(blob), 0o644); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	h, err := openHistoryStore(dir)
	if err != nil {
		t.Fatalf("open with corrupt speed journal must not abort: %v", err)
	}
	defer h.Close()
	if got := h.SpeedSnapshot(); len(got) != 2 {
		t.Fatalf("loaded %d speed samples, want 2 (intact survive, corrupt skipped)", len(got))
	}
}
