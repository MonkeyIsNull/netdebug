package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// detectInterface finds the Wi-Fi interface name, falling back to en0.
func detectInterface() string {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "en0"
	}
	return parseInterface(string(out))
}

func parseInterface(out string) string {
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "Wi-Fi") || strings.Contains(ln, "AirPort") {
			for j := i + 1; j < len(lines) && j <= i+2; j++ {
				if strings.HasPrefix(lines[j], "Device:") {
					return strings.TrimSpace(strings.TrimPrefix(lines[j], "Device:"))
				}
			}
		}
	}
	return "en0"
}

// currentNetwork returns the SSID the interface is joined to, using a fallback
// chain because no single macOS command is reliable across OS versions:
//   1. networksetup -getairportnetwork   (often empty on Sonoma+)
//   2. ipconfig getsummary               (usually still has the SSID)
//   3. system_profiler SPAirPortDataType (network name under Current Network)
func currentNetwork(iface string) string {
	if out, err := exec.Command("networksetup", "-getairportnetwork", iface).Output(); err == nil {
		if s := parseNetworksetupSSID(string(out)); usableSSID(s) {
			return s
		}
	}
	if out, err := exec.Command("ipconfig", "getsummary", iface).Output(); err == nil {
		if s := parseSummarySSID(string(out)); usableSSID(s) {
			return s
		}
	}
	if out, err := exec.Command("system_profiler", "SPAirPortDataType").Output(); err == nil {
		if s := parseSystemProfilerSSID(string(out)); usableSSID(s) {
			return s
		}
	}
	return ""
}

func usableSSID(s string) bool {
	return s != "" && s != "<redacted>"
}

// interfaceActive reports whether the Wi-Fi interface is associated (link up),
// independent of whether macOS will reveal the SSID name. This is what decides
// if the probe battery should run — we can test the connection even when the
// SSID is redacted.
func interfaceActive(iface string) bool {
	out, err := exec.Command("ifconfig", iface).Output()
	if err != nil {
		return false
	}
	return parseIfconfigActive(string(out))
}

func parseIfconfigActive(out string) bool {
	return strings.Contains(out, "status: active")
}

// parseNetworksetupSSID reads "Current Wi-Fi Network: MyNetwork".
func parseNetworksetupSSID(out string) string {
	s := strings.TrimSpace(out)
	if s == "" || strings.Contains(strings.ToLower(s), "not associated") {
		return ""
	}
	if i := strings.Index(s, ": "); i >= 0 {
		return strings.TrimSpace(s[i+2:])
	}
	return ""
}

var reSummarySSID = regexp.MustCompile(`(?m)^\s*SSID(?:_STR)?\s*:\s*(.+?)\s*$`)

// parseSummarySSID reads the SSID from `ipconfig getsummary <iface>`.
func parseSummarySSID(out string) string {
	if m := reSummarySSID.FindStringSubmatch(out); m != nil {
		v := strings.TrimSpace(m[1])
		// Skip hex-byte renderings like "47 47 47 31".
		if !looksLikeHexBytes(v) {
			return v
		}
	}
	return ""
}

func looksLikeHexBytes(s string) bool {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return false
	}
	for _, f := range fields {
		if len(f) != 2 {
			return false
		}
		for _, c := range f {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// parseSystemProfilerSSID reads the network name printed right after the
// "Current Network Information:" header.
func parseSystemProfilerSSID(out string) string {
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "Current Network Information:") {
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t == "" {
					continue
				}
				return strings.TrimSuffix(t, ":")
			}
		}
	}
	return ""
}

// joinNetwork associates iface with ssid (password may be empty for a
// remembered network). NOTE: on recent macOS this frequently fails with
// CoreWLAN "Error -3900"; prefer switching networks manually and re-running.
func joinNetwork(iface, ssid, password string) error {
	args := []string{"-setairportnetwork", iface, ssid}
	if password != "" {
		args = append(args, password)
	}
	out, err := exec.Command("networksetup", args...).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%v: %s", err, msg)
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "failed") || strings.Contains(low, "could not") || strings.Contains(low, "error") {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// keychainPassword fetches the saved Wi-Fi password for ssid from the login
// keychain. May trigger a one-time GUI keychain-access prompt.
func keychainPassword(ssid string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-ga", ssid, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// LinkInfo is the current radio association detail.
type LinkInfo struct {
	SSID    string `json:"ssid"`
	Channel string `json:"channel"` // e.g. "5g149/80"
	Band    string `json:"band"`    // "2.4 GHz" / "5 GHz" / "6 GHz"
	RSSI    int    `json:"rssi"`
}

var (
	reChannel = regexp.MustCompile(`(?m)^\s*Channel\s*:\s*([0-9a-zA-Z/]+)`)
	reRSSI    = regexp.MustCompile(`(?m)^\s*RSSI\s*:\s*(-?\d+)`)
	reSSIDwd  = regexp.MustCompile(`(?m)^\s*SSID\s*:\s*(.+)$`)
)

// parseLinkInfo extracts band/channel/RSSI from `wdutil info` output.
func parseLinkInfo(out string) LinkInfo {
	li := LinkInfo{}
	if m := reChannel.FindStringSubmatch(out); m != nil {
		li.Channel = m[1]
		switch {
		case strings.HasPrefix(m[1], "6g"):
			li.Band = "6 GHz"
		case strings.HasPrefix(m[1], "5g"):
			li.Band = "5 GHz"
		case strings.HasPrefix(m[1], "2g"):
			li.Band = "2.4 GHz"
		}
	}
	if m := reRSSI.FindStringSubmatch(out); m != nil {
		fmt.Sscanf(m[1], "%d", &li.RSSI)
	}
	if m := reSSIDwd.FindStringSubmatch(out); m != nil {
		li.SSID = strings.TrimSpace(m[1])
	}
	return li
}

// linkInfo reads the current link via `wdutil info` (fuller detail with sudo).
func linkInfo() LinkInfo {
	out, _ := exec.Command("wdutil", "info").CombinedOutput()
	return parseLinkInfo(string(out))
}
