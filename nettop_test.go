package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// dur is a seconds->Duration helper for the clamp table tests.
func dur(seconds int) time.Duration { return time.Duration(seconds) * time.Second }

// nettopFixture is a VERBATIM `LC_ALL=C /usr/bin/nettop -P -x -L 2 -s 2 -J
// bytes_in,bytes_out` capture (Apple Silicon, macOS). TWO blocks, each led by the
// real `,bytes_in,bytes_out,` header (blank-header identity column), every row with a
// TRAILING comma, and a trailing newline after the last row. The shapes that MUST be
// present, per phase11 §7, all are:
//   - the `,bytes_in,bytes_out,` header TWICE (=> two blocks)
//   - space names: "Slack Helper.61668", "OpenVPN Connect.78978"
//   - a numeric dotted name: "2.1.258.60633" (the top upstream RATE talker)
//   - a truncated dotted name: "com.apple.WebKi.13595"
//   - a >uint32 cumulative daemon: "mDNSResponder.237" (moves ~132 B out => tiny rate)
//   - 0/0 rows: "airportd.200,0,0,"
//   - a block-2-ONLY pid: "configd.129" (new in window => dropped by rateTalkers)
//   - a byte-identical-across-blocks idle: "netbiosd.19249" (=> 0 rate)
const nettopFixture = `,bytes_in,bytes_out,
syslogd.153,0,308,
apsd.159,7114,11273,
airportd.200,0,0,
symptomsd.224,0,0,
mDNSResponder.237,9631465568,459022400,
mullvad-daemon.367,0,0,
rapportd.472,6355295,600620,
identityservice.508,3914,11787,
Slack Helper.61668,138016,63314,
2.1.258.60633,26498,1120146,
remotepairingd.50961,3954,1009,
com.apple.WebKi.13595,44971,61626,
OpenVPN Connect.78978,1222071,666255,
ssh.13927,124152,48193,
netbiosd.19249,5795092,3157061,
,bytes_in,bytes_out,
configd.129,0,0,
syslogd.153,0,308,
apsd.159,7114,11273,
airportd.200,0,0,
symptomsd.224,0,0,
mDNSResponder.237,9631473317,459022532,
mullvad-daemon.367,0,0,
rapportd.472,6355295,600620,
identityservice.508,3914,11787,
Slack Helper.61668,139309,63534,
2.1.258.60633,26847,1177580,
remotepairingd.50961,3954,1009,
com.apple.WebKi.13595,44971,61626,
OpenVPN Connect.78978,1222844,666737,
ssh.13927,124152,48193,
netbiosd.19249,5795092,3157061,
`

const floatTol = 1e-6

func approxBps(a, b float64) bool { return math.Abs(a-b) <= floatTol }

// findSample returns the procSample with the given PID in a block, or ok=false.
func findSample(block []procSample, pid int) (procSample, bool) {
	for _, p := range block {
		if p.PID == pid {
			return p, true
		}
	}
	return procSample{}, false
}

func findBW(procs []ProcBandwidth, pid int) (ProcBandwidth, bool) {
	for _, p := range procs {
		if p.PID == pid {
			return p, true
		}
	}
	return ProcBandwidth{}, false
}

func TestParseNettopBlocks(t *testing.T) {
	blocks := parseNettopBlocks(nettopFixture)
	if blocks == nil {
		t.Fatal("parseNettopBlocks returned nil on the real two-block fixture")
	}
	// TWO header lines => TWO blocks.
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2 (one per header line)", len(blocks))
	}
	// Block 1 has 15 data rows (no configd); block 2 has 16 (configd added).
	if len(blocks[0]) != 15 {
		t.Errorf("block 1 rows = %d, want 15", len(blocks[0]))
	}
	if len(blocks[1]) != 16 {
		t.Errorf("block 2 rows = %d, want 16", len(blocks[1]))
	}

	// NAME comes from the BLANK-HEADER field 0 (positional), split at the LAST '.'.
	cases := []struct {
		pid      int
		wantName string
	}{
		{61668, "Slack Helper"},    // space survives the comma split
		{78978, "OpenVPN Connect"}, // space survives the comma split
		{60633, "2.1.258"},         // adversarial all-numeric dotted name
		{13595, "com.apple.WebKi"}, // truncated dotted name
		{237, "mDNSResponder"},     // huge cumulative daemon
		{19249, "netbiosd"},        // idle
	}
	for _, c := range cases {
		s, ok := findSample(blocks[0], c.pid)
		if !ok {
			t.Errorf("pid %d absent from block 1", c.pid)
			continue
		}
		if s.Name != c.wantName {
			t.Errorf("pid %d name = %q, want %q (name column is blank-header field 0)", c.pid, s.Name, c.wantName)
		}
	}

	// bytes_in / bytes_out taken by header-name match (ParseUint, NOT Atoi — these
	// exceed uint32). mDNSResponder reads 9631465568 in / 459022400 out in block 1.
	md, _ := findSample(blocks[0], 237)
	if md.BytesIn != 9631465568 || md.BytesOut != 459022400 {
		t.Errorf("mDNSResponder block1 = in %d out %d, want 9631465568 / 459022400 (ParseUint 64-bit)", md.BytesIn, md.BytesOut)
	}

	// configd.129 is block-2-ONLY.
	if _, ok := findSample(blocks[0], 129); ok {
		t.Error("configd.129 must NOT be in block 1")
	}
	if _, ok := findSample(blocks[1], 129); !ok {
		t.Error("configd.129 must be in block 2")
	}

	// The header line, trailing blank line, and any garbled row are dropped by the
	// single unparseable-bytes predicate — no stray row with name "" or "bytes_in".
	for bi, block := range blocks {
		for _, p := range block {
			if p.Name == "" || p.Name == "bytes_in" || strings.Contains(p.Name, "bytes_out") {
				t.Errorf("block %d leaked a header/blank row as a sample: %+v", bi+1, p)
			}
		}
	}
}

