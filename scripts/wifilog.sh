#!/usr/bin/env bash
#
# wifilog.sh — pull ONE hour (at most) of macOS Wi-Fi logs and analyze them.
#
# Usage:
#   ./wifilog.sh            # the last hour (now-60m .. now)          [default]
#   ./wifilog.sh -1         # the hour BEFORE last (now-2h .. now-1h)
#   ./wifilog.sh -2         # two hours ago         (now-3h .. now-2h)
#   ./wifilog.sh -N         # the 1-hour block N hours ago
#   ./wifilog.sh 30         # the last 30 minutes (bare number = minutes, max 60)
#   ./wifilog.sh --since 15:05     # everything from 3:05pm today until now
#   ./wifilog.sh --since 3:05pm    # same, 12-hour clock also accepted
#
# The block/minute modes NEVER pull more than one hour (the unified log is huge);
# to walk further back, step: ./wifilog.sh -1, then -2, ... --since is the one
# exception — use it to check "has it dropped since I changed a setting at TIME?"
#
# Produces two files in ./wifi-logs/ :
#   wifi-<timestamp>.log      # raw unified-log capture (hand this to an AI)
#   wifi-<timestamp>.summary  # human-readable report of what happened
#
# Re-exec under Homebrew Bash 5 if we were launched on macOS's stock 3.2.
if [ -z "${WIFILOG_REEXECED:-}" ] && [ "${BASH_VERSINFO[0]:-0}" -lt 5 ]; then
  for _b in /opt/homebrew/bin/bash /usr/local/bin/bash; do
    if [ -x "$_b" ]; then export WIFILOG_REEXECED=1; exec "$_b" "$0" "$@"; fi
  done
fi
set -uo pipefail

usage() { sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'; }
die()   { printf 'wifilog: %s\n\n' "$*" >&2; usage >&2; exit 1; }

# ---------------------------------------------------------------------------
# Config — parse the time window (one hour, max — except --since)
# ---------------------------------------------------------------------------
HOURS_AGO=0        # >0 => a whole-hour block that many hours back
MINUTES=60         # else: the last N minutes ending "now" (capped at 60)
SINCE=""           # a clock time ("15:05" / "3:05pm") => from then until now

case "${1:-}" in
  '' )                        ;;                       # default: last hour
  -h|--help)  usage; exit 0   ;;
  --since)    SINCE="${2:-}"; [ -z "$SINCE" ] && die "--since needs a time, e.g. --since 15:05" ;;
  --since=*)  SINCE="${1#--since=}" ;;
  -[0-9]|-[0-9][0-9])  HOURS_AGO=${1#-} ;;             # -N : hour block N hours ago
  [1-9]|[1-5][0-9]|60) MINUTES=$1       ;;             # bare minutes 1..60
  *) die "unrecognized argument: $1" ;;
esac
(( MINUTES < 1 || MINUTES > 60 )) && MINUTES=60

# Compute an explicit [START,END] window using BSD date (macOS).
if [ -n "$SINCE" ]; then
  # Accept "15:05" (24h) or "3:05pm"/"3:05 PM" (12h). Anchor to today's date.
  # Pick the format by detecting am/pm: BSD date leniently ignores a trailing
  # "pm" under %H:%M, so "3:05pm" would wrongly parse as 03:05 on fallthrough.
  _s=$(printf '%s' "$SINCE" | tr 'A-Z' 'a-z' | tr -d ' ')
  _today=$(date '+%Y-%m-%d')
  case "$_s" in
    *am|*pm) _fmt='%Y-%m-%d %I:%M%p' ;;
    *)       _fmt='%Y-%m-%d %H:%M'   ;;
  esac
  _epoch=$(date -j -f "$_fmt" "${_today} ${_s}" '+%s' 2>/dev/null) \
    || die "could not parse --since time: '$SINCE' (try 15:05 or 3:05pm)"
  _now=$(date '+%s')
  (( _epoch > _now )) && _epoch=$(( _epoch - 86400 ))   # future clock time => yesterday
  START_TS=$(date -r "$_epoch" '+%Y-%m-%d %H:%M:%S')
  END_TS=$(date '+%Y-%m-%d %H:%M:%S')
  _span=$(( (_now - _epoch + 30) / 60 ))
  WINDOW_LABEL="since ${START_TS} (${_span} min)"
  (( _span > 90 )) && printf 'wifilog: note — that is a %s-minute span; the capture may be large.\n' "$_span" >&2
