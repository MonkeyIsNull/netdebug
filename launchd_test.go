package main

// Phase 5 — launchd tests. The PURE core (renderLaunchdPlist, launchAgentPath,
// validateLaunchdOpts, composeLaunchdArgs, checkStableBinaryPath,
// parseLaunchdListOutput) is table-tested here; the impure launchctl wrappers are
// exercised by the human-run install/uninstall cycle, not by these unit tests.

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

// decodedPlist is the shape a test reads back out of rendered plist XML so key
// spelling, argument order, and boolean rendering can all be asserted.
type decodedPlist struct {
	topKeys    []string          // keys at the top-level <dict> (depth 1), in order
	progArgs   []string          // ProgramArguments strings, in order
	strings    map[string]string // top-level <key> -> its <string> value
	bools      map[string]bool   // top-level <key> -> <true/>/<false/>
	pathEnv    string            // EnvironmentVariables.PATH
	wellFormed bool
}

// decodePlist walks the rendered XML with encoding/xml. A clean token walk proves
// well-formedness; tracking <dict> depth separates the top-level keys from the
// nested EnvironmentVariables dict's PATH key.
func decodePlist(t *testing.T, xmlStr string) decodedPlist {
	t.Helper()
	d := decodedPlist{strings: map[string]string{}, bools: map[string]bool{}}
	dec := xml.NewDecoder(strings.NewReader(xmlStr))

	dictDepth := 0
	curKey := "" // current top-level key (depth-1 dict)
	inProgArgs := false
	inEnvDict := false
	envKey := ""

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "dict":
				dictDepth++
				if dictDepth == 2 && curKey == "EnvironmentVariables" {
					inEnvDict = true
				}
			case "array":
				if dictDepth == 1 && curKey == "ProgramArguments" {
					inProgArgs = true
				}
			case "key":
				var s string
				if err := dec.DecodeElement(&s, &e); err != nil {
					t.Fatalf("decode key: %v", err)
				}
				if inEnvDict {
					envKey = s
				} else if dictDepth == 1 {
					curKey = s
					d.topKeys = append(d.topKeys, s)
				}
			case "string":
				var s string
				if err := dec.DecodeElement(&s, &e); err != nil {
					t.Fatalf("decode string: %v", err)
				}
				switch {
				case inProgArgs:
					d.progArgs = append(d.progArgs, s)
				case inEnvDict && envKey == "PATH":
					d.pathEnv = s
				case dictDepth == 1:
					d.strings[curKey] = s
				}
			case "true":
				if dictDepth == 1 {
					d.bools[curKey] = true
				}
			case "false":
				if dictDepth == 1 {
					d.bools[curKey] = false
				}
			}
		case xml.EndElement:
			switch e.Name.Local {
			case "array":
				inProgArgs = false
			case "dict":
				if inEnvDict && dictDepth == 2 {
					inEnvDict = false
				}
				dictDepth--
			}
		}
	}
	d.wellFormed = true
	return d
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func setEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestRenderLaunchdPlist(t *testing.T) {
	opts := LaunchdOpts{
		Label:      "com.cobenian.netdebug",
		BinaryPath: "/usr/local/bin/netdebug",
		Args:       []string{"--serve", "--port", "8099", "--history-dir", "/abs/dir"},
		LogPath:    "/Users/x/Library/Logs/netdebug/netdebug.log",
		RunAtLoad:  true,
		KeepAlive:  true,
	}
	xmlStr, err := renderLaunchdPlist(opts)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	p := decodePlist(t, xmlStr)

	// Exact top-level key set & spelling — a typo like RunAtLoed round-trips as
	// well-formed XML and would ship a dead agent, so assert spelling explicitly.
	wantKeys := []string{
		"Label", "ProgramArguments", "RunAtLoad", "KeepAlive",
		"StandardOutPath", "StandardErrorPath", "EnvironmentVariables",
	}
	if !setEq(p.topKeys, wantKeys) {
		t.Fatalf("top-level keys = %v, want exactly %v", p.topKeys, wantKeys)
	}

	if p.strings["Label"] != opts.Label {
		t.Fatalf("Label = %q, want %q", p.strings["Label"], opts.Label)
	}

	// ProgramArguments is [BinaryPath, <Args...>] in order.
	wantArgs := append([]string{opts.BinaryPath}, opts.Args...)
	if !sliceEq(p.progArgs, wantArgs) {
		t.Fatalf("ProgramArguments = %v, want %v", p.progArgs, wantArgs)
	}
	if p.progArgs[0] != opts.BinaryPath {
		t.Fatalf("ProgramArguments[0] = %q, want BinaryPath %q", p.progArgs[0], opts.BinaryPath)
	}

	// Guards the respawn-storm bug: a daemon without --serve exits and KeepAlive
	// respawns it in a loop.
	sawServe := false
	for _, a := range p.progArgs {
		if a == "--serve" {
			sawServe = true
		}
	}
	if !sawServe {
		t.Fatal("ProgramArguments is missing --serve (respawn-storm risk)")
	}

	if !p.bools["RunAtLoad"] {
		t.Fatal("RunAtLoad = false, want true")
	}
	if !p.bools["KeepAlive"] {
		t.Fatal("KeepAlive = false, want true")
	}

	if p.strings["StandardOutPath"] != opts.LogPath {
		t.Fatalf("StandardOutPath = %q, want %q", p.strings["StandardOutPath"], opts.LogPath)
	}
	if p.strings["StandardErrorPath"] != opts.LogPath {
		t.Fatalf("StandardErrorPath = %q, want %q", p.strings["StandardErrorPath"], opts.LogPath)
	}
	if p.pathEnv != launchdPATH {
		t.Fatalf("EnvironmentVariables.PATH = %q, want %q", p.pathEnv, launchdPATH)
	}
}

