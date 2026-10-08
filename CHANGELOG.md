# Changelog

All notable changes to netdebug. The project grew through a series of numbered
development phases; this is a distilled, readable history of that arc. It
predates the first public tag, so everything lives under the `0.x` line.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased] — public release prep

- Module path set to `github.com/MonkeyIsNull/netdebug`; `go install`-able.
- README rewritten dashboard-first to match the real feature set; added
  `docs/ARCHITECTURE.md`, `examples/`, `LICENSE` (MIT), and this changelog.
- Companion `wifilog.sh` moved to `scripts/`.

## 0.x — development history

The capabilities below were built incrementally and are all present today. They
are grouped by the arc of the project rather than dated releases.

### Foundation — point-in-time probe & compare

- Bottom-up, layer-by-layer network probe (DHCP → route → gateway → internet →
  DNS → HTTP/captive-portal) with a diagnosis that names the first failing
  layer and a concrete fix hint.
- `--compare` to diff the two most recent reports side by side; JSON reports
  written to `./net-tests/`.
- Optional probe sections: `--signal` (link-quality sampling), `--ipv6`,
  `--proxy` (proxy/VPN/route-hijack), `--extras` (ARP MAC, MTU/PMTU, DHCP
  lease), and `--all`.
- `--live` auto-switch between configured networks (kept, but unreliable on
  recent macOS — prefer manual switch + `--compare`).

### Live dashboard

- **Bandwidth sampling** — `--sample` prints live up/down rates from
  `netstat -ibn` byte counters.
- **`--serve` dashboard** — a loopback-only (`127.0.0.1`), fully self-contained
  web dashboard (inline SVG charts, zero external URLs) with LIVE throughput.
- **History persistence** — 1s samples folded into per-minute (48h) / per-hour
  (90d) buckets, append-only JSONL, surviving restarts and crashes.
- **Radio correlation** — band / SSID / RSSI events; charts colored by Wi-Fi
  band so a slowdown next to a band change is obvious.
- **Dashboard SSID label** — `--ssid` / config `ssid_label` to name a
  macOS-redacted network on the page.

### Always-on

- **LaunchAgent** — `--install-launchd` / `--uninstall-launchd` /
  `--launchd-status` for a per-user (never root) agent that runs `--serve` at
  login; refuses ephemeral/translocated binary paths.

### Reachability & outages

- **Reachability probe** — rolling gateway + internet RTT / loss / jitter / DNS,
  power-gated by default (pauses on battery / locked screen;
  `--no-power-gating` to override), with a per-target drill-down table.
- **Outage journal** — debounced, timestamped outage records with best-effort
  cause classification, persisted as JSONL.

### Measurement

- **Speed test** — `--speed` download/upload/latency (parallel streams) and an
  on-demand dashboard **speed-test button** (POST `/speedtest`, SSRF-screened).
- **Bufferbloat** — `--bufferbloat` latency-under-load grade A–F plus a
  GOOD/RISKY/BAD meeting-readiness badge.

### Airspace & per-process

- **Airspace scan** — `--airspace` neighbor list, per-channel occupancy and the
  clearest 80 MHz block (passive `system_profiler`, never an active scan, never
  sudo).
- **Top talkers** — `--top-talkers` and a dashboard panel: per-process up/down
  rates via sudo-free `nettop`, ranked upstream-first.
- **Process drill-down** — `--proc <pid>` and a clickable dashboard row:
  command / path / parent / user and current ESTABLISHED remote endpoints via
  `ps` + `lsof` (best-effort, bounded reverse-DNS).

### Alerts, reports & throttle

- **Alerts** — `--alerts` opt-in native macOS notifications on
  drop / band-change / meeting-BAD / sustained-upload, with anti-spam debounce.
- **Status line** — `--status` one-line health summary (plain or SwiftBar),
  reading a running `--serve` when present.
- **Daily/weekly report** — `--report` (`--report-span day|week`) rollup from
  persisted history: totals, band time, drops, worst-sample latency, speed by
  hour.
- **Throttle watchdog** — `--throttle-watch` opt-in, 429-safe, bounded, spaced
  download sampler (≤ ~2/hr) to catch peak-hour ISP throttling.

### Latest — roam journal

- **Band / channel change (roam) journal** — timestamped band-steer /
  channel-change records, the persistent sibling of the outage journal, shown
  as its own dashboard panel.
