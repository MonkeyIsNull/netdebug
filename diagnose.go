package main

import "strings"

// fixHint suggests a concrete next action for the operator, based on which layer
// failed and what DHCP advertised. Returns "" when there's nothing useful to add.
func fixHint(ip, dhcpRouter string, hasIP, hasRoute, gwOK, inetOK bool) string {
	// eero's out-of-box LAN subnet. If we see it while things are broken, a
	// half-applied bridge mode is the usual culprit.
	eero := strings.HasPrefix(ip, "192.168.68.") || strings.HasPrefix(dhcpRouter, "192.168.68.")

	switch {
	case hasIP && !hasRoute && dhcpRouter != "":
		msg := "The advertised gateway " + dhcpRouter + " isn't routing. Check the router/AP that owns this subnet — its uplink/WAN is down or it's in a broken bridge state."
		if eero {
			msg += " The 192.168.68.x subnet is an eero default: this is the classic 'someone half-applied Bridge Mode' fault. In the eero app -> Settings -> Network Settings -> DHCP & NAT, either set it back to Automatic (router mode) with a working uplink, or properly finish Bridge Mode so clients get addresses from the main router instead of 192.168.68.x. A factory reset + clean re-setup is the reliable fix."
		}
		return msg

	case hasIP && !hasRoute && dhcpRouter == "":
		return "DHCP gave no gateway (router option). Fix the DHCP scope on the AP, or bring up the mesh/bridge link that feeds it."

	case hasIP && hasRoute && !gwOK:
		return "Gateway is set but doesn't respond — check for client isolation on the AP, or a wrong gateway address."

	case hasIP && hasRoute && gwOK && !inetOK:
		return "Gateway works but there's no internet upstream — the router's WAN is down, or this SSID is a guest VLAN with no route out."

	default:
		return ""
	}
}

// diagnose turns the layered pass/fail booleans into a single plain-English
// verdict, walking the stack from the lowest failing layer upward. The first
// failing layer is almost always the real cause, so order matters here.
func diagnose(hasIP, hasRoute, gwOK, inetOK, dnsAnyOK, dhcpResolverOK, httpOK, captive bool) string {
	switch {
	case !hasIP:
		return "DHCP FAILURE: associated but no routable IP (self-assigned 169.254 or none). " +
			"The DHCP server on this SSID isn't handing out a lease — often a broken guest VLAN or an exhausted/misconfigured DHCP scope."

	case !hasRoute:
		return "NO DEFAULT ROUTE: got an IP but no gateway. DHCP didn't provide a router option, " +
			"or macOS hasn't published the route yet (captive check still pending)."

	case !gwOK:
		return "GATEWAY UNREACHABLE: IP and route are present but the gateway doesn't answer pings. " +
			"Looks like L2 / client-isolation on the AP, or the wrong gateway was handed out."

	case !inetOK:
		return "NO UPSTREAM: the gateway is reachable but the internet is not (pings to public IPs fail). " +
			"This SSID's uplink/WAN is down or not routing out — the classic 'connects but no traffic' guest-network fault."

	case !dhcpResolverOK && dnsAnyOK:
		return "BROKEN DNS SERVER: routing and internet work, and a public resolver works, but the " +
			"DHCP-provided DNS server fails to resolve. Fix or replace the resolver the router advertises."

	case !dnsAnyOK:
		return "DNS FAILURE: routing works but no resolver can resolve names. DNS is blocked or misconfigured on this SSID."

	case captive:
		return "CAPTIVE PORTAL: lower layers work but web traffic is being intercepted by a portal that hasn't been completed."

	case !httpOK:
		return "HTTP BLOCKED: DNS resolves and IPs ping, but HTTP(S) requests fail — a firewall or proxy may be blocking web traffic."

	default:
		return "HEALTHY: all layers passed — DHCP, routing, gateway, internet, DNS, and HTTP."
	}
}
