package sre

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

type QuantumHybridKeycap struct {
	x25519Private    *ecdh.PrivateKey
	mlkemPrivateSeal *SecureSlab
	PublicKeyBytes   []byte
}

func GenerateHybridKeys() (*QuantumHybridKeycap, error) {
	cKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fallo clásico ecdh: %v", err)
	}

	qKey, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, fmt.Errorf("fallo cuántico mlkem: %v", err)
	}

	enclave, err := GetEnclave()
	if err != nil {
		return nil, fmt.Errorf("violación de enclave: %v", err)
	}

	sealedQ := enclave.Seal(qKey.Bytes())

	pubConcat := make([]byte, 0, len(cKey.PublicKey().Bytes())+len(qKey.EncapsulationKey().Bytes()))
	pubConcat = append(pubConcat, cKey.PublicKey().Bytes()...)
	pubConcat = append(pubConcat, qKey.EncapsulationKey().Bytes()...)

	return &QuantumHybridKeycap{
		x25519Private:    cKey,
		mlkemPrivateSeal: sealedQ,
		PublicKeyBytes:   pubConcat,
	}, nil
}

func EncapsulateHybrid(compositePub []byte) (ciphertext []byte, sharedSecret []byte, err error) {
	if len(compositePub) != 32+1184 {
		return nil, nil, fmt.Errorf("public key compuesta inválida")
	}

	xPub, err := ecdh.X25519().NewPublicKey(compositePub[:32])
	if err != nil {
		return nil, nil, err
	}
	qPub, err := mlkem.NewEncapsulationKey768(compositePub[32:])
	if err != nil {
		return nil, nil, err
	}

	xEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	xShared, err := xEphemeral.ECDH(xPub)
	if err != nil {
		return nil, nil, err
	}

	qShared, qCiphertext := qPub.Encapsulate()

	cipherFinal := make([]byte, 0, len(xEphemeral.PublicKey().Bytes())+len(qCiphertext))
	cipherFinal = append(cipherFinal, xEphemeral.PublicKey().Bytes()...)
	cipherFinal = append(cipherFinal, qCiphertext...)

	hash := sha256.New()
	hash.Write(xShared)
	hash.Write(qShared)
	finalSecret := hash.Sum(nil)

	return cipherFinal, finalSecret, nil
}

func (cap *QuantumHybridKeycap) DecapsulateHybrid(compositeCipher []byte) ([]byte, error) {
	if len(compositeCipher) != 32+1088 {
		return nil, fmt.Errorf("ciphertext compuesto inválido")
	}

	xPubSender, err := ecdh.X25519().NewPublicKey(compositeCipher[:32])
	if err != nil {
		return nil, err
	}
	xShared, err := cap.x25519Private.ECDH(xPubSender)
	if err != nil {
		return nil, err
	}

	enclave, _ := GetEnclave()
	qDecapsPrivBytes, err := enclave.Unseal(cap.mlkemPrivateSeal, make([]byte, 2400))
	if err != nil {
		return nil, err
	}
	defer Wipe(qDecapsPrivBytes)

	qPriv, err := mlkem.NewDecapsulationKey768(qDecapsPrivBytes)
	if err != nil {
		return nil, err
	}

	qShared, err := qPriv.Decapsulate(compositeCipher[32:])
	if err != nil {
		return nil, err
	}

	hash := sha256.New()
	hash.Write(xShared)
	hash.Write(qShared)
	finalSecret := hash.Sum(nil)

	return finalSecret, nil
}

func (cap *QuantumHybridKeycap) Wipe() {
	if cap.mlkemPrivateSeal != nil {
		enclave, err := GetEnclave()
		if err == nil {
			enclave.Free(cap.mlkemPrivateSeal)
			cap.mlkemPrivateSeal = nil
		}
	}
}
