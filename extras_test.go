package main

import "testing"

func TestParseARP(t *testing.T) {
	full := "? (192.168.1.1) at 24:a4:3c:aa:bb:cc on en0 ifscope [ethernet]"
	mac, complete := parseARP(full)
	if !complete || mac != "24:a4:3c:aa:bb:cc" {
		t.Errorf("= %q/%v, want 24:a4:3c:aa:bb:cc/true", mac, complete)
	}

	abbrev := "? (192.168.1.1) at 0:55:7b:b5:7d:f7 on en0 ifscope [ethernet]"
	mac, complete = parseARP(abbrev)
	if !complete || mac != "0:55:7b:b5:7d:f7" {
		t.Errorf("abbrev = %q/%v, want 0:55:7b:b5:7d:f7/true", mac, complete)
	}

	if mac, complete := parseARP("? (192.168.1.1) at (incomplete) on en0"); complete || mac != "" {
		t.Errorf("incomplete = %q/%v, want ''/false", mac, complete)
	}
	if mac, complete := parseARP(""); complete || mac != "" {
		t.Errorf("empty = %q/%v, want ''/false", mac, complete)
	}
}

func TestParsePingMTU(t *testing.T) {
	ok := `PING 1.1.1.1 (1.1.1.1): 1372 data bytes
1380 bytes from 1.1.1.1: icmp_seq=0 ttl=57 time=12.0 ms

--- 1.1.1.1 ping statistics ---
2 packets transmitted, 2 packets received, 0.0% packet loss
`
	if m := parsePingMTU(ok, 1372); !m.OK || m.Fragmented || m.Size != 1372 {
		t.Errorf("ok probe = %+v, want OK,!frag,1372", m)
	}

	loss := `PING 1.1.1.1 (1.1.1.1): 1472 data bytes

--- 1.1.1.1 ping statistics ---
2 packets transmitted, 0 packets received, 100.0% packet loss
`
	if m := parsePingMTU(loss, 1472); m.OK {
		t.Errorf("total-loss probe should be !OK, got %+v", m)
	}

	frag := "ping: sendto: Message too long\n"
	if m := parsePingMTU(frag, 1472); !m.Fragmented {
		t.Errorf("expected Fragmented=true for 'Message too long', got %+v", m)
	}
	fragRouter := "36 bytes from 10.0.0.1: frag needed and DF set (MTU 1400)\n"
	if m := parsePingMTU(fragRouter, 1472); !m.Fragmented {
		t.Errorf("expected Fragmented=true for 'frag needed and DF set', got %+v", m)
	}
}

func TestDecideMTU(t *testing.T) {
	// both fail => ICMP filtering, NOT a black hole
	bh, _ := decideMTU([]MTUResult{{Size: 1472, OK: false}, {Size: 1372, OK: false}})
	if bh {
		t.Error("both-fail must not be a black hole")
	}
	// large fail + small ok => black hole
	bh, detail := decideMTU([]MTUResult{{Size: 1472, OK: false}, {Size: 1372, OK: true}})
	if !bh {
		t.Error("large-fail + small-ok must be a black hole")
	}
	if detail == "" {
		t.Error("expected a black-hole detail message")
	}
	// both ok => fine
	bh, _ = decideMTU([]MTUResult{{Size: 1472, OK: true}, {Size: 1372, OK: true}})
	if bh {
		t.Error("both-ok must not be a black hole")
	}
}

const dhcpPacketHex = `op = BOOTREPLY
htype = 1
subnet_mask (ip): 255.255.255.0
router (ip_mult): {192.168.1.1}
domain_name_server (ip_mult): {192.168.1.1, 8.8.8.8}
domain_name (string): lan
lease_time (uint32): 0x15180
server_identifier (ip): 192.168.1.1
`

func TestParseDHCPLeaseHex(t *testing.T) {
	l := parseDHCPLease(dhcpPacketHex)
	if l.LeaseSecs != 86400 {
		t.Errorf("lease_secs = %d, want 86400 (from 0x15180)", l.LeaseSecs)
	}
	if l.ServerID != "192.168.1.1" {
		t.Errorf("server_id = %q, want 192.168.1.1", l.ServerID)
	}
	if l.SubnetMask != "255.255.255.0" {
		t.Errorf("subnet_mask = %q, want 255.255.255.0", l.SubnetMask)
	}
	if l.DomainName != "lan" {
		t.Errorf("domain_name = %q, want lan", l.DomainName)
	}
	if len(l.Routers) != 1 || l.Routers[0] != "192.168.1.1" {
		t.Errorf("routers = %v, want [192.168.1.1]", l.Routers)
	}
	if len(l.DNS) != 2 || l.DNS[1] != "8.8.8.8" {
		t.Errorf("dns = %v, want [192.168.1.1 8.8.8.8]", l.DNS)
	}
}

func TestParseDHCPLeaseDecimal(t *testing.T) {
	// A bare decimal lease_time must also parse (base-0).
	l := parseDHCPLease("lease_time (uint32): 86400\n")
	if l.LeaseSecs != 86400 {
		t.Errorf("lease_secs = %d, want 86400 (bare decimal)", l.LeaseSecs)
	}
}

func TestParseDHCPLeaseOptional(t *testing.T) {
	// No domain_search / ntp_servers => empty slices, no error.
	l := parseDHCPLease(dhcpPacketHex)
	if len(l.Search) != 0 || len(l.NTP) != 0 {
		t.Errorf("expected empty Search/NTP, got %v/%v", l.Search, l.NTP)
	}

	withOpts := dhcpPacketHex +
		"domain_search (string): {a.com, b.com}\n" +
		"ntp_servers (ip_mult): {192.168.1.1, 192.168.1.2}\n"
	l = parseDHCPLease(withOpts)
	if len(l.Search) != 2 || l.Search[0] != "a.com" || l.Search[1] != "b.com" {
		t.Errorf("search = %v, want [a.com b.com]", l.Search)
	}
	if len(l.NTP) != 2 || l.NTP[0] != "192.168.1.1" {
		t.Errorf("ntp = %v, want [192.168.1.1 192.168.1.2]", l.NTP)
	}
}
