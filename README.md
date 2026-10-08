# netdebug

Continuous, no-sudo, self-contained Wi-Fi and network diagnostics for macOS
(Apple Silicon). Point it at your Mac and it serves a live dashboard on
`127.0.0.1` that watches throughput, link quality, reachability, airspace, and
per-process talkers — and keeps a persistent history so you can see *when* and
*why* things went wrong.

**Why it exists.** Most network troubleshooting is a one-shot snapshot taken
*after* you notice a problem. netdebug runs continuously and remembers, so the
intermittent slowdown, the 2am drop, the band-steer that tanked your call all
leave a trail. And it does this the boring, safe way:

- **No sudo, ever.** It only shells sudo-free macOS tools (`system_profiler`,
  `ifconfig`, `netstat`, `nettop`, `ps`, `lsof`, `ping`, `route`, `scutil`). It
  never prompts for a password and never uses `wdutil`/`sudo` on the dashboard
  path — which is exactly what lets the always-on launchd agent run with no TTY.
- **Loopback only.** The server binds `127.0.0.1` *exclusively*, validated
  before and after binding. There is no flag to expose it on a LAN.
- **Self-contained.** One static Go binary, zero third-party dependencies
  (stdlib only). The dashboard is a single document with inline SVG charts — no
  CDN, no web fonts, no external requests of any kind.
- **Read-only / observational.** It measures; it does not reconfigure your
  network.

macOS / Apple Silicon only.

## Install

```sh
go install github.com/MonkeyIsNull/netdebug@latest
```

or build from source (no modules to download — it is stdlib-only):

```sh
git clone https://github.com/MonkeyIsNull/netdebug
cd netdebug
go build -o netdebug .
```

Requires Go 1.26+.

## Quick start

```sh
./netdebug --serve
```

Then open **http://127.0.0.1:8099/**. If 8099 is busy the server auto-advances
to the next free port and prints the real URL on startup. Ctrl-C to stop.

![netdebug dashboard](docs/screenshots/dashboard.png)

> **Screenshot slot.** `docs/screenshots/dashboard.png` is the intended
> dashboard image. If you are reading this before it has been captured, the
> image above is a placeholder — see [docs/screenshots/](docs/screenshots/) for
> how to grab one from a running `--serve` (ideally with `--sample`/synthetic
> data so no real SSIDs leak).

## The dashboard

`--serve` is the centerpiece. It samples continuously and renders one live,
self-contained page with these panels:

- **LIVE** — real-time up/down throughput (from `netstat -ibn` byte counters),
  charted and colored by Wi-Fi band so a slowdown that coincides with a band
  change is obvious at a glance.
- **REACHABILITY** — rolling gateway + internet RTT, loss, jitter and DNS health
  with a per-target drill-down table. Power-gated by default (pauses on battery
  / locked screen to save power; override with `--no-power-gating`).
- **HISTORY** — the persistent per-minute / per-hour history, so the chart
  survives restarts and crashes.
- **AIRSPACE** — neighboring Wi-Fi networks, per-channel occupancy and the
  clearest 80 MHz block (a *passive* `system_profiler` read — never an active
  scan, never sudo).
- **OUTAGE JOURNAL** — timestamped records of real outages (debounced so a
  single blip does not open one), with a best-effort cause classification.
- **BAND / CHANNEL CHANGE (roam) JOURNAL** — timestamped band-steer / channel-
  change events, the sibling of the outage journal.
- **TOP TALKERS** — per-process up/down rates (sudo-free `nettop`), ranked
  upstream-first; click a row to **drill into a process** (command, path,
  parent, user and its current ESTABLISHED remote endpoints via `ps` + `lsof`).
- **SPEED TEST** — an on-demand download/upload/latency test button
  (speed.cloudflare.com), so you can measure without leaving the page.

Opt-in extras on the serve path:

- **`--alerts`** — native macOS notifications on drop / band-change / meeting-BAD
  / sustained-upload (debounced; default off).
- **`--throttle-watch`** — a 429-safe, bounded, spaced download sampler
  (≤ ~2/hr) to catch peak-hour ISP throttling (default off).

## Point-in-time probe & compare

Separate from the live dashboard, netdebug can run a one-shot layer-by-layer
probe of whatever network you are on right now — walked bottom-up so the first
failing layer *is* the diagnosis (DHCP → route → gateway → internet → DNS →
HTTP/captive-portal). This path shells `ipconfig`, `route`, `netstat`, `ping`
and `system_profiler` (and, if available, `wdutil` for fuller radio detail).

```sh
./netdebug                 # probe the current network; writes JSON to ./net-tests/
#   switch networks by hand in the macOS Wi-Fi menu
./netdebug                 # probe the other one
./netdebug --compare       # diff the two most recent reports, side by side
```

