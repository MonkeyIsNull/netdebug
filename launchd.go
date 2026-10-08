package main

// Phase 5 — launchd always-on. `netdebug --install-launchd` generates and loads a
// per-user LaunchAgent (NEVER a system LaunchDaemon — no root, ever) that runs
// `netdebug --serve` at GUI login, bound to loopback only.
//
// Pure/impure split (house style):
//   PURE (unit-tested, no IO): renderLaunchdPlist, launchAgentPath,
//   validateLaunchdOpts, checkStableBinaryPath, composeLaunchdArgs,
//   parseLaunchdListOutput, launchctlArgs.
//   IMPURE (thin wrappers): resolveBinaryPath (os.Executable), installLaunchAgent,
//   uninstallLaunchAgent, launchdStatus — all launchctl argv flows through the
//   single launchctlArgs helper so the load/unload mechanism can be swapped without
//   touching the pure core.
//
// SECURITY: validateLaunchdOpts is an ALLOW-LIST gate on the install path. It parses
// the --port out of Args and runs it through validateBindAddr (the SAME loopback
// logic serve.go uses), so an always-on daemon can never be smuggled onto the LAN.
// Install validates BEFORE any side-effect: a rejected config writes no plist and
// runs no launchctl verb.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// launchdPATH is pinned into the plist's EnvironmentVariables so the daemon's
// bare-name execs resolve under launchd even if its implicit default PATH changes:
// system_profiler (/usr/sbin), ifconfig (/sbin), netstat (/usr/bin).
const launchdPATH = "/usr/bin:/bin:/usr/sbin:/sbin"

// defaultLaunchdLabel is the reverse-DNS LaunchAgent label (the user's domain).
const defaultLaunchdLabel = "com.cobenian.netdebug"

// LaunchdOpts holds everything needed to render a plist. Args is the SINGLE source
// of truth for ProgramArguments — there is no separate HistoryDir/Port field that
// could silently diverge from what the daemon actually runs. The install path
// composes Args itself (via composeLaunchdArgs); it never forwards os.Args.
type LaunchdOpts struct {
	Label      string   // reverse-DNS, e.g. "com.cobenian.netdebug"
	BinaryPath string   // absolute, EvalSymlinks'd path to the netdebug binary
	Args       []string // self-composed, e.g. ["--serve","--port","8099","--history-dir","/abs"]
	LogPath    string   // stdout+stderr log (StandardOutPath/StandardErrorPath)
	RunAtLoad  bool
	KeepAlive  bool
}

// renderLaunchdPlist returns a valid LaunchAgent plist XML for opts. PURE: struct
// in, string out, no IO. This is the unit-tested core and is INDEPENDENT of the
// launchctl mechanism.
//
// XML SAFETY: every substituted value goes through encoding/xml (xml.EscapeText),
// so a path containing & < > or " renders well-formed. The fixed scaffolding
// (<key>Label</key>, …) is constant text we control. A fmt.Sprintf("%s") plist is
// forbidden — it emits broken XML for those characters.
//
// Keys emitted (spelling asserted by test): Label, ProgramArguments, RunAtLoad,
// KeepAlive, StandardOutPath, StandardErrorPath, EnvironmentVariables(PATH).
// ProgramArguments is [BinaryPath, <Args...>] in order.
func renderLaunchdPlist(o LaunchdOpts) (string, error) {
	esc := func(s string) (string, error) {
		var b bytes.Buffer
		if err := xml.EscapeText(&b, []byte(s)); err != nil {
			return "", err
		}
		return b.String(), nil
	}

	label, err := esc(o.Label)
	if err != nil {
		return "", err
	}
	logPath, err := esc(o.LogPath)
	if err != nil {
		return "", err
	}

	// ProgramArguments: binary first, then every Arg in order.
	progArgs := make([]string, 0, len(o.Args)+1)
	progArgs = append(progArgs, o.BinaryPath)
	progArgs = append(progArgs, o.Args...)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	b.WriteString("  <key>Label</key>\n")
	fmt.Fprintf(&b, "  <string>%s</string>\n", label)

	b.WriteString("  <key>ProgramArguments</key>\n")
	b.WriteString("  <array>\n")
	for _, a := range progArgs {
		ea, err := esc(a)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "    <string>%s</string>\n", ea)
	}
	b.WriteString("  </array>\n")

	b.WriteString("  <key>RunAtLoad</key>\n")
	b.WriteString("  " + plistBool(o.RunAtLoad) + "\n")
	b.WriteString("  <key>KeepAlive</key>\n")
	b.WriteString("  " + plistBool(o.KeepAlive) + "\n")

	b.WriteString("  <key>StandardOutPath</key>\n")
	fmt.Fprintf(&b, "  <string>%s</string>\n", logPath)
	b.WriteString("  <key>StandardErrorPath</key>\n")
	fmt.Fprintf(&b, "  <string>%s</string>\n", logPath)

	b.WriteString("  <key>EnvironmentVariables</key>\n")
	b.WriteString("  <dict>\n")
	b.WriteString("    <key>PATH</key>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", launchdPATH) // constant, no user input
	b.WriteString("  </dict>\n")

	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String(), nil
}

