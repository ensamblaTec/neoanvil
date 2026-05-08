package graph

import (
	"context"
	"math"

	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

const (
	DampingFactor = float32(0.85)
	MaxIterations = 20
	Tolerance     = float32(1e-6)
)

// CalculatePageRank evalúa la centralidad (importancia sistémica) de cada nodo en el DAG.
// Calculado en RAM zero-alloc (O(E)) mediante Álgebra Lineal usando Slabs.
func CalculatePageRank(ctx context.Context, device *tensorx.CPUDevice, pool *memx.ObservablePool[memx.F32Slab], edges map[string][]string) (map[string]float32, error) {
	nodeToInt := make(map[string]int)
	intToNode := make(map[int]string)

	idx := 0
	for from, tos := range edges {
		if _, ok := nodeToInt[from]; !ok {
			nodeToInt[from] = idx
			intToNode[idx] = from
			idx++
		}
		for _, to := range tos {
			if _, ok := nodeToInt[to]; !ok {
				nodeToInt[to] = idx
				intToNode[idx] = to
				idx++
			}
		}
	}

	N := idx
	if N == 0 {
		return nil, nil
	}

	inEdges := make([][]int, N)
	outDegree := make([]int, N)

	for from, tos := range edges {
		u := nodeToInt[from]
		outDegree[u] = len(tos)
		for _, to := range tos {
			v := nodeToInt[to]
			inEdges[v] = append(inEdges[v], u)
		}
	}

	slabVt := pool.Acquire()
	defer pool.Release(slabVt, len(slabVt.Data))
	slabVNext := pool.Acquire()
	defer pool.Release(slabVNext, len(slabVNext.Data))

	if len(slabVt.Data) < N {
		slabVt.Data = make([]float32, N)
	}
	if len(slabVNext.Data) < N {
		slabVNext.Data = make([]float32, N)
	}

	vtData := slabVt.Data[:N]
	vNextData := slabVNext.Data[:N]

	for i := range N {
		vtData[i] = 1.0 / float32(N)
	}

	vt := &tensorx.Tensor[float32]{Data: vtData, Shape: tensorx.Shape{N}}
	vNext := &tensorx.Tensor[float32]{Data: vNextData, Shape: tensorx.Shape{N}}

	for range MaxIterations {
		device.SpMVPageRank(inEdges, outDegree, vt, vNext, DampingFactor)

		diff := float32(0.0)
		for i := range N {
			diff += float32(math.Abs(float64(vt.Data[i] - vNext.Data[i])))
			vt.Data[i] = vNext.Data[i]
		}

		if diff < Tolerance {
			break
		}
	}

	// Blast Radius (Colisiones dependencias Segundo Grado: M^2)
	device.SpMVBlastRadius(inEdges, vt, vNext)
	device.SpMVBlastRadius(inEdges, vNext, vt)

	ranks := make(map[string]float32)
	for i := range N {
		ranks[intToNode[i]] = vt.Data[i]
	}

	return ranks, nil
}
