// netdebug — diagnose why a Wi-Fi network "connects but has no traffic".
//
// Recommended workflow (works with macOS's Wi-Fi security model):
//  1. ./netdebug                 # test whatever network you're on now
//  2. switch networks manually   # via the macOS menu bar
//  3. ./netdebug                 # test the other one
//  4. ./netdebug --compare       # diff the two most recent runs
//
// The optional --live mode tries to switch networks itself, but on recent
// macOS that path (networksetup -setairportnetwork) often fails with CoreWLAN
// "Error -3900" — so manual switching + --compare is the reliable route.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// maxPortTries bounds the auto-pick walk for --serve: starting at the requested
// port, listenLocal tries up to this many consecutive ports. Chosen so the
// default 8099 + maxPortTries stays well under 65535.
const maxPortTries = 64

func main() {
	cfgPath := flag.String("config", "", "path to JSON config (optional; sane defaults otherwise)")
	live := flag.Bool("live", false, "try to SWITCH between configured networks (unreliable on recent macOS; disrupts connectivity)")
	compare := flag.Bool("compare", false, "compare the two most recent JSON reports in --out instead of running probes")
	label := flag.String("label", "", "name this run in the report (e.g. MyNetwork) — macOS often hides the real SSID")
	outDir := flag.String("out", "./net-tests", "directory for JSON reports")

	signalF := flag.Bool("signal", false, "sample link quality (RSSI/SNR/rate) over time")
	samplesF := flag.Int("samples", 5, "number of --signal polls (~1/sec); signal-only")
	signalCSV := flag.String("signal-csv", "", "also write raw --signal samples to this CSV path")
	ipv6F := flag.Bool("ipv6", false, "check IPv6 address/route/ping6/AAAA health")
	proxyF := flag.Bool("proxy", false, "check system proxy/PAC and VPN/route hijack")
	extrasF := flag.Bool("extras", false, "gateway MAC, path-MTU probe, full DHCP lease dump")
	allF := flag.Bool("all", false, "enable --signal, --ipv6, --proxy and --extras")
	speedF := flag.Bool("speed", false, "run a download/upload/latency speed test (parallel streams) and exit")
	bufferbloatF := flag.Bool("bufferbloat", false, "run an upload/download latency-under-load (bufferbloat) test and exit (briefly saturates the uplink then downlink)")
	speedStreamsF := flag.Int("speed-streams", 0, "override parallel streams for --speed (0 = config default)")
	speedSecondsF := flag.Int("speed-seconds", 0, "override seconds/direction for --speed (0 = config default)")
	airspaceF := flag.Bool("airspace", false, "list neighboring Wi-Fi networks + per-channel occupancy and the clearest 80MHz block, then exit (sudo-free, passive, NO active scan)")
	topTalkersF := flag.Bool("top-talkers", false, "list per-process up/down bandwidth rates (top upstream talkers, sudo-free; your processes; some system procs may be hidden), then exit")
	procF := flag.Int("proc", 0, "print identity + established connections for one PID (sudo-free: ps + lsof; reverse-DNS best-effort) and exit")
	sampleF := flag.Bool("sample", false, "run the live bandwidth sampler (prints up/down rates) and exit")
	serveF := flag.Bool("serve", false, "serve a live bandwidth dashboard on 127.0.0.1 and block until Ctrl-C")
	portF := flag.Int("port", 8099, "TCP port for --serve (loopback only; auto-picks the next free port if taken)")
	historyDirF := flag.String("history-dir", "", "override where --serve persists history JSONL (default: os.UserConfigDir()/netdebug/history)")
	intervalF := flag.Duration("interval", time.Second, "sample period for --sample/--serve (clamped to >=250ms)")
	linkIntervalF := flag.Duration("link-interval", defaultLinkInterval, "how often --serve resamples the Wi-Fi radio (clamped to >=1s; serve-only)")
	reachIntervalF := flag.Duration("reach-interval", defaultReachInterval, "how often --serve probes reachability/latency (clamped to >=1s; serve-only)")
	noPowerGatingF := flag.Bool("no-power-gating", false, "keep probing reachability on battery / locked screen (default: pause to save power; serve-only)")
	sampleSecondsF := flag.Int("sample-seconds", 0, "stop --sample after N seconds (0 = run until Ctrl-C)")
	ifaceF := flag.String("iface", "", "override the target interface (default: auto-detected Wi-Fi interface)")
	ssidF := flag.String("ssid", "", "label to show for the Wi-Fi network on the --serve dashboard (macOS redacts the real SSID); serve-only, overrides config ssid_label")
	installLaunchdF := flag.Bool("install-launchd", false, "install+load a per-user LaunchAgent that runs --serve at login (loopback only; no root), then exit")
	uninstallLaunchdF := flag.Bool("uninstall-launchd", false, "unload+remove the netdebug LaunchAgent, then exit")
	launchdStatusF := flag.Bool("launchd-status", false, "report whether the netdebug LaunchAgent is loaded/running, then exit")
	launchdLabelF := flag.String("launchd-label", defaultLaunchdLabel, "reverse-DNS LaunchAgent label for --install/uninstall/status-launchd")
	launchdBinaryF := flag.String("launchd-binary", "", "stable path to bake into the LaunchAgent (default: the running binary; use e.g. /usr/local/bin/netdebug)")
	alertsF := flag.Bool("alerts", false, "raise opt-in native macOS notifications on drop/band-change/meeting-BAD/sustained-upload (serve-only; default off)")
	statusF := flag.Bool("status", false, "print a one-line health summary (reads a running --serve, else one local probe) and exit")
	statusFormatF := flag.String("status-format", "plain", "output format for --status: plain | swiftbar")
	throttleWatchF := flag.Bool("throttle-watch", false, "take small spaced DOWNLOAD samples (<=2/hr, 429-safe) to catch peak-hour throttling (serve-only, opt-in, default off)")
	reportF := flag.Bool("report", false, "print a daily/weekly rollup (totals, band time, drops, worst-sample latency, speed-by-hour) and exit (reads persisted history; no Cloudflare)")
	reportSpanF := flag.String("report-span", "day", "span for --report: day (default) | week (trailing 7 local days)")
	flag.Parse()

	if *allF {
		*signalF, *ipv6F, *proxyF, *extrasF = true, true, true, true
	}
	if *samplesF < 1 {
		*samplesF = 1 // clamp
	}
	// orphaned-modifier warnings
	if !*signalF && *samplesF != 5 {
		fmt.Fprintln(os.Stderr, "netdebug: --samples ignored (requires --signal or --all)")
	}
	if !*signalF && *signalCSV != "" {
		fmt.Fprintln(os.Stderr, "netdebug: --signal-csv ignored (requires --signal or --all)")
	}
	// --speed-streams/--speed-seconds modify BOTH --speed and --bufferbloat (they
	// share the load phase), so they are only orphaned when NEITHER is set.
	if !*speedF && !*bufferbloatF && *speedStreamsF != 0 {
		fmt.Fprintln(os.Stderr, "netdebug: --speed-streams ignored (requires --speed or --bufferbloat)")
	}
	if !*speedF && !*bufferbloatF && *speedSecondsF != 0 {
		fmt.Fprintln(os.Stderr, "netdebug: --speed-seconds ignored (requires --speed or --bufferbloat)")
	}
	// --interval is honored by BOTH --sample and --serve, so it is only orphaned
	// when neither is set (--iface is a global interface override, so it is
	// deliberately NOT warned here). --sample-seconds is --sample-only.
	if !*sampleF && !*serveF && !*installLaunchdF && *intervalF != time.Second {
		fmt.Fprintln(os.Stderr, "netdebug: --interval ignored (requires --sample, --serve or --install-launchd)")
	}
	if !*sampleF && *sampleSecondsF != 0 {
		fmt.Fprintln(os.Stderr, "netdebug: --sample-seconds ignored (requires --sample)")
	}
	// --status reads a SEPARATE running serve on --port, so --status is a legitimate
	// --port consumer (the plan's own `--status --port 8100` workaround); add !*statusF.
	if !*serveF && !*installLaunchdF && !*statusF && *portF != 8099 {
		fmt.Fprintln(os.Stderr, "netdebug: --port ignored (requires --serve, --install-launchd or --status)")
	}
	// --report is ALSO a legitimate --history-dir consumer (it reads the JSONL there
	// lock-free), so add !*reportF alongside --serve / --install-launchd.
	if !*serveF && !*installLaunchdF && !*reportF && *historyDirF != "" {
		fmt.Fprintln(os.Stderr, "netdebug: --history-dir ignored (requires --serve, --install-launchd or --report)")
	}
	if !*serveF && !*installLaunchdF && *linkIntervalF != defaultLinkInterval {
		fmt.Fprintln(os.Stderr, "netdebug: --link-interval ignored (requires --serve or --install-launchd)")
	}
	if !*serveF && !*installLaunchdF && *reachIntervalF != defaultReachInterval {
		fmt.Fprintln(os.Stderr, "netdebug: --reach-interval ignored (requires --serve or --install-launchd)")
	}
	// --no-power-gating is SERVE-ONLY (not baked into the LaunchAgent — the installed
	// agent reads power_gating from the config file), so it is orphaned unless --serve.
	if !*serveF && *noPowerGatingF {
		fmt.Fprintln(os.Stderr, "netdebug: --no-power-gating ignored (requires --serve); set \"power_gating\":false in your config for a launchd-run dashboard")
	}
	// --ssid is FOREGROUND-SERVE-ONLY. The first guard mirrors --port's SHAPE
	// (!serve && !install) so it does not misfire when the only flag is a launchd
	// verb; the second is a distinct advisory because --ssid is genuinely NOT baked
	// into the LaunchAgent (launchd.go is off-limits — use config ssid_label there).
	if !*serveF && !*installLaunchdF && *ssidF != "" {
		fmt.Fprintln(os.Stderr, "netdebug: --ssid ignored (requires --serve)")
	}
	if *installLaunchdF && *ssidF != "" {
		fmt.Fprintln(os.Stderr, "netdebug: --ssid is not written into the LaunchAgent; set \"ssid_label\" in your config for a launchd-run dashboard")
	}
	// Phase 12 alert/status orphan + precedence warnings (follow the --no-power-gating
	// / --ssid precedents).
	if *alertsF && !*serveF {
		fmt.Fprintln(os.Stderr, "netdebug: --alerts ignored (requires --serve)")
	}
	if *alertsF && *installLaunchdF {
		fmt.Fprintln(os.Stderr, "netdebug: --alerts is not baked into the LaunchAgent; alerting is foreground-serve-only this phase")
	}
	// --throttle-watch is SERVE-ONLY (opt-in; not baked into the LaunchAgent — the
	// installed agent would read it from config in a later phase), so it is orphaned
	// unless --serve (the --no-power-gating / --alerts precedent).
	if *throttleWatchF && !*serveF {
		fmt.Fprintln(os.Stderr, "netdebug: --throttle-watch ignored (requires --serve)")
	}
	// --report-span is --report-only; an unknown value WARNS and falls back to "day".
	reportSpan := *reportSpanF
	if !*reportF && reportSpan != "day" {
		fmt.Fprintln(os.Stderr, "netdebug: --report-span ignored (requires --report)")
	}
	if *reportF && reportSpan != "day" && reportSpan != "week" {
		fmt.Fprintf(os.Stderr, "netdebug: --report-span %q invalid; using day\n", reportSpan)
		reportSpan = "day"
	}
	// --serve --status is contradictory: --status reads a SEPARATE running serve.
	// --serve WINS (the long-lived intent; --status is trivially re-run afterward).
	if *serveF && *statusF {
		fmt.Fprintln(os.Stderr, "netdebug: both --serve and --status set; running --serve (re-run --status in another shell)")
	}
	// --status-format is --status-only; an invalid value WARNS and falls back to plain.
	statusFormat := *statusFormatF
	if !*statusF && statusFormat != "plain" {
		fmt.Fprintln(os.Stderr, "netdebug: --status-format ignored (requires --status)")
	}
	if *statusF && statusFormat != "plain" && statusFormat != "swiftbar" {
		fmt.Fprintf(os.Stderr, "netdebug: --status-format %q invalid; using plain\n", statusFormat)
		statusFormat = "plain"
	}
	opt := Options{
		Signal:     *signalF,
		IPv6:       *ipv6F,
		Proxy:      *proxyF,
		DHCPExtras: *extrasF,
		Samples:    *samplesF,
		SignalCSV:  *signalCSV,
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	// Interface resolution precedence: --iface > cfg.Interface > detectInterface().
	// --iface is a GLOBAL override (not sample-only): a bare run with no --iface
	// resolves exactly as before (empty default), keeping the default byte-identical.
	iface := *ifaceF
	if iface == "" {
		iface = cfg.Interface
	}
	if iface == "" {
		iface = detectInterface()
	}

	// --status is a READ-ONLY early-return one-shot (the --airspace/--speed pattern),
	// guarded !*serveF so `--serve --status` falls through to --serve (precedence
	// warned above). It reads a running serve's /data.json on --port, falling back to
	// ONE local reach probe; it prints exactly one line and exits 0.
	if *statusF && !*serveF {
		internetIPs := cfg.PublicIPs
		dnsName := ""
		if len(cfg.DNSNames) > 0 {
			dnsName = cfg.DNSNames[0]
		}
		reachInterval := clampReachInterval(time.Duration(cfg.ReachIntervalSecs) * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		summary := fetchStatus(ctx, *portF, internetIPs, dnsName, reachInterval)
		fmt.Println(formatStatusLine(summary, statusFormat))
		return
	}

	// --report is a READ-ONLY early-return one-shot (the --status/--airspace pattern).
	// It reads the FOUR fixed JSONL filenames DIRECTLY (readFileIfExists + the parsers —
	// NO openHistoryStore, so it takes NO flock and SUCCEEDS even while a --serve holds
	// the dir; §0.1 #2). It composes the DayReport PURELY and prints it. NO Cloudflare,
	// NO sampler — it only reads already-persisted data.
	if *reportF {
		historyDir, err := resolveHistoryDir(*historyDirF, cfg.HistoryDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(2)
		}
		minData, _ := readFileIfExists(filepath.Join(historyDir, minuteFileName))
		hourData, _ := readFileIfExists(filepath.Join(historyDir, hourFileName))
		outData, _ := readFileIfExists(filepath.Join(historyDir, outageFileName))
		speedData, _ := readFileIfExists(filepath.Join(historyDir, speedFileName))
		buckets := append(parseJSONLBuckets(minData), parseJSONLBuckets(hourData)...)
		outages := parseJSONLOutages(outData)
		speeds := parseJSONLSpeed(speedData)
		rep := dailyReport(buckets, outages, speeds, time.Now(), reportSpan, cfg.ThrottleRatio, cfg.ThrottleMinSamples)
		printDayReport(rep)
		return
	}

	if *compare {
		results, err := loadRecentReports(*outDir, 2)
		if err != nil {
			fmt.Fprintln(os.Stderr, "compare:", err)
			os.Exit(2)
		}
		printReport(results)
		return
	}

	if *sampleF {
		interval := clampInterval(*intervalF)
		if interval != *intervalF {
			fmt.Fprintln(os.Stderr, "netdebug: --interval clamped to 250ms")
		}
		ctx, cancel := sampleStop(*sampleSecondsF)
		defer cancel()
		runSample(ctx, iface, interval, NewRing(defaultRingCap))
		return
	}

	if *serveF {
		if *portF < 1 || *portF > 65535 {
			fmt.Fprintf(os.Stderr, "netdebug: --port %d out of range (1-65535)\n", *portF)
			os.Exit(2)
		}
		interval := clampInterval(*intervalF)
		if interval != *intervalF {
			fmt.Fprintln(os.Stderr, "netdebug: --interval clamped to 250ms")
		}
		historyDir, err := resolveHistoryDir(*historyDirF, cfg.HistoryDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(2)
		}
		minuteTTL := clampTTL(cfg.MinuteTTLHrs, time.Hour, defaultMinuteTTL)
		hourTTL := clampTTL(cfg.HourTTLDays, 24*time.Hour, defaultHourTTL)
		// Link cadence: the flag wins when set away from its default, else the
		// config value; clampLinkInterval floors it at 1s (<=0 => 5s).
		linkInterval := *linkIntervalF
		if linkInterval == defaultLinkInterval {
			linkInterval = time.Duration(cfg.LinkIntervalSecs) * time.Second
		}
		clampedLink := clampLinkInterval(linkInterval)
		if clampedLink != linkInterval {
			fmt.Fprintln(os.Stderr, "netdebug: --link-interval clamped to >=1s")
		}
		linkInterval = clampedLink
		// Reach cadence: the flag wins when set away from its default, else the config
		// value; clampReachInterval floors it at 1s (<=0 => 5s). Inherited quirk,
		// deliberate match with --link-interval: a user who explicitly types the default
		// value is indistinguishable from unset and falls through to config.
		reachInterval := *reachIntervalF
		if reachInterval == defaultReachInterval {
			reachInterval = time.Duration(cfg.ReachIntervalSecs) * time.Second
		}
		clampedReach := clampReachInterval(reachInterval)
		if clampedReach != reachInterval {
			fmt.Fprintln(os.Stderr, "netdebug: --reach-interval clamped to >=1s")
		}
		reachInterval = clampedReach
		// Reach targets: both PublicIPs (readReach guards len 0/1 and ParseIP-validates
		// each before exec); DNS name from DNSNames[0] ("" => skip the DNS probe).
		internetIPs := cfg.PublicIPs
		dnsName := ""
		if len(cfg.DNSNames) > 0 {
			dnsName = cfg.DNSNames[0]
		}
		// Power gating: ON by default (config power_gating), the flag opts back out.
		powerGating := cfg.PowerGating && !*noPowerGatingF
		// Phase 8 outage journal: debounce thresholds clamped to >=1, TTL clamped at the
		// impure boundary (defaults 2/2/90). A stray 0 cannot make the machine open/close
		// on the first obs.
		outageStart := clampOutageThreshold(cfg.OutageStartThreshold)
		outageEnd := clampOutageThreshold(cfg.OutageEndThreshold)
		outageTTL := clampTTL(cfg.OutageTTLDays, 24*time.Hour, defaultOutageTTL)
		// Phase 11 per-process top talkers: interval clamped at the impure boundary
		// (<=0 => 10s, floor 5s) so a stray value cannot become a nettop exec storm;
		// N clamped (<1 => 5, cap 20) so the wire / panel stay bounded.
		procsInterval := clampProcsInterval(time.Duration(cfg.ProcsIntervalSecs) * time.Second)
		topTalkersN := clampTopTalkersN(cfg.TopTalkersN)
		// Phase 12 alerts: opt-in behind --alerts; all knobs clamped at the impure
		// boundary (the anti-spam contract collapses without the clamp). notify is the
		// real osascript exec; fireAlerts never calls it unless ac.enabled.
		ac := alertConfig{
			enabled:       *alertsF,
			cooldown:      clampAlertCooldown(cfg.AlertCooldownSecs),
			thresholdBps:  thresholdBps(clampHighUploadThreshold(cfg.HighUploadThresholdMbps)),
			streakSamples: clampHighUploadStreak(cfg.HighUploadStreakSamples),
		}
		// Phase 13 throttle watchdog: OPT-IN behind --throttle-watch (serve-only). ALL
		// knobs clamped at the impure boundary — a stray 0 spacing would collapse the
		// gate into the exact Cloudflare hammer this phase exists to prevent. The clamped
		// bytes/streams are baked back into cfg so the sampler (runThrottleSample(ctx,
		// cfg)) sees only clamped values. A bare --serve (no --throttle-watch) spawns no
		// watchdog goroutine and calls Cloudflare ZERO times.
		cfg.ThrottleSampleBytes = clampThrottleBytes(cfg.ThrottleSampleBytes)
		cfg.ThrottleSampleStreams = clampThrottleStreams(cfg.ThrottleSampleStreams)
		throttleMinSpacing := clampThrottleSpacing(time.Duration(cfg.ThrottleSpacingMins) * time.Minute)
		throttleTTL := clampTTL(cfg.ThrottleSampleTTLDays, 24*time.Hour, defaultThrottleSampleTTL)
		ln, addr, err := listenLocal(*portF, maxPortTries)
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(2)
		}
		// Dashboard SSID label: the --ssid flag overrides config ssid_label (flag
		// default "" is indistinguishable from unset, so config wins when the flag is
		// blank). ssidDisplay maps an empty resolution to the honest sentinel.
		label := *ssidF
		if label == "" {
			label = cfg.SSIDLabel
		}
		ssidLabel := ssidDisplay(label)
		fmt.Printf("netdebug: serving live bandwidth dashboard at http://%s/ (Ctrl-C to stop)\n", addr)
		fmt.Printf("netdebug: persisting history to %s\n", historyDir)
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := serveDashboard(ctx, iface, interval, linkInterval, ln, addr, historyDir, minuteTTL, hourTTL, ssidLabel,
			reachInterval, internetIPs, dnsName, powerGating, outageStart, outageEnd, outageTTL,
			procsInterval, topTalkersN, cfg.ProcDetailResolveDNS, ac, notify,
			*throttleWatchF, cfg, throttleMinSpacing, defaultThrottleJitterMax, defaultThrottleMaxBackoff, throttleTTL); err != nil {
			fmt.Fprintln(os.Stderr, "netdebug: serve error:", err)
			os.Exit(1)
		}
		return
	}

	if *installLaunchdF {
		runInstallLaunchd(cfg, iface, *ifaceF, *launchdLabelF, *launchdBinaryF,
			*portF, *historyDirF, *intervalF, *linkIntervalF, *reachIntervalF)
		return
	}

	if *uninstallLaunchdF {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(1)
		}
		if err := uninstallLaunchAgent(home, *launchdLabelF); err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(1)
		}
		fmt.Printf("netdebug: uninstalled LaunchAgent %q (unloaded and plist removed)\n", *launchdLabelF)
		return
	}

	if *launchdStatusF {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(1)
		}
		out, err := launchdStatus(home, *launchdLabelF)
		if err != nil {
			fmt.Fprintln(os.Stderr, "netdebug:", err)
			os.Exit(1)
		}
		fmt.Print(out)
		return
	}

	if *speedF {
		// PRECEDENCE: --speed wins over --bufferbloat (it is the existing, cheaper run).
		// Say so explicitly instead of relying on accidental early-return ordering.
		if *bufferbloatF {
			fmt.Fprintln(os.Stderr, "netdebug: both --speed and --bufferbloat set; running --speed")
		}
		if *speedStreamsF > 0 {
			cfg.SpeedStreams = *speedStreamsF
		}
		if *speedSecondsF > 0 {
			cfg.SpeedSeconds = *speedSecondsF
		}
		fmt.Printf("netdebug: running speed test (%d streams, %ds/direction) — ~%ds total...\n",
			cfg.SpeedStreams, cfg.SpeedSeconds, 2*cfg.SpeedSeconds+4)
		r := runSpeed(context.Background(), cfg)
		printSpeed(cfg, r)
		writeSpeedJSON(*outDir, r)
		return
	}

	if *bufferbloatF {
		// Shares the --speed load-phase overrides (same block as --speed above).
		if *speedStreamsF > 0 {
			cfg.SpeedStreams = *speedStreamsF
		}
		if *speedSecondsF > 0 {
			cfg.SpeedSeconds = *speedSecondsF
		}
		// Honest duration estimate: idle + up(warm-up+window) + settle + down(warm-up+
		// window) — strictly greater than --speed's 2*SpeedSeconds+4.
		fmt.Printf("netdebug: running bufferbloat test (%d streams, %ds/direction) — ~%ds total...\n",
			cfg.SpeedStreams, cfg.SpeedSeconds, 2*cfg.SpeedSeconds+7)
		fmt.Fprintln(os.Stderr, "netdebug: this briefly SATURATES your uplink then downlink (hits speed.cloudflare.com); do not run back-to-back with --speed.")
		r := runBufferbloat(cfg)
		printBufferbloat(r)
		writeBufferbloatJSON(*outDir, r)
		return
	}

	if *airspaceF {
		// Phase 10 one-shot: ONE passive system_profiler read (readRadio) feeds both
		// the joined-link parse and the neighbor parse — no second exec, no active
		// scan, no sudo. Placed BEFORE currentNetwork(iface) so we never do a second
		// radio capture. Default run / --sample stay byte-identical.
		ls, nb := readRadio(iface)
		loads := channelOccupancy(nb, ls.Channel)
		rec := clearestBlock(loads)
		fmt.Print(formatAirspace(loads, nb, ls.Channel, rec))
		return
	}

	if *topTalkersF {
		// Phase 11 one-shot: ONE nettop exec (two cumulative snapshots ~2s apart),
		// diffed to per-second RATES, ranked UPSTREAM-first. Placed BEFORE
		// currentNetwork(iface) (exactly like --airspace) so a bare run / --sample
		// never reaches it and spawns ZERO nettop. Sudo-free; an unavailable result
		// prints an honest one-line notice and does NOT prompt for or require sudo.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		procs, available := readTopTalkers(ctx)
		n := clampTopTalkersN(cfg.TopTalkersN)
		fmt.Print(formatTopTalkers(topTalkers(procs, n, false), available))
		return
	}

	if *procF != 0 {
		// Process drill-down one-shot (mirrors --top-talkers): ps + lsof for ONE pid,
		// sudo-free. Placed BEFORE currentNetwork(iface) so a bare run / --sample never
		// reaches it and spawns ZERO ps/lsof. The pid is UNTRUSTED input even here: a
		// negative/zero/over-ceiling value is rejected with the graceful "not running"
		// line and NEVER placed in argv (validProcPID gates before any exec).
		if *procF <= 0 || *procF > procPIDCeiling {
			fmt.Print(formatProcDetail(ProcDetail{PID: *procF, Connections: []ProcConn{}}))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		detail := readProcDetail(ctx, *procF, cfg.ProcDetailResolveDNS)
		fmt.Print(formatProcDetail(detail))
		return
	}

	start := currentNetwork(iface)

	var results []*NetResult

	if !*live {
		// Read-only: probe whatever we're connected to now. No switching.
		// "Associated?" comes from the link state, NOT from being able to read
		// the SSID — macOS redacts the SSID without Location permission.
		assoc := interfaceActive(iface)
		name := *label
		if name == "" {
			if start != "" {
				name = start
			} else {
				name = "(current)"
			}
		}
		switch {
		case !assoc:
			fmt.Fprintf(os.Stderr, "netdebug: %s is not associated with any Wi-Fi network.\n", iface)
		case start == "":
			fmt.Printf("netdebug: link is active but macOS is hiding the SSID name — testing it anyway.\n")
			fmt.Printf("          (use --label MyNetwork to name the run for the report/comparison)\n")
		}
		fmt.Printf("netdebug: testing current network %q on %s\n", name, iface)
		results = append(results, runBattery(cfg, iface, name, start, assoc, opt))
	} else {
		results = runLive(cfg, iface, start)
	}

	printReport(results)
	writeJSON(*outDir, results)
}

