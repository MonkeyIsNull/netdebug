package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ---- VERBATIM macOS `pmset -g batt` captures ------------------------------

const pmsetAC = `Now drawing from 'AC Power'
 -InternalBattery-0 (id=8585315)	100%; charged; 0:00 remaining present: true`

const pmsetBattery = `Now drawing from 'Battery Power'
 -InternalBattery-0 (id=8585315)	82%; discharging; 3:41 remaining present: true`

const pmsetUPS = `Now drawing from 'UPS Power'
 -InternalBattery-0 (id=8585315)	100%; charged; 0:00 remaining present: true`

// ---- VERBATIM macOS `ioreg -n Root -d1 -r` fragments ----------------------

const ioregUnlocked = `  +-o Root  <class IORegistryEntry, id 0x100000100, retain 36>
    {
      "IOConsoleLocked" = No
      "IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=Yes,"kCGSSessionUserNameKey"="adam"})
    }`

const ioregLocked = `  +-o Root  <class IORegistryEntry, id 0x100000100, retain 36>
    {
      "IOConsoleLocked" = Yes
      "IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=Yes,"kCGSSessionUserNameKey"="adam"})
    }`

// Older/edge output where the only lock-ish key is inside IOConsoleUsers and the
// always-present IOConsoleLocked boolean is absent entirely.
const ioregNoLockedKey = `  +-o Root  <class IORegistryEntry, id 0x100000100, retain 36>
    {
      "IOConsoleUsers" = ({"CGSSessionScreenIsLocked"=Yes,"kCGSSessionUserNameKey"="adam"})
    }`

func TestParsePowerSource(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantSrc    string
		wantOnBatt bool
	}{
		{"AC power", pmsetAC, "AC Power", false},
		{"battery power", pmsetBattery, "Battery Power", true},
		{"UPS power (AC-equivalent)", pmsetUPS, "UPS Power", false},
		{"garbage", "pmset: command produced nothing useful", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, onBatt := parsePowerSource(tc.out)
			if src != tc.wantSrc || onBatt != tc.wantOnBatt {
				t.Errorf("parsePowerSource = (%q,%v), want (%q,%v)", src, onBatt, tc.wantSrc, tc.wantOnBatt)
			}
		})
	}
}

func TestParseScreenLocked(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantLocked bool
		wantKnown  bool
	}{
		{"IOConsoleLocked No", ioregUnlocked, false, true},
		{"IOConsoleLocked Yes", ioregLocked, true, true}, // locked-state unverified in this env; best-effort
		{"key absent (CGSSession-only)", ioregNoLockedKey, false, false},
		{"garbage", "random unrelated ioreg text", false, false},
		{"empty", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			locked, known := parseScreenLocked(tc.out)
			if locked != tc.wantLocked || known != tc.wantKnown {
				t.Errorf("parseScreenLocked = (%v,%v), want (%v,%v)", locked, known, tc.wantLocked, tc.wantKnown)
			}
		})
	}
}

func TestShouldProbe(t *testing.T) {
	tests := []struct {
		onBattery, screenLocked, want bool
	}{
		{false, false, true},
		{true, false, false},
		{false, true, false},
		{true, true, false},
	}
	for _, tc := range tests {
		if got := shouldProbe(tc.onBattery, tc.screenLocked); got != tc.want {
			t.Errorf("shouldProbe(%v,%v) = %v, want %v", tc.onBattery, tc.screenLocked, got, tc.want)
		}
	}
}

// TestReachGateUnknownLockFailsOpen proves an UNKNOWN lock (parseScreenLocked
// known=false) is mapped to NOT-locked, so on AC with an unreadable lock signal the
// gate still allows probing (it never passes "unknown" to shouldProbe as locked).
func TestReachGateUnknownLockFailsOpen(t *testing.T) {
	g := &reachGate{
		gating:     true,
		ttl:        reachGateTTL,
		now:        time.Now,
		readPower:  func() (string, bool) { return "AC Power", false },
		readLocked: func() (bool, bool) { return false, false }, // unknown
	}
	if !g.allow() {
		t.Error("AC + unknown lock should allow probing (fail-open)")
	}
}

