package main

// Phase 9 — bufferbloat (latency-under-load) + meeting readiness. The single most
// predictive metric for an upload-heavy user who drops Slack Huddles: a link can
// read 800/150 and still ruin a call if saturating the UPSTREAM balloons RTT. This
// file grades that (idle RTT vs. RTT-while-saturated, A..F, upload headlined) and
// computes the live GOOD/RISKY/BAD meeting badge from Phase 7's idle jitter+loss.
//
// Pure/impure split (house style): BloatResult, gradeBufferbloat, worseGrade,
// buildBloatResult and meetingReady are PURE and table-tested in bufferbloat_test.go.
// runBufferbloat / printBufferbloat are the thin impure layer; runBufferbloat reuses
// speed.go's workers + speedClient + measureLatencyDuring and adds NO new HTTP stack.
//
// THE CENTRAL CORRECTNESS LESSON: every failure mode of this test wants to resolve
// to a confident FAST grade ("A") — a dead/throttled load phase, a defensive -Inf, a
// 429's tiny-body probe, a zero baseline. For an upload-heavy user a false "A" on a
// bloated link is the worst outcome: silent, reassuring, and wrong. The grading is
// built so every one of those paths resolves to "unknown" or "F", never "A".

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// gradeUnknown marks a direction with no valid, saturated measurement. On the wire
// the grade field is omitempty => the key is simply absent; printBufferbloat renders
// "unknown". It is NEVER a computed grade — "unknown" is a buildBloatResult decision
// about an invalid phase, made BEFORE gradeBufferbloat is ever called.
const gradeUnknown = ""

// Meeting-readiness status tokens — the EXACT strings the dashboard JS switches on
// (shared Go<->JS contract, defined ONCE here).
const (
	meetGood  = "GOOD"
	meetRisky = "RISKY"
	meetBad   = "BAD"
)

// Meeting thresholds (open Q8: the human may retune these; the table pins the
// current values). STRICT '<' is load-bearing — a boundary lands on the WORSE
// verdict, and a NaN/Inf input falls through to BAD (the safe value).
const (
	meetGoodJitterMs  = 20.0
	meetGoodLossPct   = 1.0
	meetRiskyJitterMs = 50.0
	meetRiskyLossPct  = 5.0
)

// Bufferbloat run tuning (impure path; not unit-tested against the network).
const (
	bloatProbeCount       = 6                // MEASURED probes per phase (one extra warm-up is discarded)
	bloatMinProbeSamples  = 3                // fewer valid 200-samples than this => the phase is "unknown"
	bloatMinSaturatedMbps = 0.5              // a phase below this never saturated => "unknown", never a grade
	bloatWarmup           = 1 * time.Second  // ramp-to-full-rate before the steady-state probe window (discarded)
	bloatSettle           = 2 * time.Second  // drain the queue between directions (uplink buffer must empty)
	bloatIdleBudget       = 20 * time.Second // hard cap on the idle-baseline measurement
)

// BloatResult is the test outcome. Per-direction grade is "" (gradeUnknown) when
// that phase produced no valid saturated measurement; a delta is omitempty and is
// populated ONLY for a valid direction (an exactly-0 valid delta is cosmetically
// omitted — the grade "A" still carries the signal; readers gate on the grade
// string, never on the delta alone).
type BloatResult struct {
	IdleRTTMs       float64 `json:"idle_rtt_ms,omitempty"`
	UpLoadedRTTMs   float64 `json:"up_loaded_rtt_ms,omitempty"`
	DownLoadedRTTMs float64 `json:"down_loaded_rtt_ms,omitempty"`
	UpDeltaMs       float64 `json:"up_delta_ms,omitempty"`   // UpLoadedRTT - Idle (valid dir only)
	DownDeltaMs     float64 `json:"down_delta_ms,omitempty"` // DownLoadedRTT - Idle (valid dir only)
	UpGrade         string  `json:"up_grade,omitempty"`      // A..F or "" (unknown)
	DownGrade       string  `json:"down_grade,omitempty"`
	Grade           string  `json:"grade,omitempty"` // overall = worseGrade(Up, Down)
	UploadMbps      float64 `json:"upload_mbps,omitempty"`
	DownloadMbps    float64 `json:"download_mbps,omitempty"`
	Streams         int     `json:"streams,omitempty"`
	Seconds         int     `json:"seconds_per_direction,omitempty"`
}

