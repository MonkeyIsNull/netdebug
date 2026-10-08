package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDefaultJSONHasNoOptInStanzas guards the "default run unchanged" claim: a
// NetResult with all opt-in pointers nil must serialize without any of the
// opt-in keys.
func TestDefaultJSONHasNoOptInStanzas(t *testing.T) {
	r := &NetResult{Network: "MyNetwork", IP: "192.168.1.10"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{`"signal"`, `"ipv6"`, `"proxy"`, `"extras"`} {
		if strings.Contains(s, key) {
			t.Errorf("default JSON unexpectedly contains %s: %s", key, s)
		}
	}
}

// TestOptInStanzasMarshalRoundTrip populates each pointer once and checks the
// documented keys survive a marshal->unmarshal. This also catches any
// reintroduced JSON tag collision (e.g. two fields sharing one tag), even if a
// future toolchain's vet misses it.
func TestOptInStanzasMarshalRoundTrip(t *testing.T) {
	orig := &NetResult{
		Network: "MyNetwork",
		Signal:  &SignalStats{Polls: 5, Samples: 5, RSSIAvg: -43, SNRAvg: 49, RateAvg: 1150, PHY: "802.11ax", Channel: "149/80"},
		IPv6:    &IPv6Result{GlobalAddr: "2600::1", Gateway: "fe80::1%en0", PingOK: true, AAAAOK: true},
		Proxy:   &ProxyResult{HTTPProxy: "10.0.0.9:3128", Tunnels: []string{"utun0"}, VPNActive: false},
		Extras: &ExtrasResult{
			GatewayMAC:  "24:a4:3c:aa:bb:cc",
			ARPComplete: true,
			MTU:         []MTUResult{{Size: 1472, OK: true}},
			Lease:       DHCPLease{ServerID: "192.168.1.1", LeaseSecs: 86400, DomainName: "lan"},
		},
	}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{`"signal"`, `"ipv6"`, `"proxy"`, `"extras"`} {
		if !strings.Contains(s, key) {
			t.Errorf("populated JSON missing %s", key)
		}
	}

	var back NetResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	// Verify a couple of fields per stanza survived (catches a tag collision
	// that would drop one of two fields sharing a tag).
	if back.Signal == nil || back.Signal.RSSIAvg != -43 || back.Signal.SNRAvg != 49 {
		t.Errorf("signal round-trip lost data: %+v", back.Signal)
	}
	if back.IPv6 == nil || back.IPv6.GlobalAddr != "2600::1" || !back.IPv6.PingOK {
		t.Errorf("ipv6 round-trip lost data: %+v", back.IPv6)
	}
	if back.Proxy == nil || back.Proxy.HTTPProxy != "10.0.0.9:3128" {
		t.Errorf("proxy round-trip lost data: %+v", back.Proxy)
	}
	if back.Extras == nil || back.Extras.Lease.LeaseSecs != 86400 || back.Extras.GatewayMAC != "24:a4:3c:aa:bb:cc" {
		t.Errorf("extras round-trip lost data: %+v", back.Extras)
	}
}

// reachExecSurface is the Phase-7 set that must stay SERVE-ONLY: the reach loop
// entry points (readReach/runReachLoop/newReachGate) plus the four impure exec
// wrappers that actually spawn a probe — pingDetail (/sbin/ping), dnsResolveTime
// (the net.Resolver-timed lookup), readPowerSource (pmset -g batt) and
// readScreenLocked (ioreg -n Root). The §0 LOCKED DECISION is that a plain
// ./netdebug and --sample spawn ZERO ping/DNS/pmset/ioreg; these symbols are the
// only code that can.
var reachExecSurface = []string{
	"readReach", "runReachLoop", "newReachGate",
	"pingDetail", "dnsResolveTime", "readPowerSource", "readScreenLocked",
	// REACHABILITY drill-down: the reverse-DNS names refresher (the only new network
	// work) must stay serve-only too — referenced ONLY in reach.go (def) + serve.go
	// (construct), so a bare/--sample run can never warm the PTR cache.
	"runReachNamesRefresh",
}

// TestReachSurfaceServeOnlySeam is the STRUCTURAL half of the plan's §4
// "DEFAULT-PATH ZERO-PROBE (crown jewel)" guard. It parses every non-test source
// file in the package and asserts that each reach exec surface symbol is
// referenced ONLY from its definition file (reach.go / reach_gate.go) and from
// serve.go, where serveDashboard is the sole construction site. In particular it
// is NEVER referenced from main.go — so neither the --sample dispatch
// (main.go:159) nor the bare run (main.go:294+) can construct a reach reader or
// reach the ping/DNS/pmset/ioreg execs. It also asserts the package has no init()
// func, so no package initialization can exec a probe. This covers the two exec
// surfaces a PATH-shim cannot (the ABSOLUTE /sbin/ping, and the exec-free DNS
// lookup), which the runtime guard below cannot reach.
func TestReachSurfaceServeOnlySeam(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	// refs[symbol] = set of non-test files that reference it as an identifier.
	refs := map[string]map[string]bool{}
	var initFuncs []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				// Idents exclude comments, so a comment mentioning a symbol
				// (serve.go and reach.go both narrate the exec surface) never
				// counts as a reference.
				if refs[x.Name] == nil {
					refs[x.Name] = map[string]bool{}
				}
				refs[x.Name][name] = true
			case *ast.FuncDecl:
				if x.Recv == nil && x.Name.Name == "init" {
					initFuncs = append(initFuncs, name)
				}
			}
			return true
		})
	}

	// reach.go defines readReach/runReachLoop/pingDetail/dnsResolveTime;
	// reach_gate.go defines newReachGate/readPowerSource/readScreenLocked;
	// serve.go constructs them inside serveDashboard. Nothing else may touch them.
	allowed := map[string]bool{"reach.go": true, "reach_gate.go": true, "serve.go": true}
	// Phase 12: readReach gains EXACTLY ONE additional sanctioned reference site —
	// alerts.go's localStatus, the --status standalone one-probe fallback. --status is
	// a DELIBERATE non-default dispatch (the plan documents it as not packet-free), and
	// the bare/--sample/plain path never calls fetchStatus, so the crown-jewel
	// invariant (default path spawns ZERO probe) is still fully guarded: by the runtime
	// guard below (it runs --sample and asserts no ping/pmset/ioreg) and by the
	// "never in main.go" assertion (main.go calls fetchStatus, never readReach itself).
	// No OTHER reach exec surface may appear in alerts.go.
	extraAllowed := map[string]map[string]bool{"readReach": {"alerts.go": true}}
	for _, sym := range reachExecSurface {
		if len(refs[sym]) == 0 {
			t.Errorf("reach exec surface %q not found in any source file — did it get renamed? update this seam guard", sym)
			continue
		}
		for f := range refs[sym] {
			if !allowed[f] && !extraAllowed[sym][f] {
				t.Errorf("reach exec surface %q referenced in %s; it must be serve-only (reach.go/reach_gate.go/serve.go), else a bare/--sample run could spawn a probe", sym, f)
			}
		}
		if refs[sym]["main.go"] {
			t.Errorf("%q referenced in main.go — the --sample/bare dispatch must take NO reach reader", sym)
		}
	}

	// serveDashboard is the sole construction site for the whole reach surface, so
	// it must itself be reachable only from main's --serve branch: referenced only
	// in serve.go (its definition) and main.go (the single call).
	for f := range refs["serveDashboard"] {
		if f != "serve.go" && f != "main.go" {
			t.Errorf("serveDashboard referenced in %s; expected only serve.go (defn) and main.go (--serve call)", f)
		}
	}

	if len(initFuncs) != 0 {
		t.Errorf("package has init() func(s) in %v; a package init must not exist (it would run a probe on EVERY dispatch path, including bare/--sample)", initFuncs)
	}
}

