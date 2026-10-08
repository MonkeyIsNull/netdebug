package main

// reach_gate.go — the shared, sudo-free power/presence gate. Because --serve runs
// 24/7 under launchd, the reach loop PAUSES probing while on BATTERY or while the
// screen is LOCKED, and resumes on AC power / unlock. Default = gating ON; a
// --no-power-gating flag / config power_gating:false opts back in.
//
// This file is the reusable surface for later phases (10 & 13 reuse the PURE funcs
// + the impure readers as reusable CODE, not a shared mutable instance). All on the
// --serve path only — nothing here is reachable from a bare run or --sample.
//
// Pure/impure split: parsePowerSource / parseScreenLocked / shouldProbe are PURE
// and table-tested; readPowerSource / readScreenLocked are the thin exec wrappers.
//
// POWER is the DEPENDABLE gate. SCREEN-LOCK is BEST-EFFORT and FAIL-OPEN: an
// unreadable lock signal degrades to power-only gating so an unreadable signal can
// never permanently pause a 24/7 AC agent.

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

// reachGateTTL caches the power/lock read so the gate is not its own exec storm:
// `ioreg -n Root -d1 -r` is ~88KB plus a pmset exec. The cache is LOCAL to the
// single runReachLoop goroutine (no mutex). It mainly protects faster Phase-10/13
// readers; at the 5s default it dedups the gate's own exec pair. Tradeoff: a
// battery/lock flip is noticed within one tick + TTL (<=~8s at defaults).
const reachGateTTL = 3 * time.Second

// reNowDrawing anchors the first `Now drawing from '<X>'` token of `pmset -g batt`.
// Verified shapes: 'AC Power', 'Battery Power', 'UPS Power'.
var reNowDrawing = regexp.MustCompile(`Now drawing from '([^']*)'`)

// reConsoleLocked matches the always-present `"IOConsoleLocked" = Yes|No` from
// `ioreg -n Root -d1 -r`. (The earlier CGSSessionScreenIsLocked candidate is ABSENT
// at Root while unlocked on current macOS — it appears only inside IOConsoleUsers
// when locked — so IOConsoleLocked is the reliable signal.)
var reConsoleLocked = regexp.MustCompile(`"IOConsoleLocked"\s*=\s*(Yes|No)`)

// parsePowerSource parses `pmset -g batt`. onBattery = source == "Battery Power";
// AC and UPS both => probe-ok; garbage/empty => ("", false) [fail-open, not
// battery]. This is the only guaranteed gate.
func parsePowerSource(out string) (source string, onBattery bool) {
	m := reNowDrawing.FindStringSubmatch(out)
	if m == nil {
		return "", false // fail-open: unknown => not battery
	}
	return m[1], m[1] == "Battery Power"
}

// parseScreenLocked parses `ioreg -n Root -d1 -r`. BEST-EFFORT, FAIL-OPEN:
//   - `"IOConsoleLocked" = Yes` => (true,  true)
//   - `"IOConsoleLocked" = No`  => (false, true)
//   - key absent / garbage / empty => (false, false) [NOT locked, unknown]
//
// The Yes-on-lock flip could NOT be verified under this task's safety rules
// (locking the screen is out of scope); it is strictly best-effort/fail-open. If
// !known, shouldProbe treats it as not-locked (the §0 degrade-to-power-only clause).
func parseScreenLocked(out string) (locked, known bool) {
	m := reConsoleLocked.FindStringSubmatch(out)
	if m == nil {
		return false, false
	}
	return m[1] == "Yes", true
}

// shouldProbe is the PURE gate predicate: probe only when NOT onBattery AND NOT
// screenLocked. The caller NEVER passes an "unknown" lock as locked (fail-open
// collapse-prevention), so an unreadable lock signal degrades to power-only gating.
func shouldProbe(onBattery, screenLocked bool) bool {
	return !onBattery && !screenLocked
}

// readPowerSource execs the sudo-free `pmset -g batt` and parses it.
func readPowerSource() (string, bool) {
	out, _ := exec.Command("pmset", "-g", "batt").CombinedOutput()
	return parsePowerSource(string(out))
}

// readScreenLocked execs the sudo-free `ioreg -n Root -d1 -r` and parses it.
func readScreenLocked() (locked, known bool) {
	out, _ := exec.CommandContext(context.Background(), "ioreg", "-n", "Root", "-d1", "-r").CombinedOutput()
	return parseScreenLocked(string(out))
}

// reachGate holds the cached power/lock read LOCAL to runReachLoop (no mutex).
// allow() re-execs pmset+ioreg only when the TTL has expired AND gating is on; when
// gating is off it NEVER execs and always returns true. The readers are injectable
// so a test can force onBattery/locked and assert the gated-tick store-nothing
// behavior and the no-exec-when-gating-off invariant.
type reachGate struct {
	gating       bool
	ttl          time.Duration
	lastRead     time.Time
	onBattery    bool
	screenLocked bool
	valid        bool
	readPower    func() (string, bool) // source, onBattery
	readLocked   func() (bool, bool)   // locked, known
	now          func() time.Time
}

// newReachGate builds the production gate (real readers, real clock, 3s TTL).
func newReachGate(gating bool) *reachGate {
	return &reachGate{
		gating:     gating,
		ttl:        reachGateTTL,
		readPower:  readPowerSource,
		readLocked: readScreenLocked,
		now:        time.Now,
	}
}

// allow reports whether this tick may probe. gating off => true with NO exec.
// Otherwise it refreshes the cached read when stale and applies shouldProbe. An
// unknown lock state is mapped to not-locked here (fail-open), so shouldProbe never
// sees an "unknown" as locked.
func (g *reachGate) allow() bool {
	if !g.gating {
		return true // power_gating:false => never exec, always probe
	}
	now := g.now()
	if !g.valid || now.Sub(g.lastRead) >= g.ttl {
		_, onBat := g.readPower()
		locked, known := g.readLocked()
		g.onBattery = onBat
		g.screenLocked = locked && known // unknown => not locked (fail-open)
		g.lastRead = now
		g.valid = true
	}
	return shouldProbe(g.onBattery, g.screenLocked)
}
