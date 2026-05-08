package tensorx

import "fmt"

type Number interface {
	~int8 | ~int32 | ~float32
}

type Shape []int

type Tensor[T Number] struct {
	Data    []T
	Shape   Shape
	Strides []int
}

func NewTensor[T Number](data []T, shape Shape) (*Tensor[T], error) {
	expectedSize := 1
	for _, dim := range shape {
		if dim <= 0 {
			return nil, fmt.Errorf("invalid shape dimension %v", shape)
		}
		expectedSize *= dim
	}

	if len(data) < expectedSize {
		return nil, fmt.Errorf("data slice (len=%d) is less than required for shape (waiting=%d)", len(data), expectedSize)
	}

	strides := make([]int, len(shape))
	stride := 1
	for i := len(shape) - 1; i >= 0; i-- {
		strides[i] = stride
		stride *= shape[i]
	}

	return &Tensor[T]{
		Data:    data[:expectedSize],
		Shape:   shape,
		Strides: strides,
	}, nil
}
