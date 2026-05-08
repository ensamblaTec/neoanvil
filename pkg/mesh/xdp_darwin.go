//go:build darwin

package mesh

import (
	"fmt"
	"log"
	"sync/atomic"
)

// Emulador Mock de AF_XDP en Darwin para testear Aritmética de Punteros sin Kernel Panics
type XDPSocket struct {
	UmemRing []byte
	TxRing   *LockFreeRing
	RxRing   *LockFreeRing
}

type LockFreeRing struct {
	Producer atomic.Uint64
	Consumer atomic.Uint64
	Mask     uint64
	Offsets  []uint64
}

func MountAFXDP(umem []byte, size uint32) (*XDPSocket, error) {
	log.Println("💻 Iniciando Mock Simulator XDP (Memoria Aislada para macOS)")
	// Requiere Size potencia de 2
	if (size & (size - 1)) != 0 {
		return nil, fmt.Errorf("SRE VETO: AF_XDP Ring Size debe ser potencia de 2 para bitmasking rápido")
	}
	return &XDPSocket{
		UmemRing: umem,
		TxRing:   &LockFreeRing{Mask: uint64(size - 1), Offsets: make([]uint64, size)},
		RxRing:   &LockFreeRing{Mask: uint64(size - 1), Offsets: make([]uint64, size)},
	}, nil
}

// Enqueue emula la escritura de la NIC o kernel bypass a una estructura Ring
func (r *LockFreeRing) Enqueue(offset uint64) bool {
	prod := r.Producer.Load()
	cons := r.Consumer.Load()
	if prod-cons >= r.Mask+1 { // Drop del paquete estricto eBPF
		return false
	}
	r.Offsets[prod&r.Mask] = offset
	r.Producer.Store(prod + 1) // Store atomic
	return true
}

func (r *LockFreeRing) Dequeue() (uint64, bool) {
	cons := r.Consumer.Load()
	prod := r.Producer.Load()
	if cons == prod {
		return 0, false
	}
	offset := r.Offsets[cons&r.Mask]
	r.Consumer.Store(cons + 1)
	return offset, true
}

func (x *XDPSocket) Close() {
	log.Println("Mock AF_XDP cerrado")
}
