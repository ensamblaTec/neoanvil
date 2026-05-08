package telemetry

import (
	"sync"
	"sync/atomic"
	"time"
)

type ToolStat struct {
	Name     string  `json:"Name"`
	Requests uint64  `json:"Requests"`
	Errors   uint64  `json:"Errors"`
	Duration float64 `json:"Duration"`
}

type SystemStats struct {
	Status             string             `json:"Status"`
	Uptime             string             `json:"Uptime"`
	AllocatedRAM_MB    float64            `json:"AllocatedRAM_MB"`
	ActiveGoroutines   int                `json:"ActiveGoroutines"`
	CurrentTask        string             `json:"CurrentTask"`
	ActiveTicket       string             `json:"ActiveTicket"`
	ActiveTicketRisk   int                `json:"ActiveTicketRisk"`
	ActiveTicketReason string             `json:"ActiveTicketReason"`
	LastActions        []string           `json:"LastActions"`
	LastVeto           string             `json:"LastVeto"`
	XAIScore           float32            `json:"XAI_Score"`
	XAIEntropy         float32            `json:"XAI_Entropy"`
	XAIAsymptote       float32            `json:"XAI_Asymptote"`
	PendingTickets     int32              `json:"PendingTickets"`
	AutoApprove        bool               `json:"AutoApprove"`
	RateLimitBlocks    int32              `json:"RateLimitBlocks"`
	CPUUsage           float64            `json:"CPUUsage"`
	PlannerPhase       string             `json:"PlannerPhase"`
	PlannerPending     int                `json:"PlannerPending"`
	PlannerCompleted   int                `json:"PlannerCompleted"`
	ActiveTools        map[string]*ToolStat `json:"ActiveTools"`
	DatabaseSizesMB    map[string]float64   `json:"DatabaseSizesMB"`
	JoulesConsumed     float64              `json:"JoulesConsumed"`
	EvaluatePlanCount  uint64               `json:"EvaluatePlanCount"`
	DirectivesCount    int32                `json:"DirectivesCount"`
	IOBytesSent        int64                `json:"IOBytesSent"`
	IOBytesRecv        int64                `json:"IOBytesRecv"`
	ChaosActive        bool                 `json:"ChaosActive"`
	ChaosLevel         int                  `json:"ChaosLevel"`
}

var (
	startTime   = time.Now()
	globalState SystemStats
	mu          sync.RWMutex

	// Atomic shadows for high-frequency counters
	atomicEvaluatePlanCount uint64
	atomicRateLimitBlocks   int32
	atomicDirectivesCount   int32
	atomicPendingTickets    int32
	atomicIOBytesSent       int64
	atomicIOBytesRecv       int64
	atomicChaosActive       int32
	atomicChaosLevel        int32
)

func ReportToolUsage(name string, duration float64, errOccurred bool) {
	mu.Lock()
	defer mu.Unlock()

	if globalState.ActiveTools == nil {
		globalState.ActiveTools = make(map[string]*ToolStat)
	}

	stat, ok := globalState.ActiveTools[name]
	if ok {
		stat.Requests++
		stat.Duration = (stat.Duration + duration) / 2.0
		if errOccurred {
			stat.Errors++
		}
	} else {
		errs := uint64(0)
		if errOccurred {
			errs = 1
		}
		globalState.ActiveTools[name] = &ToolStat{
			Name:     name,
			Requests: 1,
			Duration: duration,
			Errors:   errs,
		}
	}
}

func IncrementEvaluatePlan() {
	atomic.AddUint64(&atomicEvaluatePlanCount, 1)
}

func SetDirectivesCount(count int) {
	atomic.StoreInt32(&atomicDirectivesCount, int32(count))
}

func StageCommandTicket(ticket string, risk int, reason string) {
	mu.Lock()
	defer mu.Unlock()
	globalState.ActiveTicket = ticket
	globalState.ActiveTicketRisk = risk
	globalState.ActiveTicketReason = reason
	atomic.AddInt32(&atomicPendingTickets, 1)
}

func ClearCommandTicket() {
	mu.Lock()
	defer mu.Unlock()
	globalState.ActiveTicket = ""
	globalState.ActiveTicketRisk = 0
	globalState.ActiveTicketReason = ""
	if atomic.LoadInt32(&atomicPendingTickets) > 0 {
		atomic.AddInt32(&atomicPendingTickets, -1)
	}
}

