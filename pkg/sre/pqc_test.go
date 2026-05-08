package sre

import (
	"bytes"
	"testing"
)

func TestPQCHybrid_KEM_Roundtrip(t *testing.T) {
	// 1. Bob generates Hybrid KeyPair
	bobKeycap, err := GenerateHybridKeys()
	if err != nil {
		t.Fatalf("Fallo en generación de llaves cuánticas: %v", err)
	}
	defer bobKeycap.Wipe()

	// 2. Alice generates Ciphertext + SharedSecret
	ciphertext, aliceSecret, err := EncapsulateHybrid(bobKeycap.PublicKeyBytes)
	if err != nil {
		t.Fatalf("EncapsulateHybrid falló: %v", err)
	}

	// 3. Bob computes SharedSecret using Enclave Unseal
	bobSecret, err := bobKeycap.DecapsulateHybrid(ciphertext)
	if err != nil {
		t.Fatalf("DecapsulateHybrid falló: %v", err)
	}

	// 4. KEM Guarantee
	if !bytes.Equal(aliceSecret, bobSecret) {
		t.Fatalf("Desajuste Asimétrico: Secretos compartidos divergen.")
	}
}

func BenchmarkQuantumHybrid_Roundtrip(b *testing.B) {
	bobKeycap, _ := GenerateHybridKeys()
	defer bobKeycap.Wipe()

	for b.Loop() {
		ciphertext, _, _ := EncapsulateHybrid(bobKeycap.PublicKeyBytes)
		_, _ = bobKeycap.DecapsulateHybrid(ciphertext)
	}
}
