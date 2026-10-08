package main

import (
	"fmt"
	"strings"
)

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// headline returns just the leading LABEL: portion of a diagnosis line.
func headline(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return s
}

// printReport renders the human-readable summary to stdout.
func printReport(results []*NetResult) {
	for _, r := range results {
		ssid := r.SSID
		if ssid == "" {
			ssid = "hidden by macOS"
		}
		fmt.Printf("\n==== %s (SSID: %s) ====\n", r.Network, ssid)
		if !r.Joined {
			fmt.Printf("  DID NOT JOIN\n")
			fmt.Printf("DIAGNOSIS : %s\n", r.Diagnosis)
			continue
		}
		if r.Band != "" || r.Channel != "" {
			fmt.Printf("Radio     : %s  channel %s   RSSI %d dBm\n",
				emptyDash(r.Band), emptyDash(r.Channel), r.RSSI)
		}
		if r.Signal != nil {
			s := r.Signal
			fmt.Printf("Signal    : RSSI min/avg/max %d/%d/%d dBm (jitter %.1f)   SNR %d/%d/%d dB   rate %.0f/%.0f/%.0f Mb/s   MCS %d   %s   %s\n",
				s.RSSIMin, s.RSSIAvg, s.RSSIMax, s.RSSIJitter,
				s.SNRMin, s.SNRAvg, s.SNRMax,
				s.RateMin, s.RateAvg, s.RateMax, lastMCS(s), s.PHY, s.Channel)
		}
		fmt.Printf("Network   : IP %s   gateway %s   DHCP-router %s\n",
			emptyDash(r.IP), emptyDash(r.Gateway), emptyDash(r.DHCPRouter))
		if r.IPv6 != nil {
			fmt.Printf("IPv6      : %s\n", ipv6Summary(r.IPv6))
		}
		if r.Proxy != nil {
			fmt.Printf("Proxy     : %s\n", proxySummary(r.Proxy))
		}
		if r.Extras != nil {
			fmt.Printf("Extras    : %s\n", extrasSummary(r.Extras))
		}
		for _, c := range r.Checks {
			mark := "FAIL"
			switch {
			case c.Kind == "info":
				mark = "info"
			case c.Pass:
				mark = " ok "
			case c.Kind == "advisory":
				mark = "WARN"
			}
			fmt.Printf("  [%s] %-26s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Printf("DIAGNOSIS : %s\n", r.Diagnosis)
		if r.FixHint != "" {
			fmt.Printf("FIX HINT  : %s\n", r.FixHint)
		}
	}

	if len(results) >= 2 {
		fmt.Printf("\n==== COMPARISON ====\n")
		for _, r := range results {
			fmt.Printf("  %-10s %s\n", r.Network+":", headline(r.Diagnosis))
			if r.Signal != nil {
				fmt.Printf("             signal: SNR avg %d dB, RSSI avg %d dBm, rate avg %.0f Mb/s\n",
					r.Signal.SNRAvg, r.Signal.RSSIAvg, r.Signal.RateAvg)
			}
			if r.IPv6 != nil {
				fmt.Printf("             ipv6:   %s\n", ipv6Summary(r.IPv6))
			}
		}
	}
}

// lastMCS returns the MCS of the last valid raw sample, or 0 when Raw was
// dropped (e.g. --signal-csv mode).
func lastMCS(s *SignalStats) int {
	for i := len(s.Raw) - 1; i >= 0; i-- {
		if s.Raw[i].OK {
			return s.Raw[i].MCS
		}
	}
	return 0
}

// ipv6Summary is the one-line "global / working|duplicated / ping6 ok|fail".
func ipv6Summary(r *IPv6Result) string {
	global := emptyDash(r.GlobalAddr)
	state := "working"
	if strings.Contains(r.Note, "duplicat") {
		state = "duplicated"
	} else if r.GlobalAddr == "" {
		state = "none"
	}
	ping := "ping6 fail"
	if r.PingOK {
		ping = "ping6 ok"
	}
	s := fmt.Sprintf("%s / %s / %s", global, state, ping)
	if r.Note != "" {
		s += " — " + r.Note
	}
	return s
}

func proxySummary(r *ProxyResult) string {
	var parts []string
	if r.HTTPProxy != "" {
		parts = append(parts, "http="+r.HTTPProxy)
	}
	if r.HTTPSProxy != "" {
		parts = append(parts, "https="+r.HTTPSProxy)
	}
	if r.SOCKSProxy != "" {
		parts = append(parts, "socks="+r.SOCKSProxy)
	}
	if r.PACURL != "" {
		parts = append(parts, "pac="+r.PACURL)
	}
	s := "no proxy/PAC"
	if len(parts) > 0 {
		s = strings.Join(parts, " ")
	}
	if r.VPNActive {
		s += "   VPN: " + r.VPNDetail
	} else if len(r.Tunnels) > 0 {
		s += fmt.Sprintf("   tunnels: %s (no default route)", strings.Join(r.Tunnels, ","))
	}
	return s
}

func extrasSummary(r *ExtrasResult) string {
	s := fmt.Sprintf("gateway MAC %s", emptyDash(r.GatewayMAC))
	if r.Lease.LeaseSecs > 0 {
		s += fmt.Sprintf("   lease %ds", r.Lease.LeaseSecs)
		if r.Lease.ServerID != "" {
			s += " from " + r.Lease.ServerID
		}
	}
	if r.Lease.DomainName != "" {
		s += fmt.Sprintf("   domain %q", r.Lease.DomainName)
	}
	for _, w := range r.Warnings {
		s += "\n             ! " + w
	}
	return s
}
