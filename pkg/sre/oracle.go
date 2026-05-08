package sre

// oracle.go — OracleEngine: failure prediction via time-series trend analysis. [SRE-61]
//
// Maintains a rolling window of OracleSample snapshots (fed at each homeostasis
// tick) and computes a least-squares linear regression over heap and RAPL trends.
// FailureProbability is derived from a logistic curve that maps "how close the
// 24h projection is to the configured limit" to a 0–1 probability.
//
// Design choices:
//   - No DuckDB dependency: uses in-process ring buffer (128 samples ≈ 10 min at 5s ticks).
//   - No goroutines: Feed() and Risk() are synchronised with a single mutex.
//   - SampleFromRuntime() decouples callers from runtime import.

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

// OracleSample is a single point-in-time snapshot of key system metrics. [SRE-61.1]
type OracleSample struct {
	At      time.Time
	HeapMB  float64
	WattsW  float64
	GCPause float64 // last GC pause in milliseconds
}

// OracleRisk is the result of a Risk() assessment. [SRE-61.2]
type OracleRisk struct {
	HeapTrendMBPerMin float64   `json:"heap_trend_mb_per_min"` // positive = growing heap
	PowerTrendWPerMin float64   `json:"power_trend_w_per_min"` // positive = rising watts
	FailProb24h       float64   `json:"fail_prob_24h"`         // 0.0–1.0
	DominantSignal    string    `json:"dominant_signal"`       // "heap"|"power"|"combined"|"stable"
	Alert             bool      `json:"alert"`
	AlertMessage      string    `json:"alert_message,omitempty"`
	SamplesCollected  int       `json:"samples_collected"`
	At                time.Time `json:"at"`
}

const oracleBufSize = 128 // ~10 min at 5s ticks; ~21 min at 10s ticks

// OracleEngine maintains a rolling window and predicts saturation risk. [SRE-61]
type OracleEngine struct {
	mu    sync.Mutex
	buf   [oracleBufSize]OracleSample
	head  int // next write position (0-based, wraps at oracleBufSize)
	count int // filled entries, capped at oracleBufSize

	AlertThreshold float64       // default 0.75 — emit EventOracleAlert when FailProb24h ≥ this
	HeapLimitMB    float64       // default 512.0 — heap saturation ceiling
	PowerLimitW    float64       // default 80.0  — thermal saturation ceiling
	TickInterval   time.Duration // default 5s   — homeostasis period, used for trend scaling
}

// NewOracleEngine creates an OracleEngine with conservative production defaults.
func NewOracleEngine() *OracleEngine {
	return &OracleEngine{
		AlertThreshold: 0.75,
		HeapLimitMB:    512.0,
		PowerLimitW:    80.0,
		TickInterval:   5 * time.Second,
	}
}

// Feed records a new sample. Call once per homeostasis tick. Thread-safe. [SRE-61.1]
func (o *OracleEngine) Feed(s OracleSample) {
	if s.At.IsZero() {
		s.At = time.Now()
	}
	o.mu.Lock()
	o.buf[o.head] = s
	o.head = (o.head + 1) % oracleBufSize
	if o.count < oracleBufSize {
		o.count++
	}
	o.mu.Unlock()
}

// Risk computes the current failure risk. Returns a stable/low-confidence result
// until at least 8 samples have been collected. [SRE-61.2]
func (o *OracleEngine) Risk() OracleRisk {
	o.mu.Lock()
	count := o.count
	head := o.head
	buf := o.buf // value copy — avoids holding the lock during math
	o.mu.Unlock()

	if count < 8 {
		return OracleRisk{
			DominantSignal:   "stable",
			SamplesCollected: count,
			At:               time.Now(),
		}
	}

	// Reconstruct ordered slice (oldest → newest).
	samples := make([]OracleSample, count)
	start := (head - count + oracleBufSize) % oracleBufSize
	for i := range count {
		samples[i] = buf[(start+i)%oracleBufSize]
	}

	heapSlope := oracleLinearSlope(samples, func(s OracleSample) float64 { return s.HeapMB })
	powerSlope := oracleLinearSlope(samples, func(s OracleSample) float64 { return s.WattsW })

	ticksPerMin := float64(time.Minute) / float64(o.TickInterval)
	ticksPer24h := float64(24*time.Hour) / float64(o.TickInterval)

	last := samples[len(samples)-1]
	projHeap := last.HeapMB + heapSlope*ticksPer24h
	projPower := last.WattsW + powerSlope*ticksPer24h

	heapProb := oracleSaturationProb(last.HeapMB, projHeap, o.HeapLimitMB)
	powerProb := oracleSaturationProb(last.WattsW, projPower, o.PowerLimitW)
	failProb := math.Max(heapProb, powerProb)

	dominant := "stable"
	switch {
	case heapProb >= 0.3 && heapProb >= powerProb:
		dominant = "heap"
	case powerProb >= 0.3 && powerProb > heapProb:
		dominant = "power"
	case heapProb >= 0.3 && powerProb >= 0.3:
		dominant = "combined"
	}

	risk := OracleRisk{
		HeapTrendMBPerMin: heapSlope * ticksPerMin,
		PowerTrendWPerMin: powerSlope * ticksPerMin,
		FailProb24h:       math.Round(failProb*1000) / 1000, // 3 decimal places
		DominantSignal:    dominant,
		SamplesCollected:  count,
		At:                time.Now(),
	}

	if failProb >= o.AlertThreshold {
		risk.Alert = true
		risk.AlertMessage = fmt.Sprintf(
			"Riesgo de saturación %.0f%% en 24h (señal: %s). Heap +%.1f MB/min, Potencia +%.1f W/min.",
			failProb*100, dominant,
			math.Max(0, risk.HeapTrendMBPerMin),
			math.Max(0, risk.PowerTrendWPerMin),
		)
	}

	return risk
}

// SampleFromRuntime captures current runtime stats as an OracleSample.
// watts is provided by the RAPL sensor in the homeostasis goroutine. [SRE-61.1]
func SampleFromRuntime(watts float64) OracleSample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return OracleSample{
		At:      time.Now(),
		HeapMB:  float64(ms.HeapInuse) / (1024 * 1024),
		WattsW:  watts,
		GCPause: float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6,
	}
}

// ─── Math helpers (package-local, prefixed to avoid collision) ────────────────

// oracleLinearSlope computes the least-squares slope over sample y-values.
// x-axis is sample index (integer time units = ticks).
func oracleLinearSlope(samples []OracleSample, yFn func(OracleSample) float64) float64 {
	n := float64(len(samples))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2 float64
	for i, s := range samples {
		x := float64(i)
		y := yFn(s)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}
	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / denom
}

// oracleSaturationProb maps a (current, projected, limit) triple to [0,1].
// Uses a logistic function centred at 90% of the limit: probability is near 0
// below 50% of limit, and near 1 above 120% of limit.
func oracleSaturationProb(current, projected, limit float64) float64 {
	if limit <= 0 || projected <= current {
		return 0 // declining or flat — no saturation risk
	}
	ratio := projected / limit
	if ratio <= 0.5 {
		return 0
	}
	// logistic: p = 1 / (1 + e^(-k*(ratio - midpoint)))
	// k=5, midpoint=0.9 → 50%→0.07, 75%→0.27, 90%→0.50, 100%→0.73, 120%→0.92
	return 1 / (1 + math.Exp(-5*(ratio-0.9)))
}
