package wasm

import (
	"context"
	"testing"
)

// Minimal WebAssembly module with a single exported function `add` that
// returns sum of two i64 inputs. Built from this wat:
//
//	(module
//	  (func (export "add") (param i64 i64) (result i64)
//	    local.get 0
//	    local.get 1
//	    i64.add))
//
// Hand-encoded to avoid pulling in wat2wasm at test time.
var addWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, // magic
	0x01, 0x00, 0x00, 0x00, // version
	// type section — (i64 i64) -> i64
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7e, 0x7e, 0x01, 0x7e,
	// function section — one function, type 0
	0x03, 0x02, 0x01, 0x00,
	// export section — "add" → func 0
	0x07, 0x07, 0x01, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00,
	// code section — local.get 0; local.get 1; i64.add; end
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x7c, 0x0b,
}

// TestNewWasmHypervisor_ZeroValue [Épica 230.F]
func TestNewWasmHypervisor_ZeroValue(t *testing.T) {
	if h := NewWasmHypervisor(); h == nil {
		t.Fatal("NewWasmHypervisor returned nil")
	}
}

// TestExecWasm_Add exercises the full compile → instantiate → call path
// with a known-good Wasm blob. Confirms sandbox isolation works E2E.
// [Épica 230.F]
func TestExecWasm_Add(t *testing.T) {
	h := NewWasmHypervisor()
	ctx := t.Context()

	results, err := h.ExecWasm(ctx, addWasm, "add", 7, 11)
	if err != nil {
		t.Fatalf("ExecWasm: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0] != 18 {
		t.Errorf("7+11 expected 18, got %d", results[0])
	}
}

// TestExecWasm_InvalidBytes [Épica 230.F]
func TestExecWasm_InvalidBytes(t *testing.T) {
	h := NewWasmHypervisor()
	ctx := context.Background()
	_, err := h.ExecWasm(ctx, []byte{0xde, 0xad, 0xbe, 0xef}, "add", 0, 0)
	if err == nil {
		t.Error("expected compile error on garbage input, got nil")
	}
}

// TestExecWasm_UnknownFunction [Épica 230.F]
func TestExecWasm_UnknownFunction(t *testing.T) {
	h := NewWasmHypervisor()
	ctx := context.Background()
	_, err := h.ExecWasm(ctx, addWasm, "does_not_exist", 0, 0)
	if err == nil {
		t.Error("expected error for missing export, got nil")
	}
}