elif (( HOURS_AGO > 0 )); then
  END_TS=$(date -v-${HOURS_AGO}H '+%Y-%m-%d %H:%M:%S')
  START_TS=$(date -v-$((HOURS_AGO + 1))H '+%Y-%m-%d %H:%M:%S')
  WINDOW_LABEL="1-hour block, ${HOURS_AGO}h ago"
else
  END_TS=$(date '+%Y-%m-%d %H:%M:%S')
  START_TS=$(date -v-${MINUTES}M '+%Y-%m-%d %H:%M:%S')
  WINDOW_LABEL="last ${MINUTES} min"
fi

OUTDIR="./wifi-logs"
STAMP="$(date +%Y%m%d-%H%M%S)"
RAW="${OUTDIR}/wifi-${STAMP}.log"
SUMMARY="${OUTDIR}/wifi-${STAMP}.summary"

mkdir -p "$OUTDIR"

# Colors (only if stdout is a terminal)
if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; R=$'\033[0m'
  RED=$'\033[31m'; GRN=$'\033[32m'; YLW=$'\033[33m'; CYN=$'\033[36m'
else
  B=''; DIM=''; R=''; RED=''; GRN=''; YLW=''; CYN=''
fi

say() { printf '%s\n' "$*"; }

# ---------------------------------------------------------------------------
# 1. Figure out the Wi-Fi interface (usually en0)
# ---------------------------------------------------------------------------
WIFI_IF="$(networksetup -listallhardwareports 2>/dev/null \
  | awk '/Wi-Fi|AirPort/{getline; print $NF; exit}')"
WIFI_IF="${WIFI_IF:-en0}"

say "${B}Wi-Fi interface:${R} ${WIFI_IF}"
say "${B}Window:${R} ${WINDOW_LABEL}  (${START_TS} → ${END_TS})"
say "${B}Raw log:${R} ${RAW}"
say ""

# ---------------------------------------------------------------------------
# 2. Capture the raw log
#    Broad predicate: the Wi-Fi subsystem plus the daemons that drive
#    association / roaming / auto-join, plus anything mentioning the interface.
#    Then EXCLUDE the high-volume XPC polling chatter — 'BEGIN/END REQ [GET ...]'
#    lines are other processes querying airportd and carry no diagnostic value;
#    on a busy hour they are ~70% of the log (655k of 943k lines in testing).
#    Parenthesize the OR group so the trailing AND NOT applies to all of it.
# ---------------------------------------------------------------------------
PREDICATE='
  (
       subsystem == "com.apple.wifi"
    OR process == "wifid"
    OR process == "airportd"
    OR process == "WiFiAgent"
    OR process == "symptomsd"
    OR process == "sharingd"
    OR eventMessage CONTAINS[c] "'"${WIFI_IF}"'"
    OR eventMessage CONTAINS[c] "BSSID"
    OR eventMessage CONTAINS[c] "AutoJoin"
    OR eventMessage CONTAINS[c] "linkDown"
    OR eventMessage CONTAINS[c] "linkUp"
  )
  AND NOT eventMessage CONTAINS "REQ ["
'

say "${DIM}Collecting logs (this can take a few seconds)...${R}"
log show \
  --start "$START_TS" --end "$END_TS" \
  --info --debug \
  --style syslog \
  --predicate "$PREDICATE" \
  > "$RAW" 2>/dev/null

LINES="$(wc -l < "$RAW" | tr -d ' ')"
say "${GRN}Captured ${LINES} log lines.${R}"
say ""

# ---------------------------------------------------------------------------
# 3. Snapshot current Wi-Fi state (nice context for whoever analyzes it)
# ---------------------------------------------------------------------------
{
  echo "==================== WIFI SNAPSHOT ===================="
  echo "Generated: $(date)"
  echo "Interface: ${WIFI_IF}"
  echo "Window:    ${WINDOW_LABEL}  (${START_TS} -> ${END_TS})"
  echo
  echo "---- system_profiler (current association) ----"
  system_profiler SPAirPortDataType 2>/dev/null \
    | sed -n '/Current Network Information/,/Other Local Wi-Fi Networks/p'
  echo
  echo "---- ifconfig ${WIFI_IF} ----"
  ifconfig "$WIFI_IF" 2>/dev/null | grep -E 'inet |status|ether'
} > "$SUMMARY"