// gradeBufferbloat maps an RTT-increase-under-load (ms) to an A..F letter. It NEVER
// returns "unknown". GUARD ORDER IS LOAD-BEARING:
//  1. NaN || Inf (either sign) -> "F". MUST run FIRST — a naive `d < 5` maps -Inf to
//     "A" because -Inf < 5 is true, which is exactly the silent-fast-grade regression.
//  2. finite negative (measurement noise, loaded < idle) -> "A" (clamped).
//  3. strict-'<' ladder: <5 A, <30 B, <60 C, <100 D, <200 E, else F.
//
// Strict '<' => a boundary lands on the WORSE grade (exactly 5=>B, 30=>C, 60=>D,
// 100=>E, 200=>F). There is deliberately NO '<=' anywhere in the ladder.
func gradeBufferbloat(deltaMs float64) string {
	if math.IsNaN(deltaMs) || math.IsInf(deltaMs, 0) { // guard FIRST: covers BOTH Inf signs
		return "F"
	}
	if deltaMs < 0 { // finite negative noise clamps to the best grade
		return "A"
	}
	switch {
	case deltaMs < 5:
		return "A"
	case deltaMs < 30:
		return "B"
	case deltaMs < 60:
		return "C"
	case deltaMs < 100:
		return "D"
	case deltaMs < 200:
		return "E"
	default:
		return "F"
	}
}

// gradeRank ranks A..F as 0..5 (lower letter = better). Only ever called with a
// real A..F letter (worseGrade screens gradeUnknown first).
func gradeRank(g string) int { return strings.Index("ABCDEF", g) }

// worseGrade returns the lower (later-letter) of two grades. Domain is A..F PLUS
// gradeUnknown. An "" (unknown) direction is SKIPPED, never ranked as best:
//
//	worseGrade("", g) = g ; worseGrade(g, "") = g ; worseGrade("", "") = "".
//
// Over A..F it returns the worse letter: worse("A","F")="F", worse("C","B")="C",
// worse("B","B")="B".
func worseGrade(a, b string) string {
	if a == gradeUnknown {
		return b
	}
	if b == gradeUnknown {
		return a
	}
	if gradeRank(a) >= gradeRank(b) {
		return a
	}
	return b
}

// buildBloatResult assembles a BloatResult from idle + up-loaded + down-loaded RTTs,
// their per-measurement VALIDITY, and the measured Mbps (PURE: no network). Validity
// is explicit so a failed phase degrades to "unknown" rather than silently grading a
// bogus delta. Rules:
//   - !idleOK || idle <= 0 => the WHOLE test is ungradable (no valid baseline):
//     UpGrade = DownGrade = Grade = "" (unknown); no deltas, no idle emitted. (A raw
//     0.0 float cannot encode "absent", which is why idleOK is a separate bool —
//     delta = loaded - 0 would otherwise fabricate a grade off a garbage baseline,
//     and loaded 0 - idle would clamp to a bogus "A".)
//   - per direction: !ok => that direction grade "" (unknown), delta omitted.
//   - valid direction: delta = loaded - idle; grade = gradeBufferbloat(delta).
//   - Grade = worseGrade(UpGrade, DownGrade) (unknown-skip; both unknown => "").
func buildBloatResult(idle float64, idleOK bool, upLoaded float64, upOK bool,
	downLoaded float64, downOK bool, upMbps, downMbps float64) BloatResult {

	r := BloatResult{UploadMbps: upMbps, DownloadMbps: downMbps}

	// No valid baseline: the test is ungradable in BOTH directions. Grades stay ""
	// (unknown) and no delta is computed against a zero/absent baseline.
	if !idleOK || idle <= 0 {
		return r
	}
	r.IdleRTTMs = idle

	if upOK {
		r.UpLoadedRTTMs = upLoaded
		r.UpDeltaMs = upLoaded - idle
		r.UpGrade = gradeBufferbloat(r.UpDeltaMs)
	}
	if downOK {
		r.DownLoadedRTTMs = downLoaded
		r.DownDeltaMs = downLoaded - idle
		r.DownGrade = gradeBufferbloat(r.DownDeltaMs)
	}
	r.Grade = worseGrade(r.UpGrade, r.DownGrade)
	return r
}

