package main

// Phase 8 — pure-core table tests for the outage journal. Every function in outage.go
// is pure, so these are exhaustive and deterministic (no IO, no real clock). The
// impure store layer (appendOutage / OutagesSnapshot / at-open load+sweep) is tested
// in history_test.go; the /outages.json wire + opt-in guard in serve_test.go.

import (
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

var outBase = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// tAt returns outBase + sec seconds (monotonic-free wall clock, like a round-tripped
// Obs.T).
func tAt(sec int) time.Time { return outBase.Add(time.Duration(sec) * time.Second) }

// obs builds an Obs with a blank Link (the caller sets LinkOK explicitly — the
// fail-open bit the detector computes).
func obs(t time.Time, linkOK, gwOK, inetOK bool) Obs {
	return Obs{T: t, LinkOK: linkOK, GatewayOK: gwOK, InternetOK: inetOK}
}

// obsL is obs with a raw Link snapshot attached.
func obsL(t time.Time, linkOK, gwOK, inetOK bool, link LinkSnap) Obs {
	o := obs(t, linkOK, gwOK, inetOK)
	o.Link = link
	return o
}

// runSeq folds a sequence of Obs through stepOutage, returning the final state and
// every closed Outage.
func runSeq(startThresh, endThresh int, seq []Obs) (outageState, []Outage) {
	st := outageState{}
	var closed []Outage
	for _, o := range seq {
		var out Outage
		var ok bool
		st, out, ok = stepOutage(st, o, startThresh, endThresh)
		if ok {
			closed = append(closed, out)
		}
	}
	return st, closed
}

func approxOut(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// --- classifyCause ---------------------------------------------------------

func TestClassifyCause(t *testing.T) {
	cases := []struct {
		name                 string
		linkOK, gwOK, inetOK bool
		want                 string
	}{
		{"link dominates even when gw+inet ok", false, true, true, causeLinkDrop},
		{"link down with everything down", false, false, false, causeLinkDrop},
		{"gateway unreachable (down class)", true, false, false, causeGatewayUnrea},
		{"internet down (gateway_only mapping)", true, true, false, causeInternetDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyCause(c.linkOK, c.gwOK, c.inetOK); got != c.want {
				t.Errorf("classifyCause(%v,%v,%v) = %q, want %q", c.linkOK, c.gwOK, c.inetOK, got, c.want)
			}
		})
	}
}

func TestBad(t *testing.T) {
	if bad(obs(tAt(0), true, true, true)) {
		t.Error("healthy obs must not be bad")
	}
	if !bad(obs(tAt(0), false, true, true)) {
		t.Error("link down must be bad (even with internet up)")
	}
	if !bad(obs(tAt(0), true, true, false)) {
		t.Error("internet down must be bad")
	}
	// bad() ignores GatewayOK: a router that drops ICMP to itself but forwards fine
	// (gwOK false, inetOK true, linkOK true) is NOT an outage.
	if bad(obs(tAt(0), true, false, true)) {
		t.Error("gateway-only ICMP drop with internet up must NOT be bad")
	}
}

// --- stepOutage (the core) -------------------------------------------------

func TestStepOutageSingleBadDoesNotOpen(t *testing.T) {
	st, closed := runSeq(2, 2, []Obs{obs(tAt(0), true, true, false)})
	if st.open {
		t.Error("single bad obs must not open (startThresh 2)")
	}
	if len(closed) != 0 {
		t.Errorf("closed=%d, want 0", len(closed))
	}
}

func TestStepOutageTwoBadOpens(t *testing.T) {
	st, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false),
		obs(tAt(5), true, true, false),
	})
	if !st.open {
		t.Fatal("two consecutive bad must open")
	}
	if !st.pending.Start.Equal(tAt(0)) {
		t.Errorf("pending.Start = %v, want first bad T %v", st.pending.Start, tAt(0))
	}
	if st.pending.Cause != causeInternetDown {
		t.Errorf("cause = %q, want %q", st.pending.Cause, causeInternetDown)
	}
	if len(closed) != 0 {
		t.Errorf("no outage should close yet, got %d", len(closed))
	}
}

