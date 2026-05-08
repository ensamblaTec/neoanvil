package sre

import (
	"context"
	"time"
)

type PQCEncapsulationResult struct {
	Ciphertext   []byte
	SharedSecret []byte
}

var (
	pqcGenBreaker = NewCircuitBreaker[*QuantumHybridKeycap](3, 10*time.Second)
	pqcEncBreaker = NewCircuitBreaker[PQCEncapsulationResult](3, 10*time.Second)
	pqcDecBreaker = NewCircuitBreaker[[]byte](3, 10*time.Second)
)

func ResilientGenerateHybridKeys(ctx context.Context) (*QuantumHybridKeycap, error) {
	return pqcGenBreaker.Execute(ctx, func(c context.Context) (*QuantumHybridKeycap, error) {
		return GenerateHybridKeys()
	})
}

func ResilientEncapsulateHybrid(ctx context.Context, compositePub []byte) (PQCEncapsulationResult, error) {
	return pqcEncBreaker.Execute(ctx, func(c context.Context) (PQCEncapsulationResult, error) {
		cipher, secret, err := EncapsulateHybrid(compositePub)
		return PQCEncapsulationResult{Ciphertext: cipher, SharedSecret: secret}, err
	})
}

func (cap *QuantumHybridKeycap) ResilientDecapsulateHybrid(ctx context.Context, compositeCipher []byte) ([]byte, error) {
	return pqcDecBreaker.Execute(ctx, func(c context.Context) ([]byte, error) {
		return cap.DecapsulateHybrid(compositeCipher)
	})
}
