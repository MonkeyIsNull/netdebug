package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// SpeedResult holds the outcome of a throughput test.
type SpeedResult struct {
	LatencyMs    float64   `json:"latency_ms"`
	JitterMs     float64   `json:"jitter_ms"`
	DownloadMbps float64   `json:"download_mbps"`
	DownMin      float64   `json:"download_min_mbps"`
	DownMax      float64   `json:"download_max_mbps"`
	UploadMbps   float64   `json:"upload_mbps"`
	UpMin        float64   `json:"upload_min_mbps"`
	UpMax        float64   `json:"upload_max_mbps"`
	DownSamples  []float64 `json:"download_samples"`
	UpSamples    []float64 `json:"upload_samples"`
	Streams      int       `json:"streams"`
	Seconds      int       `json:"seconds_per_direction"`
}

// mbps converts a byte count over a duration into megabits per second.
func mbps(bytes int64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(bytes) * 8 / 1e6 / seconds
}

// speedStats returns avg/min/max and jitter (stddev) over per-second samples.
func speedStats(samples []float64) (avg, min, max, jitter float64) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	min, max = samples[0], samples[0]
	var sum float64
	for _, v := range samples {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
		sum += v
	}
	avg = sum / float64(len(samples))
	var sq float64
	for _, v := range samples {
		d := v - avg
		sq += d * d
	}
	jitter = math.Sqrt(sq / float64(len(samples)))
	return avg, min, max, jitter
}

// speedClient builds an HTTP client tuned for throughput: no overall timeout
// (streams are bounded by context), and HTTP/2 disabled so each stream is its
// own TCP connection — which saturates the link far better than one multiplexed
// HTTP/2 connection.
func speedClient(streams int) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: streams + 2,
		IdleConnTimeout:     30 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // force HTTP/1.1
	}
	return &http.Client{Transport: tr}
}

// sleepCtx waits d, or returns early if ctx is cancelled (the window closed).
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// downloadWorker continuously GETs url, counting bytes read, until ctx is done.
// A failed request or a non-200 response (Cloudflare rejects an oversized __down
// with 403 or a too-frequent one with 429, returning a tiny body whose bytes
// would poison the throughput number) is not counted: the worker backs off
// briefly and retries, so a transient hiccup degrades the stream rather than
// killing it for the rest of the window.
func downloadWorker(ctx context.Context, client *http.Client, url string, counter *int64) {
	buf := make([]byte, 128*1024)
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			sleepCtx(ctx, 100*time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			sleepCtx(ctx, 250*time.Millisecond)
			continue
		}
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				atomic.AddInt64(counter, int64(n))
			}
			if rerr != nil || ctx.Err() != nil {
				break
			}
		}
		resp.Body.Close()
	}
}

// zeroCounter is a request body that streams zeros (up to limit) while counting
// bytes handed to the transport, stopping when ctx is cancelled.
type zeroCounter struct {
	ctx       context.Context
	counter   *int64
	remaining int64
}

func (z *zeroCounter) Read(p []byte) (int, error) {
	if z.ctx.Err() != nil || z.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > z.remaining {
		n = int(z.remaining)
	}
	for i := range p[:n] {
		p[i] = 0
	}
	z.remaining -= int64(n)
	atomic.AddInt64(z.counter, int64(n))
	return n, nil
}