// must_fix #1: End is the FIRST good obs, not the closing obs.
func TestStepOutageBackdatedEndSimple(t *testing.T) {
	_, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false), // bad1
		obs(tAt(5), true, true, false), // bad2 -> open
		obs(tAt(10), true, true, true), // good1 -> End stamp
		obs(tAt(15), true, true, true), // good2 -> close
	})
	if len(closed) != 1 {
		t.Fatalf("closed=%d, want 1", len(closed))
	}
	o := closed[0]
	if !o.Start.Equal(tAt(0)) || !o.End.Equal(tAt(10)) {
		t.Errorf("Start/End = %v/%v, want %v/%v", o.Start, o.End, tAt(0), tAt(10))
	}
	if !approxOut(o.DurationSec, 10) {
		t.Errorf("DurationSec = %v, want 10 (End-Start = good1-bad1)", o.DurationSec)
	}
}

// must_fix #1 symmetric case (REQUIRED): bad,bad,good,bad,good,good => End is the good
// of the SECOND good run.
func TestStepOutageBackdatedEndSymmetric(t *testing.T) {
	_, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false),  // bad1
		obs(tAt(5), true, true, false),  // bad2 -> open
		obs(tAt(10), true, true, true),  // good1 -> End=10
		obs(tAt(15), true, true, false), // bad -> goodStreak reset
		obs(tAt(20), true, true, true),  // good1 of 2nd run -> End re-stamp=20
		obs(tAt(25), true, true, true),  // good2 -> close
	})
	if len(closed) != 1 {
		t.Fatalf("closed=%d, want 1", len(closed))
	}
	o := closed[0]
	if !o.End.Equal(tAt(20)) {
		t.Errorf("End = %v, want %v (good of the SECOND good run)", o.End, tAt(20))
	}
	if !approxOut(o.DurationSec, 20) {
		t.Errorf("DurationSec = %v, want 20 (= End(20)-Start(0))", o.DurationSec)
	}
}

// must_fix #3 order: startThresh=1/endThresh=1 records onset AND opens on the FIRST bad.
func TestStepOutageStart1End1(t *testing.T) {
	st, closed := runSeq(1, 1, []Obs{obs(tAt(0), true, true, false)})
	if !st.open {
		t.Fatal("startThresh 1 must open on the first bad obs")
	}
	if !st.pending.Start.Equal(tAt(0)) || st.pending.Cause != causeInternetDown {
		t.Errorf("onset not recorded on the opening obs: %+v", st.pending)
	}
	if len(closed) != 0 {
		t.Errorf("closed=%d, want 0 (no good yet)", len(closed))
	}
	// Then a single good obs closes it at endThresh 1.
	st, out, ok := stepOutage(st, obs(tAt(5), true, true, true), 1, 1)
	if !ok {
		t.Fatal("endThresh 1 must close on the first good obs")
	}
	if !out.Start.Equal(tAt(0)) || !out.End.Equal(tAt(5)) || !approxOut(out.DurationSec, 5) {
		t.Errorf("closed outage = %+v, want Start 0 / End 5 / Dur 5", out)
	}
	if st.open {
		t.Error("state must reset to not-open after close")
	}
}

// good-in-the-middle resets badStreak; the outage opens on the LATER 2-bad run and
// Start is the FIRST bad of the streak that crossed threshold.
func TestStepOutageGoodInMiddleResetsBadStreak(t *testing.T) {
	st, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false),  // bad1 (streak 1)
		obs(tAt(5), true, true, true),   // good -> badStreak reset
		obs(tAt(10), true, true, false), // bad1 of real run -> Start=10
		obs(tAt(15), true, true, false), // bad2 -> open
	})
	if !st.open {
		t.Fatal("must open on the later 2-bad run")
	}
	if !st.pending.Start.Equal(tAt(10)) {
		t.Errorf("Start = %v, want %v (first bad of the crossing streak)", st.pending.Start, tAt(10))
	}
	if len(closed) != 0 {
		t.Errorf("closed=%d, want 0", len(closed))
	}
}

func TestStepOutageFlappingNeverOpens(t *testing.T) {
	st, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false),
		obs(tAt(5), true, true, true),
		obs(tAt(10), true, true, false),
		obs(tAt(15), true, true, true),
		obs(tAt(20), true, true, false),
		obs(tAt(25), true, true, true),
	})
	if st.open {
		t.Error("flapping (never 2 consecutive bad) must never open")
	}
	if len(closed) != 0 {
		t.Errorf("closed=%d, want 0", len(closed))
	}
}

