package main

// Phase 3 — persistence & history. Aggregate the 1s live samples into per-minute
// and per-hour Buckets, persist the minute buckets as append-only JSONL so
// history survives a restart, reload prior buckets on startup, and expose them to
// serve.go via Snapshot (served at /history.json). Per-hour buckets are DERIVED
// from minutes (single source of truth) and persisted only once their minutes age
// out of retention — so the hour view is never empty and an hour is never
// double-counted.
//
// Pure/impure split (house style, same as bandwidth.go / serve.go): bucketStart /
// sampleBytes / foldSample / bucketize / advanceBucket / rollup /
// mergeBucketsByStart / marshalBucketLine / parseBucketLine / parseJSONLBuckets /
// pruneBuckets / historyJSON are PURE and table-tested; historyStore + its
// openHistoryStore / ingest / appendLine / Snapshot / FlushOpen / Close and the
// writeTempFile / commit atomic-rewrite primitives are the thin impure wrappers
// that flock, read, write, fsync and rename.
//
// SCOPE GUARD: Phase 4 adds ONE value field to Bucket — Band (a string, the
// window's dominant band). Bucket must still carry NO slice/map field: a string is
// safe for Snapshot's append-copy (it stays a genuine deep, race-safe copy because
// every field is a value type), but a per-band map counter would NOT be — which is
// why the live-path band tally lives on the STORE (openBandCounts), not on Bucket.
//
// PHASE 8 folds a THIRD JSONL surface into this store — the outage journal
// (outage-journal.jsonl) — sharing the SAME dir + flock as the minute/hour files
// (seam a). It is APPEND-ONLY AT RUNTIME (appendOutage never rewrites) with a ONE-SHOT
// TTL sweep at open (so the prune lost-update shape cannot occur for outages), and
// Outage — like Bucket — stays ALL-VALUE-TYPE so OutagesSnapshot's append-copy is a
// genuine deep, race-safe copy (see the SCOPE GUARD in outage.go).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Fixed on-disk filenames. The loader reads ONLY these five names (minute / hour /
// outage / speed / roam — so a stray *.tmp from a crashed rewrite is ignored); all path
// building uses filepath.Join because the default dir contains a space ("Application
// Support"). (speedFileName is declared in throttle.go, co-located with its parsers.)
const (
	minuteFileName = "history-minute.jsonl"
	hourFileName   = "history-hour.jsonl"
	outageFileName = "outage-journal.jsonl" // Phase 8: the THIRD JSONL surface (shares the dir + flock)
	roamFileName   = "roam-journal.jsonl"   // Phase 14: the band/channel CHANGE journal (same seam as outage)
	lockFileName   = ".lock"

	spanMinute = "minute"
	spanHour   = "hour"

	// defaultMinuteTTL / defaultHourTTL are the retention defaults applied when a
	// config value is absent or <= 0 (see clampTTL): minutes ~48h, hours ~90d.
	defaultMinuteTTL = 48 * time.Hour
	defaultHourTTL   = 90 * 24 * time.Hour
)

// Bucket is an aggregated window of throughput. All fields are value types, which
// is what makes Snapshot's shallow append-copy a genuine deep, race-safe copy (see
// §5). Band (Phase 4) is a string — safe for the append-copy; a per-band map
// counter would NOT be, so the live tally lives on the store (openBandCounts), not
// here. Band is omitempty so Phase-3 JSONL (no "band") still loads with Band="".
type Bucket struct {
	Start       time.Time `json:"start"`          // bucket start, wall-clock, reconstructed (see bucketStart)
	Span        string    `json:"span"`           // "minute" | "hour"
	DownBytes   uint64    `json:"down_bytes"`     // EXACT total bytes in window (cumulative-delta sum)
	UpBytes     uint64    `json:"up_bytes"`       //
	DownPeakBps float64   `json:"down_peak_bps"`  // peak rate in window, BYTES/sec (NOT bits — humanBps applies *8)
	UpPeakBps   float64   `json:"up_peak_bps"`    // BYTES/sec, same unit as Sample.DownBytesPerSec
	Samples     int       `json:"samples"`        // count folded in (Reset samples ARE counted; they add 0 bytes)
	Band        string    `json:"band,omitempty"` // Phase 4: dominant band in the window; "" => unknown/none
}

// bucketStart returns the start of t's minute or hour, RECONSTRUCTED from the
// wall-clock fields in t's own Location — NOT time.Truncate. Truncate rounds
// relative to the UTC zero instant, so in a half-hour-offset zone (+05:30 India,
// +09:30/+10:30 Adelaide) or across DST an hour-truncate lands on a :30-past
// bucket, violating "12:34:56 => 12:00:00, TZ preserved". Relies ONLY on
// wall-clock fields (JSON round-trip strips the monotonic reading), so it is
// stable for reloaded samples.
func bucketStart(t time.Time, span string) time.Time {
	y, mo, d := t.Date()
	h, mi, _ := t.Clock()
	if span == spanHour {
		return time.Date(y, mo, d, h, 0, 0, 0, t.Location())
	}
	return time.Date(y, mo, d, h, mi, 0, 0, t.Location())
}