// uploadWorker continuously POSTs zeros to url, counting bytes sent, until done.
func uploadWorker(ctx context.Context, client *http.Client, url string, counter *int64) {
	for ctx.Err() == nil {
		body := &zeroCounter{ctx: ctx, counter: counter, remaining: 100 * 1024 * 1024}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := client.Do(req)
		if err != nil {
			sleepCtx(ctx, 100*time.Millisecond)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// measureStream runs `streams` workers for `seconds`, sampling throughput once
// per second (after a 1s warm-up discarded from the stats) and returns the
// per-second aggregate avg/min/max plus the raw samples.
//
// CTX-AWARE (post-roadmap speed-button): the window derives from the caller's ctx
// (a child WithCancel), so a cancelled parent (client disconnect / server shutdown /
// hard cap on the on-demand button) stops the workers AND breaks the sampling loop
// promptly via sleepCtx, instead of the old context.Background() that nothing could
// abort. For the one-shot --speed caller (Background ctx) the behavior is byte-
// identical: sleepCtx on a never-cancelled ctx sleeps the full second, as before.
func measureStream(ctx context.Context, client *http.Client, streams, seconds int,
	worker func(context.Context, *http.Client, string, *int64), url string) (avg, min, max float64, samples []float64) {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var counter int64
	for i := 0; i < streams; i++ {
		go worker(ctx, client, url, &counter)
	}

	sleepCtx(ctx, 1*time.Second) // warm-up (ramp to full rate); not sampled
	prev := atomic.LoadInt64(&counter)
	for s := 0; s < seconds; s++ {
		sleepCtx(ctx, 1*time.Second)
		if ctx.Err() != nil {
			break // window cancelled (disconnect / shutdown / hard cap): stop promptly
		}
		cur := atomic.LoadInt64(&counter)
		samples = append(samples, mbps(cur-prev, 1.0))
		prev = cur
	}
	cancel()
	avg, min, max, _ = speedStats(samples)
	return avg, min, max, samples
}

// cfServerTimingRTT extracts a microsecond RTT field (e.g. "rtt" or "min_rtt")
// from Cloudflare's `Server-Timing: cfL4;desc="?proto=TCP&rtt=<us>&min_rtt=<us>..."`
// response header and returns it in milliseconds. This is the TCP-level network
// round-trip Cloudflare itself measures — well below a full HTTP round-trip,
// which also includes edge+worker processing — and matches the latency its own
// speed test reports. Returns false when the field is absent or unparseable.
func cfServerTimingRTT(values []string, field string) (float64, bool) {
	needle := field + "="
	for _, v := range values {
		for i := 0; ; {
			k := strings.Index(v[i:], needle)
			if k < 0 {
				break
			}
			pos := i + k
			// Require a field boundary before the match so "rtt=" does not
			// accidentally match inside "min_rtt=".
			if pos == 0 || v[pos-1] == '?' || v[pos-1] == '&' {
				rest := v[pos+len(needle):]
				j := 0
				for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
					j++
				}
				if j > 0 {
					if us, err := strconv.Atoi(rest[:j]); err == nil {
						return float64(us) / 1000.0, true
					}
				}
			}
			i = pos + len(needle)
		}
	}
	return 0, false
}

// measureLatency times `n` tiny requests and returns avg + jitter (stddev) in ms.
// When Cloudflare reports its TCP RTT in a Server-Timing header, that network
// round-trip is used; otherwise the full HTTP round-trip is the fallback. One
// extra warm-up request is sent first and discarded: it pays the TLS handshake /
// connection-setup cost that would otherwise skew the result upward.
func measureLatency(parent context.Context, client *http.Client, url string, n int) (avg, jitter float64) {
	var samples []float64
	for i := 0; i < n+1; i++ {
		if parent.Err() != nil {
			break // parent cancelled (disconnect / shutdown / hard cap): stop promptly
		}
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		start := time.Now()
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			rttMs, ok := cfServerTimingRTT(resp.Header.Values("Server-Timing"), "rtt")
			if !ok {
				rttMs = float64(time.Since(start).Microseconds()) / 1000.0
			}
			resp.Body.Close()
			if i > 0 { // discard warm-up sample
				samples = append(samples, rttMs)
			}
		}
		cancel()
	}
	avg, _, _, jitter = speedStats(samples)
	return avg, jitter
}

// loadedProbeTimeout is the per-request cap for measureLatencyDuring. It is LONGER
// than measureLatency's 5s on purpose: under severe bufferbloat the slowest probes
// ARE the signal, and a short timeout would drop them and bias the loaded RTT DOWN
// exactly when bloat is worst (a false fast grade). A probe that exceeds this is
// recorded as a ceiling-RTT sample (evidence of extreme bloat => high delta => F),
// never silently dropped.
const loadedProbeTimeout = 12 * time.Second

// measureLatencyDuring times `n` probes (plus one discarded warm-up) and returns the
// wall-clock avg RTT in ms and the count of VALID samples. It is a thin variant of
// measureLatency built for the bufferbloat loaded-probe path, with three corrections
// the verbatim measureLatency cannot provide:
//
//  1. WALL-CLOCK ONLY. It IGNORES Cloudflare's `Server-Timing: cfL4` rtt — that edge
//     TCP RTT is measured on a near-idle tiny-probe connection and does NOT sit in
//     your saturated uplink buffer, so reusing it systematically UNDER-reports upload
//     bufferbloat. Timing the application round-trip with time.Since keeps idle vs.
//     loaded method-consistent, so the delta actually rises with queueing.
//  2. REJECT NON-200. During saturation Cloudflare is most likely to 429 the
//     concurrent probe GET; a 429's tiny body returns FAST and, if counted, becomes a
//     bogus low RTT => bogus small delta => false "A". Every non-200 is dropped.
//  3. RETURN A SAMPLE COUNT + ctx-bound requests. The count lets the runner mark a
//     too-few-samples phase as "unknown" rather than reading an empty-sample avg of 0
//     as a perfect link. Each request uses the caller's ctx so the probe stops
//     promptly when the load window ends (and stops drawing on the 429 budget).
//
// A genuine per-request TIMEOUT (not a window-cancel) is recorded as a ceiling sample
// so extreme bloat grades F; a window-cancel (parent ctx done) is dropped.
func measureLatencyDuring(ctx context.Context, client *http.Client, url string, n int) (avgMs float64, okCount int) {
	var samples []float64
	for i := 0; i < n+1; i++ { // i==0 is a discarded warm-up (pays connection setup), as in measureLatency
		if ctx.Err() != nil {
			break // window closed; stop promptly
		}
		rctx, cancel := context.WithTimeout(ctx, loadedProbeTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			continue
		}
		start := time.Now()
		resp, err := client.Do(req)
		elapsed := float64(time.Since(start).Microseconds()) / 1000.0
		if err != nil {
			// Ceiling sample ONLY for a real probe timeout (evidence of extreme bloat),
			// never for a window-cancel: when the PARENT ctx is done, rctx.Err() is
			// Canceled, not DeadlineExceeded, so that path is correctly dropped.
			if i > 0 && ctx.Err() == nil && rctx.Err() == context.DeadlineExceeded {
				samples = append(samples, elapsed)
				okCount++
			}
			cancel()
			continue
		}
		io.Copy(io.Discard, resp.Body)
		status := resp.StatusCode
		resp.Body.Close()
		if i > 0 && status == http.StatusOK { // wall-clock only; non-200 dropped
			samples = append(samples, elapsed)
			okCount++
		}
		cancel()
	}
	avgMs, _, _, _ = speedStats(samples)
	return avgMs, okCount
}

// runSpeed executes the full latency/download/upload measurement. ctx threads
// through measureLatency/measureStream so the whole run is cancellable (the
// on-demand speed button wraps it in a hard-cap WithTimeout; the --speed one-shot
// passes context.Background() and so is unaffected).
func runSpeed(ctx context.Context, cfg Config) SpeedResult {
	client := speedClient(cfg.SpeedStreams)
	r := SpeedResult{Streams: cfg.SpeedStreams, Seconds: cfg.SpeedSeconds}

	r.LatencyMs, r.JitterMs = measureLatency(ctx, client, cfg.SpeedLatencyURL, 8)
	r.DownloadMbps, r.DownMin, r.DownMax, r.DownSamples =
		measureStream(ctx, client, cfg.SpeedStreams, cfg.SpeedSeconds, downloadWorker, cfg.SpeedDownURL)
	r.UploadMbps, r.UpMin, r.UpMax, r.UpSamples =
		measureStream(ctx, client, cfg.SpeedStreams, cfg.SpeedSeconds, uploadWorker, cfg.SpeedUpURL)
	return r
}

// printSpeed renders a human-readable speed report.
func printSpeed(cfg Config, r SpeedResult) {
	fmt.Println("==================== SPEED TEST ====================")
	fmt.Printf("Server    : Cloudflare (speed.cloudflare.com), %d parallel streams, %ds/direction\n",
		r.Streams, r.Seconds)
	fmt.Printf("Latency   : %.1f ms   (jitter %.1f ms)\n", r.LatencyMs, r.JitterMs)
	fmt.Printf("Download  : %6.1f Mbps   (min %.0f / max %.0f)\n", r.DownloadMbps, r.DownMin, r.DownMax)
	fmt.Printf("Upload    : %6.1f Mbps   (min %.0f / max %.0f)\n", r.UploadMbps, r.UpMin, r.UpMax)
}
