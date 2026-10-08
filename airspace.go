package main

// Phase 10 — AIRSPACE / CHANNEL CONGESTION.
//
// This file is the PURE core (Neighbor, parseNeighborNetworks, channelLabel,
// channelSpan, channelOccupancy, clearestBlock, airspaceJSON) plus ONE thin
// impure reader (readRadio) on the serve/one-shot seam. Everything pure is
// table-tested in airspace_test.go.
//
// DATA SOURCE (hard constraint, phase10 §0): the "Other Local Wi-Fi Networks:"
// section that `system_profiler SPAirPortDataType` ALREADY emits — a sudo-free,
// packet-free, NON-disruptive, cached PASSIVE scan. We NEVER trigger an active
// scan (not the deprecated airport CLI's scan flag, not a wdutil air scan, not a
// networksetup Wi-Fi switch), because an active scan forces the radio off-channel
// and can DROP the very connection we are monitoring — exactly the user's problem.
// TestNoActiveScanCommands is the automated regression guard for this.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// strongRSSI is the interference threshold: a neighbor at RSSI >= -80 dBm is a
// "strong"/harmful co-channel interferer; weaker APs are distant background. This
// promotes interference ENERGY (not AP population) to a story driver so the 5 GHz
// recommendation reflects who actually hurts you (the -46 dBm 149/157 neighbors),
// not the crowd of -86..-95 dBm 2.4 GHz APs that are effectively harmless.
const strongRSSI = -80

// Neighbor is one AP from the passive "Other Local Wi-Fi Networks:" list. Name is
// "" when macOS redacts it (the common case). A record with no parseable channel
// is dropped by parseNeighborNetworks (never a bogus channel 0), so ChannelNum is
// always non-zero on a returned Neighbor.
type Neighbor struct {
	Name       string `json:"name,omitempty"`    // "" when redacted / absent
	ChannelNum int    `json:"channel_num"`       // center channel
	Band       string `json:"band,omitempty"`    // normalizeBand output ("5 GHz")
	Width      string `json:"width,omitempty"`   // "80MHz"
	Channel    string `json:"channel,omitempty"` // "149/80" via channelLabel
	RSSI       int    `json:"rssi,omitempty"`    // dBm, negative; 0 => absent (omitted)
}

// ChannelLoad is the per-center-channel occupancy row (phase10 §1a). Count/
// StrongCount/MaxRSSI are the NEIGHBOR-ONLY discrete tally (the raw passive read);
// OverlapCount/OverlapStrong are the WIDTH-AWARE load that INCLUDES my own AP's
// channelSpan footprint, so a crowded 80MHz block renders as load on all four of
// its 20MHz subchannels.
type ChannelLoad struct {
	ChannelNum    int    `json:"channel_num"`
	Band          string `json:"band"`
	Count         int    `json:"count"`                    // neighbors whose center == this channel (discrete)
	StrongCount   int    `json:"strong_count,omitempty"`   // of Count, RSSI >= strongRSSI
	OverlapCount  int    `json:"overlap_count,omitempty"`  // APs (incl. mine) whose channelSpan covers this channel
	OverlapStrong int    `json:"overlap_strong,omitempty"` // of OverlapCount, strong (my AP counts as strong)
	Mine          bool   `json:"mine,omitempty"`           // my current channel's center
	MaxRSSI       int    `json:"max_rssi,omitempty"`       // strongest NEIGHBOR whose center == this
}

// BlockRec is the clearest-80MHz-block recommendation (phase10 §1a).
type BlockRec struct {
	Band     string `json:"band"`     // "5 GHz"
	Channels []int  `json:"channels"` // e.g. [36,40,44,48]
	Label    string `json:"label"`    // "36/80"
	DFS      bool   `json:"dfs"`      // 52-144 are DFS (radar-sensitive); 36-48 + 149-165 are not
	Load     int    `json:"load"`     // summed OverlapStrong over the block
}