// Onset cause is immutable: it does NOT mutate when the failure mode changes mid-outage.
func TestStepOutageCauseImmutable(t *testing.T) {
	_, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), false, true, true),  // link-drop onset (cause captured)
		obs(tAt(5), true, false, false), // now gateway-unreachable -> open, but cause stays
		obs(tAt(10), true, true, true),  // good
		obs(tAt(15), true, true, true),  // close
	})
	if len(closed) != 1 {
		t.Fatalf("closed=%d, want 1", len(closed))
	}
	if closed[0].Cause != causeLinkDrop {
		t.Errorf("cause = %q, want %q (ONSET cause, immutable)", closed[0].Cause, causeLinkDrop)
	}
}

// must_fix #3 (REQUIRED): two sequential outages — the SECOND record's Start/Cause/
// radio context are its OWN, not inherited from the first.
func TestStepOutageTwoSequential(t *testing.T) {
	link149 := LinkSnap{OK: true, Band: "5 GHz", Channel: "149", RSSI: -55}
	link157 := LinkSnap{OK: true, Band: "5 GHz", Channel: "157", RSSI: -60}
	_, closed := runSeq(2, 2, []Obs{
		obsL(tAt(0), true, true, true, link149),      // good on 149 -> lastGoodLink=149
		obsL(tAt(5), true, true, false, link149),     // bad1 internet-down, Start=5, ctx 149
		obsL(tAt(10), true, true, false, link149),    // bad2 -> open A
		obsL(tAt(15), true, true, true, link149),     // good1 -> End=15
		obsL(tAt(20), true, true, true, link149),     // good2 -> close A
		obsL(tAt(25), true, true, true, link157),     // good on 157 -> lastGoodLink=157
		obsL(tAt(30), false, true, true, LinkSnap{}), // bad1 link-drop, Start=30, ctx 157
		obsL(tAt(35), false, true, true, LinkSnap{}), // bad2 -> open B
		obsL(tAt(40), true, true, true, link157),     // good1 -> End=40
		obsL(tAt(45), true, true, true, link157),     // good2 -> close B
	})
	if len(closed) != 2 {
		t.Fatalf("closed=%d, want 2", len(closed))
	}
	a, b := closed[0], closed[1]
	if !a.Start.Equal(tAt(5)) || a.Cause != causeInternetDown || a.Channel != "149" {
		t.Errorf("outage A = %+v, want Start 5 / internet-down / ch 149", a)
	}
	if !b.Start.Equal(tAt(30)) {
		t.Errorf("outage B Start = %v, want %v (its OWN onset)", b.Start, tAt(30))
	}
	if b.Cause != causeLinkDrop {
		t.Errorf("outage B cause = %q, want %q (its OWN)", b.Cause, causeLinkDrop)
	}
	if b.Channel != "157" {
		t.Errorf("outage B channel = %q, want 157 (its OWN last-known-good, not A's 149)", b.Channel)
	}
}

// must_fix #2 (REQUIRED): fail-open warm-up. LinkOK is supplied true by the caller when
// the link holder is nil, so a warm-up window never fabricates a link-drop.
func TestStepOutageFailOpenWarmup(t *testing.T) {
	// nil-link + internet OK => good obs, zero outages.
	st, closed := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, true),
		obs(tAt(5), true, true, true),
	})
	if st.open || len(closed) != 0 {
		t.Errorf("fail-open healthy warm-up opened/closed an outage: open=%v closed=%d", st.open, len(closed))
	}

	// nil-link + internet down => opens internet-down (gwOK true), NEVER link-drop.
	st2, _ := runSeq(2, 2, []Obs{
		obs(tAt(0), true, true, false),
		obs(tAt(5), true, true, false),
	})
	if !st2.open || st2.pending.Cause != causeInternetDown {
		t.Errorf("warm-up internet-down case: open=%v cause=%q, want open internet-down", st2.open, st2.pending.Cause)
	}

	// nil-link + gateway down => gateway-unreachable, NEVER link-drop.
	st3, _ := runSeq(2, 2, []Obs{
		obs(tAt(0), true, false, false),
		obs(tAt(5), true, false, false),
	})
	if !st3.open || st3.pending.Cause != causeGatewayUnrea {
		t.Errorf("warm-up gateway case: open=%v cause=%q, want open gateway-unreachable", st3.open, st3.pending.Cause)
	}
}

