package main

// nettop.go — Phase 11 PER-PROCESS BANDWIDTH. "Who is eating my upstream NOW?"
//
// macOS's `nettop` exposes per-process cumulative byte counters sudo-free (for the
// invoking user's processes). A single cumulative snapshot is permanently topped by
// long-lived daemons (mDNSResponder has read gigabytes since boot and barely moves
// in a 2s window), which buries the AI assistants this feature exists to surface. So
// we take TWO cumulative snapshots ~2s apart in ONE exec (`nettop -L 2 -s 2`), match
// by PID, subtract, and divide by the window => a per-second RATE. The rate is the
// only truthful answer to "what is eating my upstream now".
//
// Pure/impure split (house style): procSample / ProcBandwidth / parseNettopBlocks /
// rateTalkers / topTalkers / clampProcsInterval / clampTopTalkersN are PURE and
// table-tested (nettop_test.go, against a VERBATIM `-L 2 -s 2` capture); only
// readTopTalkers execs. Everything here is reachable ONLY from --top-talkers and the
// --serve procs loop — a bare run / --sample execs ZERO nettop.
//
// NO SUDO: nettop shows the invoking user's processes without elevation; a complete
// system-wide accounting can need more. We DEGRADE GRACEFULLY — show what we can,
// label it honestly — and NEVER prompt for or require sudo. An empty / permission-
// denied / unparseable / exit-0-usage-text nettop becomes the "unavailable" signal
// (nil), never a crash or a wrong number.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultProcsInterval is the --serve procs-loop cadence. SLOW on purpose:
	// nettop + a 2s window is far heavier than a ping, so this is 10s, not 1s.
	defaultProcsInterval = 10 * time.Second
	// minProcsInterval is the floor. It MUST exceed the nettop window + exec timeout
	// so a wedged exec can never overlap the next tick (ticks never pile up / starve).
	minProcsInterval = 5 * time.Second
	// defaultTopTalkersN / maxTopTalkersN bound the ranked rows (wire + panel).
	defaultTopTalkersN = 5
	maxTopTalkersN     = 20

	// nettopWindowSec is the `-s` interval between the two cumulative blocks. The
	// rate is computed over EXACTLY this window (float64(nettopWindowSec)).
	nettopWindowSec = 2
	// nettopExecTimeout bounds a hung nettop: the ~2s window + margin, but kept
	// STRICTLY below minProcsInterval (5s) so a timed-out exec can never overlap the
	// next tick even at the interval floor. A healthy `-L 2 -s 2` (~2s wall) has
	// ~2.5s of headroom and is never killed; a genuinely hung nettop surfaces as
	// unavailable via context.DeadlineExceeded.
	nettopExecTimeout = 4500 * time.Millisecond
)

// procSample is one process's CUMULATIVE byte counters from ONE nettop block (the
// intermediate parse type, NOT the wire type). PID is the join key across the two
// blocks.
type procSample struct {
	Name     string
	PID      int
	BytesIn  uint64
	BytesOut uint64
}

// ProcBandwidth is the WIRE type: one process's per-second RATE, derived by
// subtracting two cumulative snapshots. Rates (not totals) so humanBps and the tile
// read truthfully. PID is surfaced prominently — nettop truncates names and some are
// opaque ("2.1.258"), so the PID is the handle the user can `ps -p <pid>`.
type ProcBandwidth struct {
	Name            string  `json:"name"`
	PID             int     `json:"pid,omitempty"`
	UpBytesPerSec   float64 `json:"up_bytes_per_sec"`
	DownBytesPerSec float64 `json:"down_bytes_per_sec"`
}

// nettopHeaderCols scans a comma-split nettop line for the bytes_in / bytes_out
// header columns BY NAME (never a hardcoded index), so a column reorder cannot read
// the wrong field. A line is a header/block-boundary iff BOTH names are present.
// VERSION NOTE: the header spelling (bytes_in/bytes_out) is nettop-version-specific;
// a macOS rename of these columns degrades the whole tile SILENTLY to unavailable
// (no header found => parseNettopBlocks returns nil), by design — never a crash or a
// wrong number.
func nettopHeaderCols(fields []string) (inIdx, outIdx int, ok bool) {
	inIdx, outIdx = -1, -1
	for i, f := range fields {
		switch strings.TrimSpace(f) {
		case "bytes_in":
			inIdx = i
		case "bytes_out":
			outIdx = i
		}
	}
	if inIdx >= 0 && outIdx >= 0 {
		return inIdx, outIdx, true
	}
	return 0, 0, false
}

