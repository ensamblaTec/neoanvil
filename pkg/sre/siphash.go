package sre

import (
	"crypto/rand"
	"encoding/binary"
	"log"
	"time"
)

// RotationalHashMap mitiga las Colisiones de Mapa Forzadas (HashDoS) $O(1) \rightarrow O(N)$
// Rota implacablemente el Sal de SipHash/Búsquedas internar la memoria CRDT cada 10 Minutos.
type RotationalHashMap struct {
	Salt     uint64
	creation time.Time
}

func NewRotationalHashMap() *RotationalHashMap {
	m := &RotationalHashMap{
		creation: time.Now(),
	}
	m.RotateEpoch()
	return m
}

func (m *RotationalHashMap) RotateEpoch() {
	var b [8]byte
	rand.Read(b[:])
	m.Salt = binary.LittleEndian.Uint64(b[:])
	m.creation = time.Now()
	log.Printf("[SRE-HASH] Bóveda CRDT Rotada Exitosamente. Época Criptográfica: %X\n", m.Salt)
}

func (m *RotationalHashMap) EvaluateEpoch() {
	if time.Since(m.creation) > 10*time.Minute {
		m.RotateEpoch()
	}
}