// must_fix #4 (REQUIRED): onset radio context comes from the LAST-KNOWN-GOOD link, not
// the blank onset snap of a link-drop.
func TestStepOutageLastKnownGoodContext(t *testing.T) {
	good := LinkSnap{OK: true, Band: "5 GHz", Channel: "149", RSSI: -55, Noise: -90, SNR: 35, SSID: "Home"}
	_, closed := runSeq(1, 1, []Obs{
		obsL(tAt(0), true, true, true, good),        // good on 149 -> lastGoodLink
		obsL(tAt(5), false, true, true, LinkSnap{}), // link-drop, BLANK onset snap -> open
		obsL(tAt(10), true, true, true, good),       // good -> close
	})
	if len(closed) != 1 {
		t.Fatalf("closed=%d, want 1", len(closed))
	}
	o := closed[0]
	if o.Channel != "149" {
		t.Errorf("Channel = %q, want 149 (from last-known-good, not the blank onset snap)", o.Channel)
	}
	if o.Band != "5 GHz" || o.RSSI != -55 || o.SSID != "Home" {
		t.Errorf("onset context not from last-known-good: %+v", o)
	}
}

// Onset-context fallback: if NO good link was ever seen, the onset snap is used.
func TestStepOutageContextFallbackToOnsetSnap(t *testing.T) {
	// internet-down with the current (associated) link present and no prior good.
	assoc := LinkSnap{OK: true, Band: "2.4 GHz", Channel: "6", RSSI: -70}
	st, _ := runSeq(1, 2, []Obs{
		obsL(tAt(0), true, true, false, assoc), // bad onset -> open (startThresh 1)
	})
	if st.pending.Channel != "6" || st.pending.Band != "2.4 GHz" {
		t.Errorf("fallback onset context = %+v, want ch 6 / 2.4 GHz from the onset snap", st.pending)
	}
}

// must_fix #5 (REQUIRED): a pause (stepReset) while OPEN resets, does NOT close across
// the gap — no record, state zeroed.
func TestStepOutageGatedPauseResets(t *testing.T) {
	st := outageState{}
	st, _, _ = stepOutage(st, obs(tAt(0), true, true, false), 2, 2) // bad1
	st, _, _ = stepOutage(st, obs(tAt(5), true, true, false), 2, 2) // bad2 -> open
	if !st.open {
		t.Fatal("precondition: outage must be open")
	}
	st = stepReset(st) // battery / screen-lock pause
	if st.open || st.badStreak != 0 || st.goodStreak != 0 || !st.pending.Start.IsZero() {
		t.Errorf("stepReset must zero the machine: %+v", st)
	}
	// Good obs after the pause must NOT close anything (the open outage was discarded).
	var out Outage
	var ok bool
	st, out, ok = stepOutage(st, obs(tAt(600), true, true, true), 2, 2)
	if ok {
		t.Errorf("a good obs after a pause-reset must not close an outage: %+v", out)
	}
	if st.open {
		t.Error("state must stay zeroed after the pause")
	}
}

// stepReset keeps lastGoodLink (a pause does not invalidate the last channel seen).
func TestStepResetKeepsLastGoodLink(t *testing.T) {
	st := outageState{lastGoodLink: LinkSnap{OK: true, Channel: "149"}}
	st.open = true
	st.badStreak = 2
	got := stepReset(st)
	if got.open || got.badStreak != 0 {
		t.Errorf("stepReset did not zero open/streak: %+v", got)
	}
	if got.lastGoodLink.Channel != "149" {
		t.Errorf("stepReset dropped lastGoodLink, want ch 149, got %q", got.lastGoodLink.Channel)
	}
}

// Long gap / sleep-wake between obs (no pause reset) => DurationSec is wall-clock
// End-Start; no synthesized intermediate outages.
func TestStepOutageLongGapWallClockDuration(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	bad1 := start
	bad2 := start.Add(5 * time.Second)
	good1 := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC) // ~2h30m later (sleep-wake)
	good2 := good1.Add(5 * time.Second)
	_, closed := runSeq(2, 2, []Obs{
		obs(bad1, true, true, false),
		obs(bad2, true, true, false),
		obs(good1, true, true, true),
		obs(good2, true, true, true),
	})
	if len(closed) != 1 {
		t.Fatalf("closed=%d, want 1 (no synthesized outages across the gap)", len(closed))
	}
	wantSec := good1.Sub(bad1).Seconds() // 2h30m05s = 9005s
	if !approxOut(closed[0].DurationSec, wantSec) {
		t.Errorf("DurationSec = %v, want %v (wall-clock End-Start)", closed[0].DurationSec, wantSec)
	}
}

