package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// DHCPLease is the parsed contents of `ipconfig getpacket`.
type DHCPLease struct {
	ServerID   string   `json:"server_id,omitempty"`
	SubnetMask string   `json:"subnet_mask,omitempty"`
	DomainName string   `json:"domain_name,omitempty"`
	LeaseSecs  int      `json:"lease_secs"`
	Routers    []string `json:"routers,omitempty"`
	DNS        []string `json:"dns,omitempty"`
	Search     []string `json:"search,omitempty"`
	NTP        []string `json:"ntp,omitempty"`
}

// MTUResult is one path-MTU black-hole probe outcome.
type MTUResult struct {
	Size       int  `json:"size"`
	OK         bool `json:"ok"`
	Fragmented bool `json:"fragmented"`
}

// ExtrasResult is the outcome of the --extras section.
type ExtrasResult struct {
	GatewayMAC  string      `json:"gateway_mac,omitempty"`
	ARPComplete bool        `json:"arp_complete"`
	MTU         []MTUResult `json:"mtu,omitempty"`
	Lease       DHCPLease   `json:"dhcp_lease"`
	Warnings    []string    `json:"warnings,omitempty"`
}

// reMAC matches a MAC address, accepting BSD-abbreviated octets (0:55:7b:...).
var reMAC = regexp.MustCompile(`([0-9a-fA-F]{1,2}:){5}[0-9a-fA-F]{1,2}`)

// parseARP extracts the gateway MAC from `arp -n <gw>`. Abbreviated octets are
// accepted. "(incomplete)" or empty input yields complete=false without panic.
func parseARP(out string) (mac string, complete bool) {
	if strings.Contains(strings.ToLower(out), "(incomplete)") {
		return "", false
	}
	if m := reMAC.FindString(out); m != "" {
		return m, true
	}
	return "", false
}

// parsePingMTU turns a `ping -D -s <size>` result into an MTUResult. OK means
// the target was reachable at that size; Fragmented is set when the local DF
// error ("Message too long") or a router "frag needed and DF set" appears
// (matched case-insensitively).
func parsePingMTU(out string, size int) MTUResult {
	_, _, ok := parsePing(out)
	low := strings.ToLower(out)
	frag := strings.Contains(low, "message too long") || strings.Contains(low, "frag needed")
	return MTUResult{Size: size, OK: ok, Fragmented: frag}
}

// decideMTU reports a path-MTU black hole ONLY when the large size fails and
// the small size succeeds. Both failing is plain ICMP filtering (NOT a black
// hole). Both succeeding is fine. It expects the largest size first.
func decideMTU(results []MTUResult) (blackhole bool, detail string) {
	if len(results) < 2 {
		return false, ""
	}
	// Largest failing while a smaller one succeeds => black hole.
	var large, small *MTUResult
	for i := range results {
		if large == nil || results[i].Size > large.Size {
			large = &results[i]
		}
		if small == nil || results[i].Size < small.Size {
			small = &results[i]
		}
	}
	if !large.OK && small.OK {
		return true, fmt.Sprintf("Path-MTU black hole: %d-byte DF pings fail but %d succeed — "+
			"something on the path is dropping full-size packets; TLS handshakes hang", large.Size, small.Size)
	}
	if !large.OK && !small.OK {
		return false, "large and small DF pings both fail — likely ICMP filtering, not a path-MTU black hole"
	}
	return false, "path MTU OK at tested sizes"
}

var (
	reLeaseTime = regexp.MustCompile(`(?m)lease_time\s*\([^)]*\)\s*:\s*(\S+)`)
	reServerID  = regexp.MustCompile(`(?m)server_identifier\s*\([^)]*\)\s*:\s*(\S+)`)
	reSubnet    = regexp.MustCompile(`(?m)subnet_mask\s*\([^)]*\)\s*:\s*(\S+)`)
	reDomain    = regexp.MustCompile(`(?m)domain_name\s*\([^)]*\)\s*:\s*(.+?)\s*$`)
	reRouter    = regexp.MustCompile(`(?m)router\s*\([^)]*\)\s*:\s*\{([^}]*)\}`)
	reSearch    = regexp.MustCompile(`(?m)domain_search\s*\([^)]*\)\s*:\s*\{([^}]*)\}`)
	reNTP       = regexp.MustCompile(`(?m)ntp_servers\s*\([^)]*\)\s*:\s*\{([^}]*)\}`)
)

