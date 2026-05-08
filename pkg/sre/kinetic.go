// Package sre — Kinetic SRE: hardware bio-feedback engine. [SRE-44]
//
// Analyzes the spectral signature of power consumption (RAPL) and memory
// allocation patterns to predict anomalies before they manifest. Uses a
// sliding window DFT to detect abnormal frequency components in Watts and
// heap growth, triggering preemptive actions (GC, load shedding, abort).
//
// Inspired by biometric anomaly detection — the system has a "healthy baseline"
// and any deviation beyond 2σ triggers investigation.
package sre

import (
	"fmt"
	"log"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// KineticSensor collects hardware bio-metrics in a sliding window. [SRE-44.1]
type KineticSensor struct {
	cfg         config.KineticConfig
	mu          sync.Mutex
	powerWindow [256]float64 // RAPL watts samples
	heapWindow  [256]float64 // heap MB samples
	gcWindow    [256]float64 // GC pause ns samples
	windowIdx   atomic.Uint32

	// Baseline statistics (healthy state)
	baselinePower SpectralBaseline
	baselineHeap  SpectralBaseline

	// Detection state
	anomalyCount atomic.Int32
	lastAnomaly  atomic.Int64
}

// SpectralBaseline stores the "healthy" frequency profile. [SRE-44.1]
type SpectralBaseline struct {
	mu          sync.RWMutex
	meanMag     float64 // mean spectral magnitude
	stdMag      float64 // standard deviation of spectral magnitudes
	peakFreq    int     // dominant frequency bin
	sampleCount int     // how many calibration samples
	calibrated  bool
}

// AnomalyReport describes a detected hardware anomaly. [SRE-44.2]
type AnomalyReport struct {
	Timestamp    int64   `json:"timestamp"`
	Type         string  `json:"type"`     // "power_anomaly", "heap_anomaly", "gc_anomaly"
	Severity     float64 `json:"severity"` // number of σ above baseline
	CurrentValue float64 `json:"current_value"`
	BaselineMean float64 `json:"baseline_mean"`
	BaselineStd  float64 `json:"baseline_std"`
	Action       string  `json:"action"` // recommended action
	Description  string  `json:"description"`
}

// KineticStats provides a summary of the sensor state. [SRE-44.1]
type KineticStats struct {
	SamplesCollected int     `json:"samples_collected"`
	PowerCalibrated  bool    `json:"power_calibrated"`
	HeapCalibrated   bool    `json:"heap_calibrated"`
	AnomalyCount     int     `json:"anomaly_count"`
	LastAnomalyAge   string  `json:"last_anomaly_age"`
	CurrentHeapMB    float64 `json:"current_heap_mb"`
	CurrentGCPause   float64 `json:"current_gc_pause_us"`
}

// NewKineticSensor creates the hardware bio-feedback sensor. [SRE-44.1]
func NewKineticSensor(kcfg config.KineticConfig) *KineticSensor {
	return &KineticSensor{cfg: kcfg}
}

// Sample collects a single hardware bio-metric reading. [SRE-44.1]
// Should be called periodically (e.g., every 1s from the homeostasis loop).
func (ks *KineticSensor) Sample() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	idx := ks.windowIdx.Add(1) - 1
	pos := idx % 256

	ks.mu.Lock()
	ks.heapWindow[pos] = float64(mem.HeapAlloc) / (1024 * 1024)
	ks.gcWindow[pos] = float64(mem.PauseNs[(mem.NumGC+255)%256]) / 1000 // µs
	ks.powerWindow[pos] = readRAPLSample()                              // may be 0 if unavailable
	ks.mu.Unlock()
}

// Calibrate establishes the "healthy" baseline from current window data. [SRE-44.1]
// Should be called after a warm-up period (e.g., 60s of normal operation).
func (ks *KineticSensor) Calibrate() {
	ks.mu.Lock()
	heap := ks.heapWindow
	power := ks.powerWindow
	ks.mu.Unlock()

	ks.calibrateBaseline(&ks.baselineHeap, heap[:])
	ks.calibrateBaseline(&ks.baselinePower, power[:])

	log.Printf("[KINETIC] Calibrated. Heap baseline: mean=%.1f std=%.1f | Power baseline: mean=%.1f std=%.1f",
		ks.baselineHeap.meanMag, ks.baselineHeap.stdMag,
		ks.baselinePower.meanMag, ks.baselinePower.stdMag)
}

func (ks *KineticSensor) calibrateBaseline(bl *SpectralBaseline, window []float64) {
	// Compute spectral magnitude via simplified DFT
	magnitudes := spectralMagnitudes(window, ks.cfg.SpectralBins)

	bl.mu.Lock()
	defer bl.mu.Unlock()

	// Compute mean and std of magnitudes
	var sum, sumSq float64
	maxMag := 0.0
	maxFreq := 0
	for i, m := range magnitudes {
		sum += m
		sumSq += m * m
		if m > maxMag {
			maxMag = m
			maxFreq = i
		}
	}

	n := float64(len(magnitudes))
	bl.meanMag = sum / n
	variance := (sumSq / n) - (bl.meanMag * bl.meanMag)
	if variance > 0 {
		bl.stdMag = math.Sqrt(variance)
	}
	bl.peakFreq = maxFreq
	bl.sampleCount++
	bl.calibrated = true
}

