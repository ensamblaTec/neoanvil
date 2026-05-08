package sre

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// TestZeroAllocJSONMarshal_Roundtrip [Épica 231.A]
func TestZeroAllocJSONMarshal_Roundtrip(t *testing.T) {
	type payload struct {
		Foo string `json:"foo"`
		Bar int    `json:"bar"`
	}
	in := payload{Foo: "hello", Bar: 42}
	var got payload
	err := ZeroAllocJSONMarshal(in, func(b []byte) error {
		return json.Unmarshal(b, &got)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip mismatch: want %+v got %+v", in, got)
	}
}

// TestZeroAllocJSONMarshal_EncoderError [Épica 231.A]
// Encode errors must NOT leak the buffer.
func TestZeroAllocJSONMarshal_EncoderError(t *testing.T) {
	// json-incompatible value: channel.
	ch := make(chan int)
	err := ZeroAllocJSONMarshal(ch, func(b []byte) error { return nil })
	if err == nil {
		t.Error("expected encode error for unsupported type")
	}
}

// TestZeroAllocJSONMarshal_CallbackError [Épica 231.A]
// When the callback returns err, it should propagate.
func TestZeroAllocJSONMarshal_CallbackError(t *testing.T) {
	sentinel := errors.New("callback failure")
	err := ZeroAllocJSONMarshal(map[string]int{"x": 1}, func(_ []byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestZeroAllocJSONMarshal_Concurrent [Épica 231.A]
// Parallel workers exercise the sync.Pool path — must not corrupt outputs
// even when the buffer is reused across goroutines.
func TestZeroAllocJSONMarshal_Concurrent(t *testing.T) {
	var wg sync.WaitGroup
	failures := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			in := map[string]int{"n": n}
			var got map[string]int
			err := ZeroAllocJSONMarshal(in, func(b []byte) error {
				return json.Unmarshal(b, &got)
			})
			if err != nil {
				failures <- err
				return
			}
			if got["n"] != n {
				failures <- errors.New("cross-goroutine corruption detected")
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
