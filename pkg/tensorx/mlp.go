package tensorx

import (
	"context"
	"fmt"
)

type MLP struct {
	W1 *Tensor[float32]
	W2 *Tensor[float32]
}

func NewMLP(w1, w2 *Tensor[float32]) *MLP {
	return &MLP{W1: w1, W2: w2}
}

func (m *MLP) Forward(ctx context.Context, cpu *CPUDevice, input *Tensor[float32]) (float32, error) {

	slab := cpu.pool.Acquire()
	defer cpu.pool.Release(slab, cap(slab.Data))

	const totalRequired = 9
	if cap(slab.Data) < totalRequired {
		slab.Data = make([]float32, totalRequired)
	} else {
		slab.Data = slab.Data[:totalRequired]
	}

	z1Data := slab.Data[0:4]
	z1, err := NewTensor(z1Data, Shape{1, 4})
	if err != nil {
		return 0, fmt.Errorf("failed to alloc z1: %w", err)
	}

	if err := cpu.MatMulF32(ctx, input, m.W1, z1); err != nil {
		return 0, fmt.Errorf("Z1 matmul failed: %w", err)
	}

	a1Data := slab.Data[4:8]
	a1, _ := NewTensor(a1Data, Shape{1, 4})
	for i := range 4 {
		val := z1.Data[i]
		if val < 0 {
			val = 0
		}
		a1.Data[i] = val
	}

	z2Data := slab.Data[8:9]
	z2, _ := NewTensor(z2Data, Shape{1, 1})

	if err := cpu.MatMulF32(ctx, a1, m.W2, z2); err != nil {
		return 0, fmt.Errorf("Z2 matmul failed: %w", err)
	}

	return z2.Data[0], nil
}

func (m *MLP) AdjustWeights(learningRate float32, success bool) {
	adjust := func(weights []float32) {
		for i := range weights {
			val := weights[i]
			if success {
				val = val * (1.0 + learningRate)
			} else {
				val = val * (1.0 - learningRate)
			}
			weights[i] = val
		}
	}

	adjust(m.W1.Data)
	adjust(m.W2.Data)
}