// --- clampOutageThreshold --------------------------------------------------

func TestClampOutageThreshold(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 2}, {-3, 2}, {1, 1}, {5, 5},
	}
	for _, c := range cases {
		if got := clampOutageThreshold(c.in); got != c.want {
			t.Errorf("clampOutageThreshold(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- outageCadence ---------------------------------------------------------

// mkOutage builds a closed outage at a local Start with a channel.
func mkOutage(start time.Time, ch string) Outage {
	return Outage{Start: start, End: start.Add(30 * time.Second), DurationSec: 30, Cause: causeInternetDown, Channel: ch}
}

func TestOutageCadence31MinutePattern(t *testing.T) {
	day := time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local)
	outs := []Outage{
		mkOutage(day, "149"),                     // :00
		mkOutage(day.Add(31*time.Minute), "149"), // :31
		mkOutage(day.Add(62*time.Minute), "149"), // :62 (10:02)
	}
	now := day.Add(62 * time.Minute) // now AT the last start — LastGap must still be 31, not 0
	c := outageCadence(outs, now)
	if c.CountToday != 3 {
		t.Errorf("CountToday = %d, want 3", c.CountToday)
	}
	if !approxOut(c.MeanGapMin, 31) || !approxOut(c.MedianGapMin, 31) || !approxOut(c.LastGapMin, 31) {
		t.Errorf("gaps = mean %v / median %v / last %v, want ~31 each", c.MeanGapMin, c.MedianGapMin, c.LastGapMin)
	}
	if c.ModeChannel != "149" {
		t.Errorf("ModeChannel = %q, want 149", c.ModeChannel)
	}
}

// Distinct EVEN-count gaps {10,40} => Mean 25, Median 25 (a wrong gaps[len/2] impl
// would give 40).
func TestOutageCadenceEvenMedian(t *testing.T) {
	day := time.Date(2026, 10, 5, 8, 0, 0, 0, time.Local)
	outs := []Outage{
		mkOutage(day, "36"),
		mkOutage(day.Add(10*time.Minute), "36"),
		mkOutage(day.Add(50*time.Minute), "36"), // gaps: 10, 40
	}
	c := outageCadence(outs, day.Add(time.Hour))
	if !approxOut(c.MeanGapMin, 25) || !approxOut(c.MedianGapMin, 25) {
		t.Errorf("mean/median = %v/%v, want 25/25", c.MeanGapMin, c.MedianGapMin)
	}
	if !approxOut(c.LastGapMin, 40) {
		t.Errorf("LastGapMin = %v, want 40", c.LastGapMin)
	}
}

// Distinct ODD-count gaps {10,20,60} => Median 20, Mean 30.
func TestOutageCadenceOddMedian(t *testing.T) {
	day := time.Date(2026, 10, 5, 8, 0, 0, 0, time.Local)
	outs := []Outage{
		mkOutage(day, "36"),
		mkOutage(day.Add(10*time.Minute), "36"),
		mkOutage(day.Add(30*time.Minute), "36"),
		mkOutage(day.Add(90*time.Minute), "36"), // gaps: 10, 20, 60
	}
	c := outageCadence(outs, day.Add(2*time.Hour))
	if !approxOut(c.MedianGapMin, 20) || !approxOut(c.MeanGapMin, 30) {
		t.Errorf("median/mean = %v/%v, want 20/30", c.MedianGapMin, c.MeanGapMin)
	}
}

// Outages spanning two calendar days: only TODAY counts (an overnight gap does not
// pollute the mean).
func TestOutageCadenceTwoDays(t *testing.T) {
	today := time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local)
	yesterday := today.Add(-24 * time.Hour)
	outs := []Outage{
		mkOutage(yesterday, "149"),
		mkOutage(yesterday.Add(20*time.Minute), "149"),
		mkOutage(today, "157"),
		mkOutage(today.Add(31*time.Minute), "157"),
	}
	c := outageCadence(outs, today.Add(time.Hour))
	if c.CountToday != 2 {
		t.Errorf("CountToday = %d, want 2 (yesterday excluded)", c.CountToday)
	}
	if !approxOut(c.MeanGapMin, 31) {
		t.Errorf("MeanGapMin = %v, want 31 (overnight gap excluded)", c.MeanGapMin)
	}
	if c.ModeChannel != "157" {
		t.Errorf("ModeChannel = %q, want 157 (today's)", c.ModeChannel)
	}
}

// A fixed-offset / UTC-stored reload buckets into the right LOCAL day after
// normalization to time.Local.
func TestOutageCadenceZoneNormalization(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	// Same instant as 09:00 local, but stored in UTC (as a reload would read it back).
	startUTC := time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local).UTC()
	c := outageCadence([]Outage{mkOutage(startUTC, "149")}, now)
	if c.CountToday != 1 {
		t.Errorf("CountToday = %d, want 1 (UTC-stored start must normalize to today local)", c.CountToday)
	}
}

