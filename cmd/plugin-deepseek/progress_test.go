package main

import (
	"testing"
)

func TestProgressEventFormat(t *testing.T) {
	var captured map[string]any
	notifyFn := func(n map[string]any) {
		captured = n
	}

	EmitProgress(notifyFn, "tok-42", 3, 10, "processing file")

	if captured == nil {
		t.Fatal("expected notify to be called")
	}
	if captured["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", captured["jsonrpc"])
	}
	if captured["method"] != "notifications/progress" {
		t.Errorf("method = %v, want notifications/progress", captured["method"])
	}
	params, ok := captured["params"].(map[string]any)
	if !ok {
		t.Fatal("params missing or wrong type")
	}
	if params["progressToken"] != "tok-42" {
		t.Errorf("progressToken = %v, want tok-42", params["progressToken"])
	}
	if params["progress"] != int64(3) {
		t.Errorf("progress = %v, want 3", params["progress"])
	}
	if params["total"] != int64(10) {
		t.Errorf("total = %v, want 10", params["total"])
	}
	if params["message"] != "processing file" {
		t.Errorf("message = %v, want 'processing file'", params["message"])
	}
}

func TestProgressEventNoOpWhenNilToken(t *testing.T) {
	called := false
	EmitProgress(func(_ map[string]any) { called = true }, nil, 1, 5, "")
	if called {
		t.Error("EmitProgress should be no-op when token is nil")
	}
}

func TestProgressEventNoOpWhenNilNotify(t *testing.T) {
	// Must not panic.
	EmitProgress(nil, "tok", 1, 5, "")
}

func TestProgressEventNoMessageField(t *testing.T) {
	var captured map[string]any
	EmitProgress(func(n map[string]any) { captured = n }, "t", 0, 0, "")
	params, _ := captured["params"].(map[string]any)
	if _, hasMsg := params["message"]; hasMsg {
		t.Error("message field should be absent when msg is empty")
	}
}
