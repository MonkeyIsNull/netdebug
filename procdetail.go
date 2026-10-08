package main

// procdetail.go — PROCESS DRILL-DOWN (post-Phase-11 enhancement). "What IS this
// process and why is it sending traffic?" Clicking a TOP TALKERS row fetches this
// per-process detail ON DEMAND from the loopback /proc endpoint: command line,
// executable path, parent, user, start time, and the live ESTABLISHED remote
// endpoints (best-effort reverse-DNS named). A sub-agent proved all of this is
// available sudo-free via ps + lsof (the opaque "2.1.258" uploader was Claude Code
// itself, POSTing context to api.anthropic.com).
//
// Pure/impure split (house style): parseProcDetail / parseLsofConns / basename /
// isGlobalIP / lsofArgs / validProcPID / formatProcDetail are PURE and table-tested
// (procdetail_test.go, against VERBATIM ps + lsof captures); only readProcDetail and
// resolveConns exec / do IO. This file is the ONLY non-test file permitted to name
// /bin/ps or /usr/sbin/lsof (the zero-default guard in serve_test.go enforces that),
// so a bare run / --sample spawns ZERO ps/lsof.
//
// SECURITY (the crux): the pid is BROWSER-SUPPLIED untrusted input. The caller MUST
// validate it as a positive base-10 integer (validProcPID) and reject anything else
// with HTTP 400 BEFORE the value ever reaches an argv — mirrors reach.go's
// net.ParseIP gate. Exec is argv (exec.CommandContext, never a shell), and every
// detail string returned to the browser (command, path, parent_name, user,
// remote_ip, AND remote_host — a reverse-DNS PTR controlled by whoever owns the
// remote peer's reverse zone, the worst case) is rendered via textContent ONLY.
//
// NO SUDO: ps + lsof see the invoking user's processes without elevation; a
// dead/unidentifiable pid DEGRADES to available:false (never a crash, never a sudo
// prompt). A hung lsof can never stall serve: CommandContext + cmd.WaitDelay reap a
// D-state child past the deadline, and the reverse-DNS overlay is bounded by ONE
// overall deadline.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// procDetailExecTimeout bounds ALL of this feature's execs (ps identity, pid
	// comm=, parent comm=, lsof) under ONE CommandContext budget — ps + lsof are fast.
	procDetailExecTimeout = 4 * time.Second
	// procDetailWaitDelay is cmd.WaitDelay (go 1.20+): a stuck D-state child (lsof on a
	// dead NFS mount / wedged socket) that CommandContext's ctx-kill cannot promptly
	// reap is force-abandoned this long after the deadline, so cmd.Output() cannot pin
	// the handler goroutine past the deadline. This is what makes "a hung lsof never
	// stalls serve" actually TRUE.
	procDetailWaitDelay = 1 * time.Second
	// revDNSOverallCap is the ONE deadline wrapping ALL reverse-DNS PTR lookups for a
	// single /proc click (they run concurrently, so total added latency is this cap
	// regardless of connection count). Worst-case handler latency is STRUCTURALLY
	// procDetailExecTimeout + revDNSOverallCap and can never exceed it.
	revDNSOverallCap = 1 * time.Second
	// revDNSPerLookup bounds one PTR lookup (a child ctx of the overall cap).
	revDNSPerLookup = 800 * time.Millisecond
	// revDNSMaxLookups is a HARD ceiling on DISTINCT reverse-DNS lookups per click, a
	// real early-return so one expand cannot fan out into a DNS storm; connections
	// beyond the cap simply show their raw IP. The connections[] array itself is NEVER
	// capped.
	revDNSMaxLookups = 8
	// procPIDCeiling rejects an over-large pid in the 400 path (BEFORE exec) so the
	// "process id too large" class is caught at the guard rather than degrading to
	// available:false. Darwin's traditional PID_MAX is 99999 (ps rejects 999999 as
	// "process id too large"); a value above this ceiling is refused.
	procPIDCeiling = 99999
)

// ProcConn is one ESTABLISHED remote endpoint parsed from an lsof NAME column.
// RemoteIP is the BARE peer IP after "->" (IPv6 brackets STRIPPED) so a later
// net.ParseIP / LookupAddr works; the UI re-brackets IPv6 for display.
type ProcConn struct {
	RemoteIP   string `json:"remote_ip"`
	RemotePort int    `json:"remote_port"`
	RemoteHost string `json:"remote_host,omitempty"` // best-effort reverse-DNS; omitted on miss
}