# ---------------------------------------------------------------------------
# 4. Analyze: pull out the key events and build a timeline
# ---------------------------------------------------------------------------
# Robust count: grep -c always prints a number; capture it, default 0.
# (No pipe here, so `set -o pipefail` can't turn a zero-count into a stray line.)
count() { local n; n=$(grep -icE "$1" "$RAW" 2>/dev/null) || true; printf '%s' "${n:-0}"; }

# --- Capture-window bounds (first/last real syslog-style timestamp) ---------
TS_RE='2026-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}'
WIN_START=$(grep -oE "^$TS_RE" "$RAW" | head -1)
WIN_END=$(grep -oE "^$TS_RE" "$RAW"   | tail -1)

# --- REAL reconnect activity (all 0 during a healthy, connected window) -----
# These are genuine state-machine events, not telemetry noise.
JOINCHG=$(count 'joinStateDidChange')
ASSOC_START=$(count 'SUSPEND AWDL.*reason=Assoc')
ROAM_EVAL=$(count 'Roam:.*matching scan|roamingProfileType|Found two matching')
DRV_LINK=$(count 'driver available \(reason=')     # actual link return (not per-RSSI callbacks)
FAULTS=$(count 'FaultReason[A-Za-z]+=[1-9]|DhcpFailure=[1-9]|beacon ?lost')

# --- Telemetry noise (context only) -----------------------------------------
RSSI_UPD=$(count 'APPLE80211_M_RSSI_CHANGED')
LQM_UPD=$(count 'APPLE80211_M_WEIGHT_AVG_LQM_UPDATE')

# --- Distinct assoc/disassoc timestamps the system recorded -----------------
# The PRIVATE-MAC/AUTO-JOIN records embed the LAST assoc/disassoc time as a
# field, so the *distinct* values are the real connect/disconnect moments.
DISASSOC_TS=$(grep -oE "disassoc=$TS_RE" "$RAW" | sed 's/disassoc=//' | sort -u)
ASSOC_TS=$(grep -oE "assoc=$TS_RE \(auto\)" "$RAW" | sed -E 's/assoc=//; s/ \(auto\)//' | sort -u)

# The stored records include last-disassoc times for OTHER saved networks/days.
# For the "recent" narrative and cadence, keep only timestamps from the capture
# day so stale cross-network records don't masquerade as a cadence.
TODAY=${WIN_START%% *}
DIS_TODAY=$(printf '%s\n'   "$DISASSOC_TS" | grep "^${TODAY}" || true)
ASSOC_TODAY=$(printf '%s\n' "$ASSOC_TS"    | grep "^${TODAY}" || true)

# Which disconnects fell INSIDE the capture window? (ISO timestamps sort as text)
DIS_IN=$(printf '%s\n' "$DIS_TODAY" | awk -v a="$WIN_START" -v b="$WIN_END" 'NF && $0>=a && $0<=b')
N_DIS_IN=$(printf '%s\n' "$DIS_IN" | grep -c . || true)

LAST_DIS=$(printf '%s\n'  "$DIS_TODAY"   | grep . | tail -1)
LAST_ASSOC=$(printf '%s\n' "$ASSOC_TODAY" | grep . | tail -1)

# --- Connection identity from the snapshot section 3 already wrote -----------
CH=$(grep -m1 'Channel:'        "$SUMMARY" | sed 's/^[[:space:]]*//; s/^Channel:[[:space:]]*//')
SEC=$(grep -m1 'Security:'      "$SUMMARY" | sed 's/^[[:space:]]*//; s/^Security:[[:space:]]*//')
SIG=$(grep -m1 'Signal / Noise' "$SUMMARY" | sed 's/^[[:space:]]*//; s/^Signal \/ Noise:[[:space:]]*//')
STAT=$(grep -m1 'status:'       "$SUMMARY" | sed 's/^[[:space:]]*//')

