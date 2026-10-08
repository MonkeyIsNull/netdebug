package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

var alertT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const testCooldown = 300 * time.Second

// seededState returns a fresh zero alertState (nil map) — decideAlerts must handle it
// via copy-on-write.
func zeroState() alertState { return alertState{} }

// ---- decideAlerts: EDGE signals (drop / band_change) ----------------------

func TestDecideAlertsDropEdge(t *testing.T) {
	// 1. NewOutage true, no prior fire => one alertDrop; state records now.
	in := AlertInputs{NewOutage: true, OutageCause: causeLinkDrop, OutageSSID: "HomeNet"}
	fired, st := decideAlerts(in, zeroState(), alertT0, testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertDrop {
		t.Fatalf("first drop: got %v, want one alertDrop", fired)
	}
	if !strings.Contains(fired[0].Body, "HomeNet") || !strings.Contains(fired[0].Body, causeLinkDrop) {
		t.Errorf("drop body = %q, want cause + SSID", fired[0].Body)
	}
	if got := st.lastFired[alertDrop]; !got.Equal(alertT0) {
		t.Errorf("drop stamp = %v, want %v", got, alertT0)
	}

	// 2. same NewOutage true 60s later (within cooldown) => ZERO fired.
	fired2, st2 := decideAlerts(in, st, alertT0.Add(60*time.Second), testCooldown)
	if len(fired2) != 0 {
		t.Errorf("drop within cooldown: got %v, want none", fired2)
	}

	// 3. NewOutage true 400s later (past cooldown) => fires again.
	fired3, _ := decideAlerts(in, st2, alertT0.Add(400*time.Second), testCooldown)
	if len(fired3) != 1 || fired3[0].Kind != alertDrop {
		t.Errorf("drop past cooldown: got %v, want one alertDrop", fired3)
	}
}

func TestDecideAlertsBandChangeEdge(t *testing.T) {
	in := AlertInputs{BandChanged: true, BandFrom: "5 GHz", BandTo: "2.4 GHz"}
	fired, _ := decideAlerts(in, zeroState(), alertT0, testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertBandChange {
		t.Fatalf("band change: got %v, want one alertBandChange", fired)
	}
	if !strings.Contains(fired[0].Body, "5 GHz") || !strings.Contains(fired[0].Body, "2.4 GHz") {
		t.Errorf("band body = %q, want both band names", fired[0].Body)
	}
}

// ---- decideAlerts: LEVEL signals (meeting_bad / high_upload) ---------------

func TestDecideAlertsMeetingBadRisingEdge(t *testing.T) {
	// GOOD -> BAD => one alertMeetingBad.
	_, good := decideAlerts(AlertInputs{MeetingStatus: meetGood}, zeroState(), alertT0, testCooldown)
	fired, _ := decideAlerts(AlertInputs{MeetingStatus: meetBad}, good, alertT0.Add(time.Second), testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertMeetingBad {
		t.Fatalf("GOOD->BAD: got %v, want one alertMeetingBad", fired)
	}
}

func TestDecideAlertsMeetingBadSustainedOnce(t *testing.T) {
	// Stays BAD across 2x cooldown, never recovering => fires EXACTLY ONCE.
	st := zeroState()
	fires := 0
	for i := 0; i < 8; i++ { // 8 * 100s = 800s > 2*300s cooldown
		var f []Alert
		f, st = decideAlerts(AlertInputs{MeetingStatus: meetBad}, st, alertT0.Add(time.Duration(i)*100*time.Second), testCooldown)
		fires += len(f)
	}
	if fires != 1 {
		t.Errorf("sustained BAD fired %d times, want exactly 1 (once-per-episode latch)", fires)
	}
}

func TestDecideAlertsMeetingBadReArmAfterCooldown(t *testing.T) {
	// BAD -> GOOD -> BAD after cooldown => fires again (re-armed + fireable).
	_, st := decideAlerts(AlertInputs{MeetingStatus: meetBad}, zeroState(), alertT0, testCooldown)
	_, st = decideAlerts(AlertInputs{MeetingStatus: meetGood}, st, alertT0.Add(10*time.Second), testCooldown)
	fired, _ := decideAlerts(AlertInputs{MeetingStatus: meetBad}, st, alertT0.Add(400*time.Second), testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertMeetingBad {
		t.Errorf("re-armed BAD past cooldown: got %v, want one alertMeetingBad", fired)
	}
}

func TestDecideAlertsMeetingBadReArmWithinCooldownSuppressed(t *testing.T) {
	// BAD -> GOOD -> BAD WITHIN cooldown => suppressed by cooldown.
	_, st := decideAlerts(AlertInputs{MeetingStatus: meetBad}, zeroState(), alertT0, testCooldown)
	_, st = decideAlerts(AlertInputs{MeetingStatus: meetGood}, st, alertT0.Add(10*time.Second), testCooldown)
	fired, _ := decideAlerts(AlertInputs{MeetingStatus: meetBad}, st, alertT0.Add(60*time.Second), testCooldown)
	if len(fired) != 0 {
		t.Errorf("re-armed BAD within cooldown: got %v, want none (cooldown flap guard)", fired)
	}
}

func TestDecideAlertsMeetingNeverFiresOnGoodRiskyEmpty(t *testing.T) {
	for _, status := range []string{"", meetGood, meetRisky} {
		fired, _ := decideAlerts(AlertInputs{MeetingStatus: status}, zeroState(), alertT0, testCooldown)
		for _, a := range fired {
			if a.Kind == alertMeetingBad {
				t.Errorf("MeetingStatus %q fired meeting_bad, want never", status)
			}
		}
	}
}

func TestDecideAlertsHighUploadRising(t *testing.T) {
	// streak >= thresholdStreak, rising => one alertHighUpload.
	in := AlertInputs{HighUploadStreak: 10, HighUploadThresholdStreak: 10, HighUploadBody: "↑135.0 Mbps sustained"}
	fired, _ := decideAlerts(in, zeroState(), alertT0, testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertHighUpload {
		t.Fatalf("high upload rising: got %v, want one alertHighUpload", fired)
	}
	if fired[0].Body != "↑135.0 Mbps sustained" {
		t.Errorf("high upload body = %q", fired[0].Body)
	}
	// below threshold => none.
	below := AlertInputs{HighUploadStreak: 9, HighUploadThresholdStreak: 10}
	f2, _ := decideAlerts(below, zeroState(), alertT0, testCooldown)
	if len(f2) != 0 {
		t.Errorf("below threshold streak: got %v, want none", f2)
	}
}

func TestDecideAlertsHighUploadSustainedOnce(t *testing.T) {
	st := zeroState()
	fires := 0
	in := AlertInputs{HighUploadStreak: 50, HighUploadThresholdStreak: 10, HighUploadBody: "x"}
	for i := 0; i < 8; i++ {
		var f []Alert
		f, st = decideAlerts(in, st, alertT0.Add(time.Duration(i)*100*time.Second), testCooldown)
		fires += len(f)
	}
	if fires != 1 {
		t.Errorf("sustained high upload fired %d times, want exactly 1", fires)
	}
}

// ---- decideAlerts: multi-condition fixed order -----------------------------

func TestDecideAlertsMultiConditionFixedOrder(t *testing.T) {
	in := AlertInputs{
		NewOutage: true, OutageCause: causeInternetDown,
		BandChanged: true, BandFrom: "5 GHz", BandTo: "6 GHz",
		MeetingStatus:    meetBad,
		HighUploadStreak: 10, HighUploadThresholdStreak: 10, HighUploadBody: "x",
	}
	fired, _ := decideAlerts(in, zeroState(), alertT0, testCooldown)
	want := []string{alertDrop, alertBandChange, alertMeetingBad, alertHighUpload}
	if len(fired) != len(want) {
		t.Fatalf("multi-condition fired %d, want %d: %v", len(fired), len(want), fired)
	}
	for i, k := range want {
		if fired[i].Kind != k {
			t.Errorf("fired[%d].Kind = %q, want %q (fixed order)", i, fired[i].Kind, k)
		}
	}
}

// ---- decideAlerts: copy-on-write immutability ------------------------------

func TestDecideAlertsImmutability(t *testing.T) {
	seeded := alertState{
		lastFired:  map[string]time.Time{alertDrop: alertT0},
		meetingBad: true,
	}
	// A fire that re-stamps drop + flips latches.
	in := AlertInputs{NewOutage: true, OutageCause: causeLinkDrop, MeetingStatus: meetGood}
	_, _ = decideAlerts(in, seeded, alertT0.Add(400*time.Second), testCooldown)

	// The INPUT state and its map are UNCHANGED (copy-on-write proof).
	if len(seeded.lastFired) != 1 {
		t.Errorf("input map grew to %d entries, want 1 (mutated!)", len(seeded.lastFired))
	}
	if got := seeded.lastFired[alertDrop]; !got.Equal(alertT0) {
		t.Errorf("input drop stamp mutated to %v, want %v", got, alertT0)
	}
	if !seeded.meetingBad {
		t.Error("input meetingBad latch mutated to false")
	}
}

// ---- decideAlerts: backward clock ------------------------------------------

func TestDecideAlertsBackwardClock(t *testing.T) {
	// A prior fire stamped in the FUTURE (now < lastFired) => fires now and re-stamps.
	st := alertState{lastFired: map[string]time.Time{alertDrop: alertT0.Add(time.Hour)}}
	now := alertT0 // earlier than the stamp
	fired, next := decideAlerts(AlertInputs{NewOutage: true}, st, now, testCooldown)
	if len(fired) != 1 || fired[0].Kind != alertDrop {
		t.Fatalf("backward clock: got %v, want one alertDrop (never wedged off)", fired)
	}
	if !next.lastFired[alertDrop].Equal(now) {
		t.Errorf("backward clock re-stamp = %v, want %v", next.lastFired[alertDrop], now)
	}
}

// ---- decideAlerts: cooldown == 0 (pinned deterministic case) ---------------

func TestDecideAlertsZeroCooldown(t *testing.T) {
	// Edge signal fires on EVERY edge (no flap damping).
	in := AlertInputs{NewOutage: true}
	_, st := decideAlerts(in, zeroState(), alertT0, 0)
	fired, _ := decideAlerts(in, st, alertT0.Add(time.Nanosecond), 0)
	if len(fired) != 1 || fired[0].Kind != alertDrop {
		t.Errorf("zero cooldown edge: got %v, want fire on every edge", fired)
	}
	// A sustained level still fires only ONCE (the latch is independent of cooldown).
	bad := AlertInputs{MeetingStatus: meetBad}
	_, lv := decideAlerts(bad, zeroState(), alertT0, 0)
	f2, _ := decideAlerts(bad, lv, alertT0.Add(time.Nanosecond), 0)
	if len(f2) != 0 {
		t.Errorf("zero cooldown sustained level: got %v, want suppressed (latch)", f2)
	}
}

// ---- escapeAppleScript / buildNotifyScript (SECURITY: adversarial) ---------

// assertInsideLiteral verifies the built script has the exact wrapper shape and that
// every raw " in the escaped payload is backslash-escaped (stays inside the literal).
func assertScriptSafe(t *testing.T, title, body string) string {
	t.Helper()
	script := buildNotifyScript(title, body)
	const prefix = `display notification "`
	const mid = `" with title "`
	const suffix = `"`
	if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, suffix) {
		t.Fatalf("script shape wrong: %q", script)
	}
	if !strings.Contains(script, mid) {
		t.Fatalf("script missing ` with title `: %q", script)
	}
	// There must be EXACTLY 4 unescaped (structural) double-quotes: the 2 around the
	// body and the 2 around the title. Count quotes NOT preceded by a backslash.
	structural := 0
	for i := 0; i < len(script); i++ {
		if script[i] == '"' {
			// count preceding consecutive backslashes
			bs := 0
			for j := i - 1; j >= 0 && script[j] == '\\'; j-- {
				bs++
			}
			if bs%2 == 0 { // an even number of backslashes => this quote is structural
				structural++
			}
		}
	}
	if structural != 4 {
		t.Errorf("script has %d structural quotes, want 4 (payload broke out!): %q", structural, script)
	}
	// No raw newline or CR survived into the script.
	if strings.ContainsAny(script, "\n\r") {
		t.Errorf("script carries a raw newline/CR (AppleScript syntax error): %q", script)
	}
	return script
}

func TestBuildNotifyScriptPlain(t *testing.T) {
	script := assertScriptSafe(t, "title", "body")
	if script != `display notification "body" with title "title"` {
		t.Errorf("plain script = %q", script)
	}
}

func TestEscapeAppleScriptAdversarial(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"lone double-quote", `a"b`},
		{"backslash before quote", `\"`},
		{"raw newline", "line1\nline2"},
		{"raw CR", "line1\rline2"},
		{"do-shell-script breakout", `" & (do shell script "id") & "`},
		{"tell-application breakout", `" ` + "\n" + ` tell application \"Finder\" to quit ` + "\n" + ` "`},
		{"process-name breakout vector", `" & (do shell script "id") & "`},
		{"emoji + RTL + accent", "café 🔥 مرحبا"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Put the adversarial payload in BOTH title and body — both are escaped.
			assertScriptSafe(t, c.in, c.in)
		})
	}
}

func TestEscapeAppleScriptBackslashOrder(t *testing.T) {
	// Input `\"` must NOT become `\\"` (which would close the literal). The single-pass
	// escaper yields `\\` + `\"` = `\\\"` — a literal backslash then a literal quote.
	got := escapeAppleScript(`\"`)
	if got != `\\\"` {
		t.Errorf("escape(`\\\"`) = %q, want `\\\\\\\"` (backslash escaped before quote)", got)
	}
}

func TestEscapeAppleScriptControlBytes(t *testing.T) {
	// A C0 control (e.g. 0x07 BEL) and 0x7F DEL become spaces; \n/\r become escapes.
	got := escapeAppleScript("a\x07b\x7fc\td")
	if strings.ContainsAny(got, "\x07\x7f\t") {
		t.Errorf("control bytes survived: %q", got)
	}
	if got != "a b c d" {
		t.Errorf("control-byte replacement = %q, want %q", got, "a b c d")
	}
}

func TestEscapeAppleScriptUTF8PassThrough(t *testing.T) {
	in := "café 🔥 مرحبا"
	got := escapeAppleScript(in)
	if got != in { // no special chars => byte-identical UTF-8, never split
		t.Errorf("UTF-8 pass-through = %q, want %q", got, in)
	}
}

func TestEscapeAppleScriptTruncatesOnRuneBoundary(t *testing.T) {
	// >128 runes of a multibyte char: truncated to 128 runes, never a split sequence.
	in := strings.Repeat("🔥", 200)
	got := escapeAppleScript(in)
	// 128 fire emoji, each 4 bytes UTF-8, nothing escaped.
	if want := strings.Repeat("🔥", maxNotifyRunes); got != want {
		t.Errorf("truncation: got %d bytes, want %d (128 runes, no split)", len(got), len(want))
	}
}

// ---- thresholdBps / stepUploadStreak ---------------------------------------

func TestThresholdBps(t *testing.T) {
	if got := thresholdBps(120); got != 15_000_000 {
		t.Errorf("thresholdBps(120) = %v, want 15000000 (1e6/8)", got)
	}
}

func TestStepUploadStreak(t *testing.T) {
	th := thresholdBps(120) // 15_000_000 bytes/sec
	tests := []struct {
		name string
		prev int
		up   float64
		want int
	}{
		{"above increments", 3, th + 1, 4},
		{"exactly at threshold counts as over", 0, th, 1},
		{"below resets", 7, th - 1, 0},
		{"zero up resets", 2, 0, 0},
	}
	for _, c := range tests {
		if got := stepUploadStreak(c.prev, c.up, th); got != c.want {
			t.Errorf("%s: stepUploadStreak(%d,%v,%v) = %d, want %d", c.name, c.prev, c.up, th, got, c.want)
		}
	}
}

func TestClampAlertKnobs(t *testing.T) {
	if got := clampAlertCooldown(0); got != 300*time.Second {
		t.Errorf("clampAlertCooldown(0) = %v, want 300s", got)
	}
	if got := clampAlertCooldown(-5); got != 300*time.Second {
		t.Errorf("clampAlertCooldown(-5) = %v, want 300s", got)
	}
	if got := clampAlertCooldown(60); got != 60*time.Second {
		t.Errorf("clampAlertCooldown(60) = %v, want 60s", got)
	}
	if got := clampHighUploadThreshold(0); got != 120 {
		t.Errorf("clampHighUploadThreshold(0) = %v, want 120", got)
	}
	if got := clampHighUploadThreshold(200); got != 200 {
		t.Errorf("clampHighUploadThreshold(200) = %v, want 200", got)
	}
	if got := clampHighUploadStreak(0); got != 10 {
		t.Errorf("clampHighUploadStreak(0) = %d, want 10", got)
	}
	if got := clampHighUploadStreak(5); got != 5 {
		t.Errorf("clampHighUploadStreak(5) = %d, want 5", got)
	}
}

// ---- formatStatusLine / summaryFromLive ------------------------------------

func TestFormatStatusLinePlain(t *testing.T) {
	s := StatusSummary{Class: "ok", HaveRates: true, DownBps: 1_537_500, UpBps: 512_500,
		GatewayRTTMs: 3, InternetRTTMs: 11, Meeting: meetGood, Source: "serve"}
	got := formatStatusLine(s, "plain")
	for _, want := range []string{"●", "ok", "↓12.3M", "↑4.1M", "gw 3ms", "inet 11ms", "MEET:GOOD", "(serve)"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain line %q missing %q", got, want)
		}
	}
}

