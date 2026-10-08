package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Check is one pass/fail probe result. Kind distinguishes a core pass/fail
// (empty) from an "advisory" (soft/weak-link, renders WARN never FAIL) or an
// "info" row. Advisory/info checks NEVER feed diagnose().
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
	Kind   string `json:"kind,omitempty"` // "" = core pass/fail; "advisory"; "info"
}

// NetResult is the full outcome of testing one network.
type NetResult struct {
	Network    string  `json:"network"`
	SSID       string  `json:"ssid"`
	Joined     bool    `json:"joined"`
	Band       string  `json:"band"`
	Channel    string  `json:"channel"`
	RSSI       int     `json:"rssi"`
	IP         string  `json:"ip"`
	Gateway    string  `json:"gateway"`
	DHCPRouter string  `json:"dhcp_router"` // router option DHCP advertised (may differ from installed route)
	RouteTable string  `json:"route_table"` // default-route lines, for offline analysis
	Checks     []Check `json:"checks"`
	Diagnosis  string  `json:"diagnosis"`
	FixHint    string  `json:"fix_hint,omitempty"` // suggested next action, when we can infer one

	// Opt-in sections; nil unless the matching flag was set, so a default run
	// serializes byte-identical to before.
	Signal *SignalStats  `json:"signal,omitempty"`
	IPv6   *IPv6Result   `json:"ipv6,omitempty"`
	Proxy  *ProxyResult  `json:"proxy,omitempty"`
	Extras *ExtrasResult `json:"extras,omitempty"`
}

func (r *NetResult) add(name string, pass bool, format string, a ...any) bool {
	return r.addKind("", name, pass, format, a...)
}

func (r *NetResult) addKind(kind, name string, pass bool, format string, a ...any) bool {
	r.Checks = append(r.Checks, Check{Name: name, Pass: pass, Kind: kind,
		Detail: fmt.Sprintf(format, a...)})
	return pass
}

// ---- individual macOS probes ----------------------------------------------

// ipv4 returns the interface's IPv4 address, or "" if none.
func ipv4(iface string) string {
	out, _ := exec.Command("ipconfig", "getifaddr", iface).Output()
	return strings.TrimSpace(string(out))
}

var (
	reGw   = regexp.MustCompile(`gateway:\s*([0-9.]+)`)
	reGwV6 = regexp.MustCompile(`gateway:\s*([0-9a-fA-F:.]+(?:%\w+)?)`) // hex + colons, optional %scope
	reIfc  = regexp.MustCompile(`interface:\s*(\w+)`)
	reDNS  = regexp.MustCompile(`domain_name_server[^:]*:\s*\{([^}]*)\}`)

	rePingLoss = regexp.MustCompile(`([\d.]+)% packet loss`)
	rePingRtt  = regexp.MustCompile(`= [\d.]+/([\d.]+)/`)
)

// parseDefaultRoute pulls the gateway and interface from `route -n get default`.
func parseDefaultRoute(out string) (gw, iface string) {
	if m := reGw.FindStringSubmatch(out); m != nil {
		gw = m[1]
	}
	if m := reIfc.FindStringSubmatch(out); m != nil {
		iface = m[1]
	}
	return
}

// parseDefaultRouteV6 pulls the gateway and interface from
// `route -n get -inet6 default`. The v6 gateway is normally a link-local
// address with a %scope suffix (e.g. fe80::1%en0), which the v4-only reGw
// (digits and dots) would drop, so this uses a hex/colon/%-aware pattern and
// retains the %scope. The v4 parser is left untouched.
func parseDefaultRouteV6(out string) (gw, iface string) {
	if m := reGwV6.FindStringSubmatch(out); m != nil {
		gw = m[1]
	}
	if m := reIfc.FindStringSubmatch(out); m != nil { // interface line carries no scope
		iface = m[1]
	}
	return
}

// defaultRoute returns the default gateway and the interface it routes through.
func defaultRoute() (gw, iface string) {
	out, _ := exec.Command("route", "-n", "get", "default").CombinedOutput()
	return parseDefaultRoute(string(out))
}

