package main

import "testing"

const scutilNone = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
  }
  FTPPassive : 1
}`

const scutilHTTP = `<dictionary> {
  HTTPEnable : 1
  HTTPPort : 3128
  HTTPProxy : 10.0.0.9
  ExceptionsList : <array> {
    0 : *.local
  }
}`

const scutilStale = `<dictionary> {
  HTTPEnable : 0
  HTTPProxy : stale.example.com
  HTTPPort : 8080
}`

const scutilPAC = `<dictionary> {
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://wpad/wpad.dat
}`

func TestParseScutilProxyNone(t *testing.T) {
	r := proxyResultFrom(parseScutilProxy(scutilNone))
	if r.HTTPProxy != "" || r.HTTPSProxy != "" || r.SOCKSProxy != "" || r.PACURL != "" {
		t.Errorf("expected all empty, got %+v", r)
	}
	if r.AutoDiscover {
		t.Error("expected AutoDiscover false")
	}
}

func TestParseScutilProxyHTTP(t *testing.T) {
	r := proxyResultFrom(parseScutilProxy(scutilHTTP))
	if r.HTTPProxy != "10.0.0.9:3128" {
		t.Errorf("http_proxy = %q, want 10.0.0.9:3128", r.HTTPProxy)
	}
}

func TestParseScutilProxyStaleDisabled(t *testing.T) {
	r := proxyResultFrom(parseScutilProxy(scutilStale))
	if r.HTTPProxy != "" {
		t.Errorf("http_proxy = %q, want empty (HTTPEnable : 0)", r.HTTPProxy)
	}
}

func TestParseScutilProxyPAC(t *testing.T) {
	r := proxyResultFrom(parseScutilProxy(scutilPAC))
	if r.PACURL != "http://wpad/wpad.dat" {
		t.Errorf("pac_url = %q, want http://wpad/wpad.dat", r.PACURL)
	}
}

func TestParseScutilProxyExceptionsListIgnored(t *testing.T) {
	// The nested array body (0 : *.local) must not leak into any field.
	s := parseScutilProxy(scutilHTTP)
	if s.HTTPProxy != "10.0.0.9" {
		t.Errorf("HTTPProxy = %q, want 10.0.0.9 (ExceptionsList must not corrupt it)", s.HTTPProxy)
	}
}

func TestParseTunnelIfaces(t *testing.T) {
	got := parseTunnelIfaces("lo0 en0 utun0 utun1 awdl0 ipsec0")
	want := []string{"utun0", "utun1", "ipsec0"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

const netstatV4 = `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            192.168.1.1        UGScg                 en0
127                127.0.0.1          UCS                   lo0
`

const netstatV6 = `Internet6:
Destination                             Gateway                     Flags         Netif Expire
default                                 fe80::%utun0                UGcIg         utun0
default                                 fe80::%utun1                UGcIg         utun1
::1                                     ::1                         UHL           lo0
`

func TestParseExtraDefaultRoutes(t *testing.T) {
	v4 := parseExtraDefaultRoutes(netstatV4, "inet")
	if len(v4) != 1 {
		t.Fatalf("v4 default routes = %d, want 1", len(v4))
	}
	if v4[0].Gateway != "192.168.1.1" || v4[0].Netif != "en0" {
		t.Errorf("v4 default = %+v, want gw 192.168.1.1 netif en0", v4[0])
	}

	v6 := parseExtraDefaultRoutes(netstatV6, "inet6")
	if len(v6) != 2 {
		t.Fatalf("v6 default routes = %d, want 2", len(v6))
	}
	if v6[0].Netif != "utun0" || v6[1].Netif != "utun1" {
		t.Errorf("v6 netifs = %q/%q, want utun0/utun1", v6[0].Netif, v6[1].Netif)
	}
}

func TestDetectVPNFalsePositive(t *testing.T) {
	tuns := []string{"utun0", "utun1", "utun2", "utun3", "utun4", "utun5", "utun6", "utun7"}
	v4 := parseExtraDefaultRoutes(netstatV4, "inet") // v4 default on en0
	v6 := parseExtraDefaultRoutes(netstatV6, "inet6") // only fe80::%utunN
	vpn, detail := detectVPN(tuns, v4, v6, "en0")
	if vpn {
		t.Errorf("expected vpn=false (utun v6 defaults + v4 on Wi-Fi), got detail %q", detail)
	}
}

func TestDetectVPNEthernetNotVPN(t *testing.T) {
	// v4 default on Ethernet (en5) while Wi-Fi is en0 — a docked Mac, NOT a VPN.
	v4 := []DefaultRoute{{Family: "inet", Gateway: "192.168.1.1", Netif: "en5"}}
	vpn, detail := detectVPN(nil, v4, nil, "en0")
	if vpn {
		t.Errorf("expected vpn=false for a plain Ethernet default route, got detail %q", detail)
	}
}

func TestDetectVPNTrue(t *testing.T) {
	// A utun owning the v4 default route => VPN.
	v4 := []DefaultRoute{{Family: "inet", Gateway: "10.8.0.1", Netif: "utun3"}}
	vpn, detail := detectVPN([]string{"utun3"}, v4, nil, "en0")
	if !vpn {
		t.Fatal("expected vpn=true when utun owns the v4 default route")
	}
	if detail == "" {
		t.Error("expected a non-empty VPN detail naming the interface")
	}
}
