package main

// Phase 13 (capstone) — THE THROTTLE WATCHDOG. A lightweight, well-spaced,
// 429-SAFE download sampler that takes a SMALL bounded transfer at most ~2x/hour
// so the daily report can chart speed-by-hour and expose suspected Comcast
// peak-hour throttling. The watchdog is OPT-IN (--throttle-watch, serve-only,
// default OFF): a bare --serve calls Cloudflare ZERO times.
//
// Pure/impure split (house style, same as history.go / speed.go):
//   PURE + table-tested: SpeedSample (all-value), shouldSample, jitterSpacing,
//   backoffSpacing, clampThrottle{Spacing,Streams,Bytes,Ratio}, marshalSpeedLine /
//   parseSpeedLine / parseJSONLSpeed, pruneSpeed, HourStat, speedByHour,
//   detectThrottle.
//   IMPURE (the SOLE new Cloudflare caller): sampleDownload / runThrottleSample,
//   and the watchdog goroutine body runThrottleWatch (spawned only under the flag).
//
// 429-SAFETY rests on FOUR invariants (all mandatory + table-tested): (1) the
// sample is a SINGLE bounded download GET (never a window-bound stream); (2)
// spacing anchors on EVERY attempt incl. a 429, with exponential backoff on
// consecutive failures; (3) jitter only ADDS delay (the 30m floor is inviolable);
// (4) a 429/403/non-200 => OK=false, recorded, EXCLUDED from stats — never a bogus
// slow number.
//
// SCOPE GUARD: SpeedSample has NO slice/map/pointer field — every field is a value
// type — so SpeedSnapshot's append-copy is a genuine deep, race-safe copy (the
// Bucket/Outage invariant). A later per-stream slice or pointer field would
// silently break that copy — keep SpeedSample ALL-VALUE.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	// speedFileName is the FOURTH JSONL surface (shares the history dir + flock).
	speedFileName = "speed-samples.jsonl"

	// The throttle sample is LIGHTWEIGHT, SPACED and 429-SAFE. These are the hard
	// floors/caps the impure-boundary clamps enforce (a stray or hostile 0 spacing
	// would collapse shouldSample's gate into the exact Cloudflare hammer this phase
	// exists to prevent — the clamps are SAFETY-CRITICAL, not cosmetic).
	defaultThrottleSpacing    = 30 * time.Minute    // the §0 budget floor; inviolable
	defaultThrottleJitterMax  = 10 * time.Minute    // additive jitter => effective ∈ [30m, 40m]
	defaultThrottleMaxBackoff = 4 * time.Hour       // exponential-backoff cap on consecutive 429s
	defaultThrottleRatio      = 0.6                 // dip threshold = ratio*baseline
	defaultThrottleMinSamples = 2                   // noise guard: fewer than this in an hour is not flagged
	defaultThrottleSampleTTL  = 30 * 24 * time.Hour // 4th-surface retention

	throttleMinBytes     = 5_000_000  // a "lightweight" sample never dips below this...
	throttleMaxBytes     = 50_000_000 // ...nor saturates above this
	defaultThrottleBytes = 10_000_000 // ~10 MB default small transfer

	maxThrottleStreams = 2 // a lightweight sample never saturates with more than this

	// throttleSampleTimeout is the HARD per-sample wall-clock cap. A sample that has
	// not finished its bounded transfer by now is abandoned (ctx-cancel) so a stalled
	// link never pins the watchdog goroutine.
	throttleSampleTimeout = 20 * time.Second

	// throttleWatchTick is the watchdog's Ticker interval. It is DELIBERATELY much
	// shorter than the spacing (<< 30m) so a battery->AC resume fires a due sample
	// within ~1 tick instead of up to a full spacing later; the PURE shouldSample gate
	// (not the tick) enforces the real budget.
	throttleWatchTick = time.Minute
)

// SpeedSample is one lightweight DOWNLOAD throughput sample persisted for the day
// chart. ALL fields are value types (see the SCOPE GUARD in the file header).
type SpeedSample struct {
	T            time.Time `json:"t"`
	DownloadMbps float64   `json:"download_mbps"`         // small-transfer proxy, NOT absolute throughput
	UploadMbps   float64   `json:"upload_mbps,omitempty"` // RESERVED; download-only sampler never sets it (absent => "not sampled", not 0)
	LatencyMs    float64   `json:"latency_ms,omitempty"`  // TTFB of the SAME status-checked 200 GET
	OK           bool      `json:"ok"`                    // false on 429/403/non-200/error — kept for the record, EXCLUDED from stats + the chart
}