// ProcDetail is the WIRE type for GET /proc?pid=<n>. Connections is NORMALIZED to []
// (never null) by the pure builder — the same []-not-null house rule as
// procsJSON/dataJSON. Available:false carries an honest empty detail. Every string
// field is attacker-INFLUENCABLE and MUST render via textContent, never innerHTML.
type ProcDetail struct {
	PID         int        `json:"pid"`
	Name        string     `json:"name,omitempty"`    // basename of Path (adversarial: textContent)
	Command     string     `json:"command,omitempty"` // full command line, verbatim (adversarial)
	Path        string     `json:"path,omitempty"`    // executable path from `comm=` (adversarial)
	PPID        int        `json:"ppid,omitempty"`
	ParentName  string     `json:"parent_name,omitempty"` // basename of parent `comm=` (adversarial)
	User        string     `json:"user,omitempty"`
	Started     string     `json:"started,omitempty"` // ps lstart verbatim ("Tue Oct  6 09:45:28 2026")
	Connections []ProcConn `json:"connections"`       // ALWAYS [] not null; empty is a valid live result
	Available   bool       `json:"available"`
	SnapshotT   time.Time  `json:"snapshot_t"` // stamped at read instant (point-in-time label)
}

// validProcPID is the untrusted-input guard: a pid is valid iff it is a positive
// base-10 integer at or below procPIDCeiling. The caller rejects everything else
// with HTTP 400 BEFORE the value ever reaches an argv (mirrors reach.go's
// net.ParseIP gate). strconv.Atoi rejects non-numeric / spaces / overflow /
// hex / "1;rm" / "$(id)" / "../1"; the sign and ceiling checks reject <=0 and
// over-large. A valid return re-serializes through strconv.Itoa into argv, so there
// is structurally no injection surface even if a byte slipped the parse.
func validProcPID(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > procPIDCeiling {
		return 0, false
	}
	return n, true
}

// lsofArgs returns the EXACT argv for the per-pid established-TCP lsof. The `-a`
// (AND) flag is MANDATORY and is the single most important correctness token: lsof
// ORs its selection filters by DEFAULT, so `-nP -p <pid> -iTCP -sTCP:ESTABLISHED`
// WITHOUT `-a` returns EVERY process's TCP sockets (a real capture: 221 lines vs 6
// with `-a`) — dropping it is both a correctness bug and a mild info-exposure. The
// `-a` scoping is enforced by the EXEC string, not the parser (the parser trusts
// `-p` scoping), so the guard test pins this argv.
func lsofArgs(pid int) []string {
	return []string{"-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:ESTABLISHED"}
}