// plistBool renders a plist boolean as the self-closing element launchd expects.
// A missing key is NOT the same as false, so false is emitted explicitly.
func plistBool(v bool) string {
	if v {
		return "<true/>"
	}
	return "<false/>"
}

// launchAgentPath returns the per-user install path (pure given home):
// ~/Library/LaunchAgents/<label>.plist.
func launchAgentPath(home, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// launchdArgsInput is the typed input to composeLaunchdArgs. A zero/empty field is
// omitted from the rendered Args, so main only fills the optionals it means to set.
type launchdArgsInput struct {
	Port          int           // required
	HistoryDir    string        // "" => omit
	Interval      time.Duration // 0 => omit
	LinkInterval  time.Duration // 0 => omit
	ReachInterval time.Duration // 0 => omit
	Iface         string        // "" => omit
}

// composeLaunchdArgs self-composes the ProgramArguments tail for the daemon. PURE:
// deterministic vector, no IO, no os.Args pass-through. --serve is UNCONDITIONALLY
// first (without it the daemon falls through to the read-only probe path and exits,
// which under KeepAlive is a respawn storm). Order is fixed so a test can assert the
// exact vector: --serve, --port, [--history-dir], [--interval], [--link-interval],
// [--reach-interval], [--iface].
func composeLaunchdArgs(in launchdArgsInput) []string {
	args := []string{"--serve", "--port", strconv.Itoa(in.Port)}
	if in.HistoryDir != "" {
		args = append(args, "--history-dir", in.HistoryDir)
	}
	if in.Interval > 0 {
		args = append(args, "--interval", in.Interval.String())
	}
	if in.LinkInterval > 0 {
		args = append(args, "--link-interval", in.LinkInterval.String())
	}
	if in.ReachInterval > 0 {
		args = append(args, "--reach-interval", in.ReachInterval.String())
	}
	if in.Iface != "" {
		args = append(args, "--iface", in.Iface)
	}
	return args
}

// validateLaunchdOpts is a live GATE on the install path. It ALLOW-LISTS; it never
// substring-scans. Any token outside the permitted flag set is rejected, so a
// free-form Args slice (or a future --bind flag) can never smuggle an unexpected
// flag into an always-on process. It PARSES the --port and runs it through
// validateBindAddr, genuinely reusing the serve.go loopback logic. Returns a
// descriptive error for any violation.
func validateLaunchdOpts(o LaunchdOpts) error {
	if strings.TrimSpace(o.BinaryPath) == "" {
		return fmt.Errorf("binary path is empty")
	}
	if !filepath.IsAbs(o.BinaryPath) {
		return fmt.Errorf("binary path %q is not absolute", o.BinaryPath)
	}
	if err := validateLaunchdLabel(o.Label); err != nil {
		return err
	}

	// Flags that consume a following value vs. the one permitted bool flag.
	valueFlags := map[string]bool{
		"--port": true, "--interval": true, "--link-interval": true,
		"--reach-interval": true, "--history-dir": true, "--iface": true,
	}

	sawServe := false
	portStr := ""
	for i := 0; i < len(o.Args); i++ {
		tok := o.Args[i]
		name := tok
		val := ""
		inlineVal := false
		if strings.HasPrefix(tok, "--") {
			if eq := strings.IndexByte(tok, '='); eq >= 0 {
				name = tok[:eq]
				val = tok[eq+1:]
				inlineVal = true
			}
		}
		switch {
		case name == "--serve":
			if inlineVal {
				return fmt.Errorf("--serve takes no value")
			}
			sawServe = true
		case valueFlags[name]:
			if !inlineVal {
				i++
				if i >= len(o.Args) {
					return fmt.Errorf("%s is missing its value", name)
				}
				val = o.Args[i]
			}
			if err := validateLaunchdFlagValue(name, val); err != nil {
				return err
			}
			if name == "--port" {
				portStr = val
			}
		default:
			// ALLOW-LIST: anything not explicitly permitted (incl. "--bind",
			// "0.0.0.0", ":8099", a bare positional) is refused.
			return fmt.Errorf("unrecognized launchd arg %q (allow-list: --serve --port --interval --link-interval --reach-interval --history-dir --iface)", tok)
		}
	}

	if !sawServe {
		return fmt.Errorf("Args must contain --serve (an always-on daemon without it respawns in a loop)")
	}
	if portStr == "" {
		return fmt.Errorf("Args must contain --port")
	}
	// Defense-in-depth: parse the port and re-run the real loopback bind check.
	if err := validateBindAddr(netJoinLoopback(portStr)); err != nil {
		return fmt.Errorf("refusing launchd --port %q: %w", portStr, err)
	}
	return nil
}

// validateLaunchdFlagValue type-checks a single permitted value flag's value.
func validateLaunchdFlagValue(name, val string) error {
	switch name {
	case "--port":
		p, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("--port %q is not an integer", val)
		}
		if p < 1 || p > 65535 {
			return fmt.Errorf("--port %d out of range (1-65535)", p)
		}
	case "--interval", "--link-interval", "--reach-interval":
		if _, err := time.ParseDuration(val); err != nil {
			return fmt.Errorf("%s %q is not a duration", name, val)
		}
	case "--history-dir":
		if val == "" {
			return fmt.Errorf("--history-dir is empty")
		}
		if !filepath.IsAbs(val) {
			return fmt.Errorf("--history-dir %q must be absolute (launchd has no stable CWD)", val)
		}
	case "--iface":
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("--iface is empty")
		}
	}
	return nil
}

