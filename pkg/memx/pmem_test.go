package memx

import (
	"os"
	"testing"
)

func TestPMEM_BumpAllocator_ZeroCopy(t *testing.T) {
	dbPath := "../../.neo/brain.db"
	defer os.Remove(dbPath)

	alloc, err := MountPMEM(dbPath, 1024*10, false)
	if err != nil {
		t.Fatalf("Imposible inyectar PMEM: %v", err)
	}

	// Instanciar Grafo sobre Disco
	offsetA, nodeA, err := alloc.InstantiateNeuralNode(99.0)
	if err != nil {
		t.Fatalf("Rebote allocating: %v", err)
	}

	offsetB, nodeB, _ := alloc.InstantiateNeuralNode(42.0)
	_ = nodeB

	// Enlace A -> B
	nodeA.RightChild = offsetB

	// Leer transitando del Disco en vivo
	childAddress := nodeA.RightChild.Resolve(alloc.BaseAddress())
	evalNodeB := (*NeuralNode)(childAddress)

	if evalNodeB.Value != 42.0 {
		t.Fatalf("Aritmética M-Zero Asintótica Corrupta. Memory Transmute failed.")
	}

	if offsetA == 0 || offsetB == 0 {
		t.Fatalf("Puntero a Nil O(1) violado")
	}
}

func BenchmarkPMEM_TransmutationLatencies(b *testing.B) {
	dbPath := "../../.neo/brain_test.db"
	alloc, _ := MountPMEM(dbPath, 1024*1024*10, false)
	defer os.Remove(dbPath)

	rootOsc, rootNode, _ := alloc.InstantiateNeuralNode(3.14)
	_, childNode, _ := alloc.InstantiateNeuralNode(1.61)

	rootNode.LeftChild = 99912 // fake index for resolve

	b.ResetTimer()
	b.ReportAllocs()

	// Bucle apretado de conversión térmica RelPtr -> Struct
	var dummy float32
	for i := 0; i < b.N; i++ {
		// Re-armando Inferencia simulando salto
		evalAddress := rootOsc.Resolve(alloc.BaseAddress())
		eval := (*NeuralNode)(evalAddress)
		_ = eval.RightChild.Resolve(alloc.BaseAddress()) // Nil Check
		eval.RightChild = rootOsc
		dummy = eval.Value

		_ = childNode
	}
	_ = dummy
}