// shouldSample is the PURE spacing gate — the "never exceed the Cloudflare budget"
// rule, table-tested, not buried in the ticker. True iff a sample is DUE. The
// never-sampled case is EXPLICIT (do NOT rely on year-1 zero-Time Sub saturation);
// a backward clock (now < last) => false (no negative-spacing sample). `spacing` is
// the EFFECTIVE spacing the caller already folded jitter/backoff into.
func shouldSample(now, lastAttemptT time.Time, spacing time.Duration) bool {
	if lastAttemptT.IsZero() {
		return true // never attempted
	}
	if now.Before(lastAttemptT) {
		return false // backward clock: never manufacture a negative-spacing sample
	}
	return now.Sub(lastAttemptT) >= spacing
}

// jitterSpacing folds an ADDITIVE, NON-NEGATIVE jitter onto the minSpacing floor so
// the interval is NEVER shorter than minSpacing: effective ∈ [minSpacing,
// minSpacing+jitterMax]. PURE given a [0,1) fraction r (the caller supplies
// rand.Float64()). A negative jitterMax or an out-of-range r is clamped so the floor
// invariant holds unconditionally.
func jitterSpacing(minSpacing, jitterMax time.Duration, r float64) time.Duration {
	if jitterMax < 0 {
		jitterMax = 0
	}
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999999
	}
	return minSpacing + time.Duration(r*float64(jitterMax))
}