# --- Derive band from the channel string (e.g. "149 (5GHz, 80MHz)") ----------
# system_profiler labels the band; fall back to the channel-number rule
# (1-14 = 2.4 GHz, 36-165 = 5 GHz) if the label is absent.
CHNUM=$(printf '%s' "$CH" | grep -oE '^[0-9]+')
WIDTH=$(printf '%s' "$CH" | grep -oE '[0-9]+MHz' | head -1)
BAND=""
case "$CH" in
  *6GHz*|*"6 GHz"*)              BAND="6 GHz"   ;;
  *5GHz*|*"5 GHz"*)              BAND="5 GHz"   ;;
  *2.4GHz*|*"2.4 GHz"*|*2GHz*)  BAND="2.4 GHz" ;;
esac
if [ -z "$BAND" ] && [ -n "$CHNUM" ]; then
  if   (( CHNUM <= 14 ));  then BAND="2.4 GHz"
  elif (( CHNUM <= 165 )); then BAND="5 GHz"
  fi
fi

# --- Signal at the moment of the last leave vs the roam threshold ------------
LEAVE=$(grep -oE 'RssiAtLeave=-?[0-9]{2,3}'         "$RAW" | head -1 | grep -oE '\-?[0-9]{2,3}')
TRIGGER=$(grep -oE 'RoamConfigTriggerRssi=-?[0-9]{2,3}' "$RAW" | head -1 | grep -oE '\-?[0-9]{2,3}')

