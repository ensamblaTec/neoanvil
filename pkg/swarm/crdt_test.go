package swarm

import (
	"testing"
)

func TestPNCounter_Convergence(t *testing.T) {
	var n1, n2, n3 PNCounter

	// Nodo 1 suma 5
	n1.Add(1, 10, 5) // +5 neto
	// Nodo 2 suma 2
	n2.Add(2, 5, 3) // +2 neto
	// Nodo 3 suma 8
	n3.Add(3, 8, 0) // +8 neto

	// Red Particionada: N1 y N2 convergen
	n1.Merge(&n2)
	n2.Merge(&n1)

	if n1.Value() != 7 || n2.Value() != 7 {
		t.Fatalf("Partición 1 falló (%d, %d)", n1.Value(), n2.Value())
	}

	n1.Add(1, 0, 1) // N1 le quita 1 -> Queda en 6

	// N3 se une
	n3.Merge(&n1) // N3 absorbe +5 -1, +2

	if n3.Value() != 14 {
		t.Fatalf("Convergencia falló, esperado 14, obtuvo %d", n3.Value())
	}

	n2.Merge(&n3)
	n1.Merge(&n2)

	// Idempotencia y conmutatividad absoluta
	v1, v2, v3 := n1.Value(), n2.Value(), n3.Value()
	if v1 != 14 || v2 != 14 || v3 != 14 {
		t.Errorf("Divergencia detectada: N1=%d N2=%d N3=%d", v1, v2, v3)
	}
}

func TestLWWSet_Convergence(t *testing.T) {
	var s1, s2 LWWSet

	hashC := uint64(777)
	s1.Add(hashC, 100, true)

	s2.Add(hashC, 50, false) // Evento más antiguo llegó tarde

	s1.Merge(&s2)
	s2.Merge(&s1)

	if s1.Contains(hashC) != true || s2.Contains(hashC) != true {
		t.Errorf("El Write más reciente debía ganar")
	}

	// S2 recibe evento de borrado más reciente (T=200)
	s2.Add(hashC, 200, false)
	s1.Merge(&s2)

	if s1.Contains(hashC) {
		t.Errorf("Debería haber sido borrado asíncronamente")
	}
}

func BenchmarkPNCounter_Merge(b *testing.B) {
	var a, c PNCounter
	a.Add(1, 100, 0)
	c.Add(2, 50, 0)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Benchmark zero-alloc math
		a.Merge(&c)
	}
}

func BenchmarkLWW_Merge(b *testing.B) {
	var a, c LWWSet
	for i := 0; i < 50; i++ {
		a.Add(uint64(i), 100, true)
		c.Add(uint64(i+25), 101, false)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Benchmark zero-alloc eviction and linear scan
		a.Merge(&c)
	}
}