func TestSplitNamePID(t *testing.T) {
	cases := []struct {
		token    string
		wantName string
		wantPID  int
	}{
		{"com.apple.WebKi.13595", "com.apple.WebKi", 13595},
		{"2.1.258.60633", "2.1.258", 60633},
		{"Slack Helper.61668", "Slack Helper", 61668},
		{"mDNSResponder.237", "mDNSResponder", 237},
		{"noPidName", "noPidName", 0},       // no trailing numeric segment
		{"trailingdot.", "trailingdot.", 0}, // trailing '.' with nothing after
		{"", "", 0},                         // empty
	}
	for _, c := range cases {
		gotName, gotPID := splitNamePID(c.token)
		if gotName != c.wantName || gotPID != c.wantPID {
			t.Errorf("splitNamePID(%q) = (%q, %d), want (%q, %d)", c.token, gotName, gotPID, c.wantName, c.wantPID)
		}
	}
}

func TestParseNettopBlocksUnavailable(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "\n\n  \n"},
		// nettop's exit-0 usage-text blob from a bad -J: no bytes_in/bytes_out header.
		{"usage text", "usage: nettop [-x] [-P] [-L samples] [-s delay] [-J cols]\n  -J   comma-separated columns\n"},
		// a header-less data-ish blob (no header => cannot pin columns => nil).
		{"no header", "someproc.5,100,200,\notherproc.6,1,2,\n"},
		// a header but ZERO data rows => unavailable, not an empty success.
		{"header only", ",bytes_in,bytes_out,\n,bytes_in,bytes_out,\n"},
	}
	for _, c := range cases {
		if got := parseNettopBlocks(c.in); got != nil {
			t.Errorf("parseNettopBlocks(%s) = %v, want nil (unavailable)", c.name, got)
		}
	}
}

func TestParseNettopHeaderNameMatch(t *testing.T) {
	// Columns REORDERED: bytes_out before bytes_in, plus an extra leading column.
	// Header-name match must still pick the right fields, never a hardcoded index.
	in := "proc,bytes_out,bytes_in,\nUploader.42,999,111,\n" +
		"proc,bytes_out,bytes_in,\nUploader.42,1999,211,\n"
	blocks := parseNettopBlocks(in)
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2", len(blocks))
	}
	s, ok := findSample(blocks[0], 42)
	if !ok {
		t.Fatal("Uploader.42 absent")
	}
	// bytes_in is the 3rd column (999 is bytes_out, 111 is bytes_in).
	if s.BytesOut != 999 || s.BytesIn != 111 {
		t.Errorf("reordered header parse = in %d out %d, want in 111 out 999", s.BytesIn, s.BytesOut)
	}
}

