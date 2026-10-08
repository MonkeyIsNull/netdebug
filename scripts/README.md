# scripts/

Companion scripts for netdebug.

## wifilog.sh — post-hoc macOS Wi-Fi log forensics

`wifilog.sh` pulls a bounded window of macOS's **unified Wi-Fi logs** and turns
them into something readable. It is a Bash script (re-execs under Homebrew Bash 5
if launched on macOS's stock 3.2) and uses only `log show`, `system_profiler`
and `ifconfig` — no sudo.

For each run it writes two files under `./wifi-logs/`:

- `wifi-<timestamp>.log` — the raw unified-log capture (hand this to an AI, grep it, archive it).
- `wifi-<timestamp>.summary` — a human-readable report: current association, the
  channel/security/signal, how many disconnect/reconnect events happened in the
  window, the timestamps of each, and (when the log exposes it) *why* the link
  left — the signal level at the last departure versus the roam threshold.

### Usage

```sh
./scripts/wifilog.sh            # the last hour (now-60m .. now)          [default]
./scripts/wifilog.sh -1         # the hour BEFORE last (now-2h .. now-1h)
./scripts/wifilog.sh -N         # the 1-hour block N hours ago
./scripts/wifilog.sh 30         # the last 30 minutes (bare number = minutes, max 60)
./scripts/wifilog.sh --since 15:05     # everything from 3:05pm today until now
./scripts/wifilog.sh --since 3:05pm    # same, 12-hour clock also accepted
```

The block/minute modes never pull more than one hour (the unified log is huge);
step backwards with `-1`, `-2`, … to walk further. `--since` is the exception —
use it to answer "has it dropped since I changed a setting at TIME?".

### How it complements netdebug

The two tools look at the same problem from opposite ends of time:

| | `wifilog.sh` | `netdebug` |
|---|---|---|
| When | **After** something happened | **While** it is happening |
| Source | macOS unified log (historical) | Live probes + continuous sampling |
| Shape | One-shot forensic snapshot | Long-running dashboard / point-in-time probe |
| Answers | "What did my Wi-Fi *do* in that window — and why did it leave?" | "What is the stack doing *right now*, and how is it trending?" |

Use `wifilog.sh` to reconstruct a drop you already noticed; use `netdebug`
(especially `--serve`) to catch the next one as it unfolds and keep a rolling
history. The outputs pair well: a `wifilog.sh` summary of *when* it dropped next
to a netdebug outage/roam journal of *what the stack saw* at that moment.
