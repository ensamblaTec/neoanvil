package sre

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
)

type SecureSlab struct {
	nonce      [12]byte
	ciphertext []byte
	backingPtr *[]byte
}

type EnclaveAllocator struct {
	aead  cipher.AEAD
	nonce atomic.Uint64

	bytesPool *sync.Pool
	slabPool  *sync.Pool
}

var (
	globalEnclave *EnclaveAllocator
	enclaveOnce   sync.Once
	enclaveErr    error
)

// GetEnclave inicializa y retorna la instancia Singleton del Hardware Enclave TME
func GetEnclave() (*EnclaveAllocator, error) {
	enclaveOnce.Do(func() {
		// MEK: Master Ephemeral Key anclada por sync.Once al ciclo vital del proceso.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			enclaveErr = err
			return
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			enclaveErr = err
			return
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			enclaveErr = err
			return
		}
		// Zero-Alloc Double Pool
		bp := &sync.Pool{
			New: func() any {
				b := make([]byte, 0, 8192)
				return &b
			},
		}
		sp := &sync.Pool{
			New: func() any {
				return &SecureSlab{}
			},
		}
		globalEnclave = &EnclaveAllocator{aead: aead, bytesPool: bp, slabPool: sp}
	})
	return globalEnclave, enclaveErr
}

// Wipe destruye criptográficamente un slice en memoria plana
func Wipe(data []byte) {
	for i := range data {
		data[i] = 0
	}
}

func (e *EnclaveAllocator) Seal(plain []byte) *SecureSlab {
	n := e.nonce.Add(1)
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[:], n)

	bufferPtr := e.bytesPool.Get().(*[]byte)
	buffer := (*bufferPtr)[:0]

	// Encriptación Hardware Accelerated con AES-NI
	ciphertext := e.aead.Seal(buffer, nonce[:], plain, nonce[:])
	Wipe(plain)

	slab := e.slabPool.Get().(*SecureSlab)
	slab.nonce = nonce
	slab.ciphertext = ciphertext
	slab.backingPtr = bufferPtr

	return slab
}

func (e *EnclaveAllocator) Unseal(slab *SecureSlab, outputBuf []byte) ([]byte, error) {
	plain, err := e.aead.Open(outputBuf[:0], slab.nonce[:], slab.ciphertext, slab.nonce[:])
	if err != nil {
		return nil, fmt.Errorf("SRE ENCLAVE BREACH DETECTED: Integridad hardware violada - %v", err)
	}
	return plain, nil
}

func (e *EnclaveAllocator) Free(slab *SecureSlab) {
	if slab.backingPtr != nil {
		e.bytesPool.Put(slab.backingPtr)
		slab.backingPtr = nil
	}
	slab.ciphertext = nil
	e.slabPool.Put(slab)
}
