package state

import (
	"fmt"
	"sync/atomic"
)

type CognitiveStage uint32

const (
	StageDefine      CognitiveStage = 1
	StageResearch    CognitiveStage = 2
	StageAnalyze     CognitiveStage = 3
	StageHypothesize CognitiveStage = 4
	StageVerify      CognitiveStage = 5
	StageSynthesize  CognitiveStage = 6
)

var (
	globalStage   atomic.Uint32
	hasResearched atomic.Bool
	hasAnalyzed   atomic.Bool
)

func init() {
	globalStage.Store(uint32(StageDefine))
}

// MarkResearched flags that the Agent successfully passed cognitive Phase 2
func MarkResearched() {
	hasResearched.Store(true)
}

// MarkAnalyzed flags that the Agent successfully submitted the mathematical evaluation
func MarkAnalyzed() {
	hasAnalyzed.Store(true)
}

// CanMutate checks if the current stage permits code mutation (O(1) lock-free).
func CanMutate() bool {
	return globalStage.Load() >= uint32(StageHypothesize)
}

// AdvanceTo transitions to the new stage. Strictly monotonic, no skipping allowed.
// AdvanceTo transitions to the new stage. Strictly monotonic, no skipping allowed.
// AdvanceTo transitions to the new stage. Permite retroceder (Agile Sprints).
// AdvanceTo transitions to the new stage. Permite retroceder (Agile Sprints).
func AdvanceTo(stage CognitiveStage) error {
	current := globalStage.Load()

	// Prevenir saltos adelante sin pasar por los stages intermedios
	if uint32(stage) > current+1 {
		return fmt.Errorf("no se pueden saltar fases cognitivas hacia adelante (intento: %d -> %d)", current, stage)
	}

	// [Agile-Ouroboros] Rehidratación y Limpieza de Variables Termodinámicas si retrocedemos (CASCADING FAILURE FIX)
	if stage <= StageResearch {
		hasResearched.Store(false)
		hasAnalyzed.Store(false)
	} else if stage <= StageAnalyze {
		hasAnalyzed.Store(false)
	}

	// SRE Cognitive Firewall Checkpoints
	if stage == StageResearch {
		if plannerDB != nil {
			pending, _ := GetPlannerState()
			if pending == 0 {
				return fmt.Errorf("[SRE-VETO] Fase 2 (Research) denegada. El Agente no ha usado neo_generate_tasks ni neo_get_next_task. Se prohíbe investigar sin un BoltDB Pipeline activo (Pending Queue == 0)")
			}
		}
	}
	if stage == StageAnalyze && !hasResearched.Load() {
		return fmt.Errorf("[SRE-VETO] Fase 3 (Analyze) denegada. El Agente no ha invocado neo_search_context para investigar en Fase 2")
	}
	if stage == StageHypothesize && !hasAnalyzed.Load() {
		return fmt.Errorf("[SRE-VETO] Fase 4 (Hypothesize) denegada. El Agente no ha invocado neo_evaluate_plan para validar la arquitectura en Fase 3")
	}

	globalStage.Store(uint32(stage))
	return nil
}

// ForceStage bypasses restrictions for testing or manual overrides.
func ForceStage(stage CognitiveStage) {
	globalStage.Store(uint32(stage))
}

// GetCurrent returns the current machine state.
func GetCurrent() uint32 {
	return globalStage.Load()
}
