# Architecture

netdebug is a single flat Go package (`package main`, ~56 `.go` files) that
compiles to one static binary. This document describes the design conventions,
the invariants the code enforces, and a file-by-file map so a new contributor
can navigate.

## Design principles

### Standard library only

`go.mod` has **zero dependencies**. Everything — the HTTP server, JSON, the
charts, the concurrency — is stdlib. This keeps the binary trivially auditable
and `go install`-able, and means there is no supply chain to vet. Please keep it
that way.

### The pure / impure split (and table-driven tests)

The repo-wide house style is a hard split between **pure** logic and **impure**
IO:

- **Pure functions** — parsing macOS command output, classification, debounce
  state machines, serialization, and the clamps that bound every config knob —
  take values in and return values out, touch no globals, and do no IO. These
  are exhaustively **table-tested** in the `*_test.go` files against captured
  real-world command output.
- **Impure wrappers** — the thin functions that actually `exec` a macOS command
  or touch the filesystem/network — are kept small and are *not* unit-tested
  against the live system.

Most feature files carry a header comment naming this boundary, and several
(`airspace.go`, `ipv6.go`, `extras.go`) use an explicit
`// ---- impure command layer ----` divider. Config knobs are clamped "at the
impure boundary" so a stray `0` from a config file can never, for example,
collapse a rate-limited sampler into a request storm.

When adding a feature: put the logic in pure functions with a table test, and
keep the `exec`/IO shim as thin as possible.

### Self-contained dashboard

The dashboard is one HTML document, held in `dashboard.go` as a Go backtick
raw-string const (inline `<style>` + two inline `<script>`s). The conventions,
all test-enforced:

- **Zero external URLs** — no CDN, no web font, no `<link>`, no `url()`, no
  `@font-face`, no external `<img>`. Charts are **inline SVG** (`<polyline>`
  over a faint fill polygon); the loading spinner is a pure-CSS rotating border.
- **`textContent`, never `innerHTML`** — every free-form string (SSID label, PHY,
  process names, reverse-DNS/raw-address fields) is written with `createElement`
  + `textContent`, so a restyle slip cannot become self-XSS on the loopback
  page.

### Loopback-only, no-sudo invariants

- The server binds `127.0.0.1` **exclusively**. `serve.go` validates the bind
  address both before binding and against the real socket address after, and
  there is no flag to bind elsewhere.
- No code path on the dashboard shells `sudo` or `wdutil`. All radio/process
  reads use sudo-free tools and degrade to an honest "unavailable" rather than
  escalating.

### Guard tests

These tests fail the build if an invariant is broken — they are the enforcement
mechanism behind the guarantees in the README. The ~11 dashboard guard tests
(`serve_test.go`) assert the page's shape and the self-contained invariant:

1. `TestDashboardHTMLHasNoExternalURLs` — the core guard: case-folded, with a
   blanket `url(` ban plus `@font-face` / `<img>` / `.woff` bans against the
   exact HTML bytes.
2. `TestDashboardHTMLHasLayoutToggle`
3. `TestDashboardHTMLWideShortensCharts`
4. `TestDashboardHTMLTallChartsUnchanged`
5. `TestDashboardHTMLHasReachDrilldown`
6. `TestDashboardHTMLHasReachTableClarity`
7. `TestDashboardHTMLHasTileInfo`
8. `TestDashboardHTMLHasMarkerDetail`
9. `TestDashboardHTMLHasRoamJournal`
10. `TestDashboardHTMLHasProcDrilldown`
11. `TestDashboardHTMLHasSpeedTest`

Plus the bind/loopback guards: `TestValidateBindAddr`,
`TestListenLocalLoopbackAndAutoPick`, `TestListenLocalUnreachableOffLoopback`,
`TestGuardedDialContextRefusesRebind`, `TestProcEndpointLoopbackGuard`, and
`TestReportHTMLHasNoExternalURLs` (the daily-report HTML obeys the same
no-external-URL rule).

## Storage model

Persistence lives under `~/Library/Application Support/netdebug/history`
(overridable via `--history-dir` or config `history_dir`):

- Live 1-second samples are folded into **per-minute** buckets (kept 48h) and
  **per-hour** buckets (kept 90d), persisted as JSONL under a single-instance
  flock with an atomic temp-file-then-rename rewrite (`history.go`).
- The **outage** and **roam** journals and the **speed** samples persist as
  their own append-only JSONL files alongside the buckets.
- `--report` reads these files directly (no flock) so it works even while a
  `--serve` holds the directory.

## macOS commands shelled (all sudo-free, argv — never a shell)