// netJoinLoopback builds "127.0.0.1:<port>" for the validateBindAddr re-check,
// mirroring listenLocal's net.JoinHostPort("127.0.0.1", …) exactly.
func netJoinLoopback(port string) string { return "127.0.0.1:" + port }

// validateLaunchdLabel enforces a reverse-DNS-shaped label: ASCII letters, digits,
// '.' and '-' only; no '/', no "..", no leading dot, non-empty. This prevents
// launchAgentPath writing the plist OUTSIDE ~/Library/LaunchAgents.
func validateLaunchdLabel(label string) error {
	if label == "" {
		return fmt.Errorf("launchd label is empty")
	}
	if strings.HasPrefix(label, ".") {
		return fmt.Errorf("launchd label %q must not start with a dot", label)
	}
	if strings.Contains(label, "..") {
		return fmt.Errorf("launchd label %q must not contain %q", label, "..")
	}
	if strings.ContainsAny(label, `/\`) {
		return fmt.Errorf("launchd label %q must not contain a path separator", label)
	}
	for _, r := range label {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-'
		if !ok {
			return fmt.Errorf("launchd label %q contains invalid character %q", label, string(r))
		}
	}
	return nil
}

// checkStableBinaryPath is the PURE refusal logic for an ephemeral/unstable binary
// path. It rejects a non-absolute path or one under a temp/translocation location —
// the #1 silent-failure risk: `go run . --install-launchd` bakes a soon-deleted
// $TMPDIR/go-build.../exe into ProgramArguments[0], which loads in the test cycle
// but is dead at next login. tmpDir is passed in (os.Getenv("TMPDIR")) so the check
// stays pure and table-testable.
func checkStableBinaryPath(path, tmpDir string) error {
	if path == "" {
		return fmt.Errorf("binary path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("binary path %q is not absolute", path)
	}
	fragments := []string{"/private/var/folders", "/var/folders", "/AppTranslocation/", "/go-build"}
	for _, f := range fragments {
		if strings.Contains(path, f) {
			return fmt.Errorf("binary path %q looks ephemeral (contains %q) — install a stable binary and pass --launchd-binary /usr/local/bin/netdebug", path, f)
		}
	}
	if tmpDir != "" {
		clean := filepath.Clean(tmpDir)
		if path == clean || strings.HasPrefix(path, clean+string(filepath.Separator)) {
			return fmt.Errorf("binary path %q is under $TMPDIR — install a stable binary and pass --launchd-binary /usr/local/bin/netdebug", path)
		}
	}
	return nil
}

// resolveBinaryPath canonicalizes the binary to bake into the plist:
// os.Executable() (or an explicit override) -> EvalSymlinks -> Abs, then the pure
// ephemeral-path refusal. A --launchd-binary override supplies a stable path
// explicitly and is run through the SAME refusal check.
func resolveBinaryPath(override string) (string, error) {
	path := override
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot determine running binary path: %w", err)
		}
		path = exe
	}
	// EvalSymlinks best-effort: a not-yet-existing override path still gets Abs'd
	// and refusal-checked, but a symlinked real binary resolves to its target.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot absolutize binary path %q: %w", path, err)
	}
	if err := checkStableBinaryPath(abs, os.Getenv("TMPDIR")); err != nil {
		return "", err
	}
	return abs, nil
}

// ---------------------------------------------------------------------------
// Impure layer: launchctl mechanism isolated in launchctlArgs.
// ---------------------------------------------------------------------------

// launchctlArgs is the ONE place the launchctl verb/argv is built, so the mechanism
// (legacy `load -w <path>` / `unload -w <path>`, `list <label>`) can be swapped
// without touching anything else. -w is used on BOTH load and unload so a
// reinstall-after-uninstall re-enables a previously-unloaded label cleanly.
func launchctlArgs(verb, pathOrLabel string) []string {
	switch verb {
	case "load", "unload":
		return []string{verb, "-w", pathOrLabel}
	case "list":
		return []string{"list", pathOrLabel}
	default:
		return []string{verb, pathOrLabel}
	}
}

// historyDirFromArgs extracts the --history-dir value (if any) so install can
// MkdirAll it. Mirrors the two forms composeLaunchdArgs can emit (it only emits the
// two-token form, but tolerate --history-dir=VAL too).
func historyDirFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--history-dir" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if strings.HasPrefix(a, "--history-dir=") {
			return strings.TrimPrefix(a, "--history-dir=")
		}
	}
	return ""
}

// installLaunchAgent validates, then (in order): MkdirAll the LogPath parent AND the
// history dir; write the plist; `plutil -lint`; then unload-then-`load -w`
// (idempotent — a changed --port/--interval/--history-dir takes effect, not the
// stale live config). Returns the written path. Validates BEFORE any side-effect.
func installLaunchAgent(o LaunchdOpts) (string, error) {
	if err := validateLaunchdOpts(o); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir: %w", err)
	}
	path := launchAgentPath(home, o.Label)

	// Create parents BEFORE writing: the LaunchAgents dir, the log dir, the history dir.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir LaunchAgents dir: %w", err)
	}
	if o.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(o.LogPath), 0o755); err != nil {
			return "", fmt.Errorf("mkdir log dir: %w", err)
		}
	}
	if hd := historyDirFromArgs(o.Args); hd != "" {
		if err := os.MkdirAll(hd, 0o755); err != nil {
			return "", fmt.Errorf("mkdir history dir %q: %w", hd, err)
		}
	}

	xmlStr, err := renderLaunchdPlist(o)
	if err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}
	if err := os.WriteFile(path, []byte(xmlStr), 0o644); err != nil {
		return "", fmt.Errorf("write plist %q: %w", path, err)
	}

	// plutil -lint catches a well-formed-but-semantically-wrong plist.
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("plutil -lint rejected %q: %v: %s", path, err, strings.TrimSpace(string(out)))
	}

	// Idempotent load: unload first (tolerate "not loaded"), then load -w.
	a := launchctlArgs("unload", path)
	_ = exec.Command("launchctl", a...).Run() // best-effort; may legitimately fail if not loaded
	a = launchctlArgs("load", path)
	if out, err := exec.Command("launchctl", a...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl load failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

// uninstallLaunchAgent runs `unload -w <path>` (unload needs the file present) then
// removes the plist. Treats "not loaded" and "file absent" as SUCCESS so an
// uninstall-when-partially-installed leaves the machine clean.
func uninstallLaunchAgent(home, label string) error {
	if err := validateLaunchdLabel(label); err != nil {
		return err
	}
	path := launchAgentPath(home, label)
	a := launchctlArgs("unload", path)
	_ = exec.Command("launchctl", a...).Run() // "not loaded" / absent file are both fine
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist %q: %w", path, err)
	}
	return nil
}

// launchdStatus reports the agent state via `launchctl list <label>` (exact lookup,
// NOT `list | grep`): PID present => running; LastExitStatus != 0 => crash-looping;
// a non-zero launchctl exit => not loaded. It also reports the plist path, the
// configured port and the log path (read from the installed plist).
func launchdStatus(home, label string) (string, error) {
	if err := validateLaunchdLabel(label); err != nil {
		return "", err
	}
	path := launchAgentPath(home, label)

	var b strings.Builder
	fmt.Fprintf(&b, "label:  %s\n", label)
	fmt.Fprintf(&b, "plist:  %s\n", path)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		b.WriteString("state:  not installed (no plist)\n")
		return b.String(), nil
	}
	if args, logPath, err := readPlistInfo(path); err == nil {
		if p := portFromArgs(args); p != "" {
			fmt.Fprintf(&b, "port:   %s (requested; a busy port auto-advances — check the log for the real URL)\n", p)
		}
		if logPath != "" {
			fmt.Fprintf(&b, "log:    %s\n", logPath)
		}
	}

	a := launchctlArgs("list", label)
	out, err := exec.Command("launchctl", a...).CombinedOutput()
	if err != nil {
		b.WriteString("state:  NOT LOADED (launchctl list has no such label)\n")
		return b.String(), nil
	}
	pid, pidOK, lastExit, lastExitOK := parseLaunchdListOutput(string(out))
	switch {
	case pidOK:
		fmt.Fprintf(&b, "state:  RUNNING (pid %d)\n", pid)
	case lastExitOK && lastExit != 0:
		fmt.Fprintf(&b, "state:  CRASH-LOOPING (last exit status %d; respawns on launchd's ~10s throttle)\n", lastExit)
	default:
		b.WriteString("state:  loaded (not currently running)\n")
	}
	return b.String(), nil
}

// portFromArgs extracts the --port value from a ProgramArguments tail.
func portFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--port=") {
			return strings.TrimPrefix(args[i], "--port=")
		}
	}
	return ""
}

// parseLaunchdListOutput extracts PID and LastExitStatus from `launchctl list
// <label>` output (a plist-ish dict of `"Key" = value;` lines). PURE: string in,
// parsed ints out, so a crash loop (LastExitStatus != 0, no PID) is distinguishable
// from a healthy running agent. A missing key returns its *OK bool false.
func parseLaunchdListOutput(out string) (pid int, pidOK bool, lastExit int, lastExitOK bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := splitLaunchdDictLine(line)
		if !ok {
			continue
		}
		switch key {
		case "PID":
			if n, err := strconv.Atoi(val); err == nil {
				pid, pidOK = n, true
			}
		case "LastExitStatus":
			if n, err := strconv.Atoi(val); err == nil {
				lastExit, lastExitOK = n, true
			}
		}
	}
	return pid, pidOK, lastExit, lastExitOK
}

// splitLaunchdDictLine parses one `"Key" = value;` line, tolerating unquoted keys
// and a trailing semicolon. Returns ok=false for lines that are not key/value.
func splitLaunchdDictLine(line string) (key, val string, ok bool) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	key = strings.Trim(key, `"`)
	val = strings.TrimSpace(line[eq+1:])
	val = strings.TrimSuffix(val, ";")
	val = strings.TrimSpace(val)
	val = strings.Trim(val, `"`)
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// readPlistInfo reads an installed plist and returns its ProgramArguments and
// StandardOutPath via encoding/xml, so launchdStatus can surface the configured
// port and log without a second source of truth.
func readPlistInfo(path string) (args []string, logPath string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	curKey := ""
	inProgArgs := false
	for {
		tok, e := dec.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, "", e
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var s string
				if e := dec.DecodeElement(&s, &t); e != nil {
					return nil, "", e
				}
				curKey = s
				if curKey != "ProgramArguments" {
					inProgArgs = false
				}
			case "array":
				if curKey == "ProgramArguments" {
					inProgArgs = true
				}
			case "string":
				var s string
				if e := dec.DecodeElement(&s, &t); e != nil {
					return nil, "", e
				}
				if inProgArgs {
					args = append(args, s)
				} else if curKey == "StandardOutPath" {
					logPath = s
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inProgArgs = false
			}
		}
	}
	return args, logPath, nil
}