// TestDefaultPathZeroReachExec is the RUNTIME half of the §4 crown-jewel guard: a
// PATH-shim proof that a --sample run spawns ZERO ping / pmset / ioreg processes.
// pmset and ioreg are used NOWHERE but reach_gate.go, and reach's latency ping is
// the only ping on the --sample path (the byte sampler execs netstat only), so a
// shim that wins on PATH for all three catches any future refactor that leaks a
// reach exec onto the default path. It complements TestReachSurfaceServeOnlySeam,
// which covers the two surfaces a PATH-shim cannot: the ABSOLUTE /sbin/ping reach
// uses (bypasses PATH) and the exec-free net.Resolver DNS lookup. The bare run
// shares main.go's dispatch seam (proven clean by the seam test) but does real
// network probing, so it is deliberately left to the structural guard.
func TestDefaultPathZeroReachExec(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found; structural seam test still guards the surface")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "netdebug")
	if out, err := exec.Command(goBin, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build netdebug: %v\n%s", err, out)
	}

	shimDir := filepath.Join(tmp, "shims")
	markerDir := filepath.Join(tmp, "markers")
	for _, d := range []string{shimDir, markerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Each shim records that it was invoked, then exits 0. If the --sample path
	// ever execs one of these (bare name resolved via PATH), the marker appears and
	// the test fails.
	shims := []string{"ping", "pmset", "ioreg"}
	for _, n := range shims {
		marker := filepath.Join(markerDir, n)
		script := "#!/bin/sh\necho called >> \"" + marker + "\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(shimDir, n), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// --iface lo0 skips detectInterface() (which execs networksetup/route on the
	// resolution path shared by every dispatch) so the only execs left are the
	// byte sampler's netstat — isolating the reach surface. --sample-seconds 1
	// stops the sampler after ~1s.
	cmd := exec.CommandContext(ctx, bin, "--sample", "--sample-seconds", "1", "--iface", "lo0")
	cmd.Env = append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run --sample: %v\n%s", err, out)
	}

	for _, n := range shims {
		if _, err := os.Stat(filepath.Join(markerDir, n)); err == nil {
			t.Errorf("--sample execed %q — the default path must spawn ZERO ping/pmset/ioreg (reach probes are serve-only)", n)
		}
	}
}