| Command | File(s) |
|---|---|
| `system_profiler` (SPAirPortDataType) | airspace.go, signal.go, wifi.go |
| `netstat` (`-ibn`, `-rn`) | bandwidth.go, probe.go, proxy.go |
| `ifconfig` | proxy.go, wifi.go, ipv6.go |
| `ipconfig` | probe.go, wifi.go, extras.go |
| `ping` / `/sbin/ping` / `ping6` | probe.go, extras.go, reach.go, ipv6.go |
| `route` | probe.go, ipv6.go |
| `networksetup` | wifi.go |
| `security` (keychain) | wifi.go |
| `wdutil` (optional, probe path only) | wifi.go |
| `scutil --proxy` | proxy.go |
| `pmset` / `ioreg` (power/lock gate) | reach_gate.go |
| `osascript` (notifications) | alerts.go |
| `ps` / `lsof` | procdetail.go |
| `nettop` | nettop.go |
| `plutil` / `launchctl` | launchd.go |

## File-by-file map

### Entry point & core probe

- **main.go** — CLI flag surface (the authoritative feature list), flag
  orphan/precedence warnings, and dispatch to each mode.
- **config.go** — the `Config`/`Network` model, `DefaultConfig`/`LoadConfig`, and
  knob clamping (so the tool runs with no config file at all).
- **probe.go** — the bottom-up layer checks (IP / route / gateway / internet /
  DNS / HTTP / captive portal) that feed the diagnosis.
- **diagnose.go** — pure diagnosis: turns the layer pass/fail booleans into a
  human verdict plus a concrete `fixHint`.
- **report.go** — terminal report rendering and the per-section summary
  formatters.
- **wifi.go** — Wi-Fi interface + link facts (`detectInterface`/`parseInterface`
  and the radio readers).

### Optional probe sections

- **signal.go** — Wi-Fi radio signal sampling (RSSI/SNR/rate/PHY), stats, and CSV
  output.
- **ipv6.go** — `--ipv6` health (address / route / ping6 / AAAA).
- **proxy.go** — proxy / VPN / default-route-hijack detection.
- **extras.go** — ARP gateway MAC, MTU / PMTU black-hole probe, DHCP lease dump.
- **correlate.go** — pure band / SSID / RSSI correlation core emitting
  drop/roam/band_change events and the categorical band-color vocabulary.

### Live dashboard & server

- **serve.go** — the `--serve` server: runs the sampler and serves the
  loopback-only dashboard, `/data.json` and `/healthz`; owns the bind-validation
  guards.
- **dashboard.go** — the entire self-contained dashboard page (inline CSS +
  scripts + inline-SVG charts) as a raw-string const.
- **bandwidth.go** — the sampling core: `netstat -ibn` byte counters into clean
  up/down deltas and a rolling ring (`--sample`).
- **reach.go** — the continuous reachability/latency probe loop (RTT / loss /
  jitter / DNS).
- **reach_gate.go** — the sudo-free power/presence gate that pauses probing on
  battery or a locked screen.
- **history.go** — bucket aggregation and the JSONL store (single-instance
  flock + atomic temp-file-then-rename rewrite); also hosts outage/roam/speed
  append.
- **store.go** — the `TimeSeriesStore` interface seam with a compile-time
  assertion that `*historyStore` satisfies it.

### Journals & analysis

- **outage.go** — the outage journal: pure detection/debounce/persistence fusing
  link-down and internet-unreachable into timestamped records with cause
  classification.
- **roam.go** — the band/channel-change ("roam") journal, a pure sibling of the
  outage journal.
- **airspace.go** — airspace / channel-congestion: neighbor parse, per-channel
  occupancy, clearest 80 MHz block (a passive `system_profiler` read, never an
  active scan).
- **daily.go** — the `--report` daily/weekly rollup composed purely from history
  buckets + outages + speed samples.

### Per-process

- **nettop.go** — per-process bandwidth: two `nettop` snapshots diffed into
  per-second rates, ranked upstream-first.
- **procdetail.go** — process drill-down for a clicked top-talker
  (command/path/parent/user + ESTABLISHED peers via `ps` + `lsof`); the only
  non-test file that names `/bin/ps` and `/usr/sbin/lsof`.

### Speed & throttle

- **speed.go** — the throughput test engine (download/upload workers, latency,
  Cloudflare server-timing RTT).
- **speedbutton.go** — the on-demand dashboard speed-test button (POST
  `/speedtest`), with a pure SSRF pre-screen on the target.
- **bufferbloat.go** — latency-under-load grading A–F and the GOOD/RISKY/BAD
  meeting-readiness badge (reuses speed.go's workers).
- **throttle.go** — the opt-in, 429-safe, bounded, spaced download sampler that
  watches for peak-hour throttling.

### Alerts & lifecycle

- **alerts.go** — opt-in native macOS notifications with a pure anti-spam
  debounce, plus the one-line `--status` formatter.
- **launchd.go** — the always-on per-user LaunchAgent install/uninstall/status
  (never a root daemon); pure plist render + arg composition + validation around
  `launchctl`.