// runInstallLaunchd implements --install-launchd: resolve a STABLE binary path,
// SELF-COMPOSE the daemon Args (never forward os.Args), validate-before-side-effect
// (installLaunchAgent runs validateLaunchdOpts first), install+load the LaunchAgent,
// and print the next steps. Exits non-zero on any error.
func runInstallLaunchd(cfg Config, iface, ifaceFlag, label, binaryOverride string,
	port int, historyDirFlag string, intervalFlag, linkIntervalFlag, reachIntervalFlag time.Duration) {

	// 1. Stable binary path (refuses go-run/translocated/temp paths).
	binPath, err := resolveBinaryPath(binaryOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netdebug:", err)
		os.Exit(2)
	}

	// 2. Re-validate --port range HERE (the --serve block's check does not run on
	//    this dispatch path).
	if port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "netdebug: --port %d out of range (1-65535)\n", port)
		os.Exit(2)
	}

	// Resolve the history dir to an ABSOLUTE path and bake it in (belt-and-suspenders
	// if HOME is ever stripped under launchd).
	historyDir, err := resolveHistoryDir(historyDirFlag, cfg.HistoryDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netdebug:", err)
		os.Exit(2)
	}
	if !filepath.IsAbs(historyDir) {
		abs, aerr := filepath.Abs(historyDir)
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "netdebug: cannot absolutize history dir:", aerr)
			os.Exit(2)
		}
		historyDir = abs
	}

	// 3. SELF-COMPOSE Args. --serve is unconditional; optionals only when set away
	//    from default.
	in := launchdArgsInput{Port: port, HistoryDir: historyDir}
	if clamped := clampInterval(intervalFlag); clamped != time.Second {
		in.Interval = clamped
	}
	// Link cadence: flag wins when set away from default, else config, floored at 1s.
	linkInterval := linkIntervalFlag
	if linkInterval == defaultLinkInterval {
		linkInterval = time.Duration(cfg.LinkIntervalSecs) * time.Second
	}
	linkInterval = clampLinkInterval(linkInterval)
	if linkInterval != defaultLinkInterval {
		in.LinkInterval = linkInterval
	}
	// Reach cadence: flag wins when set away from default, else config, floored at 1s.
	reachInterval := reachIntervalFlag
	if reachInterval == defaultReachInterval {
		reachInterval = time.Duration(cfg.ReachIntervalSecs) * time.Second
	}
	reachInterval = clampReachInterval(reachInterval)
	if reachInterval != defaultReachInterval {
		in.ReachInterval = reachInterval
	}
	if ifaceFlag != "" {
		in.Iface = ifaceFlag
	}
	args := composeLaunchdArgs(in)

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "netdebug:", err)
		os.Exit(2)
	}
	logPath := filepath.Join(home, "Library", "Logs", "netdebug", "netdebug.log")

	opts := LaunchdOpts{
		Label:      label,
		BinaryPath: binPath,
		Args:       args,
		LogPath:    logPath,
		RunAtLoad:  true,
		KeepAlive:  true,
	}

	// 4+5. installLaunchAgent validates BEFORE any side-effect, then writes+loads.
	path, err := installLaunchAgent(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netdebug: install-launchd:", err)
		os.Exit(1)
	}

	// Next steps (§7).
	labelArg := ""
	if label != defaultLaunchdLabel {
		labelArg = " --launchd-label " + label
	}
	fmt.Printf("netdebug: installed LaunchAgent at %s\n", path)
	fmt.Printf("  it is running now and will restart at every login.\n")
	fmt.Printf("  dashboard: http://127.0.0.1:%d/  (a busy port auto-advances — check the log for the real URL)\n", port)
	fmt.Printf("  logs:      %s\n", logPath)
	fmt.Printf("             tail -f %s\n", logPath)
	fmt.Printf("  status:    ./netdebug --launchd-status%s\n", labelArg)
	fmt.Printf("  uninstall: ./netdebug --uninstall-launchd%s\n", labelArg)
}

