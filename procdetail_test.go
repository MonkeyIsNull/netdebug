package main

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- VERBATIM ps / lsof fixtures (real captures, macOS 24.6.0, go1.26.0) --------

// psWebKit is the REAL output of `LC_ALL=C /bin/ps -p 13595 -o pid=,ppid=,user=,
// lstart=,command=` (one line, NO header, command LAST). The lstart is a 5-token
// ctime with a DOUBLE space before the single-digit day ("Oct  6"); the command is a
// full dotted executable path.
const psWebKit = `13595     1 adam Tue Oct  6 09:45:28 2026     /System/Volumes/Preboot/Cryptexes/App/System/Library/StagedFrameworks/Safari/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking`

// commWebKit is the REAL output of `LC_ALL=C /bin/ps -p 13595 -o comm=` — the FULL
// executable path (NOT truncated; `-o comm=` has a null header so it is not the
// 16-char-wide `-o comm`).
const commWebKit = `/System/Volumes/Preboot/Cryptexes/App/System/Library/StagedFrameworks/Safari/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking
`

// lsofWebKit is the REAL `-a`-scoped capture (only pid 13595's established TCP peers):
// 7 connections, remote IPs 104.18.32.47 x2, 104.18.39.21 x3, 13.227.180.4 x2, all
// :443, with the trailing "(ESTABLISHED)" token.
const lsofWebKit = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
com.apple 13595 adam    9u  IPv4 0xb4b8637de53f1c89      0t0  TCP 10.0.0.138:50681->104.18.32.47:443 (ESTABLISHED)
com.apple 13595 adam   10u  IPv4 0x796b94f02051d1c4      0t0  TCP 10.0.0.138:49396->104.18.39.21:443 (ESTABLISHED)
com.apple 13595 adam   17u  IPv4 0x570308cb80098ad6      0t0  TCP 10.0.0.138:50682->104.18.39.21:443 (ESTABLISHED)
com.apple 13595 adam   30u  IPv4 0x5717ce643f078b38      0t0  TCP 10.0.0.138:50745->104.18.32.47:443 (ESTABLISHED)
com.apple 13595 adam   82u  IPv4 0x1d641a6c9f47d94a      0t0  TCP 10.0.0.138:65342->104.18.39.21:443 (ESTABLISHED)
com.apple 13595 adam   87u  IPv4 0xee9c2d238ba0a980      0t0  TCP 10.0.0.138:60734->13.227.180.4:443 (ESTABLISHED)
com.apple 13595 adam   90u  IPv4 0x6402ce128f5f41fb      0t0  TCP 10.0.0.138:49546->13.227.180.4:443 (ESTABLISHED)
`

// lsofIPv6 is a REAL capture of bracketed IPv6 NAME tokens (rapportd / identityservice
// — the COMMAND col truncated to 9 chars). The parser must rsplit host:port on the
// LAST ":" and STRIP the brackets into a BARE RemoteIP.
const lsofIPv6 = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
rapportd    472 adam   15u  IPv6 0x57847dfd84cc0655      0t0  TCP [fe80:b::8a:5b91:9825:a845]:60770->[fe80:b::8cd:7269:b9b8:64c5]:62019 (ESTABLISHED)
identitys   508 adam   53u  IPv6 0xd14d50e0f03cdfeb      0t0  TCP [fe80:14::e5b2:c72e:f9c7:3375]:1024->[fe80:14::778e:640f:93cc:ed7b]:1024 (ESTABLISHED)
`

