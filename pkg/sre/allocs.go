package sre

import (
	"bytes"
	"encoding/json"
	"sync"
)

var jsonBufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// ZeroAllocJSONMarshal serializa una estructura usando un encoder y un sync.Pool de bytes para evitar Escapes to Heap repetitivos
func ZeroAllocJSONMarshal(v any, fn func([]byte) error) error {
	buf := jsonBufPool.Get().(*bytes.Buffer)
	buf.Reset()

	if err := json.NewEncoder(buf).Encode(v); err != nil {
		jsonBufPool.Put(buf)
		return err
	}

	// [SRE-BUG-FIX] Copy bytes before returning the buffer to the pool.
	// BoltDB stores a reference (not a copy) to inode.value until node.write() at
	// transaction commit. If db.Batch coalesces goroutines and the pool buffer is
	// reused by a concurrent writer before the transaction commits, the pending
	// write is silently overwritten → "unexpected end of JSON input" / corrupt JSON.
	b := buf.Bytes()
	data := make([]byte, len(b))
	copy(data, b)
	jsonBufPool.Put(buf)

	return fn(data)
}