// TestRenderLaunchdPlistEscaping feeds a path with a space, a '&' and a '<' and
// asserts both well-formedness (round-trips through encoding/xml) AND that the
// value comes back byte-identical — the fmt.Sprintf failure mode.
func TestRenderLaunchdPlistEscaping(t *testing.T) {
	hostile := `/Users/a b/My & Weird <Dir>/netdebug`
	opts := LaunchdOpts{
		Label:      "com.cobenian.netdebug",
		BinaryPath: hostile,
		Args:       []string{"--serve", "--port", "8099"},
		LogPath:    `/log/a & b <c>.log`,
		RunAtLoad:  true,
		KeepAlive:  true,
	}
	xmlStr, err := renderLaunchdPlist(opts)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	// Raw '&' or '<' in the output (unescaped) would be malformed XML.
	p := decodePlist(t, xmlStr) // fails the test if not well-formed
	if p.progArgs[0] != hostile {
		t.Fatalf("BinaryPath did not round-trip: got %q want %q", p.progArgs[0], hostile)
	}
	if p.strings["StandardOutPath"] != opts.LogPath {
		t.Fatalf("LogPath did not round-trip: got %q want %q", p.strings["StandardOutPath"], opts.LogPath)
	}
	// Belt-and-suspenders: a second xml decode of the whole doc must not error.
	if err := xml.NewDecoder(bytes.NewReader([]byte(xmlStr))).Decode(new(struct {
		XMLName xml.Name
	})); err != nil && err != io.EOF {
		// Decode into a throwaway struct — any XML error surfaces here.
		t.Fatalf("second decode errored: %v", err)
	}
}

// TestRenderLaunchdPlistBoolsFalse: false must render <false/>, not be omitted
// (a missing key is NOT false in launchd semantics).
func TestRenderLaunchdPlistBoolsFalse(t *testing.T) {
	opts := LaunchdOpts{
		Label:      "com.x.netdebug",
		BinaryPath: "/usr/local/bin/netdebug",
		Args:       []string{"--serve", "--port", "8099"},
		LogPath:    "/log/nd.log",
		RunAtLoad:  false,
		KeepAlive:  false,
	}
	xmlStr, err := renderLaunchdPlist(opts)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	if !strings.Contains(xmlStr, "<key>RunAtLoad</key>") || !strings.Contains(xmlStr, "<key>KeepAlive</key>") {
		t.Fatal("RunAtLoad/KeepAlive keys must be present even when false")
	}
	p := decodePlist(t, xmlStr)
	if v, ok := p.bools["RunAtLoad"]; !ok || v {
		t.Fatalf("RunAtLoad should render <false/>; bools[RunAtLoad]=%v ok=%v", v, ok)
	}
	if v, ok := p.bools["KeepAlive"]; !ok || v {
		t.Fatalf("KeepAlive should render <false/>; bools[KeepAlive]=%v ok=%v", v, ok)
	}
}