// lsofSpaceCmd is a REAL capture where the lsof COMMAND column contains a space
// ("Google Ch" truncated) — the fixed-index trap. The parser MUST pick the Fields
// token CONTAINING "->", not a fixed column.
const lsofSpaceCmd = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
Google Ch 22839 adam   22u  IPv4 0x34b9c656a9327fc3      0t0  TCP 10.0.0.138:51447->2.18.67.82:443 (ESTABLISHED)
`

// lsofNoDashA is a TRIMMED slice of the SAME `-p 13595` query WITHOUT `-a`: lsof ORs
// filters by default, so FOREIGN pids (rapportd 472, identityservice 508) leak in.
// The `-a` scoping is enforced by the EXEC argv (lsofArgs), NOT the parser — this
// fixture documents WHY `-a` is mandatory.
const lsofNoDashA = `COMMAND     PID USER   FD      TYPE             DEVICE  SIZE/OFF                NODE NAME
rapportd    472 adam   15u     IPv6 0x57847dfd84cc0655       0t0                 TCP [fe80:b::8a:5b91:9825:a845]:60770->[fe80:b::8cd:7269:b9b8:64c5]:62019 (ESTABLISHED)
identitys   508 adam   53u     IPv6 0xd14d50e0f03cdfeb       0t0                 TCP [fe80:14::e5b2:c72e:f9c7:3375]:1024->[fe80:14::778e:640f:93cc:ed7b]:1024 (ESTABLISHED)
`

// ---- parseProcDetail: the multi-connection WebKit capture -----------------------

func TestParseProcDetailWebKit(t *testing.T) {
	now := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	d := parseProcDetail(psWebKit, commWebKit, "/sbin/launchd\n", lsofWebKit, 13595, now)

	if !d.Available {
		t.Fatalf("WebKit capture should be available")
	}
	if d.PID != 13595 {
		t.Errorf("PID = %d, want 13595", d.PID)
	}
	if d.PPID != 1 {
		t.Errorf("PPID = %d, want 1", d.PPID)
	}
	if d.User != "adam" {
		t.Errorf("User = %q, want adam", d.User)
	}
	if d.Started != "Tue Oct  6 09:45:28 2026" {
		t.Errorf("Started = %q, want verbatim lstart with double-space day pad", d.Started)
	}
	wantCmd := `/System/Volumes/Preboot/Cryptexes/App/System/Library/StagedFrameworks/Safari/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking`
	if d.Command != wantCmd {
		t.Errorf("Command = %q, want the full dotted path verbatim", d.Command)
	}
	if d.Path != wantCmd {
		t.Errorf("Path = %q, want the comm= full path (NOT carved from argv0)", d.Path)
	}
	if d.Name != "com.apple.WebKit.Networking" {
		t.Errorf("Name = %q, want basename of the path", d.Name)
	}
	if d.ParentName != "launchd" {
		t.Errorf("ParentName = %q, want basename(/sbin/launchd)", d.ParentName)
	}
	if len(d.Connections) != 7 {
		t.Fatalf("len(Connections) = %d, want 7", len(d.Connections))
	}
	want := []ProcConn{
		{RemoteIP: "104.18.32.47", RemotePort: 443},
		{RemoteIP: "104.18.39.21", RemotePort: 443},
		{RemoteIP: "104.18.39.21", RemotePort: 443},
		{RemoteIP: "104.18.32.47", RemotePort: 443},
		{RemoteIP: "104.18.39.21", RemotePort: 443},
		{RemoteIP: "13.227.180.4", RemotePort: 443},
		{RemoteIP: "13.227.180.4", RemotePort: 443},
	}
	if !reflect.DeepEqual(d.Connections, want) {
		t.Errorf("Connections = %+v\nwant %+v (order preserved, (ESTABLISHED) dropped)", d.Connections, want)
	}
	if !d.SnapshotT.Equal(now) {
		t.Errorf("SnapshotT not stamped from now")
	}
}

// ---- parent comm= basename (full path, NOT truncated) ---------------------------

func TestParseProcDetailParentBasename(t *testing.T) {
	now := time.Now()
	cases := []struct{ parentComm, want string }{
		{"/sbin/launchd\n", "launchd"},
		{"/usr/sbin/systemstats\n", "systemstats"},
		{"", ""}, // empty parent output => ParentName omitted
	}
	for _, c := range cases {
		d := parseProcDetail(psWebKit, commWebKit, c.parentComm, "", 13595, now)
		if d.ParentName != c.want {
			t.Errorf("parentComm %q => ParentName %q, want %q", c.parentComm, d.ParentName, c.want)
		}
	}
}

// ---- dotted/spaced command + comm-sourced path (§8-A/§8-B) ----------------------

func TestParseProcDetailSpacedAppPath(t *testing.T) {
	now := time.Now()
	// An app executable path WITH a space, and a command line with args — the spaced
	// path cannot be carved from argv0, so Path must come from comm=.
	ps := `777     1 adam Mon Sep 14 15:12:25 2026     /Applications/Some App.app/Contents/MacOS/Some App --type renderer`
	comm := "/Applications/Some App.app/Contents/MacOS/Some App\n"
	d := parseProcDetail(ps, comm, "", "", 777, now)
	if !d.Available {
		t.Fatalf("should be available")
	}
	if d.PID != 777 || d.PPID != 1 || d.User != "adam" {
		t.Errorf("identity corrupted by the spaced command: pid=%d ppid=%d user=%q", d.PID, d.PPID, d.User)
	}
	if d.Started != "Mon Sep 14 15:12:25 2026" {
		t.Errorf("Started = %q (spaced command must not shift lstart)", d.Started)
	}
	if d.Command != "/Applications/Some App.app/Contents/MacOS/Some App --type renderer" {
		t.Errorf("Command = %q, want the spaced command verbatim", d.Command)
	}
	if d.Path != "/Applications/Some App.app/Contents/MacOS/Some App" {
		t.Errorf("Path = %q, want the comm= full spaced path (not carved from argv0)", d.Path)
	}
	if d.Name != "Some App" {
		t.Errorf("Name = %q, want basename of the spaced path", d.Name)
	}
}

// ---- VERBATIM internal multi-space command preserved (§8-G) ---------------------

func TestParseProcDetailMultiSpaceCommand(t *testing.T) {
	now := time.Now()
	// An internal DOUBLE space inside the command must survive byte-for-byte (the
	// skip-8-fields raw remainder, NOT a Fields rejoin that collapses runs).
	ps := `888 1 adam Mon Sep 14 15:12:25 2026     /opt/tool  --weird   spacing`
	d := parseProcDetail(ps, "/opt/tool\n", "", "", 888, now)
	if d.Command != "/opt/tool  --weird   spacing" {
		t.Errorf("Command = %q, want internal multi-space runs preserved verbatim", d.Command)
	}
}

// ---- IPv6 bracketed rows: bare IP + rsplit port ---------------------------------

func TestParseLsofConnsIPv6(t *testing.T) {
	conns := parseLsofConns(lsofIPv6)
	want := []ProcConn{
		{RemoteIP: "fe80:b::8cd:7269:b9b8:64c5", RemotePort: 62019},
		{RemoteIP: "fe80:14::778e:640f:93cc:ed7b", RemotePort: 1024},
	}
	if !reflect.DeepEqual(conns, want) {
		t.Errorf("IPv6 parse = %+v\nwant %+v (bare un-bracketed IP, port rsplit on last colon)", conns, want)
	}
}

// ---- space-containing lsof COMMAND column: scan for "->" not a fixed index ------

func TestParseLsofConnsSpaceCommand(t *testing.T) {
	conns := parseLsofConns(lsofSpaceCmd)
	want := []ProcConn{{RemoteIP: "2.18.67.82", RemotePort: 443}}
	if !reflect.DeepEqual(conns, want) {
		t.Errorf("space-COMMAND parse = %+v, want %+v (token-with-arrow, not fixed index)", conns, want)
	}
}

// ---- empty lsof => connections:[] (not null); live process, zero connections ----

func TestParseProcDetailEmptyLsof(t *testing.T) {
	now := time.Now()
	// launchd pid 1: ps identifies it, lsof -a prints NOTHING and exits 1 (no
	// established TCP). That is available:true with an EMPTY, non-nil connections slice
	// — NOT available:false.
	ps := `1     0 root Mon Sep 14 15:00:00 2026     /sbin/launchd`
	d := parseProcDetail(ps, "/sbin/launchd\n", "", "", 1, now)
	if !d.Available {
		t.Fatalf("a live process with zero connections must be available:true")
	}
	if d.Connections == nil {
		t.Fatalf("Connections must be [] (non-nil), never null")
	}
	if len(d.Connections) != 0 {
		t.Errorf("Connections = %+v, want empty", d.Connections)
	}
}

// ---- no-such-pid => available:false, empty detail, connections [] ---------------

func TestParseProcDetailDeadPID(t *testing.T) {
	now := time.Now()
	// A dead / out-of-range pid: ps emits no data row (empty stdout; the "process id
	// too large" text is on stderr, excluded by Output()). available:false, []-not-null.
	for _, ps := range []string{"", "\n", "   \n"} {
		d := parseProcDetail(ps, "", "", "", 999999, now)
		if d.Available {
			t.Errorf("ps=%q should be available:false", ps)
		}
		if d.Connections == nil {
			t.Errorf("ps=%q Connections must be [] non-nil", ps)
		}
		if d.Command != "" || d.Path != "" || d.User != "" {
			t.Errorf("ps=%q dead pid must carry an empty detail, got %+v", ps, d)
		}
	}
}

// ---- a non-numeric first token is NOT a data row (available:false) --------------

func TestParseProcDetailNonNumericFirstToken(t *testing.T) {
	now := time.Now()
	// Defensive: even if a stray line reached the parser, a non-numeric first token
	// gates availability off.
	d := parseProcDetail("ps: process id too large: 99999999999", "", "", "", 1, now)
	if d.Available {
		t.Errorf("a stray stderr-like line must NOT be read as a data row")
	}
}

// ---- listen-only / "*:*" / no-arrow rows are skipped ----------------------------

func TestParseLsofConnsSkipsNonEstablished(t *testing.T) {
	// A listen row ("*:*") and the header both lack "->" and must be skipped.
	block := `COMMAND   PID USER   FD   TYPE  DEVICE SIZE/OFF NODE NAME