// backoffSpacing escalates the floor on CONSECUTIVE 429/403 attempts (exponential,
// capped) so a persistent rate-limit backs off instead of retrying every slot.
// consecutiveFails 0 => minSpacing; 1 => 2×; 2 => 4× … capped at maxBackoff. PURE.
func backoffSpacing(minSpacing, maxBackoff time.Duration, consecutiveFails int) time.Duration {
	if consecutiveFails <= 0 {
		return minSpacing
	}
	d := minSpacing
	for i := 0; i < consecutiveFails; i++ {
		d *= 2
		if d >= maxBackoff || d <= 0 { // >=cap OR int64 overflow => clamp to cap
			return maxBackoff
		}
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// clampThrottleSpacing: <30m => 30m (hard floor; the §0 budget is inviolable). No
// upper cap. SAFETY-CRITICAL: a stray 0 would collapse the gate into a hammer.
func clampThrottleSpacing(d time.Duration) time.Duration {
	if d < defaultThrottleSpacing {
		return defaultThrottleSpacing
	}
	return d
}

// clampThrottleStreams: <1 => 1; cap maxThrottleStreams (2).
func clampThrottleStreams(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxThrottleStreams {
		return maxThrottleStreams
	}
	return n
}

// clampThrottleBytes: clamp to [throttleMinBytes, throttleMaxBytes]; a <=0 value =>
// the 10MB default.
func clampThrottleBytes(n int) int {
	if n <= 0 {
		return defaultThrottleBytes
	}
	if n < throttleMinBytes {
		return throttleMinBytes
	}
	if n > throttleMaxBytes {
		return throttleMaxBytes
	}
	return n
}

// clampThrottleRatio: outside (0,1] => default 0.6.
func clampThrottleRatio(x float64) float64 {
	if x <= 0 || x > 1 {
		return defaultThrottleRatio
	}
	return x
}

// marshalSpeedLine serializes one SpeedSample to one JSONL line (no trailing newline
// — the caller adds it as part of the single combined write, like marshalBucketLine).
func marshalSpeedLine(s SpeedSample) ([]byte, error) {
	return json.Marshal(s)
}

// parseSpeedLine parses ONE JSONL line into a SpeedSample. It REJECTS ONLY a zero T
// (and invalid JSON). It MUST NOT reject DownloadMbps==0: an OK=false 429 record
// legitimately carries zero Mbps and MUST persist (the spacing anchor + honest
// record depend on it).
func parseSpeedLine(line []byte) (SpeedSample, error) {
	var s SpeedSample
	if err := json.Unmarshal(line, &s); err != nil {
		return SpeedSample{}, err
	}
	if s.T.IsZero() {
		return SpeedSample{}, fmt.Errorf("speed sample line has zero T")
	}
	return s, nil
}

// parseJSONLSpeed reads a whole JSONL blob: split on '\n', SKIP EVERY blank or
// unparseable line and continue (a crash-interrupted rewrite or any torn/garbage
// middle line must never abort the load or panic). Missing/empty blob => empty slice.
func parseJSONLSpeed(data []byte) []SpeedSample {
	out := []SpeedSample{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		s, err := parseSpeedLine(line)
		if err != nil {
			continue // skip torn/garbage lines; never abort the whole load
		}
		out = append(out, s)
	}
	return out
}

// pruneSpeed drops samples older than now-ttl, keyed on T (mirror pruneOutages).
// Empty-safe. Keeps the 4th surface bounded (an explicit retention, not unbounded
// growth).
func pruneSpeed(samples []SpeedSample, now time.Time, ttl time.Duration) []SpeedSample {
	cutoff := now.Add(-ttl)
	out := make([]SpeedSample, 0, len(samples))
	for _, s := range samples {
		if s.T.Before(cutoff) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// HourStat is one hour-of-day speed aggregate for the chart/detection. UpSamples is
// carried SEPARATELY so a download-only design distinguishes "upload not sampled"
// (UpSamples==0 => UpAvgMbps omitted) from a measured 0.
type HourStat struct {
	Hour        int     `json:"hour"` // 0..23 LOCAL wall clock
	DownAvgMbps float64 `json:"down_avg_mbps"`
	UpAvgMbps   float64 `json:"up_avg_mbps,omitempty"` // absent while upload is unsampled
	Samples     int     `json:"samples"`               // OK download samples in the hour
	UpSamples   int     `json:"up_samples,omitempty"`  // OK upload samples (0 today)
}

// speedByHour buckets OK SpeedSamples into 0..23 LOCAL-hour averages
// (t.In(time.Local).Hour() — peak hours are a local-wall-clock concept; a
// fixed-offset-zone JSON reload / DST must not mis-bucket). OK=false samples are
// EXCLUDED. Returns a NON-nil []HourStat{} on empty. Download and upload are averaged
// over their OWN sample counts.
func speedByHour(samples []SpeedSample) []HourStat {
	type acc struct {
		downSum   float64
		downCount int
		upSum     float64
		upCount   int
	}
	byHour := make(map[int]*acc)
	for _, s := range samples {
		if !s.OK {
			continue // excluded from stats + the chart
		}
		h := s.T.In(time.Local).Hour()
		a := byHour[h]
		if a == nil {
			a = &acc{}
			byHour[h] = a
		}
		a.downSum += s.DownloadMbps
		a.downCount++
		if s.UploadMbps > 0 { // download-only sampler never sets UploadMbps, so this is 0 today
			a.upSum += s.UploadMbps
			a.upCount++
		}
	}
	out := make([]HourStat, 0, len(byHour))
	for h, a := range byHour {
		hs := HourStat{Hour: h, Samples: a.downCount}
		if a.downCount > 0 {
			hs.DownAvgMbps = a.downSum / float64(a.downCount)
		}
		if a.upCount > 0 {
			hs.UpSamples = a.upCount
			hs.UpAvgMbps = a.upSum / float64(a.upCount)
		}
		out = append(out, hs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

// percentile returns the linearly-interpolated p-percentile (p ∈ [0,1]) of xs.
// Empty => 0. Sorts a COPY so the caller's slice order is untouched.
func percentile(xs []float64, p float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n == 1 {
		return s[0]
	}
	if p <= 0 {
		return s[0]
	}
	if p >= 1 {
		return s[n-1]
	}
	pos := p * float64(n-1)
	lo := int(pos)
	frac := pos - float64(lo)
	if lo+1 >= n {
		return s[n-1]
	}
	return s[lo] + frac*(s[lo+1]-s[lo])
}

// detectThrottle flags peak-hour degradation. BASELINE is a HIGH-PERCENTILE anchor
// (the 75th percentile of per-hour DownAvg across hours with >= minSamples) — NOT the
// plain day median, which broad evening throttling would drag down and thereby mask
// itself. An hour with Samples >= minSamples AND DownAvg < ratio*baseline is flagged.
// Returns the flagged hours (sorted) + the baseline. PURE. Returns an empty (non-nil)
// slice when nothing qualifies.
func detectThrottle(hours []HourStat, ratio float64, minSamples int) (flagged []int, baselineDownMbps float64) {
	flagged = []int{}
	if minSamples < 1 {
		minSamples = 1
	}
	var downs []float64
	for _, h := range hours {
		if h.Samples >= minSamples {
			downs = append(downs, h.DownAvgMbps)
		}
	}
	if len(downs) == 0 {
		return flagged, 0
	}
	baselineDownMbps = percentile(downs, 0.75)
	threshold := ratio * baselineDownMbps
	for _, h := range hours {
		if h.Samples >= minSamples && h.DownAvgMbps < threshold {
			flagged = append(flagged, h.Hour)
		}
	}
	sort.Ints(flagged)
	return flagged, baselineDownMbps
}

// ---------------------------------------------------------------------------
// Impure layer — the SOLE new Cloudflare caller. Reached ONLY via the opt-in
// --throttle-watch watchdog (serve.go spawns runThrottleWatch under the flag).
// ---------------------------------------------------------------------------

// throttleDownURL returns the download URL with its `bytes=` parameter set to byteCap
// (the bounded small transfer). It falls back to the base URL unchanged on a parse
// error (the request would then use the configured bytes=, still bounded by the
// read-side cap below).
func throttleDownURL(base string, byteCap int) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("bytes", strconv.Itoa(byteCap))
	u.RawQuery = q.Encode()
	return u.String()
}

// sampleOneStream issues ONE bounded, ctx-aware, status-checked GET and returns the
// bytes read, the TTFB (ms, wall-clock to first response), and ok. A non-200
// (429/403/…) or any transport error => ok=false (recorded upstream, never a bogus
// slow number). It NEVER reads past want bytes (never an unbounded stream).
func sampleOneStream(sctx context.Context, client *http.Client, downURL string, want int) (bytesRead int64, ttfbMs float64, ok bool) {
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, downURL, nil)
	if err != nil {
		return 0, 0, false
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, false
	}
	ttfbMs = float64(time.Since(start).Microseconds()) / 1000.0
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { // 429/403/non-200 => recorded OK=false
		io.Copy(io.Discard, resp.Body)
		return 0, ttfbMs, false
	}
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			bytesRead += int64(n)
		}
		if bytesRead >= int64(want) { // read-side cap — never an unbounded stream
			break
		}
		if rerr != nil || sctx.Err() != nil {
			break
		}
	}
	return bytesRead, ttfbMs, true
}

// sampleDownload issues a bounded, ctx-aware, status-checked DOWNLOAD and returns
// (downloadMbps, latencyMsTTFB, ok). It threads the caller's ctx (so Ctrl-C /
// shutdown abandons an in-flight sample and wg.Wait returns promptly), applies a hard
// per-sample timeout, and honors cfg.ThrottleSampleStreams (clamped to 1..2): the
// bounded TOTAL byteCap=clampThrottleBytes(cfg.ThrottleSampleBytes) is SPLIT across
// the streams so the total transfer stays bounded at ~byteCap REGARDLESS of stream
// count (the §0 bounded-TOTAL-bytes invariant — more streams must NOT mean more
// Cloudflare bytes). Each stream caps its own transfer via a URL `bytes=` AND a
// read-side cap and checks resp.StatusCode; ANY stream that is non-200/errors makes
// the whole sample ok=false (a partial 429 is still a rate-limit, never a bogus slow
// number). Throughput is aggregate bytes / wall-clock; latency is the earliest TTFB.
// At most 1 (warm-up) + streams requests. Builds its OWN speedClient — reuse of
// downloadWorker/measureStream is FORBIDDEN.
func sampleDownload(ctx context.Context, cfg Config) (downMbps, latencyMs float64, ok bool) {
	byteCap := clampThrottleBytes(cfg.ThrottleSampleBytes)
	streams := clampThrottleStreams(cfg.ThrottleSampleStreams)
	client := speedClient(streams)

	sctx, cancel := context.WithTimeout(ctx, throttleSampleTimeout)
	defer cancel()

	// Warm-up GET (connection setup / TLS handshake) so TTFB on the measured GET is
	// not inflated by the handshake. Best-effort: result discarded, errors ignored.
	if cfg.SpeedLatencyURL != "" {
		if req, err := http.NewRequestWithContext(sctx, http.MethodGet, cfg.SpeedLatencyURL, nil); err == nil {
			if resp, derr := client.Do(req); derr == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}

	type streamResult struct {
		bytes int64
		ttfb  float64
		ok    bool
	}
	results := make([]streamResult, streams)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < streams; i++ {
		want := byteCap / streams
		if i == 0 {
			want += byteCap % streams // the first stream absorbs the rounding remainder
		}
		if want < 1 {
			want = 1
		}
		wg.Add(1)
		go func(idx, want int) {
			defer wg.Done()
			b, tt, okk := sampleOneStream(sctx, client, throttleDownURL(cfg.SpeedDownURL, want), want)
			results[idx] = streamResult{b, tt, okk}
		}(i, want)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	var total int64
	var minTTFB float64
	haveTTFB := false
	allOK := true
	for _, r := range results {
		total += r.bytes
		if !r.ok {
			allOK = false
		}
		if r.ok && (!haveTTFB || r.ttfb < minTTFB) {
			minTTFB = r.ttfb
			haveTTFB = true
		}
	}
	if !allOK {
		return 0, 0, false // any non-200/error stream => honest OK=false, never a slow number
	}
	return mbps(total, elapsed), minTTFB, true
}

// runThrottleSample wraps sampleDownload into a SpeedSample (T=now). ok=false =>
// {T, OK:false} with zero Mbps (recorded, excluded from stats). This is the injected
// production sampler (serveDashboard's throttleSampler seam defaults to it).
func runThrottleSample(ctx context.Context, cfg Config) SpeedSample {
	down, lat, ok := sampleDownload(ctx, cfg)
	if !ok {
		return SpeedSample{T: time.Now(), OK: false}
	}
	return SpeedSample{T: time.Now(), DownloadMbps: down, LatencyMs: lat, OK: true}
}

// runThrottleWatch is the watchdog goroutine body. A SINGLE short Ticker (interval
// << spacing so a battery->AC resume fires a due sample within ~1 tick). Per tick:
//  1. if !gate.allow() { continue } — PAUSED (battery/locked): append NOTHING, and
//     DO NOT touch lastAttemptT (a sample fires promptly once power returns, spaced
//     from the last REAL attempt). The gate is this goroutine's OWN reachGate — NEVER
//     shared with the reach/procs gates (reachGate is mutex-free by design).
//  2. eff := max(jitterSpacing, backoffSpacing); if !shouldSample { continue }.
//  3. lastAttemptT = now — ANCHOR ON THE ATTEMPT (incl. a 429): a rate-limited sample
//     still consumes its slot, so a persistent 429 never retries every tick.
//  4. s := sampler(ctx, cfg); store.appendSpeed(s); roll consecutiveFails for backoff.
//
// lastAttemptT is SEEDED at start from the newest persisted SpeedSnapshot T (incl.
// OK=false attempts) so spacing SURVIVES a restart. Joined on the serve WaitGroup;
// exits promptly on ctx cancel.
func runThrottleWatch(ctx context.Context, cfg Config, store TimeSeriesStore,
	gate *reachGate, sampler func(context.Context, Config) SpeedSample,
	minSpacing, jitterMax, maxBackoff time.Duration, seedLast time.Time) {

	lastAttemptT := seedLast
	consecutiveFails := 0
	t := time.NewTicker(throttleWatchTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !gate.allow() {
				continue // paused: nothing appended, lastAttemptT untouched
			}
			now := time.Now()
			eff := jitterSpacing(minSpacing, jitterMax, rand.Float64())
			if bo := backoffSpacing(minSpacing, maxBackoff, consecutiveFails); bo > eff {
				eff = bo
			}
			if !shouldSample(now, lastAttemptT, eff) {
				continue
			}
			lastAttemptT = now // ANCHOR ON THE ATTEMPT (incl. a 429)
			s := sampler(ctx, cfg)
			if err := store.appendSpeed(s); err != nil {
				fmt.Fprintf(os.Stderr, "netdebug: speed sample append failed: %v\n", err)
			}
			if s.OK {
				consecutiveFails = 0
			} else {
				consecutiveFails++
			}
		}
	}
}
