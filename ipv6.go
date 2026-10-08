package main

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Inet6Addr is one non-link-local IPv6 address with its configuration flags.
type Inet6Addr struct {
	Addr      string   `json:"addr"`
	PrefixLen int      `json:"prefix_len"`
	Flags     []string `json:"flags"`
}

// IPv6Result is the outcome of the --ipv6 health section.
type IPv6Result struct {
	GlobalAddr string `json:"global_addr"`
	AddrFlags  string `json:"addr_flags"`
	Gateway    string `json:"gateway"`
	PingOK     bool   `json:"ping_ok"`
	PingDetail string `json:"ping_detail"`
	AAAAOK     bool   `json:"aaaa_ok"`
	AAAADetail string `json:"aaaa_detail"`
	Note       string `json:"note,omitempty"`
}

var reInet6 = regexp.MustCompile(`inet6\s+([0-9a-fA-F:]+)(?:%\w+)?\s+prefixlen\s+(\d+)(.*)`)

// knownV6Flags are the per-address flag words ifconfig prints after prefixlen.
var knownV6Flags = []string{
	"autoconf", "temporary", "secured", "duplicated", "deprecated",
	"tentative", "detached", "dynamic", "optimistic", "prefixroute",
}

// parseInet6 returns one entry per `inet6` line, DROPPING fe80 link-local
// addresses, and captures each address's flag words.
func parseInet6(out string) []Inet6Addr {
	var res []Inet6Addr
	for _, ln := range strings.Split(out, "\n") {
		m := reInet6.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		addr := m[1]
		if strings.HasPrefix(strings.ToLower(addr), "fe80") {
			continue // link-local
		}
		plen, _ := strconv.Atoi(m[2])
		a := Inet6Addr{Addr: addr, PrefixLen: plen}
		tail := strings.ToLower(m[3])
		for _, f := range knownV6Flags {
			if strings.Contains(tail, f) {
				a.Flags = append(a.Flags, f)
			}
		}
		res = append(res, a)
	}
	return res
}

// hasFlag reports whether addr carries flag f.
func (a Inet6Addr) hasFlag(f string) bool {
	for _, x := range a.Flags {
		if x == f {
			return true
		}
	}
	return false
}

// pickGlobalV6 selects a usable global v6 address. healthy is true iff at least
// one global exists whose flags include NONE of duplicated/deprecated/tentative;
// that address is returned (a temporary+secured+autoconf address is fine). If
// the only global(s) are duplicated/etc., that address is returned with
// healthy=false and a note. Link-local only => empty addr, "no global v6".
func pickGlobalV6(addrs []Inet6Addr) (addr, note string, healthy bool) {
	if len(addrs) == 0 {
		return "", "no global v6", false
	}
	bad := func(a Inet6Addr) bool {
		return a.hasFlag("duplicated") || a.hasFlag("deprecated") || a.hasFlag("tentative")
	}
	for _, a := range addrs {
		if !bad(a) {
			return a.Addr, "", true
		}
	}
	// Only unhealthy globals.
	a := addrs[0]
	switch {
	case a.hasFlag("duplicated"):
		note = "only global v6 is DAD-duplicated"
	case a.hasFlag("tentative"):
		note = "only global v6 is still tentative (DAD in progress)"
	case a.hasFlag("deprecated"):
		note = "only global v6 is deprecated"
	default:
		note = "global v6 present but not usable"
	}
	return a.Addr, note, false
}

// parsePing6 extracts loss%, avg ms, and reachability from `ping6` output.
// ping6's statistics wording matches ping's, so it delegates to parsePing but
// keeps its own identity (and tests) in case that diverges.
func parsePing6(out string) (loss, avgMs float64, ok bool) {
	return parsePing(out)
}

// ---- impure command layer --------------------------------------------------

// inet6Addrs returns the interface's non-link-local v6 addresses.
func inet6Addrs(iface string) []Inet6Addr {
	out, _ := exec.Command("ifconfig", iface, "inet6").CombinedOutput()
	return parseInet6(string(out))
}

// defaultRouteV6 returns the v6 default gateway and its interface.
func defaultRouteV6() (gw, iface string) {
	out, _ := exec.Command("route", "-n", "get", "-inet6", "default").CombinedOutput()
	return parseDefaultRouteV6(string(out))
}

// ping6Host pings a v6 address with a HARDCODED count of 2. ping6 is a separate
// binary from ping (`ping -6` is rejected on macOS); its -t flag is a boolean
// hoplimit cluster, not "-t seconds", so we bound wall-clock with a context
// deadline instead of passing -t.
func ping6Host(addr string) (loss, avgMs float64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ping6", "-c", "2", addr).CombinedOutput() // -c literal
	return parsePing6(string(out))
}

// runIPv6Section performs the --ipv6 checks and fills r.IPv6. All checks are
// advisory/info; none feed diagnose().
func runIPv6Section(r *NetResult, iface string, cfg Config) {
	var res IPv6Result

	addrs := inet6Addrs(iface)
	addr, note, healthy := pickGlobalV6(addrs)
	res.GlobalAddr = addr
	res.Note = note
	for _, a := range addrs {
		if a.Addr == addr {
			res.AddrFlags = strings.Join(a.Flags, ",")
			break
		}
	}
	r.addKind("advisory", "ipv6_addr", healthy && addr != "",
		"global v6 %s [%s]", emptyDash(addr), res.AddrFlags)

	gw, _ := defaultRouteV6()
	res.Gateway = gw
	r.addKind("advisory", "ipv6_route", gw != "", "v6 default gateway %s", emptyDash(gw))

	loss, avg, pingOK := ping6Host("2606:4700:4700::1111")
	res.PingOK = pingOK
	res.PingDetail = detailLossAvg(loss, avg)
	r.addKind("advisory", "ipv6_ping", pingOK, "ping6 2606:4700:4700::1111: %s", res.PingDetail)

	// AAAA resolution via the first configured resolver, or a public default.
	server := "1.1.1.1"
	for _, s := range cfg.DNSServers {
		if s != "dhcp" {
			server = s
			break
		}
	}
	name := "cloudflare.com"
	if len(cfg.DNSNames) > 0 {
		name = cfg.DNSNames[0]
	}
	aaaaOK, aaaaDetail := resolveNet(server, name, "ip6", 4*time.Second)
	res.AAAAOK = aaaaOK
	res.AAAADetail = aaaaDetail
	r.addKind("advisory", "ipv6_aaaa", aaaaOK, "AAAA %s -> %s", name, aaaaDetail)

	// The ping6 result is the arbiter of "working" (not address flags alone).
	// A configured+preferred but non-pinging v6 stack is the hidden fault.
	if addr != "" && healthy && !pingOK {
		res.Note = "IPv6 is configured and preferred but ping6 fails — apps that prefer v6 stall before falling back to v4"
	}

	r.IPv6 = &res
}

// detailLossAvg formats a ping loss/avg pair.
func detailLossAvg(loss, avg float64) string {
	return strconv.FormatFloat(loss, 'f', 0, 64) + "% loss, avg " +
		strconv.FormatFloat(avg, 'f', 1, 64) + " ms"
}