// splitNamePID splits a nettop process-identity token into Name + PID. Identity is
// POSITIONAL (field 0 — it has a BLANK header, so it CANNOT be matched by name). The
// PID is the trailing all-numeric segment after the LAST '.':
//   - "com.apple.WebKi.13595" => ("com.apple.WebKi", 13595)
//   - "2.1.258.60633"         => ("2.1.258", 60633)   [adversarial all-numeric name]
//   - "Slack Helper.61668"    => ("Slack Helper", 61668) [spaces survive the comma split]
//   - a token with no trailing numeric segment => (whole token, 0)
func splitNamePID(token string) (name string, pid int) {
	token = strings.TrimSpace(token)
	idx := strings.LastIndex(token, ".")
	if idx < 0 || idx == len(token)-1 {
		return token, 0
	}
	n, err := strconv.Atoi(token[idx+1:])
	if err != nil || n < 0 {
		return token, 0 // no trailing numeric segment => keep the name whole, PID 0
	}
	return token[:idx], n
}

// parseNettopBlocks parses `nettop -P -x -L 2 -s N -J bytes_in,bytes_out` into one
// []procSample PER SAMPLE BLOCK (so [][]procSample). nettop output is COMMA-
// delimited (names contain spaces, e.g. "Slack Helper"), with a TRAILING comma on
// every line and a header line `,bytes_in,bytes_out,` whose process-identity column
// has a BLANK header. `-L 2` emits TWO blocks, EACH prefixed by that header line.
//
//   - A header line (both bytes_in/bytes_out present) STARTS a new block AND pins the
//     byte-column indices for that block (header-name match, not a fixed index).
//   - ONE skip predicate drops header, blank, and garbled rows: skip any row whose
//     bytes_in/bytes_out fields do NOT both parse as uint64. The header line
//     ("bytes_in"/"bytes_out" non-numeric), the trailing blank line, and any short/
//     corrupt row fall out naturally — no blind "skip line 0". ParseUint(…,64) is
//     MANDATORY: values exceed uint32 (mDNSResponder reads 9631465568 bytes).
//   - Returns nil (the unavailable signal) when NO header with bytes_in/bytes_out is
//     found (covers empty output AND nettop's exit-0 usage-text blob from a bad -J)
//     OR when zero data rows parse in total. Never panics.
func parseNettopBlocks(out string) [][]procSample {
	var blocks [][]procSample
	var cur []procSample
	inBlock := false
	biIdx, boIdx := 1, 2

	flush := func() {
		if inBlock {
			blocks = append(blocks, cur)
		}
	}

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if hi, ho, ok := nettopHeaderCols(fields); ok {
			flush() // close the previous block (if any) before opening the next
			cur = nil
			inBlock = true
			biIdx, boIdx = hi, ho
			continue
		}
		if !inBlock { // garbage before the first header (shouldn't happen) is ignored
			continue
		}
		if len(fields) <= biIdx || len(fields) <= boIdx {
			continue // short/garbled row
		}
		in, errIn := strconv.ParseUint(strings.TrimSpace(fields[biIdx]), 10, 64)
		bout, errOut := strconv.ParseUint(strings.TrimSpace(fields[boIdx]), 10, 64)
		if errIn != nil || errOut != nil {
			continue // the single unparseable-bytes predicate: header / blank / corrupt
		}
		name, pid := splitNamePID(fields[0])
		cur = append(cur, procSample{Name: name, PID: pid, BytesIn: in, BytesOut: bout})
	}
	flush()

	if len(blocks) == 0 {
		return nil // no header found: empty output / usage-text blob / unparseable
	}
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	if total == 0 {
		return nil // headers but zero data rows => unavailable, not an empty success
	}
	return blocks
}