func TestRateTalkers(t *testing.T) {
	blocks := parseNettopBlocks(nettopFixture)
	rates := rateTalkers(blocks, nettopWindowSec)
	if rates == nil {
		t.Fatal("rateTalkers returned nil on two valid blocks")
	}

	// 2.1.258.60633: out 1120146 -> 1177580 (delta 57434) over 2s => 28717 B/s up.
	up, ok := findBW(rates, 60633)
	if !ok {
		t.Fatal("pid 60633 (2.1.258) absent from rates")
	}
	if !approxBps(up.UpBytesPerSec, (1177580-1120146)/2.0) {
		t.Errorf("2.1.258 up rate = %v, want %v", up.UpBytesPerSec, (1177580-1120146)/2.0)
	}

	// mDNSResponder.237: out moves only 132 B => ~66 B/s, FAR below 2.1.258 — the
	// whole point of a rate (cumulative 459 MB would top the list).
	md, ok := findBW(rates, 237)
	if !ok {
		t.Fatal("pid 237 (mDNSResponder) absent from rates")
	}
	if !approxBps(md.UpBytesPerSec, (459022532-459022400)/2.0) {
		t.Errorf("mDNSResponder up rate = %v, want %v", md.UpBytesPerSec, (459022532-459022400)/2.0)
	}
	if md.UpBytesPerSec >= up.UpBytesPerSec {
		t.Errorf("mDNSResponder (%v) must rate BELOW 2.1.258 (%v) — rate beats cumulative", md.UpBytesPerSec, up.UpBytesPerSec)
	}

	// netbiosd.19249 is byte-identical across blocks => exactly 0 rate (idle).
	nb, ok := findBW(rates, 19249)
	if !ok {
		t.Fatal("pid 19249 (netbiosd) absent")
	}
	if nb.UpBytesPerSec != 0 || nb.DownBytesPerSec != 0 {
		t.Errorf("idle netbiosd rate = up %v down %v, want 0/0", nb.UpBytesPerSec, nb.DownBytesPerSec)
	}

	// configd.129 is block-2-ONLY (no baseline) => DROPPED, never its since-start
	// total divided by the window.
	if _, ok := findBW(rates, 129); ok {
		t.Error("configd.129 (new in window, no baseline) must be DROPPED from rates")
	}
}

func TestRateTalkersResetClamp(t *testing.T) {
	// Counter went BACKWARDS (PID reuse / process restart): cur<prev must clamp to 0,
	// never wrap to a ~1.8e19 phantom.
	blocks := [][]procSample{
		{{Name: "p", PID: 7, BytesIn: 1000, BytesOut: 5000}},
		{{Name: "p", PID: 7, BytesIn: 10, BytesOut: 50}},
	}
	rates := rateTalkers(blocks, 2)
	if len(rates) != 1 {
		t.Fatalf("len(rates) = %d, want 1", len(rates))
	}
	if rates[0].UpBytesPerSec != 0 || rates[0].DownBytesPerSec != 0 {
		t.Errorf("reset clamp = up %v down %v, want 0/0 (no uint64 wrap)", rates[0].UpBytesPerSec, rates[0].DownBytesPerSec)
	}
}

func TestRateTalkersFewerThanTwoBlocks(t *testing.T) {
	if got := rateTalkers(nil, 2); got != nil {
		t.Errorf("rateTalkers(nil) = %v, want nil", got)
	}
	one := [][]procSample{{{Name: "p", PID: 1, BytesIn: 1, BytesOut: 1}}}
	if got := rateTalkers(one, 2); got != nil {
		t.Errorf("rateTalkers(one block) = %v, want nil (a lone cumulative sample is not a rate)", got)
	}
}

