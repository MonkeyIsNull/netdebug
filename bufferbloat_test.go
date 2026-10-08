package main

import (
	"math"
	"strings"
	"testing"
)

// TestGradeBufferbloat pins the A..F ladder with LITERAL deltas (never relying on a
// subtraction landing on a boundary), every strict-'<' boundary, the negative-delta
// clamp, and — the headline regression — the guard-order proof that -Inf is "F", not
// "A".
func TestGradeBufferbloat(t *testing.T) {
	tests := []struct {
		name  string
		delta float64
		want  string
	}{
		// interior buckets
		{"2=>A", 2, "A"},
		{"20=>B", 20, "B"},
		{"45=>C", 45, "C"},
		{"80=>D", 80, "D"},
		{"150=>E", 150, "E"},
		{"400=>F", 400, "F"},
		// lower edge of A
		{"0=>A", 0, "A"},
		// BOUNDARIES: strict '<' => the WORSE grade
		{"5=>B", 5, "B"},
		{"30=>C", 30, "C"},
		{"60=>D", 60, "D"},
		{"100=>E", 100, "E"},
		{"200=>F", 200, "F"},
		// negative noise clamps to A (no panic)
		{"-20=>A", -20, "A"},
		// NON-FINITE guard-order proof (the -Inf case must NOT be "A")
		{"NaN=>F", math.NaN(), "F"},
		{"+Inf=>F", math.Inf(1), "F"},
		{"-Inf=>F", math.Inf(-1), "F"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gradeBufferbloat(tc.delta); got != tc.want {
				t.Errorf("gradeBufferbloat(%v) = %q, want %q", tc.delta, got, tc.want)
			}
		})
	}
}

// TestWorseGrade pins the A..F worse-of ordering AND the unknown-skip rule (an ""
// direction is skipped, never ranked as best).
func TestWorseGrade(t *testing.T) {
	tests := []struct {
		a, b, want string
	}{
		{"A", "F", "F"},
		{"C", "B", "C"},
		{"B", "B", "B"},
		{"F", "A", "F"},
		// unknown-skip
		{"", "B", "B"},
		{"A", "", "A"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := worseGrade(tc.a, tc.b); got != tc.want {
			t.Errorf("worseGrade(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestBuildBloatResult pins the validity-gated assembly, especially the two
// regressions that a bare-float signature would silently grade "A": a FAILED upload
// phase and a FAILED idle baseline must both degrade to "unknown", never a bogus fast
// grade.
func TestBuildBloatResult(t *testing.T) {
	t.Run("valid both", func(t *testing.T) {
		r := buildBloatResult(20, true, 180, true, 40, true, 12.5, 300)
		if r.IdleRTTMs != 20 || r.UpLoadedRTTMs != 180 || r.DownLoadedRTTMs != 40 {
			t.Fatalf("RTTs wrong: %+v", r)
		}
		if r.UpDeltaMs != 160 || r.DownDeltaMs != 20 {
			t.Fatalf("deltas wrong: up=%v down=%v", r.UpDeltaMs, r.DownDeltaMs)
		}
		if r.UpGrade != "E" || r.DownGrade != "B" || r.Grade != "E" {
			t.Fatalf("grades wrong: up=%q down=%q overall=%q, want E/B/E", r.UpGrade, r.DownGrade, r.Grade)
		}
		if r.UploadMbps != 12.5 || r.DownloadMbps != 300 {
			t.Fatalf("mbps not passed through: %+v", r)
		}
	})

	t.Run("failed upload phase => unknown, not A", func(t *testing.T) {
		// idle valid, upload phase dead (upOK=false, loaded 0), download valid.
		r := buildBloatResult(20, true, 0, false, 40, true, 0, 300)
		if r.UpGrade != gradeUnknown {
			t.Errorf("failed upload must be unknown, got %q (a bare-float sig would grade 0-20=-20 => A)", r.UpGrade)
		}
		if r.UpDeltaMs != 0 { // omitted; never computed against a dead phase
			t.Errorf("failed upload must carry no delta, got %v", r.UpDeltaMs)
		}
		if r.DownGrade != "B" {
			t.Errorf("download grade = %q, want B", r.DownGrade)
		}
		if r.Grade != "B" { // worseGrade("", "B") = the surviving direction
			t.Errorf("overall = %q, want B (the surviving direction, NEVER A)", r.Grade)
		}
	})

	t.Run("failed idle baseline => whole test unknown", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			idle   float64
			idleOK bool
		}{
			{"idleOK false", 20, false},
			{"idle zero", 0, true},
		} {
			r := buildBloatResult(c.idle, c.idleOK, 180, true, 40, true, 12.5, 300)
			if r.UpGrade != gradeUnknown || r.DownGrade != gradeUnknown || r.Grade != gradeUnknown {
				t.Errorf("%s: grades must all be unknown, got up=%q down=%q overall=%q", c.name, r.UpGrade, r.DownGrade, r.Grade)
			}
			if r.UpDeltaMs != 0 || r.DownDeltaMs != 0 || r.IdleRTTMs != 0 {
				t.Errorf("%s: no deltas/idle without a valid baseline, got %+v", c.name, r)
			}
		}
	})

	t.Run("both directions unknown => overall unknown", func(t *testing.T) {
		r := buildBloatResult(20, true, 0, false, 0, false, 0, 0)
		if r.Grade != gradeUnknown {
			t.Errorf("both unknown => overall unknown, got %q", r.Grade)
		}
	})
}

// TestMeetingReady pins the GOOD/RISKY/BAD boundaries, the worst-dimension rule, the
// internet-down case, and the NaN=>BAD safe fall-through. For non-GOOD it also
// asserts the reason names the DOMINATING dimension.
func TestMeetingReady(t *testing.T) {
	tests := []struct {
		name       string
		jitter     float64
		loss       float64
		wantStatus string
		wantReason string // substring (lowercased) the reason must contain; "" => skip
	}{
		{"good", 8, 0.2, meetGood, "within"},
		{"risky via jitter", 35, 0.5, meetRisky, "jitter"},
		{"risky via loss (worst-dim)", 8, 3, meetRisky, "loss"},
		{"bad via jitter", 80, 0, meetBad, "jitter"},
		{"bad via loss", 5, 12, meetBad, "loss"},
		// BOUNDARY OUTCOMES (strict '<')
		{"jitter exactly 20 => RISKY", 20, 0, meetRisky, "jitter"},
		{"jitter exactly 50 => BAD", 50, 0, meetBad, "jitter"},
		{"loss exactly 1 => RISKY", 8, 1, meetRisky, "loss"},
		{"loss exactly 5 => BAD", 8, 5, meetBad, "loss"},
		// INTERNET DOWN (selectInternet all-failed => InternetLoss 100, JitterMs 0)
		{"internet down", 0, 100, meetBad, "loss"},
		// DEGENERATE: NaN => BAD (strict '<' => NaN<x false => falls through)
		{"NaN jitter => BAD", math.NaN(), 0, meetBad, "jitter"},
		{"NaN loss => BAD", 8, math.NaN(), meetBad, "loss"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := meetingReady(tc.jitter, tc.loss)
			if status != tc.wantStatus {
				t.Errorf("meetingReady(%v,%v) status = %q, want %q", tc.jitter, tc.loss, status, tc.wantStatus)
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(reason), tc.wantReason) {
				t.Errorf("meetingReady(%v,%v) reason = %q, want to contain %q", tc.jitter, tc.loss, reason, tc.wantReason)
			}
		})
	}
}