// rateTalkers converts the parsed blocks into per-process RATES. It diffs the FIRST
// block (sample A) against the LAST block (sample B), keyed by PID, over intervalSec
// seconds (the -s window). It REUSES deltaRate's discipline (bandwidth.go): the
// cur<prev reset check runs BEFORE any subtraction, so a uint64 counter-went-
// backwards (PID reuse / process restart) CLAMPS to 0 and never wraps to a ~1.8e19
// phantom. A PID present in sample B but ABSENT from sample A (started mid-window, no
// baseline) is DROPPED — never divide a since-start total by the window (that would
// massively overstate a freshly launched uploader). Returns nil ONLY when fewer than
// TWO blocks are usable (a partial/truncated capture => unavailable, never a lone
// cumulative sample mislabeled as a rate); with two blocks but no overlapping PID it
// returns a non-nil empty slice (available-but-nobody-matched, not "we could not look").
func rateTalkers(blocks [][]procSample, intervalSec float64) []ProcBandwidth {
	if len(blocks) < 2 {
		return nil
	}
	a := blocks[0]
	b := blocks[len(blocks)-1]
	base := make(map[int]procSample, len(a))
	for _, p := range a {
		base[p.PID] = p
	}
	out := []ProcBandwidth{} // non-nil: >=2 blocks is "available" even with no overlap
	for _, cur := range b {
		prev, ok := base[cur.PID]
		if !ok {
			continue // new in window, no baseline => drop
		}
		up, _ := deltaRate(prev.BytesOut, cur.BytesOut, intervalSec)
		down, _ := deltaRate(prev.BytesIn, cur.BytesIn, intervalSec)
		out = append(out, ProcBandwidth{Name: cur.Name, PID: cur.PID, UpBytesPerSec: up, DownBytesPerSec: down})
	}
	return out
}

// topTalkers ranks by the chosen direction (byDown=false => upstream /
// UpBytesPerSec, the default). DESCENDING, with a TOTAL order for determinism:
// ranked-direction rate, then the other direction, then Name, then PID (PID is the
// final tiebreaker because Name collides across distinct helper PIDs). Uses
// sort.SliceStable (as airspace.go does). DROPS processes with a ZERO (or negative)
// rate in the RANKED direction — an idle / download-only process must not occupy an
// upstream slot showing "0 bps up". Caps at n; n<=0 => empty.
func topTalkers(procs []ProcBandwidth, n int, byDown bool) []ProcBandwidth {
	if n <= 0 {
		return nil
	}
	rankRate := func(p ProcBandwidth) float64 {
		if byDown {
			return p.DownBytesPerSec
		}
		return p.UpBytesPerSec
	}
	otherRate := func(p ProcBandwidth) float64 {
		if byDown {
			return p.UpBytesPerSec
		}
		return p.DownBytesPerSec
	}
	var filtered []ProcBandwidth
	for _, p := range procs {
		if rankRate(p) <= 0 {
			continue // drop idle / wrong-direction-only from this ranking
		}
		filtered = append(filtered, p)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		pi, pj := filtered[i], filtered[j]
		if ri, rj := rankRate(pi), rankRate(pj); ri != rj {
			return ri > rj
		}
		if oi, oj := otherRate(pi), otherRate(pj); oi != oj {
			return oi > oj
		}
		if pi.Name != pj.Name {
			return pi.Name < pj.Name
		}
		return pi.PID < pj.PID
	})
	if n < len(filtered) {
		filtered = filtered[:n]
	}
	return filtered
}

// clampProcsInterval mirrors clampReachInterval: d<=0 => default 10s; floor
// minProcsInterval (5s) so a stray small/zero value cannot become a nettop exec
// storm nor let a timed-out exec overlap the next tick.
func clampProcsInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultProcsInterval
	}
	if d < minProcsInterval {
		return minProcsInterval
	}
	return d
}

