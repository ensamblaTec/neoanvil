package sre

import (
	"context"
	"crypto/sha256"
	"runtime"
	"time"
)

// MaskTEMPESTSignature inunda las asimetrías térmicas y eléctricas de la CPU con criptografía simulada
// para cegar ataques Físicos de Side-Channel (Power Analysis y Radiofrecuencias).
// Accepts a context for clean shutdown — the goroutine exits when ctx is cancelled.
func MaskTEMPESTSignature(ctx context.Context) {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		dummy := make([]byte, 1024)
		ticker := time.NewTicker(10 * time.Microsecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sha256.Sum256(dummy)
				runtime.Gosched()
			}
		}
	}()
}
