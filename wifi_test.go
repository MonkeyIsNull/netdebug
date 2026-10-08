package main

import "testing"

func TestParseInterface(t *testing.T) {
	out := `Hardware Port: Ethernet
Device: en3
Ethernet Address: aa:bb:cc:dd:ee:ff

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 02:00:5e:10:00:01
`
	if got := parseInterface(out); got != "en0" {
		t.Errorf("parseInterface = %q, want en0", got)
	}
	if got := parseInterface("garbage with no ports"); got != "en0" {
		t.Errorf("parseInterface fallback = %q, want en0", got)
	}
}

func TestParseNetworksetupSSID(t *testing.T) {
	cases := map[string]string{
		"Current Wi-Fi Network: MyNetwork\n":                "MyNetwork",
		"Current Wi-Fi Network: MyNetwork-5G\n":             "MyNetwork-5G",
		"You are not associated with an AirPort network.\n": "",
		"": "",
	}
	for in, want := range cases {
		if got := parseNetworksetupSSID(in); got != want {
			t.Errorf("parseNetworksetupSSID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSummarySSID(t *testing.T) {
	out := `<dictionary> {
  InterfaceType : WiFi
  SSID : MyNetwork
  BSSID : ...
}`
	if got := parseSummarySSID(out); got != "MyNetwork" {
		t.Errorf("parseSummarySSID = %q, want MyNetwork", got)
	}
	// Hex-byte rendering should be rejected (not a usable SSID).
	hex := "  SSID : 47 47 47 31\n"
	if got := parseSummarySSID(hex); got != "" {
		t.Errorf("parseSummarySSID(hex) = %q, want empty", got)
	}
}

func TestParseIfconfigActive(t *testing.T) {
	active := `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 192.168.1.240 netmask 0xffffff00 broadcast 192.168.1.255
	status: active`
	if !parseIfconfigActive(active) {
		t.Error("expected active interface to be detected")
	}
	inactive := `en0: flags=8863<UP,BROADCAST> mtu 1500
	status: inactive`
	if parseIfconfigActive(inactive) {
		t.Error("did not expect inactive interface to be active")
	}
}

func TestParseSystemProfilerSSID(t *testing.T) {
	out := `          Current Network Information:
            MyNetwork-5G:
              PHY Mode: 802.11ac
              Channel: 149 (5GHz, 80MHz)
`
	if got := parseSystemProfilerSSID(out); got != "MyNetwork-5G" {
		t.Errorf("parseSystemProfilerSSID = %q, want 'MyNetwork-5G'", got)
	}
}

func TestLooksLikeHexBytes(t *testing.T) {
	if !looksLikeHexBytes("47 47 47 31") {
		t.Error("expected hex-bytes detection for '47 47 47 31'")
	}
	if looksLikeHexBytes("MyNetwork-5G") {
		t.Error("did not expect hex-bytes detection for 'MyNetwork-5G'")
	}
	if looksLikeHexBytes("MyNetwork") {
		t.Error("single token should not be hex-bytes")
	}
}

func TestParseLinkInfo(t *testing.T) {
	out := `    SSID                 : MyNetwork
    BSSID                : <redacted>
    RSSI                 : -61 dBm
    Channel              : 5g149/80
    Supported Channels   : 2g1/20,5g149/80
    Master Channel       : n/a
`
	li := parseLinkInfo(out)
	if li.Channel != "5g149/80" {
		t.Errorf("Channel = %q, want 5g149/80", li.Channel)
	}
	if li.Band != "5 GHz" {
		t.Errorf("Band = %q, want '5 GHz'", li.Band)
	}
	if li.RSSI != -61 {
		t.Errorf("RSSI = %d, want -61", li.RSSI)
	}
	if li.SSID != "MyNetwork" {
		t.Errorf("SSID = %q, want MyNetwork", li.SSID)
	}
}

func TestParseLinkInfoBands(t *testing.T) {
	for _, tc := range []struct{ ch, band string }{
		{"2g6/20", "2.4 GHz"},
		{"5g157/80", "5 GHz"},
		{"6g37/160", "6 GHz"},
	} {
		li := parseLinkInfo("    Channel              : " + tc.ch + "\n")
		if li.Band != tc.band {
			t.Errorf("band for %s = %q, want %q", tc.ch, li.Band, tc.band)
		}
	}
}
