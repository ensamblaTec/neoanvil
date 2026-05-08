package state

import "testing"

func TestCanMutate(t *testing.T) {
	ForceStage(StageDefine)
	if CanMutate() {
		t.Error("Expected CanMutate to be false at StageDefine")
	}

	ForceStage(StageAnalyze)
	if CanMutate() {
		t.Error("Expected CanMutate to be false at StageAnalyze")
	}

	ForceStage(StageHypothesize)
	if !CanMutate() {
		t.Error("Expected CanMutate to be true at StageHypothesize")
	}

	ForceStage(StageSynthesize)
	if !CanMutate() {
		t.Error("Expected CanMutate to be true at StageSynthesize")
	}
}

func TestAdvanceTo(t *testing.T) {
	ForceStage(StageDefine)
	if err := AdvanceTo(StageResearch); err != nil {
		t.Errorf("Expected success advancing 1->2, got: %v", err)
	}
	if err := AdvanceTo(StageHypothesize); err == nil {
		t.Error("Expected error skipping phase 2->4")
	}
	// [AGILE OUROBOROS] Permite reciclar o regresar de fase iterativamente
	if err := AdvanceTo(StageResearch); err != nil {
		t.Errorf("Expected success regressing Agile 2->2, got: %v", err)
	}
}
