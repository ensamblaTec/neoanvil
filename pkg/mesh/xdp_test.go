package mesh

import (
	"testing"
)

func TestAFXDPRingBounds(t *testing.T) {
	// Reserva TME DMA virtual 1MB (Slab del Enclave Criptográfico)
	enclaveMemory := make([]byte, 1024*1024)

	// Un anillo AF_XDP real usa 2048 o 4096 (potencia de 2)
	size := uint32(2048)
	xsk, err := MountAFXDP(enclaveMemory, size)
	if err != nil {
		t.Fatalf("Fallo crítico en AF_XDP mock: %v", err)
	}

	// Inyectar más del límite del Mask para verificar contención Lock-Free sin desbordamiento Heap (OOB)
	for i := uint64(0); i < 2050; i++ {
		success := xsk.RxRing.Enqueue(i * 2048)
		if i < 2048 && !success {
			t.Fatalf("Ring rechazó tramas válidas prematuramente en offset %d", i)
		}
		if i >= 2048 && success {
			t.Fatal("Ring no aplicó Drop en el paquete y desbordó el Tail/Head atómico (Buffer Overflow)")
		}
	}

	// Vaciar el Anillo Validando Offsets Puros
	for i := uint64(0); i < 2048; i++ {
		offset, ok := xsk.RxRing.Dequeue()
		if !ok {
			t.Fatalf("Anillo no retornó paquete estancado en índice O(1): %d", i)
		}
		if offset != i*2048 {
			t.Fatalf("Puntero Corrupto: Esperaba Offset %d, obtuve %d", i*2048, offset)
		}
	}

	_, ok := xsk.RxRing.Dequeue()
	if ok {
		t.Fatal("Anillo debía estar vacío, retornó Offsets Fantasma (Corrupción de Bitmask)")
	}
}

func BenchmarkXDPRing_Atomic(b *testing.B) {
	enclaveMemory := make([]byte, 1024*1024)
	xsk, err := MountAFXDP(enclaveMemory, 4096)
	if err != nil {
		b.Fatalf("Fallo crítico en AF_XDP benchmark: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		xsk.RxRing.Enqueue(uint64(i))
		xsk.RxRing.Dequeue()
	}
}
