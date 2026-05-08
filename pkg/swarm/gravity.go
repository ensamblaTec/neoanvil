package swarm

import (
	"sync"
	"sync/atomic"
)

type NodeMass struct {
	FreeRAMBytes atomic.Uint64
	IdleCPU      atomic.Uint32
}

// GravityRouter uses sync.Map for Zero-Alloc concurrent node state updates.
// [SRE-16.3.2] Eliminates Copy-On-Write heap thrashing from the original map clone loop.
type GravityRouter struct {
	nodes sync.Map // map[int32]*NodeMass
}

func NewGravityRouter() *GravityRouter {
	return &GravityRouter{}
}

func (gr *GravityRouter) UpdateMass(nodeID int32, ram uint64, cpu uint32) {
	mass, _ := gr.nodes.LoadOrStore(nodeID, &NodeMass{})
	nm := mass.(*NodeMass)
	nm.FreeRAMBytes.Store(ram)
	nm.IdleCPU.Store(cpu)
}

func (gr *GravityRouter) Attract() int32 {
	var bestTarget int32 = -1
	var maxForce uint64 = 0

	gr.nodes.Range(func(key, value any) bool {
		id := key.(int32)
		mass := value.(*NodeMass)

		ram := mass.FreeRAMBytes.Load()
		cpu := uint64(mass.IdleCPU.Load())
		force := (ram >> 20) + (cpu * 10)

		if force >= maxForce {
			maxForce = force
			bestTarget = id
		}
		return true
	})

	return bestTarget
}
