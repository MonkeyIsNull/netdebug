package main

// Phase 12 — ALERTS & AMBIENT. Two features, one file, the house pure/impure split:
//
//   (a) OPT-IN native macOS notifications (--serve --alerts) the instant the link
//       misbehaves — drop / band-change / meeting->BAD / sustained-high-upload — with
//       a PURE, table-tested debounce so it INFORMS rather than SPAMS.
//   (b) A one-line `--status` health summary for a shell prompt / tmux / SwiftBar.
//
// PURE CORE (table-tested in alerts_test.go): decideAlerts (the whole "don't spam"
// contract) + escapeAppleScript/buildNotifyScript (the osascript-injection boundary)
// + formatStatusLine/summaryFromLive (the ambient one-liner) + thresholdBps/
// stepUploadStreak + the clamps. IMPURE LAYER (thin): notify (one osascript exec,
// fail-soft, time-bounded) + fetchStatus/localStatus (loopback /data.json read with a
// one-local-probe fallback).
//
// SECURITY CRUX: SSID / band / process names are ATTACKER-INFLUENCABLE text (a local
// app names itself anything; a nearby AP picks its own SSID). They are interpolated
// into an AppleScript string, so escapeAppleScript is the osascript-injection guard —
// every Title/Body goes through buildNotifyScript, nothing bypasses it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---- alert KINDS (fixed output order) -------------------------------------
//
// decideAlerts ALWAYS appends in this order (never by ranging a map — Go map
// iteration is randomized, the modeChannel/bandPriority lesson), so a multi-
// condition slice assertion is deterministic.
const (
	alertDrop       = "drop"
	alertBandChange = "band_change"
	alertMeetingBad = "meeting_bad"
	alertHighUpload = "high_upload"
)

// Config defaults for the alert knobs (clamped at the impure boundary below).
const (
	defaultAlertCooldownSecs       = 300
	defaultHighUploadThresholdMbps = 120.0
	defaultHighUploadStreakSamples = 10
)

// maxNotifyRunes bounds a notification string (title or body) on a rune boundary so a
// pathological multi-KB process/SSID cannot produce a giant/garbled notification.
const maxNotifyRunes = 128

// notifyTimeout bounds one osascript exec so a wedged/hung `display notification`
// cannot stall the reach loop (which also drives outage detection).
const notifyTimeout = 3 * time.Second

// Alert is one fired notification.
type Alert struct {
	T     time.Time
	Kind  string
	Title string
	Body  string
}

// alertState is a PURE VALUE type carried in/out of decideAlerts. It holds the
// per-kind cooldown stamps PLUS the two LEVEL-signal latches. A map is a REFERENCE
// type, so decideAlerts clones lastFired before writing and returns the clone
// (copy-on-write) — otherwise the returned state and the input would alias the same
// map and the caller's prior state would mutate in place.
type alertState struct {
	lastFired  map[string]time.Time // per-kind last-fire time (cooldown flap-damper)
	meetingBad bool                 // LATCH: meeting was BAD on the last DECIDED tick
	highUpload bool                 // LATCH: upload streak was over threshold last tick
}

// AlertInputs is the pure snapshot decideAlerts reads (bools/counts/strings derived
// from reach/outage/band/meeting state — NEVER live objects). Edge signals
// (NewOutage, BandChanged) are TRUE for exactly one tick by construction; the two
// LEVEL signals (MeetingStatus, HighUploadStreak) stay asserted while sustained and
// are converted to rising edges INSIDE decideAlerts via the latches above.
type AlertInputs struct {
	NewOutage                 bool   // the outage OPEN edge, NOT a close
	OutageCause               string // link-drop | gateway-unreachable | internet-down
	OutageSSID                string // outage onset SSID (attacker-influencable text)
	BandChanged               bool   // one-tick band-change edge
	BandFrom, BandTo          string
	MeetingStatus             string // "GOOD"/"RISKY"/"BAD"/"" ; "" and non-BAD never fire
	HighUploadStreak          int    // consecutive samples at/over the upstream threshold
	HighUploadThresholdStreak int    // the N that counts as "sustained" (streak >= N)
	HighUploadBody            string // human text for the body (e.g. "↑135.0 Mbps sustained")
}