// sampleBytes returns the byte deltas one Sample contributes to its bucket,
// measured from the previously-folded sample's cumulative counters. Returns 0,0
// when s.Reset is true, when haveLast is false (no predecessor — the first sample
// ever folded seeds the baseline and contributes 0), or when a counter went
// backward (same guard order as deltaRate: compare BEFORE subtracting, so a
// uint64 wrap can never manufacture ~1.8e19). The caller advances its
// lastRx/lastTx to s.RxBytes/s.TxBytes REGARDLESS of the return (a Reset sample
// re-seeds the baseline but adds no bytes — locked decision).
func sampleBytes(lastRx, lastTx uint64, haveLast bool, s Sample) (down, up uint64) {
	if s.Reset || !haveLast {
		return 0, 0
	}
	if s.RxBytes >= lastRx { // compare BEFORE subtracting (wrap guard)
		down = s.RxBytes - lastRx
	}
	if s.TxBytes >= lastTx {
		up = s.TxBytes - lastTx
	}
	return down, up
}

// freshBucket seeds an empty bucket at start bs for span.
func freshBucket(bs time.Time, span string) Bucket {
	return Bucket{Start: bs, Span: span}
}

// foldSample merges one sample into its bucket given its pre-computed byte deltas
// (from sampleBytes): adds down/up to DownBytes/UpBytes, tracks
// DownPeakBps/UpPeakBps = max(existing, s.DownBytesPerSec/s.UpBytesPerSec), and
// increments Samples. Pure; does NOT itself know the previous sample.
func foldSample(b Bucket, s Sample, down, up uint64) Bucket {
	b.DownBytes += down
	b.UpBytes += up
	if s.DownBytesPerSec > b.DownPeakBps {
		b.DownPeakBps = s.DownBytesPerSec
	}
	if s.UpBytesPerSec > b.UpPeakBps {
		b.UpPeakBps = s.UpBytesPerSec
	}
	b.Samples++
	return b
}

// bucketize folds a full []Sample into ordered per-span buckets. Pure. It threads
// lastRx/lastTx/haveLast across the slice to feed sampleBytes, and KEYS buckets in
// a map by bucketStart(s.T, span) — NOT by consecutive runs — then emits sorted by
// Start. Keying (not runs) is mandatory: Sample.T is NOT guaranteed monotonically
// non-decreasing (a backward NTP step / sleep-wake can place an earlier T after a
// later one), and a run-based grouping would emit two buckets with the same Start
// for one minute. Map-keying coalesces out-of-order and revisited timestamps into
// one bucket. Attribution: a sample is assigned WHOLE to the bucket of its END
// timestamp T. A single sample is never "split"; only the SET is partitioned by T.
func bucketize(samples []Sample, span string) []Bucket {
	byStart := make(map[int64]Bucket)
	bandCounts := make(map[int64]map[string]int) // Phase 4: per-bucket band tally
	var lastRx, lastTx uint64
	var haveLast bool
	for _, s := range samples {
		down, up := sampleBytes(lastRx, lastTx, haveLast, s)
		bs := bucketStart(s.T, span)
		key := bs.UnixNano()
		b, ok := byStart[key]
		if !ok {
			b = freshBucket(bs, span)
			bandCounts[key] = make(map[string]int)
		}
		byStart[key] = foldSample(b, s, down, up)
		bandCounts[key][bandOf(s)]++
		lastRx, lastTx, haveLast = s.RxBytes, s.TxBytes, true
	}
	out := make([]Bucket, 0, len(byStart))
	for key, b := range byStart {
		b.Band = modeBand(bandCounts[key]) // dominant band over this bucket's samples
		out = append(out, b)
	}
	sortBucketsByStart(out)
	return out
}

// advanceBucket is the LIVE minute-boundary state machine (pure, so the boundary
// logic — where missed/duplicate-minute bugs live — is table-tested independently
// of the sampler). Given the currently-open bucket and one new sample's byte
// deltas, it decides close-or-continue:
//   - !hasOpen                        -> next = foldSample(fresh(bs,span), s, ...)        ; didClose=false
//   - hasOpen && open.Start.Equal(bs) -> next = foldSample(open, s, ...)                 ; didClose=false
//   - hasOpen && !Equal               -> closed=open, next=foldSample(fresh(bs,span),...) ; didClose=true
//
// where bs = bucketStart(s.T, span). A MULTI-minute gap (sleep-wake) takes the
// last branch: it emits EXACTLY the one open bucket and jumps to bs — it MUST NOT
// synthesize empty buckets for the skipped minutes. ALWAYS compare starts with
// time.Time.Equal, never ==, because a round-tripped/Truncate-origin time has its
// monotonic reading stripped.
func advanceBucket(open Bucket, hasOpen bool, s Sample, down, up uint64, span string) (closed Bucket, didClose bool, next Bucket) {
	bs := bucketStart(s.T, span)
	if !hasOpen {
		return Bucket{}, false, foldSample(freshBucket(bs, span), s, down, up)
	}
	if open.Start.Equal(bs) {
		return Bucket{}, false, foldSample(open, s, down, up)
	}
	return open, true, foldSample(freshBucket(bs, span), s, down, up)
}

// rollup collapses minute buckets into hour buckets, keyed by
// bucketStart(min.Start,"hour") in a map (summing DownBytes/UpBytes + Samples, max
// of the peaks), emitted sorted by Start. IDEMPOTENT by hour Start: every minute
// nests wholly in exactly one hour, so no minute is ever split and no hour is
// counted twice even if rollup is re-run on an overlapping input. The hour's Band
// (Phase 4) is modeBand over its minutes WEIGHTED by each minute's Samples count —
// a longer-occupied band dominates a brief one, and an all-"" set yields "".
func rollup(minutes []Bucket) []Bucket {
	byHour := make(map[int64]Bucket)
	bandWeights := make(map[int64]map[string]int) // per-hour Samples-weighted band tally
	for _, m := range minutes {
		hs := bucketStart(m.Start, spanHour)
		key := hs.UnixNano()
		h, ok := byHour[key]
		if !ok {
			h = freshBucket(hs, spanHour)
			bandWeights[key] = make(map[string]int)
		}
		h.DownBytes += m.DownBytes
		h.UpBytes += m.UpBytes
		h.Samples += m.Samples
		if m.DownPeakBps > h.DownPeakBps {
			h.DownPeakBps = m.DownPeakBps
		}
		if m.UpPeakBps > h.UpPeakBps {
			h.UpPeakBps = m.UpPeakBps
		}
		bandWeights[key][m.Band] += m.Samples
		byHour[key] = h
	}
	out := make([]Bucket, 0, len(byHour))
	for key, h := range byHour {
		h.Band = modeBand(bandWeights[key])
		out = append(out, h)
	}
	sortBucketsByStart(out)
	return out
}

