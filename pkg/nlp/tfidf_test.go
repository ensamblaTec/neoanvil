package nlp

import (
	"testing"
)

func TestCosineSimilarity_Similar(t *testing.T) {
	epic := "implementar el motor de telemetría y métricas RED"
	task := "desarrollar telemetría y métricas RED para el motor"

	sim := CosineSimilarity(epic, task)
	if sim < 0.40 {
		t.Errorf("Expected similarity >= 0.40 for related texts, got %f", sim)
	}
}

func TestCosineSimilarity_Disparate(t *testing.T) {
	epic := "implementar telemetría y logging de métricas RED usando Go"
	task := "configurar estilos css para el modal de login de la web UI en javascript"

	sim := CosineSimilarity(epic, task)
	if sim >= 0.40 {
		t.Errorf("Expected similarity < 0.40 for disparate texts, got %f", sim)
	}
}