// meetBand classifies one dimension into 0 (good), 1 (risky) or 2 (bad) against its
// two strict-'<' thresholds. A NaN/Inf value yields 2 (bad) because NaN<x is always
// false — the safe fall-through this whole feature depends on.
func meetBand(v, goodLt, riskyLt float64) int {
	if v < goodLt {
		return 0
	}
	if v < riskyLt {
		return 1
	}
	return 2
}

// meetingReady maps idle jitter + Internet loss to a readiness verdict + the driving
// reason. Inputs are the Phase-7 ReachSample's JitterMs and InternetLoss (NOT
// GatewayLoss — gateway loss is ~0% even when the uplink is lossy). STRICT '<' is
// load-bearing (a NaN/Inf jitter or loss falls through to BAD, the safe value):
//
//	GOOD  = jitter<20 AND loss<1
//	RISKY = jitter<50 AND loss<5   (and not GOOD)
//	else BAD
//
// WORST DIMENSION WINS (high loss with low jitter is still >=RISKY). The reason NAMES
// the dominating (worse) dimension; on a tie it names LOSS, since a lossy call path
// is the harsher symptom. GOOD carries an informative, non-over-promising reason.
//
// JitterMs==0 AMBIGUITY: a 0 jitter means truly smooth OR <2 captured RTTs OR
// internet down with jitter omitted. The internet-down case => BAD via loss (good),
// but a 1-reply 0%-loss sample can read optimistically GOOD — do not over-trust a
// near-empty sample; the badge is an INDICATOR, not a guarantee.
func meetingReady(jitterMs, lossPct float64) (status, reason string) {
	jBand := meetBand(jitterMs, meetGoodJitterMs, meetRiskyJitterMs)
	lBand := meetBand(lossPct, meetGoodLossPct, meetRiskyLossPct)

	worst := jBand
	if lBand > worst {
		worst = lBand
	}
	if worst == 0 {
		return meetGood, "jitter/loss within call limits"
	}
	if worst == 1 {
		status = meetRisky
	} else {
		status = meetBad
	}
	// Dominating dimension: jitter only when it is STRICTLY worse; ties (and loss
	// strictly worse) name loss, the harsher symptom.
	if jBand > lBand {
		reason = meetReasonDim("jitter", jitterMs, " ms", worst)
	} else {
		reason = meetReasonDim("loss", lossPct, "%", worst)
	}
	return status, reason
}

// meetReasonDim renders a human reason naming the offending dimension and its
// severity, guarding non-finite values so a NaN never prints as "NaN".
func meetReasonDim(name string, v float64, unit string, band int) string {
	sev := "elevated"
	if band >= 2 {
		sev = "high"
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return name + " " + sev + " (unreadable)"
	}
	return fmt.Sprintf("%s %s (%.1f%s)", name, sev, v, unit)
}

// ---- impure layer (thin, NOT unit-tested directly) ------------------------