// mergeBucketsByStart coalesces buckets of ONE span by Start: later entries
// replace earlier ones with the same Start (LAST-WRITE-WINS), emitted sorted by
// Start. Used by the loader (duplicate JSONL lines) and by Snapshot's hour view
// (persisted-hours merged with rollup(live-minutes)). NEVER blind-sums sequential
// lines — that is what makes duplicate Starts (shutdown-flush re-close, backward
// NTP, crash-retry re-promotion) safe.
func mergeBucketsByStart(buckets []Bucket) []Bucket {
	byStart := make(map[int64]Bucket)
	for _, b := range buckets {
		byStart[b.Start.UnixNano()] = b // last write wins
	}
	out := make([]Bucket, 0, len(byStart))
	for _, b := range byStart {
		out = append(out, b)
	}
	sortBucketsByStart(out)
	return out
}

// sortBucketsByStart sorts ascending by Start (wall-clock instant). Uses Before so
// it is stable under the monotonic-reading strip that JSON round-trips apply.
func sortBucketsByStart(b []Bucket) {
	sort.Slice(b, func(i, j int) bool { return b[i].Start.Before(b[j].Start) })
}

// marshalBucketLine serializes one Bucket to one line of JSONL (no trailing
// newline — the caller adds it as part of the single combined write).
func marshalBucketLine(b Bucket) ([]byte, error) {
	return json.Marshal(b)
}

// parseBucketLine parses ONE line of JSONL into a Bucket. It REJECTS
// semantically-empty-but-valid JSON: "{}" unmarshals into a zero Bucket with no
// error, so require Span in {"minute","hour"} AND a non-zero Start, returning an
// error otherwise (=> the line is skipped). This stops a partial object that
// happened to close its braces from being admitted.
func parseBucketLine(line []byte) (Bucket, error) {
	var b Bucket
	if err := json.Unmarshal(line, &b); err != nil {
		return Bucket{}, err
	}
	if b.Span != spanMinute && b.Span != spanHour {
		return Bucket{}, fmt.Errorf("bucket line has invalid span %q", b.Span)
	}
	if b.Start.IsZero() {
		return Bucket{}, fmt.Errorf("bucket line has zero start")
	}
	return b, nil
}

// parseJSONLBuckets reads a whole JSONL blob: split on '\n', and SKIP EVERY blank
// or unparseable line and continue (not merely the trailing one — a
// crash-interrupted rewrite or any torn/garbage middle line must never abort the
// load or panic). Missing/empty blob => empty slice, no error. Does NOT dedupe —
// the caller runs mergeBucketsByStart.
func parseJSONLBuckets(data []byte) []Bucket {
	out := []Bucket{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		b, err := parseBucketLine(line)
		if err != nil {
			continue // skip torn/garbage/"{}" lines; never abort the whole load
		}
		out = append(out, b)
	}
	return out
}

