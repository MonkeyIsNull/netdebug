package main

import (
	"encoding/json"
	"os"
	"time"
)

// Network is one Wi-Fi network to test.
type Network struct {
	Name                 string `json:"name"`                   // label in the report
	SSID                 string `json:"ssid"`                   // the actual SSID to join
	Password             string `json:"password,omitempty"`     // plaintext (optional)
	PasswordFromKeychain bool   `json:"password_from_keychain"` // else pull from login keychain
}

// Config is the whole tool configuration. Everything is tunable; sane defaults
// mean it runs with no config file at all (read-only, on the current network).
type Config struct {
	Interface  string    `json:"interface"`      // "" => auto-detect (usually en0)
	RestoreTo  string    `json:"restore_to"`     // reconnect here when done ("" => whatever we started on)
	SettleSecs int       `json:"settle_seconds"` // wait after joining before probing (DHCP + captive check)
	Repeat     int       `json:"repeat"`         // run the battery N times per network
	Networks   []Network `json:"networks"`       // networks to compare in --live mode
	PublicIPs  []string  `json:"public_ips"`     // ping targets for DNS-free reachability
	DNSServers []string  `json:"dns_servers"`    // "dhcp" = the resolver DHCP handed us
	DNSNames   []string  `json:"dns_names"`      // names to resolve
	HTTPChecks []string  `json:"http_checks"`    // URLs to GET (captive.apple.com first)
	PingCount  int       `json:"ping_count"`     // packets per ping test

	// Speed test (--speed)
	SpeedStreams    int    `json:"speed_streams"`     // parallel TCP streams per direction
	SpeedSeconds    int    `json:"speed_seconds"`     // measurement window per direction
	SpeedDownURL    string `json:"speed_down_url"`    // download endpoint
	SpeedUpURL      string `json:"speed_up_url"`      // upload endpoint
	SpeedLatencyURL string `json:"speed_latency_url"` // tiny GET for latency

	// History persistence (--serve; Phase 3). These MUST NOT be referenced by
	// NetResult / the report path — history is a wholly separate wire surface.
	HistoryDir   string `json:"history_dir"`      // "" => default under os.UserConfigDir()/netdebug/history (NOT --out)
	MinuteTTLHrs int    `json:"minute_ttl_hours"` // default 48; <=0 clamped to 48 at the impure boundary
	HourTTLDays  int    `json:"hour_ttl_days"`    // default 90; <=0 clamped to 90

	// Radio correlation (--serve; Phase 4).
	LinkIntervalSecs int `json:"link_interval_seconds"` // default 5; <=0 clamped to 5s via clampLinkInterval

	// Reachability + latency probe (--serve; Phase 7).
	ReachIntervalSecs int  `json:"reach_interval_seconds"` // default 5; <=0 clamped to 5s via clampReachInterval
	PowerGating       bool `json:"power_gating"`           // default true; false => never pause probing (battery/locked)

	// Outage journal (--serve; Phase 8). Debounce thresholds in reach cycles; TTL in
	// days. Thresholds clamped to >=1 via clampOutageThreshold; TTL via clampTTL — both
	// at the impure boundary (a stray 0 cannot make the machine open/close on obs 1).
	OutageStartThreshold int `json:"outage_start_threshold"` // default 2; <1 clamped to 2
	OutageEndThreshold   int `json:"outage_end_threshold"`   // default 2; <1 clamped to 2
	OutageTTLDays        int `json:"outage_ttl_days"`        // default 90; <=0 clamped to 90

	// Per-process bandwidth / top talkers (--serve; Phase 11). Interval in seconds
	// (default 10; <=0 => 10, floor 5 via clampProcsInterval — nettop is heavy); N is
	// the ranked-row cap (default 5; <1 => 5, cap 20 via clampTopTalkersN).
	ProcsIntervalSecs int `json:"procs_interval_seconds"`
	TopTalkersN       int `json:"top_talkers_n"`

	// Process drill-down (--serve /proc + --proc). Reverse-DNS naming of a process's
	// ESTABLISHED remote endpoints is ENABLED by default (a kill-switch, not an
	// opt-in); it is best-effort and bounded (one overall deadline), and the core
	// detail is identical with it off. The drill-down endpoint is request-driven — no
	// interval/cadence config.
	ProcDetailResolveDNS bool `json:"proc_detail_resolve_dns"`

	// Alerts & ambient (--serve --alerts; Phase 12). Native notifications on
	// drop/band-change/meeting-BAD/sustained-upload, opt-in. All clamped at the
	// impure boundary (clampAlertCooldown / clampHighUploadThreshold /
	// clampHighUploadStreak) — the anti-spam contract COLLAPSES without a clamp.
	AlertCooldownSecs       int     `json:"alert_cooldown_secs"`        // default 300; <=0 => 300
	HighUploadThresholdMbps float64 `json:"high_upload_threshold_mbps"` // default 120; <=0 => 120
	HighUploadStreakSamples int     `json:"high_upload_streak_samples"` // default 10; <1 => 10

	// Throttle watchdog (--serve --throttle-watch; Phase 13). OPT-IN, default OFF. The
	// sampler reuses SpeedDownURL/SpeedLatencyURL with bytes= swapped to
	// ThrottleSampleBytes. ALL clamped at the impure boundary (clampThrottleSpacing/
	// Streams/Bytes/Ratio) — a stray 0 spacing would collapse the gate into the exact
	// Cloudflare hammer this phase exists to prevent.
	ThrottleSpacingMins   int     `json:"throttle_spacing_mins"`    // default 30; HARD FLOOR 30 (<30 clamped up)
	ThrottleSampleStreams int     `json:"throttle_sample_streams"`  // default 1; <1 => 1, cap 2
	ThrottleSampleBytes   int     `json:"throttle_sample_bytes"`    // default 10_000_000; clamped to [5M,50M]
	ThrottleRatio         float64 `json:"throttle_ratio"`           // default 0.6; outside (0,1] => 0.6
	ThrottleMinSamples    int     `json:"throttle_min_samples"`     // default 2; noise guard
	ThrottleSampleTTLDays int     `json:"throttle_sample_ttl_days"` // default 30; <=0 => 30

	// Dashboard display label (--serve; Phase 6). DISTINCT from Network.SSID (the
	// --live join target): macOS redacts the real SSID, so this is the user-facing
	// Wi-Fi name shown on the dashboard. "" => dashboard shows "HIDDEN (macOS-redacted)".
	// A launchd-run dashboard reads its label from THIS field (the --ssid flag is
	// foreground-serve-only and is not baked into the LaunchAgent).
	SSIDLabel string `json:"ssid_label"`
}