func TestFormatStatusLineGlyphWords(t *testing.T) {
	tests := []struct {
		name        string
		s           StatusSummary
		glyph, word string
	}{
		{"gateway_only", StatusSummary{Class: "gateway_only"}, "◐", "degraded"},
		{"down", StatusSummary{Class: "down"}, "○", "down"},
		{"stale overrides ok", StatusSummary{Class: "ok", Stale: true}, "◐", "stale"},
		{"empty class => unknown", StatusSummary{Class: ""}, "○", "unknown"},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			g, w := statusGlyphWord(c.s)
			if g != c.glyph || w != c.word {
				t.Errorf("glyph/word = %q %q, want %q %q", g, w, c.glyph, c.word)
			}
			line := formatStatusLine(c.s, "plain")
			if !strings.Contains(line, c.glyph) || !strings.Contains(line, c.word) {
				t.Errorf("line %q missing glyph %q or word %q", line, c.glyph, c.word)
			}
		})
	}
}

func TestFormatStatusLineNoRatesNoMeeting(t *testing.T) {
	s := StatusSummary{Class: "ok", HaveRates: false, Meeting: "", Source: "local"}
	got := formatStatusLine(s, "plain")
	if !strings.Contains(got, "↓— ↑—") {
		t.Errorf("no-rate line %q should render dashes", got)
	}
	if !strings.Contains(got, "MEET:—") {
		t.Errorf("no-meeting line %q should render MEET:—", got)
	}
	if !strings.Contains(got, "(local)") {
		t.Errorf("line %q should surface source (local)", got)
	}
}