// pruneBuckets drops buckets older than the retention cutoff for THEIR OWN span: a
// "minute" bucket is dropped when Start < now-minuteTTL, an "hour" bucket when
// Start < now-hourTTL. A mixed slice is fine — each bucket selects its TTL by Span,
// so a minute-TTL can never be applied to an hour or vice versa. prune compares
// Start against wall-clock now (best-effort under clock skew — a backward
// correction may transiently retain/drop; acceptable).
func pruneBuckets(buckets []Bucket, now time.Time, minuteTTL, hourTTL time.Duration) []Bucket {
	minuteCutoff := now.Add(-minuteTTL)
	hourCutoff := now.Add(-hourTTL)
	out := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		cutoff := minuteCutoff
		if b.Span == spanHour {
			cutoff = hourCutoff
		}
		if b.Start.Before(cutoff) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// clampTTL converts MinuteTTLHrs/HourTTLDays-style counts to a time.Duration and
// CLAMPS a value <= 0 back to the default — a stray "minute_ttl_hours":0 in a
// config would otherwise set the cutoff to `now` and pruneBuckets would nuke the
// entire tier on first run (silent total history loss).
func clampTTL(n int, unit, def time.Duration) time.Duration {
	if n <= 0 {
		return def
	}
	return time.Duration(n) * unit
}

// historyJSON mirrors dataJSON's house rule: a pure serializer that forces each
// span to a NON-nil (possibly empty) array so the wire is {"minute":[],"hour":[]}
// and NEVER {"minute":null,"hour":null} (Go marshals a nil []Bucket to null, which
// breaks the page's array code).
func historyJSON(minute, hour []Bucket) ([]byte, error) {
	if minute == nil {
		minute = []Bucket{}
	}
	if hour == nil {
		hour = []Bucket{}
	}
	return json.Marshal(struct {
		Minute []Bucket `json:"minute"`
		Hour   []Bucket `json:"hour"`
	}{Minute: minute, Hour: hour})
}

// resolveHistoryDir resolves where history JSONL lives: the --history-dir flag
// wins, then the config history_dir, else os.UserConfigDir()/netdebug/history
// (=> ~/Library/Application Support/netdebug/history on macOS). It FAILS LOUDLY
// when os.UserConfigDir() errors (e.g. $HOME unset) rather than silently falling
// back to a relative path. All joins use filepath.Join (the default path contains
// a space — "Application Support").
func resolveHistoryDir(flagDir, configDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if configDir != "" {
		return configDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine user config dir for history (set --history-dir): %w", err)
	}
	return filepath.Join(base, "netdebug", "history"), nil
}

// ---------------------------------------------------------------------------
// Impure layer: historyStore — the durability contract.
// ---------------------------------------------------------------------------

type historyStore struct {
	dir            string
	dataMu         sync.Mutex // guards minutes/promotedHours/open/last — held BRIEFLY (never across disk IO)
	fileMu         sync.Mutex // serializes ALL disk writes (Append line vs prune rewrite) — the anti-lost-write lock
	minutes        []Bucket   // closed minute buckets still within minuteTTL (mirror of history-minute.jsonl)
	promotedHours  []Bucket   // hour buckets whose minutes have aged out (mirror of history-hour.jsonl)
	open           Bucket     // the in-progress partial minute (NOT yet closed/persisted/visible to Snapshot)
	hasOpen        bool
	openBandCounts map[string]int // Phase 4: per-band sample tally for the OPEN minute (lives on the store, not Bucket, to keep Bucket all-value)
	outages        []Outage       // Phase 8: closed outage records (mirror of outage-journal.jsonl), append-only at runtime
	speeds         []SpeedSample  // Phase 13: throttle speed samples (mirror of speed-samples.jsonl), append-only at runtime
	roams          []Transition   // Phase 14: band/channel change records (mirror of roam-journal.jsonl), append-only at runtime
	lastRx, lastTx uint64         // cumulative counters of the last-folded sample (for sampleBytes)
	haveLast       bool
	lockFile       *os.File // held flock (single-instance-per-dir guard); released on Close
}

// openHistoryStore opens the store with the DEFAULT outage TTL (90d). Thin wrapper so
// the many existing call sites (and tests) stay unchanged; the serve path threads a
// config-tunable TTL via openHistoryStoreTTL.
func openHistoryStore(dir string) (*historyStore, error) {
	return openHistoryStoreTTL(dir, defaultOutageTTL, defaultThrottleSampleTTL)
}

// openHistoryStoreTTL creates dir (os.MkdirAll 0o755) if missing, acquires an
// EXCLUSIVE non-blocking flock on <dir>/.lock and FAILS LOUDLY if another process
// holds it — two --serve instances share the default dir and would otherwise
// interleave appends and race rewrites. It SWEEPS stale *.tmp left by a prior
// crash, then loads ONLY the three fixed filenames via parseJSONLBuckets /
// parseJSONLOutages (+ mergeBucketsByStart / sortOutagesByStart) into
// minutes/promotedHours/outages. A missing file => empty slice. *.tmp files are
// IGNORED by the loader (proves the atomic-rewrite seam).
//
// Phase 8: after loading the outage journal it runs the ONE-SHOT pruneOutages sweep
// (must_fix #8/#9) and, IF the count shrank, rewrites the journal ONCE — single-
// threaded, BEFORE the reach/detector goroutine starts — so there is never a
// concurrent runtime rewrite of the journal.
func openHistoryStoreTTL(dir string, outageTTL, speedTTL time.Duration) (*historyStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create history dir %q: %w", dir, err)
	}
	lf, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open history lock in %q: %w", dir, err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("another netdebug --serve is using history dir %q: %w", dir, err)
	}

	// Sweep stale *.tmp from a prior crash so they never accumulate (the loader
	// ignores them regardless — it reads only the two fixed names).
	if tmps, gerr := filepath.Glob(filepath.Join(dir, "*.tmp")); gerr == nil {
		for _, t := range tmps {
			_ = os.Remove(t)
		}
	}

	h := &historyStore{dir: dir, lockFile: lf}
	minData, err := readFileIfExists(filepath.Join(dir, minuteFileName))
	if err != nil {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("read %s: %w", minuteFileName, err)
	}
	hourData, err := readFileIfExists(filepath.Join(dir, hourFileName))
	if err != nil {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("read %s: %w", hourFileName, err)
	}
	h.minutes = mergeBucketsByStart(parseJSONLBuckets(minData))
	h.promotedHours = mergeBucketsByStart(parseJSONLBuckets(hourData))

	// Phase 8: load the outage journal (third fixed name), sort by Start, then run the
	// ONE-SHOT TTL sweep. If any record aged out, rewrite the file ONCE here (single-
	// threaded, before any goroutine starts) so appendOutage can stay pure append-only.
	outData, err := readFileIfExists(filepath.Join(dir, outageFileName))
	if err != nil {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("read %s: %w", outageFileName, err)
	}
	loaded := parseJSONLOutages(outData)
	sortOutagesByStart(loaded)
	swept := pruneOutages(loaded, time.Now(), outageTTL)
	if len(swept) != len(loaded) {
		if werr := h.rewriteOutageFile(swept); werr != nil {
			syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			lf.Close()
			return nil, fmt.Errorf("outage journal TTL sweep rewrite: %w", werr)
		}
	}
	h.outages = swept

	// Phase 13: load the speed-samples journal (FOURTH fixed name), sort by T, then run
	// the ONE-SHOT TTL sweep. If any sample aged out, rewrite the file ONCE here
	// (single-threaded, before any goroutine starts) so appendSpeed stays pure
	// append-only — exactly mirroring the Phase-8 outage surface.
	speedData, err := readFileIfExists(filepath.Join(dir, speedFileName))
	if err != nil {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("read %s: %w", speedFileName, err)
	}
	loadedSpeeds := parseJSONLSpeed(speedData)
	sortSpeedsByT(loadedSpeeds)
	sweptSpeeds := pruneSpeed(loadedSpeeds, time.Now(), speedTTL)
	if len(sweptSpeeds) != len(loadedSpeeds) {
		if werr := h.rewriteSpeedFile(sweptSpeeds); werr != nil {
			syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			lf.Close()
			return nil, fmt.Errorf("speed journal TTL sweep rewrite: %w", werr)
		}
	}
	h.speeds = sweptSpeeds

	// Phase 14: load the roam journal (FIFTH fixed name), sort by T, then run the
	// ONE-SHOT TTL sweep — EXACTLY the Phase-8 outage surface (and it reuses the outage
	// TTL: band/channel changes and outages share the same 90d retention horizon). If
	// any record aged out, rewrite the file ONCE here (single-threaded, before any
	// goroutine starts) so appendRoam stays pure append-only.
	roamData, err := readFileIfExists(filepath.Join(dir, roamFileName))
	if err != nil {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("read %s: %w", roamFileName, err)
	}
	loadedRoams := parseJSONLRoams(roamData)
	sortRoamsByT(loadedRoams)
	sweptRoams := pruneRoams(loadedRoams, time.Now(), outageTTL)
	if len(sweptRoams) != len(loadedRoams) {
		if werr := h.rewriteRoamFile(sweptRoams); werr != nil {
			syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			lf.Close()
			return nil, fmt.Errorf("roam journal TTL sweep rewrite: %w", werr)
		}
	}
	h.roams = sweptRoams
	return h, nil
}

