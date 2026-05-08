package tensorx

import "context"

type ComputeDevice interface {
	MatMulF32(ctx context.Context, a, b *Tensor[float32], c *Tensor[float32]) error
	CosineDistance(a, b *Tensor[float32]) (float32, error)
}
