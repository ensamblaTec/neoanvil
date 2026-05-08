package sre

import (
	"bytes"
	"testing"
)

func TestEnclave_SymmetricSlab(t *testing.T) {
	e, err := GetEnclave()
	if err != nil {
		t.Fatalf("No se pudo iniciar el HW Enclave: %v", err)
	}

	secret := []byte("ROOT_CERTIFICATE_MASTER_KEY")
	copySecret := make([]byte, len(secret))
	copy(copySecret, secret)

	slab := e.Seal(secret)

	// Comprobar borrado (Wipe Ouroboros)
	for _, b := range secret {
		if b != 0 {
			t.Fatalf("Seal no machacó la RAM original como demanda SRE")
		}
	}

	outBuf := make([]byte, 1024)
	plain, err := e.Unseal(slab, outBuf)
	if err != nil {
		t.Fatalf("Fallo de Unseal criptográfico: %v", err)
	}

	if !bytes.Equal(plain, copySecret) {
		t.Fatalf("Corrupción en estado: %s != %s", plain, copySecret)
	}

	e.Free(slab)
}

func BenchmarkEnclave_Roundtrip(b *testing.B) {
	e, _ := GetEnclave()
	dummyData := []byte("PAYLOAD_TENSORIAL_MATRIZ_1MB_DUMMY_FOR_AES_NI_HW_ACCELERATED_CHIP_TEST")
	srcBuf := make([]byte, len(dummyData))
	outBuf := make([]byte, len(dummyData)+1024)

	b.ReportAllocs()
	for b.Loop() {
		copy(srcBuf, dummyData)
		slab := e.Seal(srcBuf)
		_, _ = e.Unseal(slab, outBuf)
		e.Free(slab)
	}
}