func TestFormatStatusLineSwiftbar(t *testing.T) {
	s := StatusSummary{Class: "ok", HaveRates: true, DownBps: 1_537_500, UpBps: 512_500,
		GatewayRTTMs: 3, InternetRTTMs: 11, Meeting: meetGood, Source: "serve"}
	got := formatStatusLine(s, "swiftbar")
	lines := strings.Split(got, "\n")
	// EXACTLY ONE title line before the first '---'.
	sep := -1
	for i, l := range lines {
		if l == "---" {
			sep = i
			break
		}
	}
	if sep != 1 {
		t.Fatalf("swiftbar: '---' at line %d, want exactly one title line before it:\n%s", sep, got)
	}
	if !strings.Contains(lines[0], "●") || !strings.Contains(lines[0], "ok") {
		t.Errorf("swiftbar title line %q must carry glyph+word", lines[0])
	}
	if !strings.Contains(got, "source: serve") {
		t.Errorf("swiftbar missing source detail line:\n%s", got)
	}
}

func TestSummaryFromLiveServe(t *testing.T) {
	now := alertT0
	ld := LiveData{
		Interval: 5,
		Samples:  []Sample{{DownBytesPerSec: 1000, UpBytesPerSec: 2000}},
		Reach:    &ReachSample{T: now.Add(-5 * time.Second), Class: "ok", GatewayRTTMs: 3, InternetRTTMs: 11},
		Meta:     &LiveMeta{MeetingStatus: meetGood},
	}
	s := summaryFromLive(ld, now, 5*time.Second)
	if s.Source != "serve" || s.Class != "ok" || !s.HaveRates || s.Meeting != meetGood {
		t.Errorf("serve summary = %+v", s)
	}
	if s.Stale {
		t.Error("fresh (5s old at 5s interval) must NOT be stale")
	}
	if s.DownBps != 1000 || s.UpBps != 2000 {
		t.Errorf("rates = %v/%v, want 1000/2000", s.DownBps, s.UpBps)
	}
}

