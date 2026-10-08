package main

import "time"

// ---------------------------------------------------------------------------
// TimeSeriesStore — the storage SEAM.
//
// The rest of the program (serve.go's newServeMux / serveDashboard, throttle.go's
// runThrottleWatch, the sampler sink) depends on this INTERFACE rather than on the
// concrete *historyStore. The method set is derived from, and limited to, exactly
// what those consumers actually call: nothing is exported here that no consumer
// needs. *historyStore remains the one and only implementation — this is a pure
// behavior-preserving seam so a future alternate backend could satisfy the same
// contract without touching its consumers.
//
// The compile-time assertion below makes the interface track the concrete type: if
// a historyStore method signature drifts, the build fails here rather than silently
// diverging.
type TimeSeriesStore interface {
	// ingest folds one per-sample reading into the open minute (the sampler sink).
	ingest(s Sample)

	// Snapshot returns copies of the closed minute buckets and the hour rollup.
	Snapshot() (minute, hour []Bucket)

	// FlushOpen persists the in-progress partial minute (shutdown path).
	FlushOpen() error

	// appendOutage durably records a closed outage.
	appendOutage(o Outage) error

	// OutagesSnapshot returns a copy of the recorded outages.
	OutagesSnapshot() []Outage

	// appendSpeed durably records a throttle speed sample.
	appendSpeed(s SpeedSample) error

	// SpeedSnapshot returns a copy of the recorded speed samples.
	SpeedSnapshot() []SpeedSample

	// appendRoam durably records a band/channel/BSSID change.
	appendRoam(r Transition) error

	// RoamsSnapshot returns a copy of the recorded band/channel changes.
	RoamsSnapshot() []Transition

	// prune ages out minute/hour buckets past their TTLs (promoting as needed).
	prune(now time.Time, minuteTTL, hourTTL time.Duration) error

	// Close flushes nothing but releases the single-instance flock.
	Close() error
}

// Compile-time assertion: *historyStore must satisfy TimeSeriesStore. Keeps the
// interface from silently drifting away from the concrete implementation.
var _ TimeSeriesStore = (*historyStore)(nil)