// decideAlerts is the WHOLE "don't spam" contract. PURE, no IO, no exec. Returns the
// Alerts to FIRE now (fixed kind order) + the next alertState. See the file header
// and the §2a plan contract: copy-on-write state; edge signals (drop, band_change)
// fire on their one-tick pulse gated only by cooldown (a flap guard); level signals
// (meeting_bad, high_upload) fire ONCE PER EPISODE — on the rising edge (clear ->
// asserted) AND fireable, suppressed while sustained (the latch stays set across the
// cooldown boundary), re-armed only when the condition CLEARS. A MISSING cooldown key
// fires (first-ever). A stamp in the FUTURE (backward NTP step) is treated as
// "cooldown elapsed" so a backward clock never wedges alerts off. cooldown==0 is a
// pinned, deterministic case: edges fire on every edge; levels still fire only on the
// rising edge.
func decideAlerts(in AlertInputs, st alertState, now time.Time, cooldown time.Duration) ([]Alert, alertState) {
	// COPY-ON-WRITE: clone st.lastFired into the returned state; never mutate the
	// input map (nil clones to an empty map, so a zero-value state is safe).
	next := alertState{
		lastFired:  make(map[string]time.Time, len(st.lastFired)),
		meetingBad: st.meetingBad,
		highUpload: st.highUpload,
	}
	for k, v := range st.lastFired {
		next.lastFired[k] = v
	}

	var fired []Alert

	// fireable: a MISSING key (ok==false) => fire (first-ever, never via `if !ok
	// {skip}`). A stamp in the FUTURE (now.Before(last), a backward NTP step) => treat
	// as elapsed => fire and re-stamp. Otherwise fire iff now.Sub(last) >= cooldown.
	fireable := func(kind string) bool {
		last, ok := next.lastFired[kind]
		if !ok {
			return true
		}
		if now.Before(last) {
			return true
		}
		return now.Sub(last) >= cooldown
	}
	fire := func(kind, title, body string) {
		fired = append(fired, Alert{T: now, Kind: kind, Title: title, Body: body})
		next.lastFired[kind] = now
	}

	// 1. drop (EDGE) — one-tick outage-open pulse; cooldown is only a flap guard.
	if in.NewOutage && fireable(alertDrop) {
		fire(alertDrop, "netdebug: connection dropped", dropBody(in))
	}

	// 2. band_change (EDGE).
	if in.BandChanged && fireable(alertBandChange) {
		fire(alertBandChange, "netdebug: Wi-Fi band changed",
			fmt.Sprintf("%s → %s", in.BandFrom, in.BandTo))
	}

	// 3. meeting_bad (LEVEL, once per episode). Only BAD fires; RISKY/GOOD/"" never do.
	// Rising edge = BAD now AND not BAD on the last decided tick.
	meetingBadNow := in.MeetingStatus == meetBad
	if meetingBadNow && !st.meetingBad && fireable(alertMeetingBad) {
		fire(alertMeetingBad, "netdebug: meeting quality BAD",
			"video-call quality degraded (jitter/loss over call limits)")
	}
	next.meetingBad = meetingBadNow // re-arms the next episode on a clear

	// 4. high_upload (LEVEL, once per episode). A STREAK (>= N samples), not a spike.
	highNow := in.HighUploadThresholdStreak > 0 && in.HighUploadStreak >= in.HighUploadThresholdStreak
	if highNow && !st.highUpload && fireable(alertHighUpload) {
		fire(alertHighUpload, "netdebug: sustained upload", in.HighUploadBody)
	}
	next.highUpload = highNow

	return fired, next
}

// dropBody renders the drop notification body from the onset cause + SSID. Both are
// plain here; the osascript escaping happens in buildNotifyScript at notify time.
func dropBody(in AlertInputs) string {
	cause := in.OutageCause
	if cause == "" {
		cause = "connection lost"
	}
	if in.OutageSSID != "" {
		return fmt.Sprintf("%s on %q", cause, in.OutageSSID)
	}
	return cause
}

// ---- osascript injection boundary (THE headline must_fix) -----------------