// basename returns the final path element, or "" for an empty input. comm= on macOS
// is a FULL path (NOT truncated — the exec is `-o comm=` with a null header, which
// prints the whole path), so Name/ParentName REQUIRE basename:
// "/sbin/launchd" => "launchd", "/usr/sbin/systemstats" => "systemstats".
func basename(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// fieldOffset returns the byte offset in s of the first character AFTER skipping n
// whitespace-delimited fields (and any whitespace before the remainder). It is the
// key to keeping a spaced value (the command line, or a VERBATIM internal
// multi-space run) byte-exact: a strings.Fields rejoin would collapse multi-space
// runs. Used to carve the raw command remainder and the raw lstart substring.
func fieldOffset(s string, n int) int {
	i, L := 0, len(s)
	for f := 0; f < n; f++ {
		for i < L && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		for i < L && s[i] != ' ' && s[i] != '\t' {
			i++
		}
	}
	for i < L && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// parseProcDetail builds a ProcDetail from the ps identity line (the 5-field exec
// `-o pid=,ppid=,user=,lstart=,command=`, command LAST), the pid comm= (Path), the
// parent comm= (ParentName), and the lsof block. PURE: no exec, no IO, never panics.
//
// AVAILABILITY = "a ps identity data row parsed" — NOT the exit code (empty lsof
// exits 1; a dead pid's ps exits non-zero). Gate: strings.Fields(psLine) has len>=9
// (pid, ppid, user, 5-token lstart, >=1 command token) AND tokens[0] parses as a
// positive int. A non-numeric first token (a stray stderr line, though Output()
// already excludes stderr) => no data row => available:false.
//
// The identity parse is TOKEN-COUNTING, NOT fixed-width: column offsets shift with
// the lstart day-pad ("Oct  6" vs "Oct 16") and pid width. lstart (tokens 3..7) and
// Command (the remainder after the 8th field) are carved as RAW byte-substrings so
// the internal double-space day-pad and any internal multi-space command run are
// preserved VERBATIM, not collapsed by a Fields rejoin. An embedded-newline command
// truncates at the first line — a graceful display degrade, never a crash.
func parseProcDetail(psLine, pidCommOut, parentCommOut, lsofOut string, pid int, now time.Time) ProcDetail {
	d := ProcDetail{PID: pid, Connections: []ProcConn{}, SnapshotT: now}

	line := strings.TrimRight(psLine, "\r\n")
	f := strings.Fields(line)
	if len(f) < 9 {
		return d // no data row (dead / unidentifiable pid) => available:false
	}
	p0, err := strconv.Atoi(f[0])
	if err != nil || p0 <= 0 {
		return d // first token not a positive int => not a real ps data row
	}
	d.Available = true
	d.PID = p0
	if ppid, err := strconv.Atoi(f[1]); err == nil && ppid > 0 {
		d.PPID = ppid
	}
	d.User = f[2]
	// lstart = the RAW substring spanning tokens 3..7 (verbatim, double-space day-pad
	// preserved); Command = the RAW remainder after the 8th field (verbatim).
	off3 := fieldOffset(line, 3)
	off8 := fieldOffset(line, 8)
	if off8 <= len(line) && off3 <= off8 {
		d.Started = strings.TrimRight(line[off3:off8], " \t")
		d.Command = line[off8:]
	}

	d.Path = strings.TrimSpace(pidCommOut)
	d.Name = basename(d.Path)
	if pn := basename(strings.TrimSpace(parentCommOut)); pn != "" {
		d.ParentName = pn
	}

	d.Connections = parseLsofConns(lsofOut)
	return d
}

// parseLsofConns parses the foreign ESTABLISHED endpoints from an lsof block. PURE.
// Per row it scans the whitespace Fields for the token CONTAINING "->" (NOT a fixed
// index — lsof's COMMAND column is truncated to 9 chars and CAN contain a space,
// e.g. "Google Ch", shifting fixed indices; the header line and "*:*" listen rows
// have no "->" and fall out). It takes the foreign side AFTER "->", splits host:port
// on the LAST ":" (so a bracketed IPv6 "[..]:443" survives), and STRIPS the IPv6
// brackets into a BARE RemoteIP. The trailing "(ESTABLISHED)" is its own Fields
// token and is naturally excluded. Always returns a non-nil slice ([]-not-null): an
// empty lsof (a live process with zero connections) yields [].
func parseLsofConns(lsofOut string) []ProcConn {
	conns := []ProcConn{}
	for _, line := range strings.Split(lsofOut, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		var tok string
		for _, fld := range strings.Fields(line) {
			if strings.Contains(fld, "->") {
				tok = fld
				break
			}
		}
		if tok == "" {
			continue // header / listen-only / "*:*" / any row with no foreign peer
		}
		foreign := tok[strings.Index(tok, "->")+2:]
		colon := strings.LastIndex(foreign, ":") // rsplit so bracketed IPv6 survives
		if colon < 0 {
			continue
		}
		ipPart := foreign[:colon]
		portPart := foreign[colon+1:]
		ipPart = strings.TrimPrefix(ipPart, "[")
		ipPart = strings.TrimSuffix(ipPart, "]")
		if ipPart == "" || ipPart == "*" {
			continue
		}
		port, err := strconv.Atoi(portPart)
		if err != nil || port <= 0 {
			continue
		}
		conns = append(conns, ProcConn{RemoteIP: ipPart, RemotePort: port})
	}
	return conns
}

// isGlobalIP reports whether ip is a globally-routable public address. It began as
// a reverse-DNS "is a PTR worthwhile?" heuristic (PTR of loopback / private / link-
// local / unspecified / multicast / CGNAT addresses mostly NXDOMAINs), and is now
// ALSO the ONE security predicate shared by the speed-button SSRF guard — the pure
// validateSpeedTarget pre-screen AND the dial-time guardedDialContext both call it,
// so they can never drift (speedbutton.go). It rejects loopback / RFC1918 private /
// link-local unicast (169.254/16 incl. the 169.254.169.254 cloud-metadata IP AND
// fe80::/10) / link-local multicast / unspecified (0.0.0.0, ::) / multicast / CGNAT
// (100.64.0.0/10) / NAT64 (64:ff9b::/96, which can embed a private IPv4 a hostile
// resolver supplies). IPv4-mapped IPv6 (::ffff:127.0.0.1 etc.) is normalized by the
// stdlib predicates via To4(), so those map to the same verdict. PURE.
func isGlobalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return false // CGNAT 100.64.0.0/10
	}
	// NAT64 well-known prefix 64:ff9b::/96 — the embedded IPv4 does NOT survive To4(),
	// so a hostile resolver could smuggle a private v4 target behind it; block the
	// whole prefix (a public target is still reachable over native IPv4/IPv6).
	if len(ip) == net.IPv6len && ip.To4() == nil &&
		ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
		return false
	}
	return true
}

