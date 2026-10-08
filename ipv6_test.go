package main

import (
	"strings"
	"testing"
)

const ifconfigInet6 = `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet6 fe80::14:93ff:fee0:6c19%en0 prefixlen 64 scopeid 0x9
	inet6 2600:1700:abcd:1234:1ced:2f0f:1111:2222 prefixlen 64 autoconf temporary
	inet6 2600:1700:abcd:1234:145:8fbb:cccc:dddd prefixlen 64 duplicated autoconf secured
`

func TestParseInet6(t *testing.T) {
	addrs := parseInet6(ifconfigInet6)
	if len(addrs) != 2 {
		t.Fatalf("got %d addrs, want 2 (fe80 dropped)", len(addrs))
	}
	if addrs[0].Addr != "2600:1700:abcd:1234:1ced:2f0f:1111:2222" {
		t.Errorf("addr[0] = %q", addrs[0].Addr)
	}
	if !addrs[0].hasFlag("autoconf") || !addrs[0].hasFlag("temporary") {
		t.Errorf("addr[0] flags = %v, want autoconf+temporary", addrs[0].Flags)
	}
	if !addrs[1].hasFlag("duplicated") || !addrs[1].hasFlag("secured") {
		t.Errorf("addr[1] flags = %v, want duplicated+secured", addrs[1].Flags)
	}
	for _, a := range addrs {
		if strings.HasPrefix(a.Addr, "fe80") {
			t.Errorf("fe80 link-local leaked: %q", a.Addr)
		}
	}
}

func TestPickGlobalV6Healthy(t *testing.T) {
	addrs := []Inet6Addr{{Addr: "2600::1", Flags: []string{"autoconf", "secured"}}}
	addr, note, healthy := pickGlobalV6(addrs)
	if !healthy || addr != "2600::1" || note != "" {
		t.Errorf("= %q/%q/%v, want 2600::1//true", addr, note, healthy)
	}
}

func TestPickGlobalV6PrefersWorkingOverDuplicated(t *testing.T) {
	addrs := []Inet6Addr{
		{Addr: "2600::dup", Flags: []string{"duplicated", "secured"}},
		{Addr: "2600::temp", Flags: []string{"autoconf", "temporary"}},
	}
	addr, _, healthy := pickGlobalV6(addrs)
	if !healthy {
		t.Error("expected healthy when a working temporary global exists")
	}
	if addr != "2600::temp" {
		t.Errorf("addr = %q, want the working temporary 2600::temp", addr)
	}
}

func TestPickGlobalV6OnlyDuplicated(t *testing.T) {
	addrs := []Inet6Addr{{Addr: "2600::dup", Flags: []string{"duplicated", "secured"}}}
	addr, note, healthy := pickGlobalV6(addrs)
	if healthy {
		t.Error("expected unhealthy when the only global is duplicated")
	}
	if addr != "2600::dup" {
		t.Errorf("addr = %q, want 2600::dup", addr)
	}
	if !strings.Contains(strings.ToLower(note), "duplicat") {
		t.Errorf("note = %q, want it to mention duplicated/DAD", note)
	}
}

func TestPickGlobalV6None(t *testing.T) {
	addr, note, healthy := pickGlobalV6(nil)
	if healthy || addr != "" || note != "no global v6" {
		t.Errorf("= %q/%q/%v, want ''/'no global v6'/false", addr, note, healthy)
	}
}

func TestParseDefaultRouteV6(t *testing.T) {
	out := `   route to: default
destination: default
       mask: default
    gateway: fe80::255:7bff:feb5:7df7%en0
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC>
 recvpipe  sendpipe  ssthresh
       0         0         0
`
	gw, iface := parseDefaultRouteV6(out)
	if gw != "fe80::255:7bff:feb5:7df7%en0" {
		t.Errorf("gateway = %q, want fe80::255:7bff:feb5:7df7%%en0 (with %%scope)", gw)
	}
	if iface != "en0" {
		t.Errorf("interface = %q, want en0", iface)
	}
}

func TestParseDefaultRouteV4Untouched(t *testing.T) {
	// The v4 parser must not regress after adding the v6 one.
	out := `    gateway: 10.0.0.1
  interface: en0
`
	gw, iface := parseDefaultRoute(out)
	if gw != "10.0.0.1" || iface != "en0" {
		t.Errorf("v4 parse = %q/%q, want 10.0.0.1/en0", gw, iface)
	}
}

func TestParsePing6(t *testing.T) {
	success := `PING6(56=40+8+8 bytes) 2600::1 --> 2606:4700:4700::1111
16 bytes from 2606:4700:4700::1111, icmp_seq=0 hlim=57 time=13.2 ms
16 bytes from 2606:4700:4700::1111, icmp_seq=1 hlim=57 time=12.8 ms

--- 2606:4700:4700::1111 ping6 statistics ---
2 packets transmitted, 2 packets received, 0.0% packet loss
round-trip min/avg/max/std-dev = 12.8/13.0/13.2/0.2 ms
`
	loss, avg, ok := parsePing6(success)
	if !ok || loss != 0 || avg != 13.0 {
		t.Errorf("= %v/%v/%v, want 0/13.0/true", loss, avg, ok)
	}

	fail := `PING6(56=40+8+8 bytes) 2600::1 --> 2606:4700:4700::1111

--- 2606:4700:4700::1111 ping6 statistics ---
2 packets transmitted, 0 packets received, 100.0% packet loss
`
	loss, _, ok = parsePing6(fail)
	if ok || loss != 100 {
		t.Errorf("= %v/%v, want 100/false", loss, ok)
	}
}
