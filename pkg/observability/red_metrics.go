package observability

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type ToolMetrics struct {
	Requests uint64
	Errors   uint64
	Duration uint64
}

type RedMetrics struct {
	tools      sync.Map
	history    [1024]float64
	historyIdx atomic.Uint32
}

func (rm *RedMetrics) RecordLatency(latencyMS float64) {
	idx := rm.historyIdx.Add(1) - 1
	rm.history[idx%1024] = latencyMS
}

func (rm *RedMetrics) AnalyzeFFT() (maxFreq int, maxMagnitude float64) {
	var input [1024]complex128
	for i := 0; i < 1024; i++ {
		input[i] = complex(rm.history[i], 0)
	}

	n := 1024
	for i, j := 0, 0; i < n; i++ {
		if i < j {
			input[i], input[j] = input[j], input[i]
		}
		bit := n >> 1
		for j >= bit {
			j -= bit
			bit >>= 1
		}
		j += bit
	}

	for length := 2; length <= n; length <<= 1 {
		halfLen := length >> 1
		angle := -2 * math.Pi / float64(length)
		omegaLen := complex(math.Cos(angle), math.Sin(angle))

		for i := 0; i < n; i += length {
			omega := complex(1, 0)
			for j := 0; j < halfLen; j++ {
				u := input[i+j]
				v := input[i+j+halfLen] * omega
				input[i+j] = u + v
				input[i+j+halfLen] = u - v
				omega *= omegaLen
			}
		}
	}

	for i := 1; i < n/2; i++ {
		mag := math.Hypot(real(input[i]), imag(input[i]))
		if mag > maxMagnitude {
			maxMagnitude = mag
			maxFreq = i
		}
	}
	return
}

var GlobalMetrics = &RedMetrics{}

func (rm *RedMetrics) RecordCall(toolName string, duration time.Duration, isError bool) {
	val, ok := rm.tools.Load(toolName)
	if !ok {
		val, _ = rm.tools.LoadOrStore(toolName, &ToolMetrics{})
	}

	metrics := val.(*ToolMetrics)
	atomic.AddUint64(&metrics.Requests, 1)
	if isError {
		atomic.AddUint64(&metrics.Errors, 1)
	}
	atomic.AddUint64(&metrics.Duration, uint64(duration.Microseconds()))
}

func (rm *RedMetrics) EmitSummary(workspace ...string) map[string]map[string]uint64 {
	summary := make(map[string]map[string]uint64)

	rm.tools.Range(func(key, value any) bool {
		toolName := key.(string)
		metrics := value.(*ToolMetrics)

		req := atomic.LoadUint64(&metrics.Requests)
		errs := atomic.LoadUint64(&metrics.Errors)
		dur := atomic.LoadUint64(&metrics.Duration)

		summary[toolName] = map[string]uint64{
			"requests": req,
			"errors":   errs,
			"duration": dur,
		}
		return true
	})

	data, err := json.MarshalIndent(summary, "", "  ")
	if err == nil {
		// [SRE-16.2.2] Atomic write to disk — avoids deadlock in MCP stdio environment
		base := "."
		if len(workspace) > 0 && workspace[0] != "" {
			base = workspace[0]
		}
		_ = os.MkdirAll(filepath.Join(base, ".neo", "logs"), 0755)
		_ = os.WriteFile(filepath.Join(base, ".neo", "logs", "metrics_summary.json"), data, 0644)
	}

	return summary
}
