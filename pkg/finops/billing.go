package finops

import (
	"encoding/json"
	"log"
	"sync/atomic"
)

type CloudCost struct {
	CPUCost float64 `json:"cpu_cost"`
	RAMCost float64 `json:"ram_cost"`
}

// CurrentTCO rastrea el gasto termo-financiero del Agente SRE
var CurrentTCO CloudCost

// IngestCostMetrics deserializa MESH costs
func IngestCostMetrics(payload []byte) float64 {
	var metrics CloudCost
	if err := json.Unmarshal(payload, &metrics); err != nil {
		log.Println("[SRE-FINOPS] Error parseando JSON de costos Cloud. Retornando penalty nulo.")
		return 0.0
	}
	CurrentTCO = metrics
	return metrics.CPUCost + metrics.RAMCost
}

// GetTotalPenalty unifica la sumatoria del costo (Cloud + Local)
func GetTotalPenalty() float64 {
	return CurrentTCO.CPUCost + CurrentTCO.RAMCost
}

var GlobalThermalTicks int64

// IngestHardwareMetric trackea el costo biologico
func IngestHardwareMetric(nanoseconds int64) {
	atomic.AddInt64(&GlobalThermalTicks, nanoseconds)
}