someproc  123 adam    5u  IPv4  0x0      0t0  TCP *:8080 (LISTEN)
someproc  123 adam    6u  IPv4  0x0      0t0  TCP 10.0.0.1:52000->93.184.216.34:443 (ESTABLISHED)`
	conns := parseLsofConns(block)
	want := []ProcConn{{RemoteIP: "93.184.216.34", RemotePort: 443}}
	if !reflect.DeepEqual(conns, want) {
		t.Errorf("parse = %+v, want only the ESTABLISHED peer %+v", conns, want)
	}
}

// ---- THE `-a` GUARD: the exec argv MUST include -a (and the scoping tokens) ------

func TestLsofArgsHasDashA(t *testing.T) {
	args := lsofArgs(13595)
	want := []string{"-nP", "-a", "-p", "13595", "-iTCP", "-sTCP:ESTABLISHED"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("lsofArgs = %v, want %v", args, want)
	}
	// Explicit: the -a (AND) flag is the single most important correctness token.
	found := false
	for _, a := range args {
		if a == "-a" {
			found = true
		}
	}
	if !found {
		t.Error("lsofArgs MUST contain -a; without it lsof ORs filters and leaks EVERY process's sockets")
	}
	// Demonstration of WHY: the no-`-a` capture leaks foreign pids. The parser trusts
	// -p scoping, so it would happily surface those foreign connections if -a were
	// dropped from the argv.
	leaked := parseLsofConns(lsofNoDashA)
	if len(leaked) == 0 {
		t.Error("the no-`-a` fixture should contain foreign connections (the leak -a prevents)")
	}
}

// ---- validProcPID: the untrusted-input guard ------------------------------------

func TestValidProcPID(t *testing.T) {
	bad := []string{
		"", "abc", "-1", "0", "1.5", "0x10", "99999999999999999999",
		"1 2", "1;rm -rf /", "$(id)", "../1", "1%0a", " 1\t", "100000", "999999",
	}
	for _, s := range bad {
		if _, ok := validProcPID(s); ok {
			t.Errorf("validProcPID(%q) = ok, want rejected", s)
		}
	}
	good := map[string]int{"1": 1, "13595": 13595, "99999": 99999, "+1": 1, "01": 1}
	for s, want := range good {
		n, ok := validProcPID(s)
		if !ok || n != want {
			t.Errorf("validProcPID(%q) = (%d,%v), want (%d,true)", s, n, ok, want)
		}
	}
}

// ---- isGlobalIP: skip non-global so revDNS only PTRs public addresses -----------

func TestIsGlobalIP(t *testing.T) {
	global := []string{"104.18.32.47", "13.227.180.4", "2606:4700::1"}
	for _, s := range global {
		if !isGlobalIP(net.ParseIP(s)) {
			t.Errorf("isGlobalIP(%s) = false, want true", s)
		}
	}
	nonGlobal := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1",
		"fe80::1", "0.0.0.0", "224.0.0.1", "100.64.0.1", "100.127.255.255", "::1"}
	for _, s := range nonGlobal {
		if isGlobalIP(net.ParseIP(s)) {
			t.Errorf("isGlobalIP(%s) = true, want false (skip PTR)", s)
		}
	}
	if isGlobalIP(nil) {
		t.Error("isGlobalIP(nil) must be false")
	}
}

// ---- resolveConns: dedupe, cap, bound, raw-IP fallback --------------------------

func TestResolveConnsBounded(t *testing.T) {
	// Swap in a controllable resolver: a FIXED name for every global IP, instant.
	orig := lookupAddrFunc
	defer func() { lookupAddrFunc = orig }()
	var calls int
	var mu sync.Mutex
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{"host-" + ip + ".example.com."}, nil
	}
	// 7 WebKit conns, 3 distinct global IPs. Non-global/private must be skipped.
	conns := parseLsofConns(lsofWebKit)
	conns = append(conns, ProcConn{RemoteIP: "10.0.0.5", RemotePort: 22}) // private: skipped
	resolveConns(context.Background(), conns)

	// Exactly 3 distinct lookups (dedup of 104.18.39.21 x3, 104.18.32.47 x2,
	// 13.227.180.4 x2; the private 10.0.0.5 skipped).
	if calls != 3 {
		t.Errorf("lookup calls = %d, want 3 distinct (deduped, non-global skipped)", calls)
	}
	// Every global conn got a name (including the duplicates mapped back); trailing dot
	// trimmed; the private one keeps RemoteHost empty.
	for _, c := range conns {
		if c.RemoteIP == "10.0.0.5" {
			if c.RemoteHost != "" {
				t.Errorf("private IP must keep raw (no PTR): %+v", c)
			}
			continue
		}
		if c.RemoteHost != "host-"+c.RemoteIP+".example.com" {
			t.Errorf("conn %+v RemoteHost not mapped / dot not trimmed", c)
		}
	}
}

// ---- resolveConns: a slow resolver cannot exceed the overall cap ----------------

func TestResolveConnsSlowResolverBound(t *testing.T) {
	orig := lookupAddrFunc
	defer func() { lookupAddrFunc = orig }()
	// A resolver that blocks until ITS ctx is cancelled (never returns a name) — the
	// overall cap must still unblock resolveConns.
	lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	conns := parseLsofConns(lsofWebKit)
	start := time.Now()
	resolveConns(context.Background(), conns)
	elapsed := time.Since(start)
	// Must return by ~revDNSOverallCap (1s), never hang; allow margin for scheduling.
	if elapsed > revDNSOverallCap+500*time.Millisecond {
		t.Errorf("resolveConns took %v, must be bounded by revDNSOverallCap=%v", elapsed, revDNSOverallCap)
	}
	// On the cap firing, the core is intact: raw IPs, no names.
	for _, c := range conns {
		if c.RemoteHost != "" {
			t.Errorf("slow resolver should resolve nothing; got %+v", c)
		}
		if c.RemoteIP == "" {
			t.Errorf("core raw IP must survive a resolver timeout")
		}
	}
}

// ---- formatProcDetail: the --proc one-shot table --------------------------------

func TestFormatProcDetail(t *testing.T) {
	now := time.Date(2026, 10, 6, 14, 30, 15, 0, time.UTC)
	d := parseProcDetail(psWebKit, commWebKit, "/sbin/launchd\n", lsofWebKit, 13595, now)
	out := formatProcDetail(d)
	for _, want := range []string{
		"PROCESS DETAIL", "13595", "com.apple.WebKit.Networking", "launchd",
		"adam", "Tue Oct  6 09:45:28 2026", "104.18.32.47:443",
		"ESTABLISHED connections (7)", "Snapshot at 14:30:15", "No sudo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatProcDetail missing %q:\n%s", want, out)
		}
	}

	// Unavailable => honest one line, no crash, no sudo prompt.
	un := formatProcDetail(ProcDetail{PID: 4242, Connections: []ProcConn{}})
	if !strings.Contains(un, "no longer running") || !strings.Contains(un, "4242") {
		t.Errorf("unavailable formatProcDetail = %q", un)
	}

	// IPv6 re-bracketed for display.
	d6 := parseProcDetail(`999 1 adam Mon Sep 14 15:12:25 2026     /x`, "/x\n", "", lsofIPv6, 999, now)
	out6 := formatProcDetail(d6)
	if !strings.Contains(out6, "[fe80:b::8cd:7269:b9b8:64c5]:62019") {
		t.Errorf("IPv6 should be re-bracketed in the terminal table:\n%s", out6)
	}
}