// runBufferbloat generates load and measures loaded latency, REUSING speed.go's
// workers + speedClient + measureLatencyDuring. It CANNOT reuse measureStream (that
// blocks for `seconds`, self-cancels its own context and does NOT join its workers —
// leaving no window to probe during saturation and leaking a still-pushing upload
// POST into the next phase). This runner owns its own ctx + WaitGroup per phase.
//
// A DEDICATED probe client (its own speedClient, never the worker client) measures
// the loaded RTT: the probe MUST share the network bottleneck (that queueing delay IS
// the measurement) but is isolated at the LOCAL transport so it is not head-of-line
// blocked behind a mid-POST worker connection. measureLatencyDuring is wall-clock
// only (ignores Server-Timing cfL4, which under-reports upload bloat), rejects
// non-200 (a 429's tiny fast body must not poison the latency path) and returns a
// valid-sample count so a failed phase degrades to "unknown", never a bogus fast "A".
func runBufferbloat(cfg Config) BloatResult {
	probeClient := speedClient(cfg.SpeedStreams) // DEDICATED probe transport
	workClient := speedClient(cfg.SpeedStreams)  // saturating workers

	// 1. IDLE baseline with NO load running. Same helper + endpoint + method as the
	//    loaded probe, so the delta is apples-to-apples.
	idleCtx, idleCancel := context.WithTimeout(context.Background(), bloatIdleBudget)
	idle, idleN := measureLatencyDuring(idleCtx, probeClient, cfg.SpeedLatencyURL, bloatProbeCount)
	idleCancel()
	idleOK := idleN >= bloatMinProbeSamples && idle > 0

	// 2. UPLOAD phase (PRIMARY): saturate the uplink, probe RTT in steady state.
	upLoaded, upN, upMbps := bloatLoadPhase(cfg, workClient, probeClient, uploadWorker, cfg.SpeedUpURL)
	upOK := upN >= bloatMinProbeSamples && upMbps >= bloatMinSaturatedMbps

	// 3. SETTLE/DRAIN: let the uplink queue empty before the download phase, so the
	//    download delta is not measured over a still-draining upload buffer. Both
	//    deltas share the SINGLE pre-load idle baseline.
	time.Sleep(bloatSettle)

	// 4. DOWNLOAD phase: same shape against the download endpoint.
	downLoaded, downN, downMbps := bloatLoadPhase(cfg, workClient, probeClient, downloadWorker, cfg.SpeedDownURL)
	downOK := downN >= bloatMinProbeSamples && downMbps >= bloatMinSaturatedMbps

	r := buildBloatResult(idle, idleOK, upLoaded, upOK, downLoaded, downOK, upMbps, downMbps)
	r.Streams = cfg.SpeedStreams
	r.Seconds = cfg.SpeedSeconds
	return r
}

// bloatLoadPhase saturates one direction with cfg.SpeedStreams workers, then — after
// a warm-up ramp, in the saturated steady state — probes the loaded RTT CONCURRENTLY
// while sampling the byte counter over the same window for throughput. It then
// cancels AND JOINS the workers (a bare cancel leaves an in-flight POST pushing bytes
// into the next phase). Returns the loaded avg RTT, the valid-sample count, and Mbps.
func bloatLoadPhase(cfg Config, workClient, probeClient *http.Client,
	worker func(context.Context, *http.Client, string, *int64), url string) (loadedRTT float64, okCount int, throughput float64) {

	ctx, cancel := context.WithCancel(context.Background())
	var counter int64
	var wg sync.WaitGroup
	for i := 0; i < cfg.SpeedStreams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, workClient, url, &counter)
		}()
	}

	sleepCtx(ctx, bloatWarmup) // ramp to full rate; not sampled

	// Steady state: probe loaded RTT while sampling throughput over the SAME window.
	window := time.Duration(cfg.SpeedSeconds) * time.Second
	if window <= 0 {
		window = time.Second
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, window)
	startBytes := atomic.LoadInt64(&counter)
	startT := time.Now()
	loadedRTT, okCount = measureLatencyDuring(probeCtx, probeClient, cfg.SpeedLatencyURL, bloatProbeCount)
	elapsed := time.Since(startT).Seconds()
	endBytes := atomic.LoadInt64(&counter)
	probeCancel()
	throughput = mbps(endBytes-startBytes, elapsed)

	cancel()  // stop the workers
	wg.Wait() // JOIN before returning — no leftover POST into the next phase
	return loadedRTT, okCount, throughput
}