// readFileIfExists reads a file, returning (nil, nil) if it does not exist (a
// missing history file is not an error — it is a fresh store).
func readFileIfExists(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// ingest folds one live sample into the open minute and, on a minute close,
// persists the closed bucket. This is the sampler's per-sample hook (serve mode
// only). Sequence (durability + concurrency):
//  1. dataMu: compute deltas, advanceBucket, advance open/last; stash closed if didClose. NO disk IO under dataMu.
//  2. if didClose: appendLine(closed) — fsync to DISK FIRST, under fileMu only.
//  3. dataMu: minutes = append(minutes, closed). (memory updated only AFTER the
//     bucket is durable, so Snapshot never advertises a bucket that would vanish on restart.)
func (h *historyStore) ingest(s Sample) {
	h.dataMu.Lock()
	down, up := sampleBytes(h.lastRx, h.lastTx, h.haveLast, s)
	closed, didClose, next := advanceBucket(h.open, h.hasOpen, s, down, up, spanMinute)
	h.open = next
	h.hasOpen = true
	h.lastRx, h.lastTx, h.haveLast = s.RxBytes, s.TxBytes, true
	// Phase 4 dominant-band: on a close, finalize the closed minute's Band from the
	// tally accumulated so far, then reset and SEED the new minute with THIS
	// sample's band (it belongs to `next`, the freshly opened minute). On a
	// continuation, THIS sample folds into the open minute, so tally it.
	if didClose {
		closed.Band = modeBand(h.openBandCounts)
		h.openBandCounts = map[string]int{bandOf(s): 1}
	} else {
		if h.openBandCounts == nil {
			h.openBandCounts = make(map[string]int)
		}
		h.openBandCounts[bandOf(s)]++
	}
	h.dataMu.Unlock()

	if !didClose {
		return
	}

	if err := h.appendLine(closed); err != nil {
		fmt.Fprintf(os.Stderr, "netdebug: history append failed: %v\n", err)
		return // memory NOT advanced — Snapshot never advertises a non-durable bucket
	}
	h.dataMu.Lock()
	h.minutes = append(h.minutes, closed)
	h.dataMu.Unlock()
}

// appendLine is the ONLY durable write path for a new minute. Under fileMu: open
// history-minute.jsonl O_APPEND|O_CREATE|O_WRONLY; build ONE buffer =
// marshalBucketLine(b) followed by '\n'; write it in a SINGLE f.Write (NEVER json
// in one Write and '\n' in another — a crash between the two persists a
// terminator-less line and the next append concatenates two JSON objects on one
// physical line); f.Sync(); Close. No long-lived fd is ever cached (a cached
// O_APPEND fd would write to the unlinked old inode after a prune rename and be
// lost).
func (h *historyStore) appendLine(b Bucket) error {
	line, err := marshalBucketLine(b)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')

	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	f, err := os.OpenFile(filepath.Join(h.dir, minuteFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil { // SINGLE write: object + '\n' together
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Snapshot returns an independent, consistent copy of BOTH spans under ONE dataMu
// acquisition (never a torn two-span read). minute = a copy of `minutes` (closed
// minutes only; the open partial minute is NOT included). hour =
// mergeBucketsByStart(promotedHours ++ rollup(minutes)) — rollup covers recent
// hours (so the hour view is populated IMMEDIATELY, not empty for 48h), promoted
// covers aged-out hours; any overlap coalesces by Start (identical values) so no
// hour is double-counted. Copies via append([]Bucket(nil), ...) — a true deep copy
// ONLY while Bucket stays all-value (scope guard).
func (h *historyStore) Snapshot() (minute, hour []Bucket) {
	h.dataMu.Lock()
	defer h.dataMu.Unlock()
	minute = append([]Bucket(nil), h.minutes...)
	derived := rollup(h.minutes)
	combined := append([]Bucket(nil), h.promotedHours...)
	combined = append(combined, derived...)
	hour = mergeBucketsByStart(combined)
	return minute, hour
}

// FlushOpen persists the in-progress partial minute as a valid Bucket (Samples<60)
// on graceful shutdown — up to ~59s would otherwise be dropped on every clean
// Ctrl-C. Called by serveDashboard AFTER the sampler goroutine is joined; the
// accumulator lives in the STORE (not loop-local), so it is still reachable then.
// Appends via appendLine + mirrors into memory.
func (h *historyStore) FlushOpen() error {
	h.dataMu.Lock()
	if !h.hasOpen || h.open.Samples == 0 {
		h.dataMu.Unlock()
		return nil
	}
	b := h.open
	b.Band = modeBand(h.openBandCounts) // Phase 4: finalize the partial minute's dominant band
	h.hasOpen = false
	h.dataMu.Unlock()

	if err := h.appendLine(b); err != nil {
		return err
	}
	h.dataMu.Lock()
	h.minutes = append(h.minutes, b)
	h.dataMu.Unlock()
	return nil
}

// appendOutage durably records one CLOSED outage (Phase 8, seam a). APPEND-ONLY: it
// builds ONE buffer = marshalOutageLine(o) + '\n' and writes it in a SINGLE f.Write +
// f.Sync (never object and '\n' in separate writes — the appendLine durability rule),
// under fileMu, with no cached fd. The durable write happens FIRST; only then does it
// mirror into h.outages under dataMu — so OutagesSnapshot never advertises a non-
// durable record. Lock order is fileMu-then-(release)-then-dataMu (never nested, never
// inverted vs the minute path) so no deadlock. Called LOCK-FREE by the detector closure
// (stepOutage holds no store lock); it can at worst wait one fsync behind a minute
// append and can NEVER stall the 1s byte sampler.
func (h *historyStore) appendOutage(o Outage) error {
	line, err := marshalOutageLine(o)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')

	if err := h.appendOutageBytes(buf); err != nil {
		return err
	}
	h.dataMu.Lock()
	h.outages = append(h.outages, o)
	h.dataMu.Unlock()
	return nil
}

// appendOutageBytes is the durable file write for appendOutage, isolated so fileMu is
// released (via defer) BEFORE appendOutage takes dataMu.
func (h *historyStore) appendOutageBytes(buf []byte) error {
	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	f, err := os.OpenFile(filepath.Join(h.dir, outageFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil { // SINGLE write: object + '\n' together
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// OutagesSnapshot returns an independent, sorted copy of the closed outage records
// under dataMu. Copies via append([]Outage(nil), ...) — a true deep copy ONLY while
// Outage stays all-value (scope guard).
func (h *historyStore) OutagesSnapshot() []Outage {
	h.dataMu.Lock()
	defer h.dataMu.Unlock()
	out := append([]Outage(nil), h.outages...)
	sortOutagesByStart(out)
	return out
}

// appendSpeed durably records one throttle SpeedSample (Phase 13, seam a) — the
// Phase-8 appendOutage plumbing EXACTLY: build ONE buffer = marshalSpeedLine(s) + '\n'
// and write it in a SINGLE f.Write + f.Sync under fileMu (released before dataMu), then
// mirror into h.speeds under dataMu — so SpeedSnapshot never advertises a non-durable
// sample. No fileMu contention: minute append ~1/min, outage rare, speed <= ~2/hr. A
// 429 record (OK=false, zero Mbps) IS persisted (the spacing anchor + honest record).
func (h *historyStore) appendSpeed(s SpeedSample) error {
	line, err := marshalSpeedLine(s)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')

	if err := h.appendSpeedBytes(buf); err != nil {
		return err
	}
	h.dataMu.Lock()
	h.speeds = append(h.speeds, s)
	h.dataMu.Unlock()
	return nil
}

// appendSpeedBytes is the durable file write for appendSpeed, isolated so fileMu is
// released (via defer) BEFORE appendSpeed takes dataMu (the appendOutageBytes shape).
func (h *historyStore) appendSpeedBytes(buf []byte) error {
	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	f, err := os.OpenFile(filepath.Join(h.dir, speedFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil { // SINGLE write: object + '\n' together
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SpeedSnapshot returns an independent, T-sorted copy of the speed samples under
// dataMu. Copies via append([]SpeedSample(nil), ...) — a true deep copy ONLY while
// SpeedSample stays all-value (scope guard). Used to seed the watchdog's lastAttemptT
// and by the /report handler.
func (h *historyStore) SpeedSnapshot() []SpeedSample {
	h.dataMu.Lock()
	defer h.dataMu.Unlock()
	out := append([]SpeedSample(nil), h.speeds...)
	sortSpeedsByT(out)
	return out
}

// appendRoam durably records one band/channel/BSSID change (Phase 14, seam a) — the
// Phase-8 appendOutage plumbing EXACTLY: build ONE buffer = marshalRoamLine(r) + '\n'
// and write it in a SINGLE f.Write + f.Sync under fileMu (released before dataMu), then
// mirror into h.roams under dataMu — so RoamsSnapshot never advertises a non-durable
// record. No fileMu contention: a roam is rare (a human-visible steer), far rarer than
// the ~1/min minute append. Called LOCK-FREE from the reach detector closure.
func (h *historyStore) appendRoam(r Transition) error {
	line, err := marshalRoamLine(r)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')

	if err := h.appendRoamBytes(buf); err != nil {
		return err
	}
	h.dataMu.Lock()
	h.roams = append(h.roams, r)
	h.dataMu.Unlock()
	return nil
}

// appendRoamBytes is the durable file write for appendRoam, isolated so fileMu is
// released (via defer) BEFORE appendRoam takes dataMu (the appendOutageBytes shape).
func (h *historyStore) appendRoamBytes(buf []byte) error {
	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	f, err := os.OpenFile(filepath.Join(h.dir, roamFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil { // SINGLE write: object + '\n' together
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RoamsSnapshot returns an independent, T-sorted copy of the band/channel change
// records under dataMu. Copies via append([]Transition(nil), ...) — a true deep copy
// ONLY while Transition stays all-value (scope guard).
func (h *historyStore) RoamsSnapshot() []Transition {
	h.dataMu.Lock()
	defer h.dataMu.Unlock()
	out := append([]Transition(nil), h.roams...)
	sortRoamsByT(out)
	return out
}

// sortSpeedsByT sorts ascending by T using Before (monotonic-strip safe), mirroring
// sortOutagesByStart.
func sortSpeedsByT(s []SpeedSample) {
	sort.Slice(s, func(i, j int) bool { return s[i].T.Before(s[j].T) })
}

// Close releases the flock and nils the lock fd.
func (h *historyStore) Close() error {
	if h.lockFile == nil {
		return nil
	}
	syscall.Flock(int(h.lockFile.Fd()), syscall.LOCK_UN)
	err := h.lockFile.Close()
	h.lockFile = nil
	return err
}

// ---------------------------------------------------------------------------
// Atomic-rewrite primitives (durability) — a test seam so "crash mid-rewrite
// never corrupts history" is provable by INVARIANT.
// ---------------------------------------------------------------------------

// writeTempFile creates the temp IN THE TARGET DIRECTORY (os.CreateTemp(dir,...))
// — NOT os.CreateTemp("",...), which lands in $TMPDIR, a DIFFERENT volume from
// ~/Library/Application Support on macOS, degrading os.Rename to a non-atomic
// cross-volume copy and silently voiding atomicity. Writes all buckets (each
// marshalBucketLine + '\n'), tmp.Sync(), tmp.Close(). Returns the temp path and
// does NOT rename. On ANY error, removes the temp.
func writeTempFile(dir, basename string, buckets []Bucket) (tmpPath string, err error) {
	tmp, err := os.CreateTemp(dir, basename+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			tmpPath = ""
		}
	}()

	var buf bytes.Buffer
	for _, b := range buckets {
		line, mErr := marshalBucketLine(b)
		if mErr != nil {
			err = mErr
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		// Close reported an error; remove the temp via the defer (which also calls
		// Close again — harmless on an already-closed file).
		return "", err
	}
	return tmpPath, nil
}

// commit renames tmpPath over target (atomic on one filesystem) and fsyncs the
// PARENT DIR afterward so the rename itself is power-loss durable on APFS. On
// rename error, removes the temp so a failed prune leaves no litter. Never
// truncates target in place.
func commit(tmpPath, target, dir string) error {
	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil // rename already succeeded; dir-fsync is best-effort durability
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// rewriteFile is the atomic temp+rename rewrite of one history file (used by the
// prune cycle). writeTempFile in dir, then commit over the fixed filename.
func (h *historyStore) rewriteFile(basename string, buckets []Bucket) error {
	tmpPath, err := writeTempFile(h.dir, basename, buckets)
	if err != nil {
		return err
	}
	return commit(tmpPath, filepath.Join(h.dir, basename), h.dir)
}

// writeOutageTempFile is the Outage-typed sibling of writeTempFile (must_fix #9: do
// NOT shoehorn Outages through the []Bucket signatures). It creates the temp IN THE
// TARGET DIRECTORY (atomic rename stays on one volume), writes all outages (each
// marshalOutageLine + '\n'), fsyncs, closes, and returns the temp path WITHOUT
// renaming. On ANY error it removes the temp. The existing *.tmp glob in
// openHistoryStoreTTL already sweeps this temp.
func writeOutageTempFile(dir, basename string, outages []Outage) (tmpPath string, err error) {
	tmp, err := os.CreateTemp(dir, basename+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			tmpPath = ""
		}
	}()

	var buf bytes.Buffer
	for _, o := range outages {
		line, mErr := marshalOutageLine(o)
		if mErr != nil {
			err = mErr
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return tmpPath, nil
}

// rewriteOutageFile is the atomic temp+rename rewrite of the outage journal. Used ONLY
// by the ONE-SHOT at-open TTL sweep (never at runtime — appendOutage is append-only),
// so there is no concurrent-rewrite lost-update shape to inherit.
func (h *historyStore) rewriteOutageFile(outages []Outage) error {
	tmpPath, err := writeOutageTempFile(h.dir, outageFileName, outages)
	if err != nil {
		return err
	}
	return commit(tmpPath, filepath.Join(h.dir, outageFileName), h.dir)
}

// writeSpeedTempFile is the SpeedSample-typed sibling of writeTempFile/
// writeOutageTempFile (do NOT shoehorn SpeedSamples through the []Bucket signatures).
// It creates the temp IN THE TARGET DIRECTORY (atomic rename stays on one volume),
// writes all samples (each marshalSpeedLine + '\n'), fsyncs, closes, and returns the
// temp path WITHOUT renaming. On ANY error it removes the temp. The existing *.tmp
// glob in openHistoryStoreTTL already sweeps this temp.
func writeSpeedTempFile(dir, basename string, samples []SpeedSample) (tmpPath string, err error) {
	tmp, err := os.CreateTemp(dir, basename+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			tmpPath = ""
		}
	}()

	var buf bytes.Buffer
	for _, s := range samples {
		line, mErr := marshalSpeedLine(s)
		if mErr != nil {
			err = mErr
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return tmpPath, nil
}

// rewriteSpeedFile is the atomic temp+rename rewrite of the speed-samples journal.
// Used ONLY by the ONE-SHOT at-open TTL sweep (never at runtime — appendSpeed is
// append-only), so there is no concurrent-rewrite lost-update shape to inherit.
func (h *historyStore) rewriteSpeedFile(samples []SpeedSample) error {
	tmpPath, err := writeSpeedTempFile(h.dir, speedFileName, samples)
	if err != nil {
		return err
	}
	return commit(tmpPath, filepath.Join(h.dir, speedFileName), h.dir)
}

// writeRoamTempFile is the Transition-typed sibling of writeOutageTempFile (do NOT
// shoehorn Transitions through the []Bucket / []Outage signatures). It creates the temp
// IN THE TARGET DIRECTORY (atomic rename stays on one volume), writes all roams (each
// marshalRoamLine + '\n'), fsyncs, closes, and returns the temp path WITHOUT renaming.
// On ANY error it removes the temp. The existing *.tmp glob in openHistoryStoreTTL
// already sweeps this temp.
func writeRoamTempFile(dir, basename string, roams []Transition) (tmpPath string, err error) {
	tmp, err := os.CreateTemp(dir, basename+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			tmpPath = ""
		}
	}()

	var buf bytes.Buffer
	for _, r := range roams {
		line, mErr := marshalRoamLine(r)
		if mErr != nil {
			err = mErr
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return tmpPath, nil
}

// rewriteRoamFile is the atomic temp+rename rewrite of the roam journal. Used ONLY by
// the ONE-SHOT at-open TTL sweep (never at runtime — appendRoam is append-only), so
// there is no concurrent-rewrite lost-update shape to inherit.
func (h *historyStore) rewriteRoamFile(roams []Transition) error {
	tmpPath, err := writeRoamTempFile(h.dir, roamFileName, roams)
	if err != nil {
		return err
	}
	return commit(tmpPath, filepath.Join(h.dir, roamFileName), h.dir)
}

// prune runs one retention + hour-promotion cycle (called once/minute by the prune
// goroutine, NEVER inline in the 1s sampler tick). Sequence:
//  1. snapshot minutes/promotedHours under dataMu, then do ALL disk work unlocked.
//  2. promote WHOLE HOURS ONLY: an hour H (start hs) is promotable iff
//     hs.Add(time.Hour) is before cutoff (= now-minuteTTL) — so ALL 60 of H's
//     minutes are expired. Partially-expired hours keep ALL their minutes. This
//     forbids the TTL-bisected-hour double-promotion.
//  3. newHours = rollup(minutes-being-promoted); keptMinutes = the rest.
//     promotedHours' = pruneBuckets(merge(promotedHours ++ newHours), now, _, hourTTL).
//  4. Write HOUR FILE FIRST, THEN MINUTE FILE (a crash between leaves the promoted
//     hour in both files; Snapshot's merge coalesces the duplicate — no double-count,
//     and writing the additive hour first guarantees an hour is never LOST).
//  5. commit memory under dataMu.
//  6. Any write error is LOGGED and the cycle aborts WITHOUT mutating memory.
func (h *historyStore) prune(now time.Time, minuteTTL, hourTTL time.Duration) error {
	h.dataMu.Lock()
	minutes := append([]Bucket(nil), h.minutes...)
	promoted := append([]Bucket(nil), h.promotedHours...)
	h.dataMu.Unlock()

	cutoff := now.Add(-minuteTTL)
	var toPromote, keptMinutes []Bucket
	for _, m := range minutes {
		hs := bucketStart(m.Start, spanHour)
		if hs.Add(time.Hour).Before(cutoff) { // whole hour has fully expired
			toPromote = append(toPromote, m)
		} else {
			keptMinutes = append(keptMinutes, m)
		}
	}

	if len(toPromote) == 0 {
		// Nothing to promote; still prune the hour tier for TTL expiry so the hour
		// file does not grow past hourTTL.
		newPromoted := pruneBuckets(promoted, now, minuteTTL, hourTTL)
		if len(newPromoted) == len(promoted) {
			return nil // no change on either tier
		}
		if err := h.rewriteFile(hourFileName, newPromoted); err != nil {
			fmt.Fprintf(os.Stderr, "netdebug: history prune (hour rewrite) failed: %v\n", err)
			return err
		}
		h.dataMu.Lock()
		h.promotedHours = newPromoted
		h.dataMu.Unlock()
		return nil
	}

	newHours := rollup(toPromote)
	newPromoted := pruneBuckets(mergeBucketsByStart(append(append([]Bucket(nil), promoted...), newHours...)), now, minuteTTL, hourTTL)

	// HOUR FILE FIRST (additive — an hour is never lost), THEN MINUTE FILE.
	if err := h.rewriteFile(hourFileName, newPromoted); err != nil {
		fmt.Fprintf(os.Stderr, "netdebug: history prune (hour rewrite) failed: %v\n", err)
		return err
	}
	if err := h.rewriteFile(minuteFileName, keptMinutes); err != nil {
		fmt.Fprintf(os.Stderr, "netdebug: history prune (minute rewrite) failed: %v\n", err)
		return err
	}

	h.dataMu.Lock()
	h.minutes = keptMinutes
	h.promotedHours = newPromoted
	h.dataMu.Unlock()
	return nil
}