// clampTopTalkersN mirrors clampOutageThreshold: n<1 => default 5; cap at
// maxTopTalkersN (20) so an unbounded N cannot bloat the /procs.json wire / panel.
func clampTopTalkersN(n int) int {
	if n < 1 {
		return defaultTopTalkersN
	}
	if n > maxTopTalkersN {
		return maxTopTalkersN
	}
	return n
}

// formatTopTalkers renders the --top-talkers one-shot terminal table: Process / PID
// / Up / Down (Up is the headline — rates already ranked upstream-first by the
// caller), the "~2s window" truth, and the honest sudo-free footnote (mirrors
// formatAirspace's trailing Note: convention). PURE (string out) so it is table-
// testable. An unavailable read prints an honest one-line notice — it NEVER prompts
// for or requires sudo. talkers is expected pre-ranked (topTalkers order).
func formatTopTalkers(talkers []ProcBandwidth, available bool) string {
	var b strings.Builder
	b.WriteString("netdebug: TOP TALKERS — per-process bandwidth rates (upstream-first, ~2s window)\n\n")
	if !available {
		b.WriteString("Per-process data unavailable (nettop returned nothing parseable, or was\n")
		b.WriteString("unreadable). This is a sudo-free read and we do NOT escalate.\n")
		return b.String()
	}
	if len(talkers) == 0 {
		b.WriteString("No upstream activity from your processes in the sample window.\n\n")
	} else {
		fmt.Fprintf(&b, "  %-24s %-8s %-13s %s\n", "PROCESS", "PID", "UP", "DOWN")
		for _, p := range talkers {
			pid := "—"
			if p.PID != 0 {
				pid = strconv.Itoa(p.PID)
			}
			fmt.Fprintf(&b, "  %-24s %-8s %-13s %s\n",
				truncName(p.Name), pid, humanBps(p.UpBytesPerSec), humanBps(p.DownBytesPerSec))
		}
		b.WriteString("\n")
	}
	b.WriteString("Note: rates are a per-second average over a ~2s window (a bursty uploader may\n")
	b.WriteString("read below its peak). Shows YOUR processes only; some system processes may be\n")
	b.WriteString("hidden without elevated rights (ps -p <pid> to identify an opaque name).\n")
	return b.String()
}

// truncName bounds an adversarial process name for the fixed-width terminal table so
// one long name cannot shove the columns off-screen (the dashboard uses textContent,
// never this). 23 chars + an ellipsis fits the %-24s column.
func truncName(name string) string {
	if name == "" {
		return "(unknown)"
	}
	const max = 23
	if len(name) > max {
		return name[:max-1] + "…"
	}
	return name
}

// readTopTalkers execs the ONE nettop snapshot command and returns (talkers,
// available). UNAVAILABLE (nil, false) on: a non-nil exec error INCLUDING a timeout
// kill (context.DeadlineExceeded); a nil parseNettopBlocks (empty / permission-
// denied / exit-0 usage text / unparseable); or a nil rateTalkers (fewer than two
// usable blocks). Availability hinges on PARSE SUCCESS, never on the exit code (a bad
// -J exits 0 with usage text) and never on len(talkers)==0 (everyone idle is
// available-but-empty, which is NOT "we could not look").
//
// Absolute /usr/bin/nettop (launchd thins PATH; same hardening as reach.go's
// /sbin/ping). LC_ALL=C is belt-and-suspenders against a locale digit-grouping
// surprise. exec.CommandContext bounds a hung nettop and lets --serve shutdown abort
// an in-flight run.
func readTopTalkers(ctx context.Context) (procs []ProcBandwidth, available bool) {
	cctx, cancel := context.WithTimeout(ctx, nettopExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/usr/bin/nettop",
		"-P", "-x", "-L", "2", "-s", strconv.Itoa(nettopWindowSec), "-J", "bytes_in,bytes_out")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, false // includes DeadlineExceeded from the timeout kill
	}
	blocks := parseNettopBlocks(string(out))
	if blocks == nil {
		return nil, false // empty / permission-denied / usage-text / unparseable
	}
	talkers := rateTalkers(blocks, float64(nettopWindowSec))
	if talkers == nil {
		return nil, false // fewer than two usable blocks
	}
	return talkers, true
}