{
  echo
  echo "############################################################"
  echo "#   WHAT YOUR WI-FI ACTUALLY DID"
  echo "############################################################"
  echo
  echo "Window captured : ${WIN_START:-?}  ->  ${WIN_END:-?}   (interface ${WIFI_IF})"
  if [ -n "$BAND" ]; then
    echo "Band            : ${BAND}${CHNUM:+  —  channel ${CHNUM}}${WIDTH:+ (${WIDTH})}"
  elif [ -n "$CH" ]; then
    echo "Channel         : ${CH}"
  fi
  [ -n "$SEC" ]  && echo "Security        : ${SEC}"
  [ -n "$SIG" ]  && echo "Signal at end   : ${SIG}"
  [ -n "$STAT" ] && echo "Interface       : ${STAT}"
  echo

  # ---- Headline: did a disconnect happen in this window? -------------------
  if [ "$N_DIS_IN" -eq 0 ] && [ "$ASSOC_START" -eq 0 ] && [ "$JOINCHG" -eq 0 ]; then
    echo "RESULT: No disconnect happened during this window — the link was healthy."
    echo
    echo "  Your Mac stayed associated the entire time. The radio logged only routine"
    echo "  telemetry: ${RSSI_UPD} signal-strength readings and ${LQM_UPD} link-quality"
    echo "  updates. That is exactly what a stable, connected Wi-Fi link looks like."
  else
    echo "RESULT: ${N_DIS_IN} disconnect/reconnect event(s) happened during this window."
    echo
    echo "  Sequence: healthy link -> disassociation -> driver re-available ->"
    echo "  Auto-Join/roam evaluation (${ROAM_EVAL}) -> association restart"
    echo "  (${ASSOC_START}) -> re-associated. Join-state changes: ${JOINCHG}."
    if [ -n "$DIS_IN" ]; then
      echo "  Disconnect moment(s) inside the window:"
      printf '%s\n' "$DIS_IN" | sed 's/^/    - /'
    fi
  fi
  echo

  # ---- Most recent event on record, even if just before the window ---------
  if [ -n "$LAST_DIS" ]; then
    echo "Most recent disconnect on record : ${LAST_DIS}"
    [ -n "$LAST_ASSOC" ] && echo "Most recent auto-reconnect       : ${LAST_ASSOC}"
    if [ -n "$WIN_START" ] && [[ "$LAST_DIS" < "$WIN_START" ]]; then
      echo "  NOTE: that disconnect happened BEFORE this capture began — you just"
      echo "  missed it. Re-run with a longer window (e.g. ./wifilog.sh 30) to capture"
      echo "  the drop itself, or leave a capture running when the problem recurs."
    fi
    echo
  fi

  # ---- Why did it leave? Signal vs roam threshold --------------------------
  if [ -n "$LEAVE" ] && [ -n "$TRIGGER" ]; then
    echo "WHY IT LEFT (signal vs roam threshold)"
    echo "  Signal when it last left : ${LEAVE} dBm"
    echo "  Roam trigger threshold   : ${TRIGGER} dBm"
    if [ "$LEAVE" -gt "$TRIGGER" ]; then
      echo "  -> It left with a STRONG signal (above the ${TRIGGER} dBm trigger), so this"
      echo "     was NOT a weak-signal roam. Something on the router/AP side dropped it"
      echo "     — a periodic deauth, a client-inactivity timeout, or a WPA key rekey."
      echo "     If the gaps below show a regular cadence, that points to an AP timer."
    else
      echo "  -> It left with a weak signal (at/below the ${TRIGGER} dBm trigger), which"
      echo "     is a normal signal-driven roam (you moved, or the AP got weak)."
    fi
    echo
  fi

  # ---- Cadence: gaps between successive disconnects TODAY (BSD date, no gawk)
  # Use the capture-day set only; the full list mixes in stored last-disassoc
  # times for other saved networks/days, which are not a cadence.
  if [ "$(printf '%s\n' "$DIS_TODAY" | grep -c . || true)" -gt 1 ]; then
    echo "DISCONNECT CADENCE today (gap between successive disconnects — watch for a pattern)"
    prev=""
    while IFS= read -r ts; do
      [ -z "$ts" ] && continue
      e=$(date -j -f "%Y-%m-%d %H:%M:%S" "$ts" +%s 2>/dev/null || true)
      if [ -n "$prev" ] && [ -n "$e" ]; then
        d=$((e - prev))
        printf '  %s   (+%d min %d sec since previous)\n' "$ts" $((d/60)) $((d%60))
      else
        printf '  %s   (first today)\n' "$ts"
      fi
      prev=$e
    done <<< "$DIS_TODAY"
    echo
    echo "  A near-constant gap (e.g. always ~30 min) is a strong sign of an AP-side"
    echo "  timer, not a Wi-Fi problem on your Mac."
    echo
  fi

  # ---- Faults + known red herring ------------------------------------------
  if [ "$FAULTS" -gt 0 ]; then
    echo "FAULTS: non-zero fault counters present (DHCP/ARP/beacon-loss)."
    grep -oE 'FaultReason[A-Za-z]+=[1-9][0-9]*' "$RAW" | sort | uniq -c | sed 's/^/  /'
    echo
  fi
  echo "(Harmless: any 'FAILED to query/update roaming profile ... -528342013' lines —"
  echo " that ioctl is unsupported on many Macs and never indicates a fault.)"

  # ---- Evidence appendix for an AI / deeper dig ----------------------------
  echo
  echo "############################################################"
  echo "#   EVIDENCE (log line references — for deeper analysis)"
  echo "############################################################"
  echo
  grep -nE 'joinStateDidChange|SUSPEND AWDL.*Assoc|Roam:.*(matching scan|profile type)|driver available \(reason=|AUTO-JOIN:.*(Best|selected|associat)' "$RAW" \
    | awk '{ if (length($0) > 200) $0 = substr($0,1,200) "…"; print }' \
    | head -60
  echo
  echo "(Full detail is in the raw .log — hand that whole file to an AI if you want"
  echo " a second opinion.)"
} >> "$SUMMARY"

# ---------------------------------------------------------------------------
# 5. Show the summary and tell the user where the files are
# ---------------------------------------------------------------------------
say "${B}${CYN}==================== SUMMARY ====================${R}"
cat "$SUMMARY"
say ""
say "${GRN}Done.${R}"
say "  Raw log (give this to an AI):  ${B}${RAW}${R}"
say "  Summary / analysis:            ${B}${SUMMARY}${R}"
say ""
say "${DIM}Tips:${R}"
say "${DIM}  - Missed the event? Step back an hour at a time:  ./wifilog.sh -1  (then -2, -3, ...)${R}"
say "${DIM}  - Some driver lines only appear with elevated logging:  sudo ./wifilog.sh${R}"