func TestLaunchAgentPath(t *testing.T) {
	tests := []struct {
		home, label, want string
	}{
		{"/Users/adam", "com.x", "/Users/adam/Library/LaunchAgents/com.x.plist"},
		{"/Users/adam", "com.cobenian.netdebug", "/Users/adam/Library/LaunchAgents/com.cobenian.netdebug.plist"},
	}
	for _, tc := range tests {
		if got := launchAgentPath(tc.home, tc.label); got != tc.want {
			t.Errorf("launchAgentPath(%q,%q) = %q, want %q", tc.home, tc.label, got, tc.want)
		}
	}
}

func TestComposeLaunchdArgs(t *testing.T) {
	tests := []struct {
		name string
		in   launchdArgsInput
		want []string
	}{
		{
			name: "port only",
			in:   launchdArgsInput{Port: 8099},
			want: []string{"--serve", "--port", "8099"},
		},
		{
			name: "port + history dir",
			in:   launchdArgsInput{Port: 8099, HistoryDir: "/abs/dir"},
			want: []string{"--serve", "--port", "8099", "--history-dir", "/abs/dir"},
		},
		{
			name: "all optionals in fixed order",
			in: launchdArgsInput{
				Port: 9000, HistoryDir: "/h", Interval: 2 * time.Second,
				LinkInterval: 10 * time.Second, ReachInterval: 7 * time.Second, Iface: "en0",
			},
			want: []string{
				"--serve", "--port", "9000", "--history-dir", "/h",
				"--interval", "2s", "--link-interval", "10s", "--reach-interval", "7s", "--iface", "en0",
			},
		},
		{
			name: "reach-interval only",
			in:   launchdArgsInput{Port: 8099, ReachInterval: 5 * time.Second},
			want: []string{"--serve", "--port", "8099", "--reach-interval", "5s"},
		},
		{
			name: "zero optionals are omitted",
			in:   launchdArgsInput{Port: 8099, Interval: 0, LinkInterval: 0, Iface: ""},
			want: []string{"--serve", "--port", "8099"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := composeLaunchdArgs(tc.in)
			if !sliceEq(got, tc.want) {
				t.Fatalf("composeLaunchdArgs = %v, want %v", got, tc.want)
			}
			// Every composed vector must start with --serve.
			if got[0] != "--serve" {
				t.Fatalf("composed args do not start with --serve: %v", got)
			}
			// And the self-composed vector must pass the install gate.
			o := LaunchdOpts{Label: "com.cobenian.netdebug", BinaryPath: "/usr/local/bin/netdebug", Args: got}
			if err := validateLaunchdOpts(o); err != nil {
				t.Fatalf("composed args rejected by validateLaunchdOpts: %v", err)
			}
		})
	}
}