// ---- impure layer (thin, NOT unit-tested directly) ------------------------

// lookupAddrFunc is the reverse-DNS seam. It defaults to the Go-native resolver's
// LookupAddr; a test overrides it to inject a slow/controlled resolver (the
// revDNS-bound test). PreferGo is LOAD-BEARING: the default cgo resolver does NOT
// reliably cancel an in-flight getnameinfo, so a wedged resolver would leak
// goroutines and stall PAST the cap. PreferGo bypasses mDNSResponder (a .local/VPN
// name may miss) — acceptable for best-effort. Do NOT "fix" this back to cgo and
// reintroduce the hang vector.
var lookupAddrFunc = func(ctx context.Context, ip string) ([]string, error) {
	r := &net.Resolver{PreferGo: true}
	return r.LookupAddr(ctx, ip)
}

// runPSOutput execs the ABSOLUTE /bin/ps (launchd thins PATH; same hardening as
// reach.go's /sbin/ping) with LC_ALL=C via cmd.Output() (STDOUT only, so the
// "ps: process id too large" STDERR text never reaches the parser — availability is
// by PARSE, not exit code) and cmd.WaitDelay set so a stuck child cannot pin the
// goroutine past the deadline. The pid is ALREADY integer-validated by the caller
// and re-serialized via strconv.Itoa into argv, never a shell.
func runPSOutput(ctx context.Context, args ...string) string {
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = procDetailWaitDelay
	out, _ := cmd.Output()
	return string(out)
}

// runLsof execs the ABSOLUTE /usr/sbin/lsof with the MANDATORY `-a`-scoped argv
// (lsofArgs) under LC_ALL=C + cmd.WaitDelay. An empty output + exit 1 (a live
// process with NO established connections) is the NORMAL "no connections" case and
// is returned as an empty string, which parseLsofConns maps to [].
func runLsof(ctx context.Context, pid int) string {
	cmd := exec.CommandContext(ctx, "/usr/sbin/lsof", lsofArgs(pid)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = procDetailWaitDelay
	out, _ := cmd.Output()
	return string(out)
}

// readProcDetail execs /bin/ps (identity) + /bin/ps -o comm= (path) + /usr/sbin/lsof
// -a (connections), then /bin/ps -o comm= on the ppid (parent name, ONLY if ppid>0),
// all under ONE CommandContext (procDetailExecTimeout). Returns a ProcDetail;
// available:false (empty detail) on exec error / timeout / no ps data row. If
// resolveDNS, the bounded reverse-DNS overlay (resolveConns) runs AFTER the core is
// built, as a purely additive step that can only ADD remote_host, never fail the
// detail. ctx is threaded so a client disconnect / server shutdown aborts an
// in-flight ps/lsof/PTR. The pid is ALREADY validated by the caller.
func readProcDetail(ctx context.Context, pid int, resolveDNS bool) ProcDetail {
	now := time.Now()
	cctx, cancel := context.WithTimeout(ctx, procDetailExecTimeout)
	defer cancel()

	psLine := runPSOutput(cctx, "-p", strconv.Itoa(pid), "-o", "pid=,ppid=,user=,lstart=,command=")
	pidComm := runPSOutput(cctx, "-p", strconv.Itoa(pid), "-o", "comm=")
	lsofOut := runLsof(cctx, pid)

	// A first parse (pure, cheap, no IO) learns the ppid so the parent comm= exec runs
	// ONLY when ppid>0; the second parse folds in the parent output as the single
	// source of truth for ParentName.
	core := parseProcDetail(psLine, pidComm, "", lsofOut, pid, now)
	parentComm := ""
	if core.Available && core.PPID > 0 {
		parentComm = runPSOutput(cctx, "-p", strconv.Itoa(core.PPID), "-o", "comm=")
	}
	d := parseProcDetail(psLine, pidComm, parentComm, lsofOut, pid, now)

	if resolveDNS && d.Available && len(d.Connections) > 0 {
		resolveConns(ctx, d.Connections)
	}
	return d
}

// resolveConns is the bounded reverse-DNS overlay. It mutates RemoteHost in place and
// can only ever ADD a name; the core detail (raw IPs) is already complete without it.
// Order: dedupe distinct RemoteIPs -> skip non-global (ParseIP-gated) -> truncate to
// revDNSMaxLookups distinct (a real early return) -> concurrent PreferGo LookupAddr,
// one goroutine per distinct IP, each with a per-lookup ctx, all under ONE
// revDNSOverallCap ctx -> select(done vs cap). On the cap firing, proceed with
// whatever resolved; NEVER block on stragglers (they unwind on their own ctx; we do
// not wg.Wait on possibly-leaked lookups). The map writes and the final read are both
// mutex-guarded, so a straggler that completes after the select cannot race.
func resolveConns(ctx context.Context, conns []ProcConn) {
	seen := map[string]bool{}
	var distinct []string
	for _, c := range conns {
		if seen[c.RemoteIP] {
			continue
		}
		ip := net.ParseIP(c.RemoteIP)
		if !isGlobalIP(ip) {
			continue
		}
		seen[c.RemoteIP] = true
		distinct = append(distinct, c.RemoteIP)
		if len(distinct) >= revDNSMaxLookups {
			break // hard distinct-lookup ceiling: beyond this, raw IPs show
		}
	}
	if len(distinct) == 0 {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, revDNSOverallCap)
	defer cancel()
	names := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ip := range distinct {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			lctx, lcancel := context.WithTimeout(cctx, revDNSPerLookup)
			defer lcancel()
			addrs, err := lookupAddrFunc(lctx, ip)
			if err != nil || len(addrs) == 0 {
				return
			}
			mu.Lock()
			names[ip] = strings.TrimSuffix(addrs[0], ".")
			mu.Unlock()
		}(ip)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-cctx.Done(): // the overall cap fired: proceed with whatever resolved so far
	}

	mu.Lock()
	defer mu.Unlock()
	for i := range conns {
		if h, ok := names[conns[i].RemoteIP]; ok {
			conns[i].RemoteHost = h
		}
	}
}