// runLive attempts to join and test each configured network, then restore.
func runLive(cfg Config, iface, start string) []*NetResult {
	if len(cfg.Networks) == 0 {
		fmt.Fprintln(os.Stderr, "--live needs at least one network in the config (\"networks\": [...])")
		os.Exit(2)
	}
	restore := cfg.RestoreTo
	if restore == "" {
		restore = start
	}
	restoreFn := func() {
		if restore == "" {
			return
		}
		fmt.Printf("\nnetdebug: restoring connection to %q...\n", restore)
		_ = joinNetwork(iface, restore, passwordFor(cfg, restore))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; restoreFn(); os.Exit(130) }()
	defer restoreFn()

	var results []*NetResult
	for _, n := range cfg.Networks {
		fmt.Printf("\nnetdebug: joining %q...\n", n.SSID)
		if err := joinNetwork(iface, n.SSID, passwordFor(cfg, n.Name)); err != nil {
			fmt.Printf("  join failed: %v\n", err)
			if strings.Contains(err.Error(), "-3900") {
				fmt.Println("  (macOS blocked the automatic join. Switch to this network by hand,")
				fmt.Println("   run `./netdebug` on it, then `./netdebug --compare`.)")
			}
			results = append(results, runBattery(cfg, iface, n.Name, n.SSID, false, Options{}))
			continue
		}
		time.Sleep(cfg.SettleDuration())
		results = append(results, runBattery(cfg, iface, n.Name, n.SSID, true, Options{}))
	}
	return results
}

