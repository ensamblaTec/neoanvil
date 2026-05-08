package wasm

import (
	"context"
	"fmt"
	"log"

	"github.com/tetratelabs/wazero"
)

// WasmHypervisor SRE v1 (Aislamiento Extremo O(N))
type WasmHypervisor struct{}

func NewWasmHypervisor() *WasmHypervisor {
	return &WasmHypervisor{}
}

// ExecWasm compila e instancia Wasm dinámicamente con aislamiento absoluto.
func (h *WasmHypervisor) ExecWasm(ctx context.Context, wasmBytes []byte, functionName string, params ...uint64) ([]uint64, error) {
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }()

	compiledMod, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		log.Printf("[SRE-WASM] Fallo de JIT eBPF Compilación: %v\n", err)
		return nil, fmt.Errorf("wasm compile fail: %w", err)
	}

	mod, err := r.InstantiateModule(ctx, compiledMod, wazero.NewModuleConfig())
	if err != nil {
		log.Printf("[SRE-WASM] Fatal error montando entorno Sandbox: %v\n", err)
		return nil, fmt.Errorf("wasm instantiate fail: %w", err)
	}

	fn := mod.ExportedFunction(functionName)
	if fn == nil {
		return nil, fmt.Errorf("function %s not found in Wasm binary", functionName)
	}

	results, err := fn.Call(ctx, params...)
	if err != nil {
		return nil, fmt.Errorf("wasm pure call fail: %w", err)
	}

	return results, nil
}