// channelLabel builds the "149/80" form from (num, width). EXTRACTED verbatim from
// the inline code in parseAirportSample (signal.go) and shared by BOTH parsers so
// SignalSample.Channel and Neighbor.Channel can never drift.
func channelLabel(num int, width string) string {
	if width != "" {
		return strconv.Itoa(num) + "/" + strings.TrimSuffix(width, "MHz")
	}
	if num != 0 {
		return strconv.Itoa(num)
	}
	return ""
}

// neighborFieldKey returns the canonical key for a recognized neighbor field line
// (reusing the SAME prefixes parseAirportSample keys off) and the value after the
// prefix. ok=false for any other line. These keys both drive record boundaries in
// a headerless run and tell a field line apart from a "<name>:" header.
func neighborFieldKey(t string) (key, val string, ok bool) {
	for _, k := range []string{"PHY Mode:", "Channel:", "Network Type:", "Security:", "Signal / Noise:"} {
		if strings.HasPrefix(t, k) {
			return k, strings.TrimSpace(strings.TrimPrefix(t, k)), true
		}
	}
	return "", "", false
}

// parseNeighborNetworks extracts the "Other Local Wi-Fi Networks:" block from
// `system_profiler SPAirPortDataType`.
//
// STEP 1 (indent-scope, EXACTLY like parseAirportSample): find the FIRST "Other
// Local Wi-Fi Networks:" header, record its indent H, collect the following lines
// more indented than H, and STOP at the first non-blank line at indent <= H (the
// next interface, e.g. "awdl0:" at indent 8). First-match + the <=H stop are
// load-bearing: they exclude awdl0's own trailing Current Network Information and
// scope to the target iface's first block.
//
// STEP 2 (record split, robust to BOTH observed layouts): walk the block. A field
// line sets a field on the current record; a "<name>:" header line (a line ending
// in ":" that is NOT a field key) OR a re-appearing field key that the current
// record already set (the occasionally-observed HEADERLESS run) both start a new
// record. Records are flushed on each boundary and at end.
//
// NAME RULE: Name = TrimSuffix(headerLine, ":"); if that is not a usable SSID (""
// or the "<redacted>" sentinel) Name stays "". SKIP RULE: a record is kept only
// when ChannelNum != 0 (an unparseable/absent channel => dropped, never ch 0); a
// record with a channel but no Signal/Noise is KEPT (RSSI 0, omitted).
//
// EMPTY OUTCOMES: header ABSENT => nil; header PRESENT but zero parseable
// neighbors => empty len-0 slice.
func parseNeighborNetworks(out string) []Neighbor {
	lines := strings.Split(out, "\n")
	start := -1
	var headerIndent int
	for i, ln := range lines {
		if strings.Contains(ln, "Other Local Wi-Fi Networks:") {
			start = i
			headerIndent = indentOf(ln)
			break
		}
	}
	if start < 0 {
		return nil // header absent (distinct from present-but-empty)
	}

	var neighbors []Neighbor
	var cur Neighbor
	have := false
	seen := map[string]bool{}

	flush := func() {
		if have && cur.ChannelNum != 0 {
			neighbors = append(neighbors, cur)
		}
		cur = Neighbor{}
		have = false
		seen = map[string]bool{}
	}

	for j := start + 1; j < len(lines); j++ {
		ln := lines[j]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if indentOf(ln) <= headerIndent {
			break // next section / interface — scope ends here
		}
		t := strings.TrimSpace(ln)
		if key, val, ok := neighborFieldKey(t); ok {
			if seen[key] { // a field we already set re-appears => headerless record boundary
				flush()
			}
			have = true
			seen[key] = true
			switch key {
			case "Channel:":
				num, band, width := parseChannelSpec(val)
				cur.ChannelNum, cur.Band, cur.Width = num, band, width
				cur.Channel = channelLabel(num, width)
			case "Signal / Noise:":
				if rssi, _, okSN := parseSignalNoise(val); okSN {
					cur.RSSI = rssi
				}
			}
			continue
		}
		if strings.HasSuffix(t, ":") { // "<name>:" header (not a field key)
			flush()
			have = true
			name := strings.TrimSuffix(t, ":")
			if usableSSID(name) { // "" or "<redacted>" => leave Name ""
				cur.Name = name
			}
		}
		// any other line (e.g. a stray unrecognized field) is ignored
	}
	flush()

	if neighbors == nil {
		return []Neighbor{} // header present, zero parseable => empty, not nil
	}
	return neighbors
}