// formatProcDetail renders the --proc one-shot terminal table (mirrors
// formatTopTalkers / formatAirspace). PURE (string out). An unavailable detail
// prints an honest one line and NEVER prompts for or requires sudo.
func formatProcDetail(d ProcDetail) string {
	var b strings.Builder
	b.WriteString("netdebug: PROCESS DETAIL — identity + established connections (sudo-free: ps + lsof)\n\n")
	if !d.Available {
		fmt.Fprintf(&b, "pid %d: process no longer running (or not identifiable).\n", d.PID)
		b.WriteString("This is a sudo-free read and we do NOT escalate.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-9s %d\n", "PID", d.PID)
	if d.Name != "" {
		fmt.Fprintf(&b, "  %-9s %s\n", "Name", d.Name)
	}
	if d.User != "" {
		fmt.Fprintf(&b, "  %-9s %s\n", "User", d.User)
	}
	if d.PPID > 0 {
		parent := d.ParentName
		if parent == "" {
			parent = "—"
		}
		fmt.Fprintf(&b, "  %-9s %d (%s)\n", "Parent", d.PPID, parent)
	}
	if d.Started != "" {
		fmt.Fprintf(&b, "  %-9s %s\n", "Started", d.Started)
	}
	if d.Path != "" {
		fmt.Fprintf(&b, "  %-9s %s\n", "Path", d.Path)
	}
	if d.Command != "" {
		fmt.Fprintf(&b, "  %-9s %s\n", "Command", d.Command)
	}
	b.WriteString("\n")
	if len(d.Connections) == 0 {
		b.WriteString("  No established TCP connections right now.\n")
	} else {
		fmt.Fprintf(&b, "  ESTABLISHED connections (%d):\n", len(d.Connections))
		for _, c := range d.Connections {
			ipdisp := c.RemoteIP
			if strings.Contains(ipdisp, ":") {
				ipdisp = "[" + ipdisp + "]" // re-bracket IPv6 for display
			}
			host := ""
			if c.RemoteHost != "" {
				host = "  " + c.RemoteHost
			}
			fmt.Fprintf(&b, "    %s:%d%s\n", ipdisp, c.RemotePort, host)
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Snapshot at %s — connections churn; reverse-DNS is best-effort.\n", d.SnapshotT.Format("15:04:05"))
	b.WriteString("Shows YOUR processes only; some system processes may be hidden without elevated\n")
	b.WriteString("rights. No sudo is used or required.\n")
	return b.String()
}