func TestOutageCadenceEmptyAndSingle(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	empty := outageCadence(nil, now)
	if empty.CountToday != 0 || empty.MeanGapMin != 0 || empty.MedianGapMin != 0 || empty.LastGapMin != 0 {
		t.Errorf("empty cadence = %+v, want all zero", empty)
	}
	single := outageCadence([]Outage{mkOutage(now.Add(-time.Hour), "149")}, now)
	if single.CountToday != 1 || single.MeanGapMin != 0 || single.LastGapMin != 0 {
		t.Errorf("single cadence = %+v, want count 1 and zero gaps", single)
	}
	if single.ModeChannel != "149" {
		t.Errorf("single ModeChannel = %q, want 149", single.ModeChannel)
	}
}

// A backward clock step cannot produce a negative gap (clamped >= 0).
func TestOutageCadenceNegativeGapClamped(t *testing.T) {
	day := time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local)
	// Two outages where the "later" slice entry has an EARLIER start is impossible post-
	// sort, but a zero-length gap (same start) must stay >= 0.
	outs := []Outage{mkOutage(day, "149"), mkOutage(day, "149")}
	c := outageCadence(outs, day.Add(time.Hour))
	if c.LastGapMin < 0 || c.MeanGapMin < 0 {
		t.Errorf("gaps must be clamped >= 0, got last %v mean %v", c.LastGapMin, c.MeanGapMin)
	}
}

// --- modeChannel -----------------------------------------------------------

func TestModeChannel(t *testing.T) {
	if got := modeChannel(nil); got != "" {
		t.Errorf("empty => %q, want \"\"", got)
	}
	outs := []Outage{
		{Channel: "149"}, {Channel: "149"}, {Channel: "157"}, {Channel: ""},
	}
	if got := modeChannel(outs); got != "149" {
		t.Errorf("mode = %q, want 149 (most frequent)", got)
	}
	// Tie => lexicographically smallest channel string (deterministic).
	tie := []Outage{{Channel: "157"}, {Channel: "149"}}
	if got := modeChannel(tie); got != "149" {
		t.Errorf("tie mode = %q, want 149 (smallest string tie-break)", got)
	}
	// All empty => "".
	if got := modeChannel([]Outage{{Channel: ""}, {Channel: ""}}); got != "" {
		t.Errorf("all-empty mode = %q, want \"\"", got)
	}
}

// --- JSONL round-trip + rejects --------------------------------------------

func TestMarshalParseOutageLineRoundTrip(t *testing.T) {
	o := Outage{
		Start:       tAt(0),
		End:         tAt(90),
		DurationSec: 90,
		Cause:       causeLinkDrop,
		Band:        "5 GHz",
		Channel:     "149/80",
		RSSI:        -55,
		Noise:       -90,
		SNR:         35,
		SSID:        "Home",
	}
	line, err := marshalOutageLine(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := parseOutageLine(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Start.Equal(o.Start) || !got.End.Equal(o.End) || got.DurationSec != o.DurationSec ||
		got.Cause != o.Cause || got.Band != o.Band || got.Channel != o.Channel ||
		got.RSSI != o.RSSI || got.Noise != o.Noise || got.SNR != o.SNR || got.SSID != o.SSID {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, o)
	}
}

func TestParseOutageLineRejects(t *testing.T) {
	valid := Outage{Start: tAt(0), End: tAt(10), DurationSec: 10, Cause: causeInternetDown}
	mk := func(mut func(*Outage)) []byte {
		o := valid
		mut(&o)
		b, _ := marshalOutageLine(o)
		return b
	}
	cases := []struct {
		name string
		line []byte
	}{
		{"zero start", mk(func(o *Outage) { o.Start = time.Time{} })},
		{"empty cause", mk(func(o *Outage) { o.Cause = "" })},
		{"non-whitelisted cause", mk(func(o *Outage) { o.Cause = "gremlins" })},
		{"zero end", mk(func(o *Outage) { o.End = time.Time{} })},
		{"end before start", mk(func(o *Outage) { o.End = o.Start.Add(-time.Second) })},
		{"negative duration", mk(func(o *Outage) { o.DurationSec = -1 })},
		{"garbage", []byte("not json")},
		{"empty object", []byte("{}")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseOutageLine(c.line); err == nil {
				t.Errorf("parseOutageLine(%s) = nil error, want rejection", c.line)
			}
		})
	}
}