// parseDHCPDNS pulls the DHCP-offered DNS servers from `ipconfig getpacket`.
func parseDHCPDNS(out string) []string {
	m := reDNS.FindStringSubmatch(out)
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

// dhcpDNS returns the DNS servers DHCP handed out on this interface.
func dhcpDNS(iface string) []string {
	out, _ := exec.Command("ipconfig", "getpacket", iface).CombinedOutput()
	return parseDHCPDNS(string(out))
}

// dhcpRouter returns the router (gateway) option DHCP advertised, or "" if none.
// Distinguishes "AP offered no gateway" from "gateway offered but dead/unreachable".
func dhcpRouter(iface string) string {
	out, _ := exec.Command("ipconfig", "getoption", iface, "router").Output()
	return strings.TrimSpace(string(out))
}

// routeTableDefaults returns the default-route lines of the inet routing table,
// captured for offline analysis after switching away from a broken network.
func routeTableDefaults() string {
	out, _ := exec.Command("netstat", "-rn", "-f", "inet").Output()
	var b strings.Builder
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(ln, "default") || strings.Contains(ln, "Destination") {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

// parsePing extracts loss%, avg ms, and reachability from `ping` output.
func parsePing(out string) (loss, avgMs float64, ok bool) {
	loss = 100.0
	if m := rePingLoss.FindStringSubmatch(out); m != nil {
		loss, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := rePingRtt.FindStringSubmatch(out); m != nil {
		avgMs, _ = strconv.ParseFloat(m[1], 64)
	}
	return loss, avgMs, loss < 100.0
}

// pingHost pings host and returns loss%, avg ms, and reachable.
func pingHost(host string, count, timeoutSec int) (loss, avgMs float64, ok bool) {
	out, _ := exec.Command("ping", "-c", strconv.Itoa(count), "-t", strconv.Itoa(timeoutSec), host).CombinedOutput()
	return parsePing(string(out))
}

// resolveNet resolves name using a specific DNS server over the given IP
// network ("ip" for any, "ip6" for AAAA-only), isolating resolver health.
func resolveNet(server, name, network string, timeout time.Duration) (bool, string) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "udp", net.JoinHostPort(server, "53"))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ips, err := r.LookupIP(ctx, network, name)
	if err != nil {
		return false, err.Error()
	}
	strs := make([]string, len(ips))
	for i, ip := range ips {
		strs[i] = ip.String()
	}
	return len(ips) > 0, strings.Join(strs, ",")
}

// resolveVia resolves name using a specific DNS server, isolating resolver
// health. Thin wrapper over resolveNet for the core battery (A + AAAA).
func resolveVia(server, name string, timeout time.Duration) (bool, string) {
	return resolveNet(server, name, "ip", timeout)
}

// httpCheck GETs url without following redirects, so a captive-portal
// interception is visible. captive.apple.com must return 200 with "Success".
func httpCheck(url string, timeout time.Duration) (ok bool, captive bool, detail string) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return false, false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if strings.Contains(url, "captive.apple.com") {
		if resp.StatusCode == 200 && strings.Contains(string(body), "Success") {
			return true, false, "got expected Success page"
		}
		loc := resp.Header.Get("Location")
		return false, true, fmt.Sprintf("status %d, redirect=%q (captive portal)", resp.StatusCode, loc)
	}
	good := resp.StatusCode >= 200 && resp.StatusCode < 400
	return good, false, fmt.Sprintf("status %d", resp.StatusCode)
}

// ---- the battery -----------------------------------------------------------

// Options selects which opt-in diagnostic sections runBattery runs. All zero
// values mean "core battery only" — a default run is unchanged.
type Options struct {
	Signal     bool
	IPv6       bool
	Proxy      bool
	DHCPExtras bool // feature 4, the --extras flag
	Samples    int
	SignalCSV  string
}

// runBattery runs the full layered test against the currently-joined network
// and fills in a diagnosis. It performs NO network switching.
func runBattery(cfg Config, iface, name, ssid string, joined bool, opt Options) *NetResult {
	r := &NetResult{Network: name, SSID: ssid, Joined: joined}
	if !joined {
		r.Diagnosis = "COULD NOT JOIN: association/auth failed or the network was out of range."
		return r
	}

	li := linkInfo()
	r.Band, r.Channel, r.RSSI = li.Band, li.Channel, li.RSSI

	// 1. DHCP / IP
	ip := ipv4(iface)
	r.IP = ip
	hasIP := ip != "" && !strings.HasPrefix(ip, "169.254.")
	r.add("dhcp", hasIP, "IPv4 = %q", ip)

	// 2. Default route (and what DHCP claimed the router should be)
	gw, gwIf := defaultRoute()
	r.Gateway = gw
	r.DHCPRouter = dhcpRouter(iface)
	r.RouteTable = routeTableDefaults()
	hasRoute := gw != "" && (gwIf == iface || gwIf == "")
	r.add("route", hasRoute, "installed gateway=%q via %q; DHCP-advertised router=%q",
		gw, gwIf, r.DHCPRouter)

	// 3. Gateway reachability
	gwOK := false
	if gw != "" {
		loss, avg, ok := pingHost(gw, cfg.PingCount, 3)
		gwOK = ok
		r.add("gateway_ping", ok, "%.0f%% loss, avg %.1f ms", loss, avg)
	} else {
		r.add("gateway_ping", false, "no gateway to ping")
	}

	// 4. Internet reachability by IP (no DNS)
	inetOK := false
	for _, pip := range cfg.PublicIPs {
		loss, avg, ok := pingHost(pip, cfg.PingCount, 3)
		if ok {
			inetOK = true
		}
		r.add("internet_ping:"+pip, ok, "%.0f%% loss, avg %.1f ms", loss, avg)
	}

	// 5. DNS — DHCP resolver vs public, resolved separately
	dnsAnyOK := false
	dhcpResolverOK := true
	for _, srv := range cfg.DNSServers {
		label := srv
		servers := []string{srv}
		if srv == "dhcp" {
			servers = dhcpDNS(iface)
			if len(servers) == 0 {
				r.add("dns:dhcp", false, "no DHCP-provided resolver")
				dhcpResolverOK = false
				continue
			}
		}
		srvOK := false
		for _, nm := range cfg.DNSNames {
			ok, detail := resolveVia(servers[0], nm, 4*time.Second)
			r.add("dns:"+label+":"+nm, ok, "%s -> %s", nm, detail)
			if ok {
				srvOK = true
				break
			}
		}
		if srvOK {
			dnsAnyOK = true
		} else if srv == "dhcp" {
			dhcpResolverOK = false
		}
	}

	// 6. HTTP / captive portal
	httpOK := false
	captive := false
	for _, u := range cfg.HTTPChecks {
		ok, isCaptive, detail := httpCheck(u, 8*time.Second)
		if ok {
			httpOK = true
		}
		if isCaptive {
			captive = true
		}
		r.add("http:"+u, ok, "%s", detail)
	}

	r.Diagnosis = diagnose(hasIP, hasRoute, gwOK, inetOK, dnsAnyOK, dhcpResolverOK, httpOK, captive)

	// Sharpen the no-route case with the DHCP router option — it tells us
	// whether the AP offered a gateway at all.
	if !hasRoute {
		if r.DHCPRouter != "" {
			r.Diagnosis += fmt.Sprintf(" DHCP advertised a gateway (%s) but no default route is usable — the gateway is dead or unreachable, e.g. a mesh node that lost backhaul to the main router.", r.DHCPRouter)
		} else {
			r.Diagnosis += " DHCP advertised NO router option — the AP handed out an address but never told the client where the gateway is (misconfigured DHCP scope or a broken mesh/bridge)."
		}
	}

	r.FixHint = fixHint(r.IP, r.DHCPRouter, hasIP, hasRoute, gwOK, inetOK)

	// ---- opt-in sections -----------------------------------------------
	// Each runs AFTER the core verdict is set, appends only advisory/info
	// checks, and fills a pointer field. None feed diagnose().
	if opt.Signal {
		runSignalSection(r, iface, opt)
	}
	if opt.IPv6 {
		runIPv6Section(r, iface, cfg)
	}
	if opt.Proxy {
		runProxySection(r, iface)
	}
	if opt.DHCPExtras {
		runExtrasSection(r, iface, gw, cfg)
	}

	return r
}
