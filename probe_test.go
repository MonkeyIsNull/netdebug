package main

import (
	"strings"
	"testing"
)

func TestParseDefaultRoute(t *testing.T) {
	out := `   route to: default
destination: default
       mask: default
    gateway: 10.0.0.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC>
`
	gw, iface := parseDefaultRoute(out)
	if gw != "10.0.0.1" {
		t.Errorf("gateway = %q, want 10.0.0.1", gw)
	}
	if iface != "en0" {
		t.Errorf("interface = %q, want en0", iface)
	}
}

func TestParseDefaultRouteNone(t *testing.T) {
	gw, iface := parseDefaultRoute("route: writing to routing socket: not in table\n")
	if gw != "" || iface != "" {
		t.Errorf("expected empty gw/iface, got %q/%q", gw, iface)
	}
}

func TestParseDHCPDNS(t *testing.T) {
	out := `op = BOOTREPLY
domain_name_server (ip_mult): {10.0.0.1, 8.8.8.8}
router (ip_mult): {10.0.0.1}
`
	dns := parseDHCPDNS(out)
	if len(dns) != 2 || dns[0] != "10.0.0.1" || dns[1] != "8.8.8.8" {
		t.Errorf("parseDHCPDNS = %v, want [10.0.0.1 8.8.8.8]", dns)
	}
	if got := parseDHCPDNS("no dns here"); got != nil {
		t.Errorf("parseDHCPDNS(none) = %v, want nil", got)
	}
}

func TestParsePingSuccess(t *testing.T) {
	out := `PING 1.1.1.1 (1.1.1.1): 56 data bytes
64 bytes from 1.1.1.1: icmp_seq=0 ttl=57 time=12.3 ms
64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=11.9 ms

--- 1.1.1.1 ping statistics ---
3 packets transmitted, 3 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 11.1/12.3/13.5/0.9 ms
`
	loss, avg, ok := parsePing(out)
	if !ok {
		t.Error("expected reachable")
	}
	if loss != 0.0 {
		t.Errorf("loss = %v, want 0", loss)
	}
	if avg != 12.3 {
		t.Errorf("avg = %v, want 12.3", avg)
	}
}

func TestParsePingTotalLoss(t *testing.T) {
	out := `PING 10.9.9.9 (10.9.9.9): 56 data bytes

--- 10.9.9.9 ping statistics ---
3 packets transmitted, 0 packets received, 100.0% packet loss
`
	loss, _, ok := parsePing(out)
	if ok {
		t.Error("expected unreachable")
	}
	if loss != 100.0 {
		t.Errorf("loss = %v, want 100", loss)
	}
}

func TestFixHint(t *testing.T) {
	// eero advertised-but-dead gateway (the eero case) -> bridge-mode hint.
	h := fixHint("192.168.68.102", "192.168.68.1", true, false, false, false)
	if !strings.Contains(h, "eero") || !strings.Contains(h, "Bridge Mode") {
		t.Errorf("expected eero/bridge-mode hint, got %q", h)
	}
	// Advertised-but-dead on a non-eero subnet -> generic router hint, no eero text.
	h = fixHint("10.0.0.50", "10.0.0.1", true, false, false, false)
	if strings.Contains(h, "eero") {
		t.Errorf("did not expect eero text for non-eero subnet, got %q", h)
	}
	if !strings.Contains(h, "10.0.0.1") {
		t.Errorf("expected the advertised gateway in the hint, got %q", h)
	}
	// No router option at all.
	h = fixHint("192.168.68.102", "", true, false, false, false)
	if !strings.Contains(h, "no gateway") {
		t.Errorf("expected no-gateway hint, got %q", h)
	}
	// Gateway works but no upstream.
	h = fixHint("192.168.1.5", "192.168.1.1", true, true, true, false)
	if !strings.Contains(h, "upstream") {
		t.Errorf("expected upstream hint, got %q", h)
	}
	// Healthy -> no hint.
	if h := fixHint("192.168.1.5", "192.168.1.1", true, true, true, true); h != "" {
		t.Errorf("expected empty hint for healthy, got %q", h)
	}
}

func TestDiagnose(t *testing.T) {
	// each row: hasIP, hasRoute, gwOK, inetOK, dnsAnyOK, dhcpResolverOK, httpOK, captive
	cases := []struct {
		name string
		args [8]bool
		want string // expected leading label
	}{
		{"dhcp fail", [8]bool{false, false, false, false, false, false, false, false}, "DHCP FAILURE"},
		{"no route", [8]bool{true, false, false, false, false, false, false, false}, "NO DEFAULT ROUTE"},
		{"gw unreachable", [8]bool{true, true, false, false, false, false, false, false}, "GATEWAY UNREACHABLE"},
		{"no upstream", [8]bool{true, true, true, false, false, false, false, false}, "NO UPSTREAM"},
		{"broken dhcp dns", [8]bool{true, true, true, true, true, false, false, false}, "BROKEN DNS SERVER"},
		{"dns fail", [8]bool{true, true, true, true, false, false, false, false}, "DNS FAILURE"},
		{"captive", [8]bool{true, true, true, true, true, true, false, true}, "CAPTIVE PORTAL"},
		{"http blocked", [8]bool{true, true, true, true, true, true, false, false}, "HTTP BLOCKED"},
		{"healthy", [8]bool{true, true, true, true, true, true, true, false}, "HEALTHY"},
	}
	for _, c := range cases {
		a := c.args
		got := diagnose(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7])
		if headline(got) != c.want {
			t.Errorf("%s: diagnose headline = %q, want %q", c.name, headline(got), c.want)
		}
	}
}
