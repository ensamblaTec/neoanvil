package mctx

import (
	"fmt"
	"log"
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// DoubleBufferEngine custodia los Tensores de Inferencia (ML)
// Integrando Privilegios Híbridos: Prod en Hot-Memory, Dev en AES-GCM Enclave
type DoubleBufferEngine struct {
	slabsProd      [2][]float32
	activeSlab     atomic.Uint32
	readonlyHib    atomic.Bool
	devSeal        *sre.SecureSlab
	size           int
	meanThetaCache []float32
	distCache      []float32
	rawBuf         []byte
}

// Inicializa el motor neural
func NewFederatedEngine(tensorSize int) *DoubleBufferEngine {
	enclave, _ := sre.GetEnclave()
	initialBytes := make([]byte, tensorSize*4)
	sealed := enclave.Seal(initialBytes)

	return &DoubleBufferEngine{
		slabsProd: [2][]float32{
			make([]float32, tensorSize),
			make([]float32, tensorSize),
		},
		devSeal:        sealed,
		size:           tensorSize,
		meanThetaCache: make([]float32, tensorSize),
		distCache:      make([]float32, 1000),
		rawBuf:         make([]byte, tensorSize*4),
	}
}

func (e *DoubleBufferEngine) ReadProd() []float32 {
	idx := e.activeSlab.Load() & 1
	return e.slabsProd[idx]
}

// Purgado de Heap: Transmutación nativa 0 Allocs O(1) usando SliceHeader nativo
func float32ToBytes(floats []float32) []byte {
	if len(floats) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&floats[0])), len(floats)*4)
}

func bytesToFloat32(bytes []byte) []float32 {
	if len(bytes) == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&bytes[0])), len(bytes)/4)
}

func (e *DoubleBufferEngine) SyncQuorumState(activeSeminals int, totalSeminals int) {
	if float32(activeSeminals) <= float32(totalSeminals)/2.0 {
		e.readonlyHib.Store(true) // Split-Brain Hibernation
	} else {
		e.readonlyHib.Store(false)
	}
}

// Multi-Krum Bizantino (Parche 9) + Destrucción de Garbage Collector
func (e *DoubleBufferEngine) AggregateBackground(thetas [][]float32) {
	if e.readonlyHib.Load() {
		log.Println("[SRE-QUORUM] Aislamiento Bucle Detectado (Split-Brain). Mutaciones Vetadas.")
		return
	}
	if len(thetas) == 0 {
		return
	}

	enclave, _ := sre.GetEnclave()

	rawBytes, err := enclave.Unseal(e.devSeal, e.rawBuf)
	if err != nil {
		log.Printf("[SRE-ERROR] Missing unseal: %v\n", err)
		return
	}
	dev := bytesToFloat32(rawBytes)

	meanTheta := e.meanThetaCache[:e.size]
	clear(meanTheta)
	computeMeanTheta(thetas, meanTheta)

	if len(thetas) > len(e.distCache) {
		e.distCache = make([]float32, len(thetas)*2)
	}
	distances := e.distCache[:len(thetas)]
	meanDist := computeDistancesAndMean(thetas, meanTheta, distances)
	sigma3 := computeSigma3(distances, meanDist)

	for idx, t := range thetas {
		if distances[idx]-meanDist > sigma3 {
			log.Printf("[SRE-EKF] OOM Veneno Bizantino Descartado (Dist: %f, 3Sig: %f)\n", distances[idx], sigma3)
			continue
		}
		for i := 0; i < e.size && i < len(t); i++ {
			dev[i] += t[i]
		}
	}

	enclave.Free(e.devSeal)
	e.devSeal = enclave.Seal(float32ToBytes(dev))
	clear(rawBytes)
}

func computeMeanTheta(thetas [][]float32, meanTheta []float32) {
	for _, t := range thetas {
		for i := range t {
			meanTheta[i] += t[i]
		}
	}
	n := float32(len(thetas))
	for i := range meanTheta {
		meanTheta[i] /= n
	}
}

func computeDistancesAndMean(thetas [][]float32, meanTheta []float32, distances []float32) float32 {
	var meanDist float32
	for idx, t := range thetas {
		var dist float32
		for i := range t {
			diff := t[i] - meanTheta[i]
			dist += diff * diff
		}
		dist = float32(math.Sqrt(float64(dist)))
		distances[idx] = dist
		meanDist += dist
	}
	if len(distances) > 0 {
		meanDist /= float32(len(distances))
	}
	return meanDist
}

func computeSigma3(distances []float32, meanDist float32) float32 {
	var variance float32
	for _, d := range distances {
		diff := d - meanDist
		variance += diff * diff
	}
	stdDev := float32(math.Sqrt(float64(variance / float32(len(distances)))))
	return stdDev * 3.0
}

func (e *DoubleBufferEngine) SwapEpoch(decayRate float32) {
	oldIdx := e.activeSlab.Load() & 1
	newIdx := (oldIdx + 1) & 1

	enclave, _ := sre.GetEnclave()
	rawBytes, err := enclave.Unseal(e.devSeal, e.rawBuf)
	if err != nil {
		log.Printf("[SRE-ERROR] Missing unseal: %v\n", err)
		return
	}
	dev := bytesToFloat32(rawBytes)

	passiveProd := e.slabsProd[newIdx]
	copy(passiveProd, dev)

	e.activeSlab.Store(newIdx)

	oldDev := e.slabsProd[oldIdx]
	for i := range oldDev {
		oldDev[i] *= decayRate
	}

	enclave.Free(e.devSeal)
	e.devSeal = enclave.Seal(float32ToBytes(oldDev))

	clear(rawBytes)
}

func (dbe *DoubleBufferEngine) HLCDivergenceCheck(incomingTime int64, quorumMedian int64) error {
	divergence := incomingTime - quorumMedian
	if divergence < 0 {
		divergence = -divergence
	}
	if divergence > 500 {
		return fmt.Errorf("[SRE-QUARANTINE] Time-Taint Físico detectado: Divergencia %d ms", divergence)
	}
	return nil
}