// printBufferbloat renders the human report (mirrors printSpeed). It LEADS with the
// UPLOAD idle/loaded/delta/grade (the upload-heavy headline), shows download as
// secondary, NAMES which direction drove the overall Grade, and ends with a one-line
// verdict. An "unknown" direction prints "unknown (load phase failed / throttled)",
// never a letter.
func printBufferbloat(r BloatResult) {
	fmt.Println("================ BUFFERBLOAT TEST ================")
	fmt.Printf("Server    : Cloudflare (speed.cloudflare.com), %d streams, %ds/direction\n", r.Streams, r.Seconds)
	if r.IdleRTTMs > 0 {
		fmt.Printf("Idle RTT  : %.1f ms (baseline)\n", r.IdleRTTMs)
	} else {
		fmt.Printf("Idle RTT  : unknown (no valid baseline — test ungradable)\n")
	}

	fmt.Println("-- UPLOAD (primary) --")
	printBloatDirection(r.UpGrade, r.IdleRTTMs, r.UpLoadedRTTMs, r.UpDeltaMs, r.UploadMbps, r.IdleRTTMs > 0)
	fmt.Println("-- download --")
	printBloatDirection(r.DownGrade, r.IdleRTTMs, r.DownLoadedRTTMs, r.DownDeltaMs, r.DownloadMbps, r.IdleRTTMs > 0)

	if r.Grade == gradeUnknown {
		fmt.Printf("Overall   : unknown (no valid saturated measurement)\n")
	} else {
		driver := bloatDriver(r)
		if driver != "" {
			fmt.Printf("Overall   : %s  (driven by %s)\n", r.Grade, driver)
		} else {
			fmt.Printf("Overall   : %s\n", r.Grade)
		}
	}
	fmt.Printf("Verdict   : %s\n", bloatVerdict(r.Grade))
}

// printBloatDirection renders one direction's line. An unknown grade (failed/
// throttled/un-saturated phase, or no baseline) never prints a letter.
func printBloatDirection(grade string, idle, loaded, delta, mbps float64, haveBaseline bool) {
	if grade == gradeUnknown {
		if !haveBaseline {
			fmt.Printf("  grade unknown (no valid idle baseline)\n")
		} else {
			fmt.Printf("  grade unknown (load phase failed / throttled)\n")
		}
		if mbps > 0 {
			fmt.Printf("  throughput %.1f Mbps\n", mbps)
		}
		return
	}
	fmt.Printf("  loaded RTT %.1f ms   (idle %.1f ms, +%.1f ms under load)   grade %s\n",
		loaded, idle, delta, grade)
	if mbps > 0 {
		fmt.Printf("  throughput %.1f Mbps\n", mbps)
	}
}

// bloatDriver names the direction(s) whose grade equals the overall worse grade.
func bloatDriver(r BloatResult) string {
	if r.Grade == gradeUnknown {
		return ""
	}
	up := r.UpGrade == r.Grade
	down := r.DownGrade == r.Grade
	switch {
	case up && down:
		return "upload and download"
	case up:
		return "upload"
	case down:
		return "download"
	default:
		return ""
	}
}

// bloatVerdict is the one-line plain-language summary keyed by the overall grade.
func bloatVerdict(grade string) string {
	switch grade {
	case gradeUnknown:
		return "could not measure latency under load (phase failed or throttled) — rerun when the link is idle"
	case "A", "B":
		return "low latency under load — calls should hold up"
	case "C", "D":
		return "noticeable latency under load — calls may stutter when the link is busy"
	default: // E, F
		return "severe latency under load (bufferbloat) — expect dropped/garbled calls while uploading"
	}
}
