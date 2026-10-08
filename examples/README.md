# Examples

Common netdebug invocations, plus sanitized sample output. All SSIDs, IPs and
MAC/BSSID values in the sample files are placeholders (`MyNetwork`, `10.0.0.x`,
`redacted`).

## Live dashboard

```sh
netdebug --serve                       # dashboard on http://127.0.0.1:8099/
netdebug --serve --port 9000           # choose a port (auto-advances if busy)
netdebug --serve --ssid "MyNetwork"    # label a macOS-redacted SSID on the page
netdebug --serve --no-power-gating     # keep probing on battery / locked screen
netdebug --serve --alerts              # opt-in native notifications
netdebug --serve --throttle-watch      # opt-in peak-hour throttle sampler
```

## Terminal one-shots

```sh
netdebug --sample                      # live up/down rates, Ctrl-C to stop
netdebug --sample --interval 2s        # slower cadence (clamped to >= 250ms)
netdebug --speed                       # download/upload/latency speed test
netdebug --bufferbloat                 # latency-under-load grade A-F
netdebug --airspace                    # neighbors + channel occupancy
netdebug --top-talkers                 # per-process bandwidth, upstream-first
netdebug --proc 12345                  # identity + connections for one PID
netdebug --status                      # one-line health (reads a running --serve)
netdebug --status --port 9000          # ... reading a serve on a non-default port
netdebug --report                      # daily rollup from persisted history
netdebug --report --report-span week   # trailing-7-day rollup
```

## Point-in-time probe & compare

```sh
netdebug                               # probe the current network -> ./net-tests/
netdebug --all                         # + signal, ipv6, proxy, extras sections
netdebug --label "MyNetwork"           # name the run (macOS often hides the SSID)
#   switch networks by hand in the Wi-Fi menu, then:
netdebug                               # probe the other one
netdebug --compare                     # diff the two most recent reports
```

See [`nettest-sample.json`](nettest-sample.json) for a sanitized probe report.

## Launchd (always-on at login)

```sh
netdebug --install-launchd                          # install + load now and at login
netdebug --install-launchd --port 9000              # bake in a port
netdebug --install-launchd --launchd-binary /usr/local/bin/netdebug  # stable path
netdebug --launchd-status                           # loaded / running / crash-looping?
netdebug --uninstall-launchd                        # unload + remove
```

Use the **same** `--launchd-label` (default `com.cobenian.netdebug`) across all
three commands if you install under a custom label.

## History-dir override

```sh
netdebug --serve  --history-dir /abs/path/to/history   # persist elsewhere
netdebug --report --history-dir /abs/path/to/history   # read from the same place
```

Default: `~/Library/Application Support/netdebug/history`.

## Config file

netdebug runs with **no config file at all**. To tune it, copy the example and
pass `--config`:

```sh
cp config.example.json config.json
# edit config.json, then:
netdebug --config config.json
```

Fields of note (all optional; see [`../config.example.json`](../config.example.json)):

- `ssid_label` — the name shown for the Wi-Fi network on the dashboard (a
  launchd-run dashboard reads its label from here; the `--ssid` flag is
  foreground-only).
- `history_dir`, `minute_ttl_hours`, `hour_ttl_days` — history location/retention.
- `power_gating` — set `false` for a launchd-run dashboard that should keep
  probing on battery / locked screen.
- `public_ips`, `dns_names` — reachability probe targets.
- `reach_interval_seconds`, `link_interval_seconds` — probe cadences.
- `proc_detail_resolve_dns` — kill-switch for reverse-DNS on process endpoints.

## Companion log forensics

```sh
./scripts/wifilog.sh                   # pull + summarize the last hour of Wi-Fi logs
./scripts/wifilog.sh --since 3:05pm    # since a clock time today
```

See [`wifilog-sample.summary`](wifilog-sample.summary) for sanitized output and
[`../scripts/README.md`](../scripts/README.md) for how it complements netdebug.