func TestSummaryFromLiveStale(t *testing.T) {
	now := alertT0
	// 20s old at a 5s interval => older than 3*5=15s => stale.
	ld := LiveData{Interval: 5, Reach: &ReachSample{T: now.Add(-20 * time.Second), Class: "ok"}}
	s := summaryFromLive(ld, now, 5*time.Second)
	if !s.Stale {
		t.Error("20s-old sample at 5s interval must be stale")
	}
	line := formatStatusLine(s, "plain")
	if !strings.Contains(line, "stale") || strings.Contains(line, " ok ") {
		t.Errorf("stale line %q must say stale, never ok", line)
	}
}

func TestSummaryFromLiveNoReach(t *testing.T) {
	// Absent reach key => Class "" => "○ unknown".
	ld := LiveData{Interval: 5, Samples: []Sample{{DownBytesPerSec: 1}}}
	s := summaryFromLive(ld, alertT0, 5*time.Second)
	if s.Class != "" {
		t.Errorf("no-reach Class = %q, want empty", s.Class)
	}
	g, w := statusGlyphWord(s)
	if g != "○" || w != "unknown" {
		t.Errorf("no-reach glyph/word = %q %q, want ○ unknown", g, w)
	}
}

// ---- wiring: injected notifier spy (opt-in guarantee) ----------------------

