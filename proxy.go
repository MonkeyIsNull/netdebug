package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ProxySettings is the internal parse result of `scutil --proxy`.
type ProxySettings struct {
	HTTPEnable, HTTPSEnable, SOCKSEnable, PACEnable, AutoDiscover bool
	HTTPProxy, HTTPSProxy, SOCKSProxy, PACURL                     string
	HTTPPort, HTTPSPort, SOCKSPort                                int
}

// DefaultRoute is one default route from `netstat -rn`.
type DefaultRoute struct {
	Family  string `json:"family"`
	Gateway string `json:"gateway"`
	Netif   string `json:"netif"`
}

// ProxyResult is the outcome of the --proxy section.
type ProxyResult struct {
	HTTPProxy    string   `json:"http_proxy,omitempty"`
	HTTPSProxy   string   `json:"https_proxy,omitempty"`
	SOCKSProxy   string   `json:"socks_proxy,omitempty"`
	PACURL       string   `json:"pac_url,omitempty"`
	AutoDiscover bool     `json:"auto_discover"`
	Tunnels      []string `json:"tunnels,omitempty"`
	VPNActive    bool     `json:"vpn_active"`
	VPNDetail    string   `json:"vpn_detail,omitempty"`
}

var reScutilKV = regexp.MustCompile(`^\s*([A-Za-z0-9]+)\s*:\s*(.*)$`)

// parseScutilProxy walks the flat `scutil --proxy` dictionary. Absent *Enable
// keys mean disabled. The nested "ExceptionsList : <array> { 0 : *.local ... }"
// body is skipped so its "0 :"/"1 :" lines are not misread as proxy keys.
func parseScutilProxy(out string) ProxySettings {
	var s ProxySettings
	// The output is one outer "<dictionary> { ... }". Top-level keys live at
	// brace depth 1; a value like "ExceptionsList : <array> { ... }" opens a
	// deeper block whose body (its "0 :"/"1 :" lines) must NOT be read as keys.
	depth := 0
	for _, ln := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(ln)
		opens := strings.Count(ln, "{")
		closes := strings.Count(ln, "}")

		// Only top-level key/value lines that don't themselves open a nested
		// container are proxy settings.
		readable := depth == 1 && opens == 0
		depth += opens - closes
		if !readable {
			continue
		}
		m := reScutilKV.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		key, val := m[1], strings.TrimSpace(m[2])
		switch key {
		case "HTTPEnable":
			s.HTTPEnable = val == "1"
		case "HTTPProxy":
			s.HTTPProxy = val
		case "HTTPPort":
			s.HTTPPort, _ = strconv.Atoi(val)
		case "HTTPSEnable":
			s.HTTPSEnable = val == "1"
		case "HTTPSProxy":
			s.HTTPSProxy = val
		case "HTTPSPort":
			s.HTTPSPort, _ = strconv.Atoi(val)
		case "SOCKSEnable":
			s.SOCKSEnable = val == "1"
		case "SOCKSProxy":
			s.SOCKSProxy = val
		case "SOCKSPort":
			s.SOCKSPort, _ = strconv.Atoi(val)
		case "ProxyAutoConfigEnable":
			s.PACEnable = val == "1"
		case "ProxyAutoConfigURLString":
			s.PACURL = val
		case "ProxyAutoDiscoveryEnable":
			s.AutoDiscover = val == "1"
		}
	}
	return s
}

// proxyResultFrom surfaces a proxy string ONLY when its *Enable is set (macOS
// retains a stale HTTPProxy value with HTTPEnable : 0), formatting host:port.
func proxyResultFrom(s ProxySettings) ProxyResult {
	var r ProxyResult
	if s.HTTPEnable && s.HTTPProxy != "" {
		r.HTTPProxy = hostPort(s.HTTPProxy, s.HTTPPort)
	}
	if s.HTTPSEnable && s.HTTPSProxy != "" {
		r.HTTPSProxy = hostPort(s.HTTPSProxy, s.HTTPSPort)
	}
	if s.SOCKSEnable && s.SOCKSProxy != "" {
		r.SOCKSProxy = hostPort(s.SOCKSProxy, s.SOCKSPort)
	}
	if s.PACEnable && s.PACURL != "" {
		r.PACURL = s.PACURL
	}
	r.AutoDiscover = s.AutoDiscover
	return r
}

func hostPort(host string, port int) string {
	if port > 0 {
		return fmt.Sprintf("%s:%d", host, port)
	}
	return host
}

var reTunnelIface = regexp.MustCompile(`^(utun|ppp|ipsec)\d`)

// parseTunnelIfaces returns tunnel interfaces (utun*/ppp*/ipsec*) from the
// space-separated `ifconfig -l` output.
func parseTunnelIfaces(out string) []string {
	var res []string
	for _, f := range strings.Fields(out) {
		if reTunnelIface.MatchString(f) {
			res = append(res, f)
		}
	}
	return res
}