The diagnosis names the first failing layer, e.g. *"NO UPSTREAM: gateway
reachable but the internet is not — this SSID's uplink is down."* Add `--all`
(or `--signal` / `--ipv6` / `--proxy` / `--extras`) for link-quality sampling,
IPv6 health, proxy/VPN/route-hijack detection and ARP/MTU/DHCP-lease probes.

> `--live` (auto-switch between configured networks) exists but is unreliable on
> recent macOS — `networksetup -setairportnetwork` often fails with CoreWLAN
> `Error -3900`. Prefer the manual-switch + `--compare` workflow above.

## One-shot modes

Each of these runs once and exits (no server):

```sh
./netdebug --sample          # print live up/down rates to the terminal
./netdebug --speed           # download/upload/latency speed test
./netdebug --bufferbloat     # latency-under-load (bufferbloat) grade A–F
./netdebug --airspace        # neighbor list + channel occupancy + clearest block
./netdebug --top-talkers     # per-process bandwidth, upstream-first
./netdebug --proc <pid>      # identity + established connections for one PID
./netdebug --status          # one-line health summary (reads a running --serve)
./netdebug --report          # daily/weekly rollup from persisted history
```

See [examples/](examples/) for a fuller tour with sample output.

## Always-on at login (per-user LaunchAgent)

For 24/7 logging, install a per-user **LaunchAgent** that runs `--serve` at every
login. This is a *user* agent, **never** a system daemon — no root, your login
session only, writes only to your own directories.

```sh
./netdebug --install-launchd                 # install + load now, and at every login
./netdebug --install-launchd --port 9000     # bake in a specific port
./netdebug --launchd-status                  # is it loaded / running / crash-looping?
./netdebug --uninstall-launchd               # unload + remove it
```

It writes `~/Library/LaunchAgents/com.cobenian.netdebug.plist` and `launchctl
load -w`s it; logs go to `~/Library/Logs/netdebug/netdebug.log` (no rotation —
truncate it yourself). Use the same `--launchd-label` for all three commands,
and point it at a stable binary path (e.g. `/usr/local/bin/netdebug`) —
`--install-launchd` refuses an obviously-ephemeral `go run` / translocated path.

## History: where it lives and how long it is kept

Samples fold into persistent buckets so the history chart survives restarts:

- **Location:** `~/Library/Application Support/netdebug/history`
- **Retention:** per-minute buckets kept **48h**; per-hour buckets kept **90d**.
- **Overrides:** `--history-dir /abs/path` on the command line, or `history_dir`
  / `minute_ttl_hours` / `hour_ttl_days` in the config file.

The outage and roam journals persist alongside the buckets as append-only JSONL.

## Safety guarantees

- **Loopback only.** The server binds `127.0.0.1` *exclusively*. The bind is
  validated both before and after binding (the real socket address is
  re-checked), so it can never land on `0.0.0.0`, `::`, or a LAN IP. There is no
  flag to bind elsewhere — this is by design, not a default. Enforced by guard
  tests (see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)).
- **No sudo, ever.** The dashboard reads the radio via `system_profiler` plus
  `ifconfig`; it never shells `wdutil`/`sudo` and never prompts for a password.
  Per-process **top talkers** read `nettop` the same way — it shows the processes
  visible to your user, so some system processes may be unattributable; netdebug
  degrades to an honest "unavailable" rather than escalating.
- **Self-contained page.** The dashboard HTML has zero external URLs (no CDN, no
  web font, no `<link>`, no `url()`), charts are inline SVG, and all free-form
  strings are written via `textContent`, never `innerHTML`.

## Configuration

netdebug runs with **no config file at all** (sane defaults, read-only, current
network). To tune it, copy `config.example.json` to `config.json` and pass
`--config config.json`. See [examples/](examples/) and
[config.example.json](config.example.json) for the full field set.

## Scope & honest caveats

- **macOS / Apple Silicon only.** It relies on macOS command output.
- **Read-only / observational.** It measures and reports; it does not change your
  network configuration.
- **macOS redacts some radio detail.** Without Location permission macOS hides
  the SSID and BSSID; netdebug shows link *state* honestly and lets you label the
  network for the dashboard/report (`--ssid`, or `ssid_label` in config).
- **Top-talkers visibility is per-user.** Some system processes may need elevated
  rights to attribute — netdebug will not escalate to get them.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — design, invariants, guard tests,
  and a file-by-file map for contributors.
- [CHANGELOG.md](CHANGELOG.md) — the development history by phase.
- [examples/](examples/) — common invocations and sanitized sample output.
- [scripts/wifilog.sh](scripts/wifilog.sh) — companion script for post-hoc
  macOS Wi-Fi *log* forensics (see [scripts/README.md](scripts/README.md)).

## Tests

```sh
go build ./...
go vet ./...
go test ./...
```

Unit tests are table-driven over every macOS-output parser and all the pure
decision logic; they run offline with no network access. A set of guard tests
enforces the loopback-only and self-contained-HTML invariants.

## License

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 Adam Guyot.