// escapeAppleScript makes one string SAFE to interpolate inside an AppleScript
// double-quoted literal. SSID / band / process / --ssid / --label text is
// ATTACKER-INFLUENCABLE, so this is the osascript-injection boundary. Rules:
//  1. TRUNCATE to <=maxNotifyRunes on a RUNE boundary FIRST, so a multibyte sequence
//     is never split and a multi-KB payload cannot produce a giant script.
//  2. Backslash -> \\ and double-quote -> \" . A SINGLE-PASS rune switch handles each
//     rune independently, so the "escape quote-first reintroduces breakout" ordering
//     hazard of sequential string-replacement cannot occur here: input `\"` becomes
//     `\\` + `\"` = a literal backslash then a literal quote, never a closing quote.
//  3. newline -> \n and carriage return -> \r (a RAW \n/\r inside a "-quoted
//     AppleScript literal is a SYNTAX ERROR, not just ugly).
//  4. Every OTHER C0 control byte (0x00..0x1F) and 0x7F -> a space (AppleScript cannot
//     carry them in a literal; a space keeps the text readable).
//  5. All other UTF-8 (emoji / RTL / accented / combining marks) passes through as its
//     runes. strconv.Quote/%q is deliberately NOT used: it is injection-safe but emits
//     Go-only \xNN/\uNNNN escapes AppleScript cannot parse.
func escapeAppleScript(s string) string {
	if utf8.RuneCountInString(s) > maxNotifyRunes {
		r := []rune(s)
		s = string(r[:maxNotifyRunes])
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteByte(' ')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// buildNotifyScript builds the COMPLETE osascript program for one notification,
// running BOTH title and body through escapeAppleScript (band/SSID/process can appear
// in either). Shape: display notification "<body>" with title "<title>". This escaped
// output is the ONLY thing that varies in the eventual argv; nothing wraps it further
// and no shell is ever involved.
func buildNotifyScript(title, body string) string {
	return `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
}

// ---- impure notify layer (thin) -------------------------------------------

// notifyFailOnce rate-limits the "notifications unavailable" log so a broken/headless
// osascript cannot spam the serve log (it also drives outage detection).
var notifyFailOnce sync.Once

// notify execs the ABSOLUTE "/usr/bin/osascript" (NEVER bare "osascript", which is
// PATH-shadowable under launchd's thin PATH — the same anti-shadowing rule as
// reach.go's /sbin/ping) with the built script as a SINGLE argv element. NO shell, NO
// sh -c, NO Sprintf into a shell string. FAIL-SOFT AND TIME-BOUNDED: a failure
// (headless / launchd / no-GUI / wedged osascript) is logged ONCE and returned, never
// fatal, never kills serve; a stuck osascript is killed at notifyTimeout so it cannot
// stall the reach loop.
func notify(ctx context.Context, a Alert) error {
	cctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/usr/bin/osascript", "-e", buildNotifyScript(a.Title, a.Body))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if _, err := cmd.CombinedOutput(); err != nil {
		notifyFailOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "netdebug: notifications unavailable (osascript failed: %v); continuing without alerts\n", err)
		})
		return err
	}
	return nil
}

// fireAlerts is the injected wiring seam (so the opt-in guarantee is TESTABLE). When
// enabled is false it returns st UNCHANGED and calls notify ZERO times — the opt-in
// contract. When enabled, it runs decideAlerts and notifies each fired Alert in the
// fixed kind order; notify is fail-soft (it logs its own, rate-limited, failure) so a
// broken notifier never propagates up into the reach loop.
func fireAlerts(ctx context.Context, enabled bool, in AlertInputs, st alertState, now time.Time,
	cooldown time.Duration, notifyFn func(context.Context, Alert) error) alertState {
	if !enabled {
		return st
	}
	fired, next := decideAlerts(in, st, now, cooldown)
	for _, a := range fired {
		if notifyFn != nil {
			_ = notifyFn(ctx, a) // fail-soft; notifyFn logs (rate-limited)
		}
	}
	return next
}

// ---- high-upload streak (pure) --------------------------------------------

// thresholdBps converts a Mbps threshold to BYTES/sec in ONE pure, tested place:
// mbps * 1e6 / 8 (decimal 1e6 + the single *8, matching humanBps' convention).
// Sample.UpBytesPerSec is BYTES/sec, so comparing it to a bits/sec or 1<<20 number
// would silently mis-trigger.
func thresholdBps(mbps float64) float64 { return mbps * 1e6 / 8 }

// stepUploadStreak is the PURE high-upload counter step: up>=thresholdBps => prev+1,
// else 0. Exactly == threshold counts as over (the streak>=N convention).
func stepUploadStreak(prev int, upBytesPerSec, thresholdBps float64) int {
	if upBytesPerSec >= thresholdBps {
		return prev + 1
	}
	return 0
}

// ---- impure-boundary clamps (the anti-spam contract needs these) ----------

// clampAlertCooldown: secs<=0 => 300s default. decideAlerts stays clamp-agnostic.
func clampAlertCooldown(secs int) time.Duration {
	if secs <= 0 {
		return time.Duration(defaultAlertCooldownSecs) * time.Second
	}
	return time.Duration(secs) * time.Second
}

// clampHighUploadThreshold: mbps<=0 => 120 default.
func clampHighUploadThreshold(mbps float64) float64 {
	if mbps <= 0 {
		return defaultHighUploadThresholdMbps
	}
	return mbps
}

// clampHighUploadStreak: n<1 => 10 default (a sustained streak needs at least 1).
func clampHighUploadStreak(n int) int {
	if n < 1 {
		return defaultHighUploadStreakSamples
	}
	return n
}

// ---- ambient one-liner (--status) -----------------------------------------

// StatusSummary is the pure input to formatStatusLine, built from a /data.json decode
// (Source "serve") or the local one-probe fallback (Source "local"). HaveRates
// distinguishes "0 bps measured" from "no rate available" (the one-read fallback
// cannot compute a rate). Stale mirrors the serve-side meetingStaleFactor check so an
// OLD reach sample is not shown as a live "ok".
type StatusSummary struct {
	Class         string  // reachClass output: "ok"/"gateway_only"/"down"/"" (unknown/absent)
	HaveRates     bool    // false => render "—" for rates (fallback / warm-up)
	DownBps       float64 // bytes/sec
	UpBps         float64 // bytes/sec
	GatewayRTTMs  float64
	InternetRTTMs float64
	Meeting       string // "GOOD"/"RISKY"/"BAD"/"" (absent => MEET:—)
	Stale         bool   // reach sample older than meetingStaleFactor*interval
	Source        string // "serve" | "local"
}

// statusGlyphWord maps a StatusSummary to the pinned glyph+WORD. BOTH are mandatory in
// BOTH formats (the glyphs ● ◐ ○ are near-indistinguishable at prompt/menu-bar sizes;
// the WORD is the reliable signal). Stale overrides the class word (never a stale
// "ok"); an empty Class renders "○ unknown", never a blank glyph.
func statusGlyphWord(s StatusSummary) (glyph, word string) {
	if s.Stale {
		return "◐", "stale"
	}
	switch s.Class {
	case "ok":
		return "●", "ok"
	case "gateway_only":
		return "◐", "degraded"
	case "down":
		return "○", "down"
	default:
		return "○", "unknown"
	}
}

// compactBits renders a BYTES/sec rate as compact bits/sec (G/M/K suffix, no "bps")
// for the one-liner. Negative/NaN/Inf => "0" (humanBps's guard convention).
func compactBits(bytesPerSec float64) string {
	if bytesPerSec != bytesPerSec || bytesPerSec < 0 || bytesPerSec > 1e308 {
		return "0"
	}
	bits := bytesPerSec * 8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.1fG", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1fM", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.0fK", bits/1e3)
	default:
		return fmt.Sprintf("%.0f", bits)
	}
}

// statusRates renders the "↓.. ↑.." fragment, or "↓— ↑—" when no rate is available.
func statusRates(s StatusSummary) string {
	if !s.HaveRates {
		return "↓— ↑—"
	}
	return "↓" + compactBits(s.DownBps) + " ↑" + compactBits(s.UpBps)
}

// rttStr renders an RTT as "<n>ms" or "—" when absent (0/negative is never a real RTT).
func rttStr(ms float64) string {
	if ms <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0fms", ms)
}

// meetStr renders "MEET:<status>", "MEET:—" when absent.
func meetStr(m string) string {
	if m == "" {
		return "MEET:—"
	}
	return "MEET:" + m
}

// formatStatusLine renders ONE line. format "swiftbar" emits EXACTLY ONE title line
// (carrying glyph+word so health shows in the menu bar — SwiftBar ROTATES multiple
// pre-'---' lines), then '---', then detail lines. Any other format (incl. "plain")
// renders the single compact prompt/tmux line.
func formatStatusLine(s StatusSummary, format string) string {
	glyph, word := statusGlyphWord(s)
	rates := statusRates(s)
	rtt := "gw " + rttStr(s.GatewayRTTMs) + " inet " + rttStr(s.InternetRTTMs)
	meet := meetStr(s.Meeting)

	if format == "swiftbar" {
		// Exactly ONE title line before '---'.
		var b strings.Builder
		b.WriteString(glyph + " " + word + "\n")
		b.WriteString("---\n")
		b.WriteString(rates + "\n")
		b.WriteString(rtt + "\n")
		b.WriteString(meet + "\n")
		b.WriteString("source: " + s.Source)
		return b.String()
	}

	return fmt.Sprintf("netdebug %s %s  %s  %s  %s  (%s)",
		glyph, word, rates, rtt, meet, s.Source)
}

// summaryFromLive builds a StatusSummary from a decoded /data.json (Source "serve").
// PURE with an INJECTED now (the stale check is testable). Stale = reach sample older
// than meetingStaleFactor * interval (the wire interval_sec, or the passed fallback
// when the wire omits it), mirroring the serve-side meeting staleness rule.
func summaryFromLive(ld LiveData, now time.Time, fallbackInterval time.Duration) StatusSummary {
	s := StatusSummary{Source: "serve"}
	if ld.Reach != nil {
		r := ld.Reach
		s.Class = r.Class
		s.GatewayRTTMs = r.GatewayRTTMs
		s.InternetRTTMs = r.InternetRTTMs
		interval := time.Duration(ld.Interval * float64(time.Second))
		if interval <= 0 {
			interval = fallbackInterval
		}
		if interval > 0 && now.Sub(r.T) > time.Duration(meetingStaleFactor)*interval {
			s.Stale = true
		}
	}
	if n := len(ld.Samples); n > 0 {
		last := ld.Samples[n-1]
		s.HaveRates = true
		s.DownBps = last.DownBytesPerSec
		s.UpBps = last.UpBytesPerSec
	}
	if ld.Meta != nil {
		s.Meeting = ld.Meta.MeetingStatus
	}
	return s
}

// fetchStatus discovers the running serve the SAME way serve picks its port (scan
// startPort..startPort+maxPortTries with a cheap loopback GET /healthz, use the FIRST
// live port), then GETs the tiny GET /data.json?n=1 snapshot. FALLS BACK to ONE
// bounded local probe (localStatus) on ANY failure (no live port / refused / non-200 /
// unparseable body), never crashes. The loopback client has its OWN short timeout so a
// wedged serve cannot freeze the prompt.
func fetchStatus(ctx context.Context, startPort int, internetIPs []string, dnsName string, interval time.Duration) StatusSummary {
	client := &http.Client{Timeout: 2 * time.Second}
	for p := startPort; p < startPort+maxPortTries && p <= 65535; p++ {
		base := fmt.Sprintf("http://127.0.0.1:%d", p)
		hz, err := client.Get(base + "/healthz")
		if err != nil {
			continue // refused-fast: this port has no serve
		}
		body, _ := io.ReadAll(io.LimitReader(hz.Body, 64))
		hz.Body.Close()
		if hz.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
			continue
		}
		// Live serve found. Read the tiny latest snapshot.
		resp, err := client.Get(base + "/data.json?n=1")
		if err != nil {
			break // found a serve but the read failed => fall back
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil || rerr != nil || resp.StatusCode != http.StatusOK {
			break
		}
		var ld LiveData
		if json.Unmarshal(b, &ld) != nil {
			break
		}
		return summaryFromLive(ld, time.Now(), interval)
	}
	return localStatus(ctx, internetIPs, dnsName)
}

// localStatus is the no-serve fallback: ONE bounded reach probe (gateway + <=2
// internet pings + 1 DNS — gentle, does NOT saturate and does NOT active-scan, but is
// real ICMP, so --status is not packet-free standalone). Rates are "—" (HaveRates
// false) — ONE counter read cannot produce a bytes/sec rate and a prompt one-liner
// must stay fast. Meeting is computed from the fresh reach sample (the live serve
// path reads it from meta instead).
func localStatus(ctx context.Context, internetIPs []string, dnsName string) StatusSummary {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r := readReach(cctx, internetIPs, dnsName, nil)
	status, _ := meetingReady(r.JitterMs, r.InternetLoss)
	return StatusSummary{
		Class:         r.Class,
		HaveRates:     false,
		GatewayRTTMs:  r.GatewayRTTMs,
		InternetRTTMs: r.InternetRTTMs,
		Meeting:       status,
		Source:        "local",
	}
}
