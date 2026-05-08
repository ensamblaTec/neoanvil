//go:build darwin

package sre

import (
	"log"
	"runtime"
)

// PinThread Darwin Mock (SchedSetaffinity native requires CGO)
func PinThread(coreID int) {
	runtime.LockOSThread()
	// Aislamiento lógico Mac - OSX prefiere Soft-affinity nativa
	log.Printf("[SRE] Thread Lock atado conceptualmente al Núcleo P/E-Core %d\n", coreID)
}

// UnpinThread releases the OS-thread lock acquired by PinThread. [367.A]
func UnpinThread() {
	runtime.UnlockOSThread()
}