func IncrementRateLimitBlock() {
	atomic.AddInt32(&atomicRateLimitBlocks, 1)
}

func SetTask(task string) {
	mu.Lock()
	globalState.CurrentTask = task
	mu.Unlock()
	EmitEvent("TERMODINÁMICA", "TAREA ACTUAL: "+task)
}

func LogAction(action string) {
	mu.Lock()
	defer mu.Unlock()

	ts := time.Now().Format("15:04:05")
	msg := ts + " - " + action

	globalState.LastActions = append(globalState.LastActions, msg)
	if len(globalState.LastActions) > 50 {
		globalState.LastActions = globalState.LastActions[1:]
	}
	EmitEvent("FIREHOSE", msg)
}

func ReportVeto(reason string) {
	mu.Lock()
	defer mu.Unlock()
	globalState.LastVeto = time.Now().Format("15:04:05") + " - " + reason
	EmitEvent("INMUNOLOGÍA", "VETO SRE: "+reason)
}

func ClearVeto() {
	mu.Lock()
	globalState.LastVeto = ""
	mu.Unlock()
}

func GetSystemStats() SystemStats {
	mu.RLock()
	defer mu.RUnlock()
	
	// Sync atomic counters back to struct for serialization
	globalState.EvaluatePlanCount = atomic.LoadUint64(&atomicEvaluatePlanCount)
	globalState.RateLimitBlocks = atomic.LoadInt32(&atomicRateLimitBlocks)
	globalState.DirectivesCount = atomic.LoadInt32(&atomicDirectivesCount)
	globalState.PendingTickets = atomic.LoadInt32(&atomicPendingTickets)
	globalState.IOBytesSent = atomic.LoadInt64(&atomicIOBytesSent)
	globalState.IOBytesRecv = atomic.LoadInt64(&atomicIOBytesRecv)
	globalState.ChaosActive = atomic.LoadInt32(&atomicChaosActive) == 1
	globalState.ChaosLevel = int(atomic.LoadInt32(&atomicChaosLevel))

	return globalState
}

func SetPendingTickets(count int) {
	atomic.StoreInt32(&atomicPendingTickets, int32(count))
}

func SetAutoApprove(mode bool) {
	mu.Lock()
	globalState.AutoApprove = mode
	mu.Unlock()
}

func SetActiveTicket(ticket string) {
	mu.Lock()
	globalState.ActiveTicket = ticket
	mu.Unlock()
}

func GetActiveTools() []ToolStat {
	mu.RLock()
	defer mu.RUnlock()
	results := make([]ToolStat, 0, len(globalState.ActiveTools))
	for _, v := range globalState.ActiveTools {
		results = append(results, *v)
	}
	return results
}

func OverrideTools(tools []ToolStat) {
	mu.Lock()
	defer mu.Unlock()
	globalState.ActiveTools = make(map[string]*ToolStat)
	for i := range tools {
		globalState.ActiveTools[tools[i].Name] = &tools[i]
	}
}

func ReportXAI(score, entropy, asymptote float32) {
	mu.Lock()
	defer mu.Unlock()
	globalState.XAIScore = score
	globalState.XAIEntropy = entropy
	globalState.XAIAsymptote = asymptote
}

// [SRE-24.1.2] SetIOStats syncs MCP transport IO counters into telemetry state.
func SetIOStats(recv, sent int64) {
	atomic.StoreInt64(&atomicIOBytesRecv, recv)
	atomic.StoreInt64(&atomicIOBytesSent, sent)
}

// IsChaosActive returns true if a chaos siege is currently running. [SRE-25.3.1]
func IsChaosActive() bool {
	return atomic.LoadInt32(&atomicChaosActive) == 1
}

// [SRE-24.1.3] SetChaosState signals an active GameDay siege to the TUI.
func SetChaosState(active bool, level int) {
	if active {
		atomic.StoreInt32(&atomicChaosActive, 1)
	} else {
		atomic.StoreInt32(&atomicChaosActive, 0)
	}
	atomic.StoreInt32(&atomicChaosLevel, int32(level))
}