// passwordFor finds the password for a network by name or SSID: explicit
// plaintext first, else the login keychain, else empty (remembered network).
func passwordFor(cfg Config, nameOrSSID string) string {
	for _, n := range cfg.Networks {
		if n.Name == nameOrSSID || n.SSID == nameOrSSID {
			if n.Password != "" {
				return n.Password
			}
			if n.PasswordFromKeychain {
				if pw, err := keychainPassword(n.SSID); err == nil {
					return pw
				}
			}
		}
	}
	return ""
}

func writeJSON(dir string, results []*NetResult) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "nettest-"+time.Now().Format("20060102-150405")+".json")
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return
	}
	if os.WriteFile(path, b, 0o644) == nil {
		fmt.Printf("\nJSON report: %s\n", path)
	}
}

// writeSpeedJSON persists a --speed result. It uses a distinct filename prefix
// so --compare (which globs nettest-*.json) never mistakes it for a probe run.
func writeSpeedJSON(dir string, r SpeedResult) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "speedtest-"+time.Now().Format("20060102-150405")+".json")
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	if os.WriteFile(path, b, 0o644) == nil {
		fmt.Printf("\nJSON report: %s\n", path)
	}
}

// writeBufferbloatJSON persists a --bufferbloat result. It uses a DISTINCT filename
// prefix ("bufferbloat-") so --compare's nettest-*.json glob never matches it (nor
// does it collide with speedtest-*.json).
func writeBufferbloatJSON(dir string, r BloatResult) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "bufferbloat-"+time.Now().Format("20060102-150405")+".json")
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	if os.WriteFile(path, b, 0o644) == nil {
		fmt.Printf("\nJSON report: %s\n", path)
	}
}

// loadRecentReports reads the newest n JSON reports and flattens their results,
// so a series of single-network runs can be compared side by side.
func loadRecentReports(dir string, n int) ([]*NetResult, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "nettest-*.json"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no reports found in %s — run ./netdebug first", dir)
	}
	sort.Strings(entries) // timestamped names sort chronologically
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	var all []*NetResult
	for _, f := range entries {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rs []*NetResult
		if json.Unmarshal(b, &rs) == nil {
			all = append(all, rs...)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("reports in %s could not be read", dir)
	}
	return all, nil
}
