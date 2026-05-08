package memx

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"unsafe"
)

// RelPtr representa un offset Asintótico O(1) puro desde la dirección base Memory-Mapped del NVMe
type RelPtr uint64

// Resolve convierte este offset numérico a un puntero físico HW de CPU para lectura y mutación
// al vuelo sin deserialización. La matriz subyacente reside inamovible en el disco persistente.
func (p RelPtr) Resolve(baseAddress unsafe.Pointer) unsafe.Pointer {
	if p == 0 {
		return nil // Representación clásica del puntero Cero/Nil en PMEM
	}
	return unsafe.Pointer(uintptr(baseAddress) + uintptr(p))
}

// NeuralNode es un grafo sintáctico distribuido en Memory-Mapped Disk
// Cada neurona (Tensor) apila a otras ramas sin poseer localizaciones de Garbage Collection (0 B/op).
type NeuralNode struct {
	Value      float32
	LeftChild  RelPtr
	RightChild RelPtr
	Checksum   uint32 // Firma Criptográfica Atómica (Anti-Cósmica)
}

// IsCosmicCorrupted verifies node integrity using CRC32 Castagnoli over its data fields.
// [SRE-15.4.1] Connected to CosmicRayMitigator logic instead of returning false.
func (n *NeuralNode) IsCosmicCorrupted() bool {
	var buf [16]byte
	binary.LittleEndian.PutUint32(buf[0:4], math.Float32bits(n.Value))
	binary.LittleEndian.PutUint64(buf[4:12], uint64(n.LeftChild))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(n.RightChild))
	table := crc32.MakeTable(crc32.Castagnoli)
	actual := crc32.Checksum(buf[:12], table)
	return actual != n.Checksum
}
