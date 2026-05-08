package mctx

import (
	"math"
	"sync/atomic"
)

type Node struct {
	state      atomic.Uint64
	Parent     uint32
	FirstChild uint32
	Sibling    uint32
	_          [64]byte
}

func packState(visits uint32, score float32) uint64 {
	scoreBits := math.Float32bits(score)
	return (uint64(visits) << 32) | uint64(scoreBits)
}

func unpackState(state uint64) (uint32, float32) {
	visits := uint32(state >> 32)
	score := math.Float32frombits(uint32(state & 0xFFFFFFFF))
	return visits, score
}