func TestValidateLaunchdOpts(t *testing.T) {
	const bin = "/usr/local/bin/netdebug"
	const label = "com.cobenian.netdebug"

	mustPass := []struct {
		name string
		opts LaunchdOpts
	}{
		{"minimal serve+port", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099"}}},
		{"with history dir", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--history-dir", "/abs/dir"}}},
		{"with durations and iface", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--interval", "2s", "--link-interval", "10s", "--reach-interval", "7s", "--iface", "en0"}}},
		{"inline --port=", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port=8099"}}},
		{"port at low bound", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "1"}}},
		{"port at high bound", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "65535"}}},
	}
	for _, tc := range mustPass {
		t.Run("pass/"+tc.name, func(t *testing.T) {
			if err := validateLaunchdOpts(tc.opts); err != nil {
				t.Fatalf("expected PASS, got error: %v", err)
			}
		})
	}

	mustFail := []struct {
		name string
		opts LaunchdOpts
	}{
		{"unknown flag --bind", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--bind", "0.0.0.0"}}},
		{"smuggled 0.0.0.0 positional", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "0.0.0.0"}}},
		{"smuggled :8099 positional", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", ":8099"}}},
		{"missing --serve", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--port", "8099"}}},
		{"missing --port", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve"}}},
		{"port out of range high", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "70000"}}},
		{"port zero", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "0"}}},
		{"port non-integer", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "abc"}}},
		{"relative binary path", LaunchdOpts{Label: label, BinaryPath: "netdebug", Args: []string{"--serve", "--port", "8099"}}},
		{"empty binary path", LaunchdOpts{Label: label, BinaryPath: "", Args: []string{"--serve", "--port", "8099"}}},
		{"empty label", LaunchdOpts{Label: "", BinaryPath: bin, Args: []string{"--serve", "--port", "8099"}}},
		{"label with slash", LaunchdOpts{Label: "com/x", BinaryPath: bin, Args: []string{"--serve", "--port", "8099"}}},
		{"label with dotdot", LaunchdOpts{Label: "..com.x", BinaryPath: bin, Args: []string{"--serve", "--port", "8099"}}},
		{"label leading dot", LaunchdOpts{Label: ".com.x", BinaryPath: bin, Args: []string{"--serve", "--port", "8099"}}},
		{"relative history dir", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--history-dir", "rel/dir"}}},
		{"serve takes no value", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve=1", "--port", "8099"}}},
		{"bad interval", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--interval", "notaduration"}}},
		{"bad reach-interval", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port", "8099", "--reach-interval", "notaduration"}}},
		{"port missing value", LaunchdOpts{Label: label, BinaryPath: bin, Args: []string{"--serve", "--port"}}},
	}
	for _, tc := range mustFail {
		t.Run("fail/"+tc.name, func(t *testing.T) {
			if err := validateLaunchdOpts(tc.opts); err == nil {
				t.Fatalf("expected ERROR, got nil (config would be allowed)")
			}
		})
	}
}

func TestCheckStableBinaryPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		tmpDir  string
		wantErr bool
	}{
		{"stable usr local", "/usr/local/bin/netdebug", "", false},
		{"stable home bin", "/Users/adam/bin/netdebug", "", false},
		{"non-absolute", "netdebug", "", true},
		{"empty", "", "", true},
		{"private var folders", "/private/var/folders/xy/abc/T/go-build123/exe/netdebug", "", true},
		{"var folders", "/var/folders/xy/abc/T/exe/netdebug", "", true},
		{"go-build fragment", "/somewhere/go-build456/b001/exe/netdebug", "", true},
		{"app translocation", "/private/var/folders/AppTranslocation/ABC/d/netdebug", "", true},
		{"under TMPDIR", "/tmp/custom/netdebug", "/tmp/custom", true},
		{"stable despite TMPDIR set elsewhere", "/usr/local/bin/netdebug", "/tmp/custom", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStableBinaryPath(tc.path, tc.tmpDir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkStableBinaryPath(%q,%q) err=%v, wantErr=%v", tc.path, tc.tmpDir, err, tc.wantErr)
			}
		})
	}
}

func TestParseLaunchdListOutput(t *testing.T) {
	running := `{
	"StandardOutPath" = "/log/nd.log";
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.cobenian.netdebug";
	"OnDemand" = false;
	"LastExitStatus" = 0;
	"PID" = 4242;
	"Program" = "/usr/local/bin/netdebug";
};`
	pid, pidOK, le, leOK := parseLaunchdListOutput(running)
	if !pidOK || pid != 4242 {
		t.Fatalf("running: pid=%d ok=%v, want 4242/true", pid, pidOK)
	}
	if !leOK || le != 0 {
		t.Fatalf("running: lastExit=%d ok=%v, want 0/true", le, leOK)
	}

	crashing := `{
	"Label" = "com.cobenian.netdebug";
	"LastExitStatus" = 256;
	"OnDemand" = false;
};`
	pid, pidOK, le, leOK = parseLaunchdListOutput(crashing)
	if pidOK {
		t.Fatalf("crashing: expected no PID, got %d", pid)
	}
	if !leOK || le != 256 {
		t.Fatalf("crashing: lastExit=%d ok=%v, want 256/true", le, leOK)
	}

	empty := ``
	_, pidOK, _, leOK = parseLaunchdListOutput(empty)
	if pidOK || leOK {
		t.Fatalf("empty output should yield no PID and no LastExitStatus")
	}
}

func TestLaunchctlArgs(t *testing.T) {
	tests := []struct {
		verb, arg string
		want      []string
	}{
		{"load", "/p.plist", []string{"load", "-w", "/p.plist"}},
		{"unload", "/p.plist", []string{"unload", "-w", "/p.plist"}},
		{"list", "com.x", []string{"list", "com.x"}},
	}
	for _, tc := range tests {
		if got := launchctlArgs(tc.verb, tc.arg); !sliceEq(got, tc.want) {
			t.Errorf("launchctlArgs(%q,%q) = %v, want %v", tc.verb, tc.arg, got, tc.want)
		}
	}
	// -w must be used on BOTH load and unload so reinstall-after-uninstall re-enables.
	if got := launchctlArgs("load", "/p"); got[1] != "-w" {
		t.Errorf("load missing -w: %v", got)
	}
	if got := launchctlArgs("unload", "/p"); got[1] != "-w" {
		t.Errorf("unload missing -w: %v", got)
	}
}
