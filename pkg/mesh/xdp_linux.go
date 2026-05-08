//go:build linux

package mesh

import (
	"log"
	"sync/atomic"
)

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
	log.Printf("[SRE-XDP] Fallback activo Linux. Faltan bindings completos de libbpf en sys/unix cross-comp")
	return &XDPSocket{
		UmemRing: umem,
		TxRing:   &LockFreeRing{Mask: uint64(size - 1), Offsets: make([]uint64, size)},
		RxRing:   &LockFreeRing{Mask: uint64(size - 1), Offsets: make([]uint64, size)},
	}, nil
}

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

func (x *XDPSocket) Close() {}