// parseDHCPLease is a keyed extractor over `ipconfig getpacket`. lease_time is
// parsed base-0 (strconv.ParseInt(s, 0, 64)) so it reads both 0x15180 (hex) and
// a bare 86400. Absent options yield zero-value slices. It reuses parseDHCPDNS
// for the resolver list so the two parsers of the same output cannot drift.
func parseDHCPLease(out string) DHCPLease {
	var l DHCPLease
	if m := reServerID.FindStringSubmatch(out); m != nil {
		l.ServerID = strings.TrimSpace(m[1])
	}
	if m := reSubnet.FindStringSubmatch(out); m != nil {
		l.SubnetMask = strings.TrimSpace(m[1])
	}
	if m := reDomain.FindStringSubmatch(out); m != nil {
		l.DomainName = strings.TrimSpace(m[1])
	}
	if m := reLeaseTime.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(m[1]), 0, 64); err == nil {
			l.LeaseSecs = int(v)
		}
	}
	l.Routers = braceList(reRouter, out)
	l.Search = braceList(reSearch, out)
	l.NTP = braceList(reNTP, out)
	l.DNS = parseDHCPDNS(out)
	return l
}

// braceList extracts a comma-separated {a, b, c} list captured by re, or nil.
func braceList(re *regexp.Regexp, out string) []string {
	m := re.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	var res []string
	for _, p := range strings.Split(m[1], ",") {
		if s := strings.TrimSpace(p); s != "" {
			res = append(res, s)
		}
	}
	return res
}

// ---- impure command layer --------------------------------------------------

func arpGateway(gw string) (mac string, complete bool) {
	out, _ := exec.Command("arp", "-n", gw).CombinedOutput()
	return parseARP(string(out))
}

// pingMTU sends a DF (don't-fragment) ping of the given size with a HARDCODED
// count of 2. Size is a literal constant chosen by the caller, never a flag.
func pingMTU(target string, size int) MTUResult {
	out, _ := exec.Command("ping", "-D", "-s", strconv.Itoa(size), "-c", "2", target).CombinedOutput()
	return parsePingMTU(string(out), size)
}

func dhcpPacket(iface string) string {
	out, _ := exec.Command("ipconfig", "getpacket", iface).CombinedOutput()
	return string(out)
}

// mtuSizes are the literal DF probe sizes (payload bytes). 1472 = 1500 MTU less
// 28 bytes IP+ICMP header; 1372 = a 1400-MTU tunnel. Never wired to a flag.
var mtuSizes = []int{1472, 1372}

// runExtrasSection performs the --extras checks and fills r.Extras. Checks are
// advisory/info; none feed diagnose(). It reuses one ipconfig getpacket call.
func runExtrasSection(r *NetResult, iface, gw string, cfg Config) {
	var res ExtrasResult

	if gw == "" {
		res.Warnings = append(res.Warnings, "no gateway to probe (skipped ARP + MTU)")
		r.addKind("advisory", "gateway_mac", false, "no gateway to probe")
	} else {
		mac, complete := arpGateway(gw)
		res.GatewayMAC = mac
		res.ARPComplete = complete
		r.addKind("advisory", "gateway_mac", complete, "gateway %s MAC %s", gw, emptyDash(mac))

		// Path-MTU black-hole probe against the first configured public IP
		// (upstream black holes are on the path out, not the LAN).
		target := ""
		if len(cfg.PublicIPs) > 0 {
			target = cfg.PublicIPs[0]
		}
		if target != "" {
			for _, sz := range mtuSizes {
				m := pingMTU(target, sz)
				res.MTU = append(res.MTU, m)
				r.addKind("advisory", fmt.Sprintf("mtu:%d", sz), m.OK,
					"DF %d bytes to %s: ok=%v fragmented=%v", sz, target, m.OK, m.Fragmented)
			}
			if bh, detail := decideMTU(res.MTU); bh {
				res.Warnings = append(res.Warnings, detail)
			}
		}
	}

	// One getpacket, fed to both the lease parser and the (core) DNS parser.
	pkt := dhcpPacket(iface)
	res.Lease = parseDHCPLease(pkt)
	leaseDetail := fmt.Sprintf("lease %ds", res.Lease.LeaseSecs)
	if res.Lease.ServerID != "" {
		leaseDetail += " from " + res.Lease.ServerID
	}
	if res.Lease.DomainName != "" {
		leaseDetail += fmt.Sprintf(", domain %q", res.Lease.DomainName)
	}
	r.addKind("info", "dhcp_lease", true, "%s", leaseDetail)

	r.Extras = &res
}