// TestReachGateGatingOffNeverExecs proves power_gating:false short-circuits BEFORE
// any exec: the injected readers must never be called, and allow() is always true.
func TestReachGateGatingOffNeverExecs(t *testing.T) {
	var powerCalls, lockCalls int32
	g := &reachGate{
		gating:     false,
		ttl:        reachGateTTL,
		now:        time.Now,
		readPower:  func() (string, bool) { atomic.AddInt32(&powerCalls, 1); return "AC Power", false },
		readLocked: func() (bool, bool) { atomic.AddInt32(&lockCalls, 1); return false, true },
	}
	for i := 0; i < 5; i++ {
		if !g.allow() {
			t.Fatal("gating off must always allow")
		}
	}
	if powerCalls != 0 || lockCalls != 0 {
		t.Errorf("gating off must never exec the gate readers, got power=%d lock=%d", powerCalls, lockCalls)
	}
}

// TestReachGateCachesReads proves the TTL cache dedups the exec pair: many allow()
// calls within one TTL window read the power/lock sources only ONCE.
func TestReachGateCachesReads(t *testing.T) {
	var powerCalls int32
	fakeNow := time.Unix(0, 0)
	g := &reachGate{
		gating:     true,
		ttl:        3 * time.Second,
		now:        func() time.Time { return fakeNow },
		readPower:  func() (string, bool) { atomic.AddInt32(&powerCalls, 1); return "AC Power", false },
		readLocked: func() (bool, bool) { return false, true },
	}
	for i := 0; i < 10; i++ {
		g.allow()
	}
	if got := atomic.LoadInt32(&powerCalls); got != 1 {
		t.Errorf("within one TTL the gate should read power once, got %d", got)
	}
	// Advance past the TTL: one more read.
	fakeNow = fakeNow.Add(4 * time.Second)
	g.allow()
	if got := atomic.LoadInt32(&powerCalls); got != 2 {
		t.Errorf("after TTL expiry the gate should re-read, got %d", got)
	}
}

// TestRunReachLoopCadence mirrors TestRunLinkLoopCadence: with an injected spy
// read and a short interval, the loop calls read at the reach cadence (≈
// window/interval), stores into the holder + ring, and exits promptly on cancel
// with no late store. The gate allows every tick.
func TestRunReachLoopCadence(t *testing.T) {
	var calls int32
	spy := func(context.Context) ReachSample {
		atomic.AddInt32(&calls, 1)
		return ReachSample{Class: "ok", GatewayOK: true, InternetOK: true}
	}
	gate := &reachGate{gating: false} // never pauses, never execs
	holder := &reachHolder{}
	ring := newReachRing()
	interval := 20 * time.Millisecond
	window := 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runReachLoop(ctx, interval, spy, gate, holder, ring, nil); close(done) }()

	time.Sleep(window)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runReachLoop did not exit promptly on ctx cancel")
	}

	n := atomic.LoadInt32(&calls)
	if n < 4 || n > 25 {
		t.Errorf("read calls = %d, want ~%d at the reach cadence (not 1/sec)", n, int(window/interval))
	}
	if holder.load() == nil {
		t.Error("holder should hold the latest sample after reads")
	}
	if len(ring.Snapshot()) == 0 {
		t.Error("ring should have accumulated samples")
	}

	settled := atomic.LoadInt32(&calls)
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != settled {
		t.Errorf("read called %d more times after cancel", got-settled)
	}
}

// TestRunReachLoopGatedStoresNothing is the gated-tick proof: with the gate forced
// to onBattery, every tick is paused — read is NEVER called, the holder stays nil,
// and the ring stays empty across the whole window.
func TestRunReachLoopGatedStoresNothing(t *testing.T) {
	var calls int32
	spy := func(context.Context) ReachSample {
		atomic.AddInt32(&calls, 1)
		return ReachSample{Class: "ok"}
	}
	gate := &reachGate{
		gating:     true,
		ttl:        reachGateTTL,
		now:        time.Now,
		readPower:  func() (string, bool) { return "Battery Power", true }, // forced on battery
		readLocked: func() (bool, bool) { return false, true },
	}
	holder := &reachHolder{}
	ring := newReachRing()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runReachLoop(ctx, 15*time.Millisecond, spy, gate, holder, ring, nil); close(done) }()

	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("gated loop called read %d times, want 0 (paused on battery)", got)
	}
	if holder.load() != nil {
		t.Error("gated loop stored into the holder, want nil")
	}
	if len(ring.Snapshot()) != 0 {
		t.Error("gated loop added to the ring, want empty")
	}
}