func TestRateTalkersTwoBlocksNoOverlap(t *testing.T) {
	// Two blocks but NO shared PID => available-but-empty (non-nil), NOT nil: "nobody
	// matched" is distinct from "we could not look".
	blocks := [][]procSample{
		{{Name: "a", PID: 1, BytesIn: 1, BytesOut: 1}},
		{{Name: "b", PID: 2, BytesIn: 2, BytesOut: 2}},
	}
	got := rateTalkers(blocks, 2)
	if got == nil {
		t.Fatal("two blocks with no overlap must return a non-nil empty slice (available)")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestTopTalkers(t *testing.T) {
	procs := []ProcBandwidth{
		{Name: "big-up", PID: 1, UpBytesPerSec: 1000, DownBytesPerSec: 10},
		{Name: "mid-up", PID: 2, UpBytesPerSec: 500, DownBytesPerSec: 20},
		{Name: "low-up", PID: 3, UpBytesPerSec: 100, DownBytesPerSec: 30},
		{Name: "download-only", PID: 4, UpBytesPerSec: 0, DownBytesPerSec: 9999},
		{Name: "idle", PID: 5, UpBytesPerSec: 0, DownBytesPerSec: 0},
	}

	// n=3 upstream: the 3 largest UP, descending; download-only + idle DROPPED.
	top := topTalkers(procs, 3, false)
	if len(top) != 3 {
		t.Fatalf("len(top) = %d, want 3", len(top))
	}
	wantOrder := []int{1, 2, 3}
	for i, pid := range wantOrder {
		if top[i].PID != pid {
			t.Errorf("top[%d].PID = %d, want %d", i, top[i].PID, pid)
		}
	}
	for _, p := range top {
		if p.PID == 4 {
			t.Error("download-only proc (up=0) must be DROPPED from the upstream ranking")
		}
	}

	// byDown=true: the symmetric downstream ranking — download-only now TOPS it.
	down := topTalkers(procs, 5, true)
	if len(down) == 0 || down[0].PID != 4 {
		t.Errorf("downstream top = %+v, want pid 4 (download-only) first", down)
	}

	// n > len => all survivors (3 have up>0).
	all := topTalkers(procs, 99, false)
	if len(all) != 3 {
		t.Errorf("n>len upstream = %d survivors, want 3", len(all))
	}
	// n <= 0 => empty.
	if got := topTalkers(procs, 0, false); len(got) != 0 {
		t.Errorf("n=0 => %d, want 0", len(got))
	}
	if got := topTalkers(procs, -1, false); len(got) != 0 {
		t.Errorf("n=-1 => %d, want 0", len(got))
	}
}

func TestTopTalkersTieBreak(t *testing.T) {
	// Equal UP rate: broken by DOWN (desc), then Name (asc), then PID (asc).
	procs := []ProcBandwidth{
		{Name: "bravo", PID: 10, UpBytesPerSec: 100, DownBytesPerSec: 5},
		{Name: "alpha", PID: 20, UpBytesPerSec: 100, DownBytesPerSec: 5},
		{Name: "alpha", PID: 15, UpBytesPerSec: 100, DownBytesPerSec: 5},
		{Name: "charlie", PID: 1, UpBytesPerSec: 100, DownBytesPerSec: 9},
	}
	top := topTalkers(procs, 4, false)
	// charlie first (higher down), then alpha/15, alpha/20 (name asc, pid asc), bravo.
	wantPIDs := []int{1, 15, 20, 10}
	for i, pid := range wantPIDs {
		if top[i].PID != pid {
			t.Errorf("tie-break top[%d].PID = %d, want %d (total order: down, name, pid)", i, top[i].PID, pid)
		}
	}
}

func TestTopTalkersFromFixture(t *testing.T) {
	// End-to-end: the real capture, ranked upstream, surfaces 2.1.258.60633 as #1 —
	// the AI-assistant-style talker the headline exists for — and never a cumulative
	// daemon.
	blocks := parseNettopBlocks(nettopFixture)
	rates := rateTalkers(blocks, nettopWindowSec)
	top := topTalkers(rates, 5, false)
	if len(top) == 0 {
		t.Fatal("no upstream talkers from the fixture")
	}
	if top[0].PID != 60633 {
		t.Errorf("top upstream talker PID = %d (%q), want 60633 (2.1.258)", top[0].PID, top[0].Name)
	}
	// netbiosd (idle) and the all-zero daemons must NOT appear.
	for _, p := range top {
		if p.PID == 19249 {
			t.Error("idle netbiosd must not appear in the upstream ranking")
		}
	}
}

func TestClampProcsInterval(t *testing.T) {
	cases := []struct {
		in   int // seconds
		want int // seconds
	}{
		{0, 10},  // <=0 => default 10
		{-5, 10}, // negative => default
		{1, 5},   // below floor => 5
		{4, 5},   // below floor => 5
		{5, 5},   // floor
		{10, 10}, // default
		{30, 30}, // passthrough
	}
	for _, c := range cases {
		got := clampProcsInterval(dur(c.in))
		if got != dur(c.want) {
			t.Errorf("clampProcsInterval(%ds) = %v, want %ds", c.in, got, c.want)
		}
	}
}

func TestClampTopTalkersN(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 5},   // <1 => default
		{-3, 5},  // negative => default
		{1, 1},   //
		{5, 5},   //
		{20, 20}, // cap
		{21, 20}, // over cap => 20
		{1000, 20},
	}
	for _, c := range cases {
		if got := clampTopTalkersN(c.in); got != c.want {
			t.Errorf("clampTopTalkersN(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatTopTalkers(t *testing.T) {
	// Available with talkers: header, the always-on sudo-free footnote, and the ~2s
	// window truth all present; an opaque PID is shown so the user can ps -p it.
	talkers := []ProcBandwidth{
		{Name: "2.1.258", PID: 60633, UpBytesPerSec: 28717, DownBytesPerSec: 174},
	}
	out := formatTopTalkers(talkers, true)
	for _, want := range []string{"PROCESS", "PID", "UP", "DOWN", "60633", "2.1.258", "~2s window", "YOUR processes only", "ps -p"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatTopTalkers(available) missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "sudo ") {
		t.Errorf("footnote must not instruct the user to sudo:\n%s", out)
	}

	// Unavailable: an honest one-line notice, NO table header, and it does not
	// instruct escalation.
	un := formatTopTalkers(nil, false)
	if !strings.Contains(un, "unavailable") {
		t.Errorf("unavailable output must say so:\n%s", un)
	}
	if strings.Contains(un, "PROCESS") {
		t.Errorf("unavailable output must not print the table header:\n%s", un)
	}
}