func TestFireAlertsOptInSpy(t *testing.T) {
	// ALL FOUR triggers simultaneously true.
	in := AlertInputs{
		NewOutage: true, OutageCause: causeLinkDrop,
		BandChanged: true, BandFrom: "5 GHz", BandTo: "2.4 GHz",
		MeetingStatus:    meetBad,
		HighUploadStreak: 10, HighUploadThresholdStreak: 10, HighUploadBody: "x",
	}

	// --alerts OFF => notifier called ZERO times, state returned UNCHANGED.
	calls := 0
	spy := func(_ context.Context, _ Alert) error { calls++; return nil }
	seeded := alertState{lastFired: map[string]time.Time{alertDrop: alertT0}, meetingBad: true}
	out := fireAlerts(context.Background(), false, in, seeded, alertT0.Add(time.Hour), testCooldown, spy)
	if calls != 0 {
		t.Errorf("alerts OFF: notifier called %d times, want 0 (opt-in)", calls)
	}
	if len(out.lastFired) != 1 || !out.meetingBad {
		t.Errorf("alerts OFF must return state unchanged, got %+v", out)
	}

	// --alerts ON => notifier called once per fired kind (4).
	calls = 0
	_ = fireAlerts(context.Background(), true, in, zeroState(), alertT0, testCooldown, spy)
	if calls != 4 {
		t.Errorf("alerts ON: notifier called %d times, want 4 (one per trigger)", calls)
	}
}

func TestFireAlertsGatedPreservesState(t *testing.T) {
	// A gated/paused tick does NOT call decideAlerts; modelled at the seam as
	// enabled=false => the latch state is preserved verbatim for the next episode.
	st := alertState{lastFired: map[string]time.Time{alertMeetingBad: alertT0}, meetingBad: true, highUpload: true}
	out := fireAlerts(context.Background(), false, AlertInputs{MeetingStatus: meetGood}, st, alertT0.Add(time.Hour), testCooldown, nil)
	if !out.meetingBad || !out.highUpload || len(out.lastFired) != 1 {
		t.Errorf("gated tick must preserve alertState, got %+v", out)
	}
}
