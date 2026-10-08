package main

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestMeasureStreamJoinsWorkers is the root-cause regression for the on-demand speed
// button: measureStream used to return the instant the window closed, WITHOUT waiting
// for its worker goroutines to finish. The button's ok-aware upload worker records its
// outcome only when its long streaming request returns — which is AFTER the window-end
// cancel — so onDemandSpeed read the tally while it was still empty and reported a bogus
// failure on EVERY run. This test proves measureStream now JOINS its workers: a worker
// that records only after it observes ctx cancellation MUST have recorded by the time
// measureStream returns.
func TestMeasureStreamJoinsWorkers(t *testing.T) {
	var recorded int64
	// This worker mimics the real upload worker's shape: it blocks (as if mid-request)
	// until the window-end cancel, then records its outcome — exactly the ordering that
	// exposed the missing join.
	worker := func(ctx context.Context, _ *http.Client, _ string, _ *int64) {
		<-ctx.Done()                      // "request" unblocks only when the window closes
		time.Sleep(10 * time.Millisecond) // a touch of unwind latency, as a real Do has
		atomic.AddInt64(&recorded, 1)
	}
	// seconds=0 so the measurement window is just the 1s warm-up — keeps the test quick
	// while still exercising the full cancel+join path.
	measureStream(context.Background(), nil, 3, 0, worker, "http://unused.invalid/")
	if got := atomic.LoadInt64(&recorded); got != 3 {
		t.Fatalf("measureStream returned before its 3 workers finished: recorded=%d, want 3 (workers were not joined)", got)
	}
}

func TestMbps(t *testing.T) {
	// 100 MB in 8 s = 100e6*8 bits / 8 s = 100 Mbit/s... check: bytes*8/1e6/sec
	if got := mbps(12_500_000, 1.0); math.Abs(got-100) > 0.01 {
		t.Errorf("mbps(12.5MB,1s) = %.3f, want 100", got)
	}
	if got := mbps(0, 5); got != 0 {
		t.Errorf("mbps(0,5) = %v, want 0", got)
	}
	if got := mbps(1000, 0); got != 0 {
		t.Errorf("mbps(x,0) = %v, want 0 (guard against div-by-zero)", got)
	}
}

func TestSpeedStats(t *testing.T) {
	avg, min, max, jitter := speedStats([]float64{10, 20, 30})
	if avg != 20 {
		t.Errorf("avg = %v, want 20", avg)
	}
	if min != 10 || max != 30 {
		t.Errorf("min/max = %v/%v, want 10/30", min, max)
	}
	// stddev of {10,20,30} (population) = sqrt(200/3) ≈ 8.165
	if math.Abs(jitter-8.165) > 0.01 {
		t.Errorf("jitter = %.3f, want ~8.165", jitter)
	}
	if a, _, _, j := speedStats(nil); a != 0 || j != 0 {
		t.Errorf("speedStats(nil) should be zero-valued, got avg=%v jitter=%v", a, j)
	}
}

func TestCFServerTimingRTT(t *testing.T) {
	const cfL4 = `cfL4;desc="?proto=TCP&rtt=30067&min_rtt=20062&rtt_var=14670&sent=6"`
	tests := []struct {
		name   string
		values []string
		field  string
		wantMs float64
		wantOK bool
	}{
		{"rtt field", []string{cfL4}, "rtt", 30.067, true},
		{"min_rtt not matched by rtt boundary", []string{cfL4}, "min_rtt", 20.062, true},
		{"missing field", []string{cfL4}, "cwnd", 0, false},
		{"other header ignored, cfL4 found", []string{"cfSpeedWorker;dur=30", cfL4}, "rtt", 30.067, true},
		{"no header", nil, "rtt", 0, false},
		{"malformed value", []string{`cfL4;desc="?rtt=abc"`}, "rtt", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cfServerTimingRTT(tc.values, tc.field)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && math.Abs(got-tc.wantMs) > 0.001 {
				t.Errorf("rtt = %v ms, want %v ms", got, tc.wantMs)
			}
		})
	}
}
