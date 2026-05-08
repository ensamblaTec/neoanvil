//go:build linux

package sre

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// PinThread Enjaula el Hilo en el Núcleo del Procesador, bloqueando Spectre (Side-Channel Cache).
// Core 0/1 -> Wasm Alien. Core 2/3 -> IA/Enclave Secrets.
func PinThread(coreID int) {
	runtime.LockOSThread()
	var cpuset unix.CPUSet
	cpuset.Set(coreID)
	unix.SchedSetaffinity(0, &cpuset) //nolint:errcheck // best-effort affinity; failure is non-fatal
}

// UnpinThread releases the OS-thread lock acquired by PinThread. [367.A]
func UnpinThread() {
	runtime.UnlockOSThread()
}