// Analyze checks current readings against baseline and returns anomalies. [SRE-44.2]
func (ks *KineticSensor) Analyze() []AnomalyReport {
	ks.mu.Lock()
	heap := ks.heapWindow
	power := ks.powerWindow
	gc := ks.gcWindow
	ks.mu.Unlock()

	var reports []AnomalyReport

	// Check heap anomaly
	if ks.baselineHeap.calibrated {
		if report := ks.checkAnomaly("heap_anomaly", heap[:], &ks.baselineHeap); report != nil {
			report.Action = ks.recommendHeapAction(report.Severity)
			reports = append(reports, *report)
		}
	}

	// Check power anomaly
	if ks.baselinePower.calibrated {
		if report := ks.checkAnomaly("power_anomaly", power[:], &ks.baselinePower); report != nil {
			report.Action = ks.recommendPowerAction(report.Severity)
			reports = append(reports, *report)
		}
	}

	// Direct GC pause check (no spectral — just threshold)
	idx := ks.windowIdx.Load()
	if idx > 0 {
		lastGC := gc[(idx-1)%256]
		gcThreshold := float64(ks.cfg.GCPauseThresholdUs)
		if lastGC > gcThreshold {
			reports = append(reports, AnomalyReport{
				Timestamp:    time.Now().Unix(),
				Type:         "gc_anomaly",
				Severity:     lastGC / (gcThreshold / 2), // normalize to σ-like scale
				CurrentValue: lastGC,
				Description:  fmt.Sprintf("GC pause %.1fms exceeds %.0fµs threshold", lastGC/1000, gcThreshold),
				Action:       "reduce allocation rate, check pool utilization",
			})
		}
	}

	if len(reports) > 0 {
		ks.anomalyCount.Add(int32(len(reports)))
		ks.lastAnomaly.Store(time.Now().Unix())
	}

	return reports
}

func (ks *KineticSensor) checkAnomaly(anomalyType string, window []float64, baseline *SpectralBaseline) *AnomalyReport {
	baseline.mu.RLock()
	defer baseline.mu.RUnlock()

	if !baseline.calibrated || baseline.stdMag == 0 {
		return nil
	}

	magnitudes := spectralMagnitudes(window, ks.cfg.SpectralBins)

	var sum float64
	for _, m := range magnitudes {
		sum += m
	}
	currentMean := sum / float64(len(magnitudes))

	// Calculate deviation in standard deviations (σ)
	sigma := (currentMean - baseline.meanMag) / baseline.stdMag

	if sigma > ks.cfg.AnomalyThresholdSigma { // > configured σ threshold is anomalous
		return &AnomalyReport{
			Timestamp:    time.Now().Unix(),
			Type:         anomalyType,
			Severity:     sigma,
			CurrentValue: currentMean,
			BaselineMean: baseline.meanMag,
			BaselineStd:  baseline.stdMag,
			Description:  fmt.Sprintf("%s: %.1fσ above baseline (current=%.1f, baseline=%.1f±%.1f)", anomalyType, sigma, currentMean, baseline.meanMag, baseline.stdMag),
		}
	}

	return nil
}

func (ks *KineticSensor) recommendHeapAction(sigma float64) string {
	switch {
	case sigma > ks.cfg.HeapCriticalSigma:
		return "CRITICAL: force GC + flush all caches + shed load"
	case sigma > ks.cfg.HeapWarningSigma:
		return "WARNING: trigger GC + reduce batch sizes"
	default:
		return "WATCH: monitor heap growth rate"
	}
}

func (ks *KineticSensor) recommendPowerAction(sigma float64) string {
	switch {
	case sigma > ks.cfg.HeapCriticalSigma:
		return "CRITICAL: possible thermal runaway — throttle all workers"
	case sigma > ks.cfg.HeapWarningSigma:
		return "WARNING: abnormal power draw — check for infinite loops"
	default:
		return "WATCH: monitor power consumption trend"
	}
}

// Stats returns a summary of the kinetic sensor state. [SRE-44.1]
func (ks *KineticSensor) Stats() KineticStats {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	stats := KineticStats{
		SamplesCollected: int(ks.windowIdx.Load()),
		PowerCalibrated:  ks.baselinePower.calibrated,
		HeapCalibrated:   ks.baselineHeap.calibrated,
		AnomalyCount:     int(ks.anomalyCount.Load()),
		CurrentHeapMB:    float64(mem.HeapAlloc) / (1024 * 1024),
		CurrentGCPause:   float64(mem.PauseNs[(mem.NumGC+255)%256]) / 1000,
	}

	lastTs := ks.lastAnomaly.Load()
	if lastTs > 0 {
		stats.LastAnomalyAge = time.Since(time.Unix(lastTs, 0)).Round(time.Second).String()
	} else {
		stats.LastAnomalyAge = "never"
	}

	return stats
}

// ─── Spectral Analysis [SRE-44.1] ─────────────────────────────────────────

// spectralMagnitudes computes the magnitude spectrum of a signal using a simplified DFT.
// Returns `bins` frequency magnitudes. This is O(N*bins) which is fine for N=256, bins=8.
func spectralMagnitudes(signal []float64, bins int) []float64 {
	n := len(signal)
	if n == 0 {
		return make([]float64, bins)
	}

	magnitudes := make([]float64, bins)
	for k := range bins {
		var realPart, imagPart float64
		freq := float64(k+1) / float64(n)
		for i := range n {
			angle := 2 * math.Pi * freq * float64(i)
			realPart += signal[i] * math.Cos(angle)
			imagPart -= signal[i] * math.Sin(angle)
		}
		magnitudes[k] = math.Sqrt(realPart*realPart+imagPart*imagPart) / float64(n)
	}

	return magnitudes
}

// readRAPLSample reads a single RAPL energy sample. Returns 0 if unavailable.
func readRAPLSample() float64 {
	// RAPL requires two readings with a time delta for actual watts.
	// In production this would be sampled by a background goroutine.
	// For now, return 0 — the baseline calibration handles this gracefully.
	return 0
}