func TestParseJSONLOutagesResilience(t *testing.T) {
	o1 := Outage{Start: tAt(0), End: tAt(10), DurationSec: 10, Cause: causeLinkDrop}
	o2 := Outage{Start: tAt(100), End: tAt(130), DurationSec: 30, Cause: causeInternetDown}
	l1, _ := marshalOutageLine(o1)
	l2, _ := marshalOutageLine(o2)
	blob := string(l1) + "\n" + "garbage-middle\n" + "{}\n" + string(l2) + "\n" + `{"start":"2026-10-0`
	got := parseJSONLOutages([]byte(blob))
	if len(got) != 2 {
		t.Fatalf("parsed %d outages, want 2 (intact survive, torn/garbage skipped)", len(got))
	}
	// Empty blob => empty, non-nil.
	if got := parseJSONLOutages(nil); got == nil || len(got) != 0 {
		t.Errorf("nil blob => %v, want empty non-nil slice", got)
	}
}

// --- pruneOutages ----------------------------------------------------------

func TestPruneOutages(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ttl := 90 * 24 * time.Hour
	recent := Outage{Start: now.Add(-time.Hour), End: now, Cause: causeLinkDrop}
	old := Outage{Start: now.Add(-100 * 24 * time.Hour), End: now.Add(-100 * 24 * time.Hour).Add(time.Minute), Cause: causeLinkDrop}
	got := pruneOutages([]Outage{old, recent}, now, ttl)
	if len(got) != 1 || !got[0].Start.Equal(recent.Start) {
		t.Errorf("prune = %+v, want only the recent outage", got)
	}
	if out := pruneOutages(nil, now, ttl); len(out) != 0 {
		t.Errorf("prune(nil) = %v, want empty", out)
	}
}

func TestSortOutagesByStart(t *testing.T) {
	a := Outage{Start: tAt(30), Cause: causeLinkDrop}
	b := Outage{Start: tAt(0), Cause: causeLinkDrop}
	c := Outage{Start: tAt(15), Cause: causeLinkDrop}
	s := []Outage{a, b, c}
	sortOutagesByStart(s)
	if !s[0].Start.Equal(tAt(0)) || !s[1].Start.Equal(tAt(15)) || !s[2].Start.Equal(tAt(30)) {
		t.Errorf("not sorted ascending by Start: %+v", s)
	}
}

// --- outageJSON (pure wire) ------------------------------------------------

func TestOutageJSONNeverNull(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	b, err := outageJSON(nil, now)
	if err != nil {
		t.Fatalf("outageJSON(nil): %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"outages":[]`) {
		t.Errorf("nil outages did not normalize to []: %s", s)
	}
	if strings.Contains(s, "null") {
		t.Errorf("outageJSON must never emit null: %s", s)
	}
	if !strings.Contains(s, `"cadence"`) || !strings.Contains(s, `"count_today":0`) {
		t.Errorf("cadence object missing/empty: %s", s)
	}
}

func TestOutageJSONSortsAndSummarizes(t *testing.T) {
	day := time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local)
	outs := []Outage{
		mkOutage(day.Add(31*time.Minute), "149"),
		mkOutage(day, "149"), // deliberately out of order
	}
	b, err := outageJSON(outs, day.Add(time.Hour))
	if err != nil {
		t.Fatalf("outageJSON: %v", err)
	}
	s := string(b)
	// Sorted ascending => the 09:00 record's start appears before the 09:31 one.
	i0 := strings.Index(s, day.Format("15:04"))
	i1 := strings.Index(s, day.Add(31*time.Minute).Format("15:04"))
	if i0 < 0 || i1 < 0 || i0 > i1 {
		t.Errorf("outages not sorted ascending by start in the wire: %s", s)
	}
	if !strings.Contains(s, `"count_today":2`) || !strings.Contains(s, `"mode_channel":"149"`) {
		t.Errorf("cadence summary wrong: %s", s)
	}
}