// the standard aligned 5 GHz channel blocks (phase10 §1a). The UNII numbering is
// NOT uniform across the 144->149 gap, so these are PINNED, never computed from 36.
var blocks5_80 = [][]int{
	{36, 40, 44, 48}, {52, 56, 60, 64},
	{100, 104, 108, 112}, {116, 120, 124, 128},
	{132, 136, 140, 144}, {149, 153, 157, 161},
}
var blocks5_160 = [][]int{
	{36, 40, 44, 48, 52, 56, 60, 64},
	{100, 104, 108, 112, 116, 120, 124, 128},
}

// widthMHz reads the leading integer of a width token ("80MHz" / "80" => 80).
func widthMHz(width string) int {
	n := 0
	for _, c := range width {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

// channelSpan returns the 20MHz subchannel centers an AP occupies (phase10 §1a).
// This single helper subsumes the 2.4 GHz overlap model (overlaps24): a 2.4 GHz AP
// on ch 4/6/8 correctly congests the 1/6/11 non-overlapping set.
func channelSpan(num int, band, width string) []int {
	mhz := widthMHz(width)
	switch band {
	case "2.4 GHz":
		// 5 MHz channel spacing: 20MHz => ±2, 40MHz => ±4. Clamp to 1..13.
		off := mhz / 10
		if off < 1 {
			off = 2 // bare/unknown width behaves like 20MHz
		}
		var out []int
		for c := num - off; c <= num+off; c++ {
			if c >= 1 && c <= 13 {
				out = append(out, c)
			}
		}
		if out == nil {
			out = []int{num}
		}
		return out
	case "6 GHz":
		// 6 GHz channels 1,5,9,...,233 ARE uniform (step 4), aligned to channel 1.
		size := mhz / 20
		if size < 1 {
			return []int{num}
		}
		sub := (num - 1) / 4        // 0-based 20MHz index
		base := (sub / size) * size // first sub index of the aligned block
		var out []int
		for k := 0; k < size; k++ {
			out = append(out, 1+4*(base+k))
		}
		return out
	default:
		// 5 GHz (and any non-2.4/6 band): pinned-block lookup.
		switch mhz {
		case 160:
			for _, b := range blocks5_160 {
				if containsInt(b, num) {
					return append([]int(nil), b...)
				}
			}
			return []int{num}
		case 80:
			for _, b := range blocks5_80 {
				if containsInt(b, num) {
					return append([]int(nil), b...)
				}
			}
			return []int{num}
		case 40:
			// the adjacent pair within the containing 80MHz block.
			for _, b := range blocks5_80 {
				if b[0] == num || b[1] == num {
					return []int{b[0], b[1]}
				}
				if b[2] == num || b[3] == num {
					return []int{b[2], b[3]}
				}
			}
			return []int{num}
		default:
			return []int{num} // 20MHz / center not in any block
		}
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// bandForChannelNum infers the band of MY joined channel from its number (the
// LinkSnap "157/80" form carries no band label). 1..14 => 2.4 GHz, 36+ => 5 GHz.
// 6 GHz my-AP is not inferred (this radio has no 6E; the 6 GHz span math is
// exercised only by neighbors carrying an explicit "(6GHz,...)" band token).
func bandForChannelNum(num int) string {
	switch {
	case num >= 1 && num <= 14:
		return "2.4 GHz"
	case num >= 36:
		return "5 GHz"
	default:
		return ""
	}
}

// parseChannelLabel splits the LinkSnap "157/80" form into (num, width "80MHz").
// A bare "157" yields (157, ""). Distinct from parseChannelSpec, which takes the
// RAW "157 (5GHz, 80MHz)" form.
func parseChannelLabel(s string) (num int, width string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	numPart := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		numPart = s[:i]
		w := strings.TrimSpace(s[i+1:])
		if w != "" {
			width = w + "MHz"
		}
	}
	num, _ = strconv.Atoi(strings.TrimSpace(numPart))
	return num, width
}

// channelOccupancy computes the per-center-channel load table (phase10 §1a). It
// returns both the discrete neighbor tally (Count/StrongCount/MaxRSSI) and the
// width-aware overlap load (OverlapCount/OverlapStrong, which INCLUDE my own AP's
// footprint). myChannel is the LinkSnap "157/80" form; "" means not associated.
// Sorted by band (2.4 -> 5 -> 6, chart left-to-right) then NUMERIC channel.
func channelOccupancy(neighbors []Neighbor, myChannel string) []ChannelLoad {
	myNum, myWidth := parseChannelLabel(myChannel)
	myBand := bandForChannelNum(myNum)

	// precompute each AP's width-aware span.
	type spanned struct {
		band string
		span []int
		rssi int
		mine bool
	}
	var aps []spanned
	for _, n := range neighbors {
		aps = append(aps, spanned{band: n.Band, span: channelSpan(n.ChannelNum, n.Band, n.Width), rssi: n.RSSI})
	}
	if myNum != 0 && myBand != "" {
		aps = append(aps, spanned{band: myBand, span: channelSpan(myNum, myBand, myWidth), mine: true})
	}

	// collect the set of center channels: every neighbor center + my center.
	type centerKey struct {
		band string
		num  int
	}
	order := []centerKey{}
	index := map[centerKey]*ChannelLoad{}
	ensure := func(band string, num int) *ChannelLoad {
		k := centerKey{band, num}
		if cl, ok := index[k]; ok {
			return cl
		}
		cl := &ChannelLoad{ChannelNum: num, Band: band}
		index[k] = cl
		order = append(order, k)
		return cl
	}

	for _, n := range neighbors {
		cl := ensure(n.Band, n.ChannelNum)
		cl.Count++
		if n.RSSI != 0 {
			if cl.MaxRSSI == 0 || n.RSSI > cl.MaxRSSI {
				cl.MaxRSSI = n.RSSI
			}
			if n.RSSI >= strongRSSI {
				cl.StrongCount++
			}
		}
	}
	if myNum != 0 && myBand != "" {
		ensure(myBand, myNum).Mine = true
		// Seed EVERY 20MHz subchannel of my own block as a row (phase10 §1a/§4): my
		// 157/80 must render the pile-up on ALL of 149/153/157/161, not just the
		// center 157 — so 153/161 appear even when no neighbor is CENTERED there.
		for _, c := range channelSpan(myNum, myBand, myWidth) {
			ensure(myBand, c)
		}
	}

	// width-aware overlap: for each center, count every AP (incl. mine) whose span
	// covers it in the same band.
	for _, k := range order {
		cl := index[k]
		for _, ap := range aps {
			if ap.band != k.band || !containsInt(ap.span, k.num) {
				continue
			}
			cl.OverlapCount++
			if ap.mine || (ap.rssi != 0 && ap.rssi >= strongRSSI) {
				cl.OverlapStrong++
			}
		}
	}

	out := make([]ChannelLoad, 0, len(order))
	for _, k := range order {
		out = append(out, *index[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		bi, bj := bandOrder(out[i].Band), bandOrder(out[j].Band)
		if bi != bj {
			return bi < bj
		}
		return out[i].ChannelNum < out[j].ChannelNum
	})
	return out
}

// bandOrder lays bands out left-to-right 2.4 -> 5 -> 6 on the chart (and in the
// wire array). Unknown bands sort last.
func bandOrder(band string) int {
	switch band {
	case "2.4 GHz":
		return 0
	case "5 GHz":
		return 1
	case "6 GHz":
		return 2
	default:
		return 3
	}
}

// is5GHzDFS reports whether a 5 GHz channel is in the DFS (radar-sensitive) range
// 52..144. UNII-1 (36-48) and UNII-3 (149-165) are non-DFS.
func is5GHzDFS(num int) bool {
	return num >= 52 && num <= 144
}

// clearestBlock scores the six standard 5 GHz 80MHz blocks by summed OverlapStrong
// and returns the least-loaded one (prefer non-DFS on a tie). Pure; no
// Supported-Channels parse needed. Returns an empty BlockRec (Channels nil) when
// the loads carry NO 5 GHz data at all — so a sparse/2.4-only scan yields no bogus
// "move to 36/80" advice.
func clearestBlock(loads []ChannelLoad) BlockRec {
	strongByNum := map[int]int{}
	any5 := false
	for _, cl := range loads {
		if cl.Band == "5 GHz" {
			any5 = true
			strongByNum[cl.ChannelNum] += cl.OverlapStrong
		}
	}
	if !any5 {
		return BlockRec{}
	}
	var best BlockRec
	haveBest := false
	for _, b := range blocks5_80 {
		load := 0
		for _, ch := range b {
			load += strongByNum[ch]
		}
		dfs := is5GHzDFS(b[0])
		rec := BlockRec{
			Band:     "5 GHz",
			Channels: append([]int(nil), b...),
			Label:    strconv.Itoa(b[0]) + "/80",
			DFS:      dfs,
			Load:     load,
		}
		if !haveBest || rec.Load < best.Load || (rec.Load == best.Load && !rec.DFS && best.DFS) {
			best = rec
			haveBest = true
		}
	}
	return best
}

// airspaceJSON serializes the /airspace.json payload. PURE: normalizes nil
// Channels AND nil Neighbors to [] (the house []-not-null rule, exactly like
// dataJSON/historyJSON). recommendation is null when its Channels is empty.
func airspaceJSON(loads []ChannelLoad, neighbors []Neighbor, myChannel string, rec BlockRec) ([]byte, error) {
	if loads == nil {
		loads = []ChannelLoad{}
	}
	if neighbors == nil {
		neighbors = []Neighbor{}
	}
	var recPtr *BlockRec
	if len(rec.Channels) > 0 {
		recPtr = &rec
	}
	return json.Marshal(struct {
		Channels       []ChannelLoad `json:"channels"`
		Neighbors      []Neighbor    `json:"neighbors"`
		MyChannel      string        `json:"my_channel"`
		Recommendation *BlockRec     `json:"recommendation"`
	}{Channels: loads, Neighbors: neighbors, MyChannel: myChannel, Recommendation: recPtr})
}

// neighborName renders a (possibly redacted) neighbor name for display.
func neighborName(name string) string {
	if name == "" {
		return "(hidden)"
	}
	return name
}

// rssiStr renders a dBm reading, or "—" when absent (0).
func rssiStr(rssi int) string {
	if rssi == 0 {
		return "—"
	}
	return strconv.Itoa(rssi) + " dBm"
}

// formatAirspace renders the --airspace one-shot terminal output: the neighbor
// table, the width-aware per-channel occupancy with your channel marked, the
// clearest-80MHz-block recommendation, and the passive/staleness caveat. PURE
// (string in, string out) so it is table-testable; the single system_profiler read
// happens in the caller. channels is expected pre-sorted (channelOccupancy order).
func formatAirspace(channels []ChannelLoad, neighbors []Neighbor, myChannel string, rec BlockRec) string {
	var b strings.Builder
	b.WriteString("netdebug: AIRSPACE — neighboring Wi-Fi + channel occupancy (passive, no active scan)\n\n")

	if myChannel != "" {
		num, _ := parseChannelLabel(myChannel)
		band := bandForChannelNum(num)
		if band != "" {
			fmt.Fprintf(&b, "Your channel: %s (%s)\n\n", myChannel, band)
		} else {
			fmt.Fprintf(&b, "Your channel: %s\n\n", myChannel)
		}
	} else {
		b.WriteString("Your channel: — (not associated / SSID hidden)\n\n")
	}

	if len(neighbors) == 0 {
		b.WriteString("No ambient scan data yet (the cached neighbor list may be empty or sparse —\n")
		b.WriteString("macOS populates it passively; it is NOT refreshed on demand here).\n\n")
	} else {
		fmt.Fprintf(&b, "Neighbor APs seen: %d\n", len(neighbors))
		fmt.Fprintf(&b, "  %-8s %-5s %-7s %-9s %s\n", "BAND", "CH", "WIDTH", "RSSI", "NAME")
		// table order: band then numeric channel (mirror the chart).
		ns := append([]Neighbor(nil), neighbors...)
		sort.SliceStable(ns, func(i, j int) bool {
			bi, bj := bandOrder(ns[i].Band), bandOrder(ns[j].Band)
			if bi != bj {
				return bi < bj
			}
			return ns[i].ChannelNum < ns[j].ChannelNum
		})
		for _, n := range ns {
			band := n.Band
			if band == "" {
				band = "?"
			}
			width := n.Width
			if width == "" {
				width = "—"
			}
			fmt.Fprintf(&b, "  %-8s %-5d %-7s %-9s %s\n", band, n.ChannelNum, width, rssiStr(n.RSSI), neighborName(n.Name))
		}
		b.WriteString("\n")

		b.WriteString("Channel occupancy (bar = overlap load incl. YOUR AP; strong = RSSI >= -80 dBm):\n")
		lastBand := ""
		for _, cl := range channels {
			if cl.Band != lastBand {
				fmt.Fprintf(&b, "  %s\n", cl.Band)
				lastBand = cl.Band
			}
			bar := strings.Repeat("#", cl.OverlapCount)
			if bar == "" {
				bar = "·"
			}
			line := fmt.Sprintf("    ch%-4d %-10s overlap %d (%d strong)  discrete %d",
				cl.ChannelNum, bar, cl.OverlapCount, cl.OverlapStrong, cl.Count)
			if cl.MaxRSSI != 0 {
				line += fmt.Sprintf("  worst %d dBm", cl.MaxRSSI)
			}
			if cl.Mine {
				line += "  [YOU]"
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	if len(rec.Channels) > 0 {
		dfs := "non-DFS"
		if rec.DFS {
			dfs = "DFS (radar-sensitive)"
		}
		fmt.Fprintf(&b, "Clearest 80MHz block: %s (%s)  [summed strong-overlap load: %d]\n\n", rec.Label, dfs, rec.Load)
	}

	b.WriteString("Note: ambient cached passive scan — may be minutes stale; not a live airtime\n")
	b.WriteString("meter (no active scan performed, so your link is never forced off-channel).\n")
	return b.String()
}

// ---- impure layer (ONE reader; no standalone second-exec) ------------------

// readRadio does the SINGLE `system_profiler SPAirPortDataType` exec and feeds the
// ONE output string to BOTH parsers (the joined-link parse AND the neighbor parse),
// so there is ZERO extra process and both reads share one capture instant. It
// REPLACES readLinkSnap at its one serve call site (the dedicated link goroutine)
// and also backs the --airspace one-shot. interfaceActive (ifconfig) supplies the
// association bit, as in the old readLinkSnap. PASSIVE ONLY — see the file header:
// no active scan is ever issued.
func readRadio(iface string) (LinkSnap, []Neighbor) {
	out, _ := exec.Command("system_profiler", "SPAirPortDataType").Output()
	sp := string(out)
	ls := linkSnapFromAirport(parseAirportSample(sp), parseSystemProfilerSSID(sp), interfaceActive(iface))
	return ls, parseNeighborNetworks(sp)
}