// parseExtraDefaultRoutes returns the default routes from `netstat -rn -f <family>`.
// Sequoia columns are: Destination Gateway Flags Netif Expire (no Refs/Use), so
// Netif is the field after Flags; a trailing empty Expire is tolerated. Only
// rows whose Destination is "default" are returned.
func parseExtraDefaultRoutes(out, family string) []DefaultRoute {
	var res []DefaultRoute
	for _, ln := range strings.Split(out, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 4 || fields[0] != "default" {
			continue
		}
		// Destination Gateway Flags Netif [Expire]
		res = append(res, DefaultRoute{
			Family:  family,
			Gateway: fields[1],
			Netif:   fields[3],
		})
	}
	return res
}

// detectVPN flags a VPN ONLY when a tunnel interface owns the inet (v4) default
// route. It NEVER fires on tunnel existence alone, on utun v6 default routes
// alone (a stock Mac carries utun0-7 plus fe80::%utunN v6 defaults with no VPN),
// nor on a plain non-Wi-Fi default such as Ethernet on a docked Mac (that is
// reported separately as an informational note, not a VPN).
// tuns/v6def are accepted for signature stability but intentionally unexamined —
// only a tunnel owning the v4 default is a VPN.
func detectVPN(tuns []string, v4def, v6def []DefaultRoute, wifiIface string) (vpn bool, detail string) {
	for _, r := range v4def {
		if reTunnelIface.MatchString(r.Netif) {
			return true, fmt.Sprintf("%s owns the IPv4 default route", r.Netif)
		}
	}
	return false, ""
}

// ---- impure command layer --------------------------------------------------

func scutilProxy() ProxySettings {
	out, _ := exec.Command("scutil", "--proxy").Output()
	return parseScutilProxy(string(out))
}

func tunnelIfaces() []string {
	out, _ := exec.Command("ifconfig", "-l").Output()
	return parseTunnelIfaces(string(out))
}

func extraDefaultRoutes(family string) []DefaultRoute {
	out, _ := exec.Command("netstat", "-rn", "-f", family).Output()
	fam := "inet"
	if family == "inet6" {
		fam = "inet6"
	}
	return parseExtraDefaultRoutes(string(out), fam)
}

// runProxySection performs the --proxy checks and fills r.Proxy. Checks are
// advisory/info; none feed diagnose().
func runProxySection(r *NetResult, iface string) {
	settings := scutilProxy()
	res := proxyResultFrom(settings)

	tuns := tunnelIfaces()
	res.Tunnels = tuns
	v4def := extraDefaultRoutes("inet")
	v6def := extraDefaultRoutes("inet6")
	vpn, vdetail := detectVPN(tuns, v4def, v6def, iface)
	res.VPNActive = vpn
	res.VPNDetail = vdetail

	proxied := res.HTTPProxy != "" || res.HTTPSProxy != "" || res.SOCKSProxy != "" || res.PACURL != ""
	detail := "no system proxy or PAC configured"
	if proxied {
		var parts []string
		if res.HTTPProxy != "" {
			parts = append(parts, "http="+res.HTTPProxy)
		}
		if res.HTTPSProxy != "" {
			parts = append(parts, "https="+res.HTTPSProxy)
		}
		if res.SOCKSProxy != "" {
			parts = append(parts, "socks="+res.SOCKSProxy)
		}
		if res.PACURL != "" {
			parts = append(parts, "pac="+res.PACURL)
		}
		detail = "A system proxy/PAC routes traffic (" + strings.Join(parts, ", ") +
			"); if it's down, apps fail though the network is healthy"
	}
	// pass = no proxy/PAC set (a set proxy is a WARN advisory, not a FAIL).
	r.addKind("advisory", "proxy", !proxied, "%s", detail)

	vpnDetail := "no VPN owning the route"
	if vpn {
		vpnDetail = vdetail + " — the tunnel, not this Wi-Fi, decides reachability"
	} else if len(tuns) > 0 {
		vpnDetail = fmt.Sprintf("tunnel interfaces present (%s) but none owns the default route", strings.Join(tuns, ","))
	}
	r.addKind("info", "vpn", true, "%s", vpnDetail)

	// A v4 default over a plain non-Wi-Fi, non-tunnel interface (e.g. Ethernet
	// on a docked Mac) is not a VPN — but it does mean the Wi-Fi being tested
	// isn't carrying this machine's traffic. Note it informationally.
	for _, dr := range v4def {
		if dr.Netif != "" && dr.Netif != iface && dr.Netif != "lo0" && !reTunnelIface.MatchString(dr.Netif) {
			r.addKind("info", "default_iface", true,
				"IPv4 default route is on %s, not Wi-Fi %s (wired/docked?) — traffic is going via that interface, not this Wi-Fi", dr.Netif, iface)
			break
		}
	}

	r.Proxy = &res
}