// DefaultConfig returns usable defaults so the tool works with zero config.
func DefaultConfig() Config {
	return Config{
		Interface:  "",
		SettleSecs: 8,
		Repeat:     1,
		PublicIPs:  []string{"1.1.1.1", "8.8.8.8"},
		DNSServers: []string{"dhcp", "1.1.1.1"},
		DNSNames:   []string{"apple.com", "cloudflare.com"},
		HTTPChecks: []string{
			"http://captive.apple.com/hotspot-detect.html",
			"https://www.apple.com",
		},
		PingCount: 3,

		SpeedStreams:    4,
		SpeedSeconds:    8,
		SpeedDownURL:    "https://speed.cloudflare.com/__down?bytes=50000000",
		SpeedUpURL:      "https://speed.cloudflare.com/__up",
		SpeedLatencyURL: "https://speed.cloudflare.com/__down?bytes=0",

		MinuteTTLHrs: 48,
		HourTTLDays:  90,

		LinkIntervalSecs: 5,

		ReachIntervalSecs: 5,
		PowerGating:       true,

		OutageStartThreshold: defaultOutageStartThresh,
		OutageEndThreshold:   defaultOutageEndThresh,
		OutageTTLDays:        90,

		ProcsIntervalSecs:    10,
		TopTalkersN:          defaultTopTalkersN,
		ProcDetailResolveDNS: true, // ENABLED by default (bounded best-effort); a kill-switch

		AlertCooldownSecs:       defaultAlertCooldownSecs,
		HighUploadThresholdMbps: defaultHighUploadThresholdMbps,
		HighUploadStreakSamples: defaultHighUploadStreakSamples,

		ThrottleSpacingMins:   30,
		ThrottleSampleStreams: 1,
		ThrottleSampleBytes:   defaultThrottleBytes,
		ThrottleRatio:         defaultThrottleRatio,
		ThrottleMinSamples:    defaultThrottleMinSamples,
		ThrottleSampleTTLDays: 30,
	}
}

// LoadConfig reads a JSON config over the defaults. Fields absent in the file
// keep their default value; fields present override it.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// SettleDuration is SettleSecs as a time.Duration.
func (c Config) SettleDuration() time.Duration {
	return time.Duration(c.SettleSecs) * time.Second
}
