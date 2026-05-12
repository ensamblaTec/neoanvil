package wasmx

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

type Sandbox struct {
	runtime wazero.Runtime
	module  wazero.CompiledModule
	cpu     *tensorx.CPUDevice
}

func NewSandbox(ctx context.Context, wasmBinary []byte, cpu *tensorx.CPUDevice) (*Sandbox, error) {
	r := wazero.NewRuntime(ctx)

	compiled, err := r.CompileModule(ctx, wasmBinary)
	if err != nil {
		return nil, fmt.Errorf("failed to compile module wasm: %w", err)
	}

	return &Sandbox{
		runtime: r,
		module:  compiled,
		cpu:     cpu,
	}, nil
}

func (sandbox *Sandbox) Execute(ctx context.Context, code string, maxFuel uint64) (score float32, metricsTable string, err error) {
	return EvaluatePlan(ctx, sandbox.cpu, code)
}

func (sandbox *Sandbox) Close(ctx context.Context) error {
	return sandbox.runtime.Close(ctx)
}

func (sandbox *Sandbox) Runtime() wazero.Runtime {
	return sandbox.runtime
}

type LocalEmbedder struct {
	dim          int
	fallbackMode atomic.Bool
	mod          atomic.Pointer[wazero.CompiledModule]
	runtime      wazero.Runtime
}

func NewLocalEmbedder(ctx context.Context, runtime wazero.Runtime, workspace string) *LocalEmbedder {
	wasmPath := filepath.Join(workspace, ".neo", "models", "embedder.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		log.Printf("[SRE-WARN] embedder.wasm no encontrado. Usando Fast-Hash Fallback.")
		e := &LocalEmbedder{dim: 768, runtime: runtime}
		e.fallbackMode.Store(true)
		return e
	}

	embedder := &LocalEmbedder{
		dim:     768,
		runtime: runtime,
	}

	go func() {
		wasmBytes, err := os.ReadFile(wasmPath)
		if err != nil {
			log.Printf("[SRE-WARN] Error asíncrono leyendo embedder.wasm: %v", err)
			embedder.fallbackMode.Store(true)
			return
		}

		mod, err := runtime.CompileModule(ctx, wasmBytes)
		if err != nil {
			log.Printf("[SRE-WARN] Error asíncrono compilando embedder.wasm: %v", err)
			embedder.fallbackMode.Store(true)
			return
		}

		embedder.mod.Store(&mod)
	}()

	return embedder
}

func (l *LocalEmbedder) Dimension() int {
	return l.dim
}

func (l *LocalEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {

	if l.fallbackMode.Load() || l.mod.Load() == nil {
		return l.fastHashFallback(text), nil
	}

	mod, err := l.runtime.InstantiateModule(ctx, *l.mod.Load(), wazero.NewModuleConfig().WithName("embedder"))
	if err != nil {
		log.Printf("[SRE-WARN] WASM instantiate failed: %v. Usando Fallback.", err)
		return l.fastHashFallback(text), nil
	}
	defer mod.Close(ctx)

	embedFn := mod.ExportedFunction("embed")
	mallocFn := mod.ExportedFunction("malloc")
	freeFn := mod.ExportedFunction("free")

	if embedFn == nil || mallocFn == nil {
		return l.fastHashFallback(text), nil
	}

	textLen := uint64(len(text))
	res, err := mallocFn.Call(ctx, textLen)
	if err != nil || len(res) == 0 {
		return nil, fmt.Errorf("WASM malloc failed for input")
	}
	textPtr := res[0]

	if !mod.Memory().Write(uint32(textPtr), []byte(text)) {
		return nil, fmt.Errorf("failed to write text to WASM linear memory")
	}

	outSize := uint64(l.dim * 4)
	resOut, err := mallocFn.Call(ctx, outSize)
	if err != nil || len(resOut) == 0 {
		return nil, fmt.Errorf("WASM malloc failed for output tensor")
	}
	outPtr := resOut[0]

	if freeFn != nil {
		defer freeFn.Call(ctx, textPtr, textLen)
		defer freeFn.Call(ctx, outPtr, outSize)
	}

	_, err = embedFn.Call(ctx, textPtr, textLen, outPtr)
	if err != nil {
		return nil, fmt.Errorf("WASM neural inference crashed: %w", err)
	}

	bytes, ok := mod.Memory().Read(uint32(outPtr), uint32(outSize))
	if !ok {
		return nil, fmt.Errorf("failed to read tensor from WASM memory")
	}

	vec := make([]float32, l.dim)
	for i := 0; i < l.dim; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(bytes[i*4 : (i+1)*4]))
	}

	return vec, nil
}

func (l *LocalEmbedder) fastHashFallback(text string) []float32 {
	vec := make([]float32, l.dim)
	if len(text) == 0 {
		return vec
	}
	for i := 0; i < len(text); i++ {
		vec[i%l.dim] += float32(text[i])
	}
	var norm float32
	for i := range l.dim {
		norm += vec[i] * vec[i]
	}
	if norm > 0 {
		inv := 1.0 / float32(math.Sqrt(float64(norm)))
		for j := range l.dim {
			vec[j] *= inv
		}
	}
	return vec
}

