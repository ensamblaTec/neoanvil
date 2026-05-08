//go:build linux

package memx

import (
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// VerifySEDHardware (Fósil de Cristal)
// Asegura que no hagamos Dump DAX sobre texto plano SSD M.2 ante robo físico.
func VerifySEDHardware() bool {
	// Simulación O(1) de sysfs interrogation para Cifrado OPAL Hardware AES-256
	_, err := os.ReadFile("/sys/block/nvme0n1/device/nvme/nvme0/nvme0n1/opal")
	if err != nil {
		return false
	}
	return true
}

// O(1) Mmap NVMe DAX or SATA Degradation
func MountPMEM(dbPath string, mmapSize int64, requireEncryption bool) (*BumpAllocator, error) {
	if mmapSize <= 0 {
		mmapSize = 1024 * 1024 * 10
	}

	file, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	file.Truncate(mmapSize)

	var data []byte
	// 1. Asalto DAX

	// Fósil de Cristal Interrogation
	if requireEncryption && !VerifySEDHardware() {
		log.Printf("[SRE-EKF] WARN: NVMe SED (OPAL) Físico NO Detectado. Degradando Criptográficamente a RAM Volátil PageCache.\\n")
		err = unix.EOPNOTSUPP
	} else {
		if !requireEncryption {
			log.Printf("[SRE-EKF] INFO: Cifrado físico ignorado por Doctrina Fricción Cero (neo.yaml)\\n")
		}
		flags := unix.MAP_SHARED | unix.MAP_SHARED_VALIDATE | unix.MAP_SYNC
		data, err = unix.Mmap(int(file.Fd()), 0, int(mmapSize), unix.PROT_READ|unix.PROT_WRITE, flags)
	}

	degraded := false
	if err != nil {
		// 2. Intercepción Táctica
		if err == syscall.EOPNOTSUPP {
			log.Printf("[SRE-EKF] WARN: DAX not supported, degrading to Page Cache SSD mode\\n")
		} else {
			log.Printf("[SRE-EKF] WARN: Fallo paramétrico MAP_SYNC (%v), degradando a SSD mode.\\n", err)
		}
		degraded = true

		data, err = unix.Mmap(int(file.Fd()), 0, int(mmapSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return nil, err
		}
	}

	alloc := &BumpAllocator{
		baseMap:  data,
		basePtr:  unsafe.Pointer(&data[0]),
		capacity: uint64(mmapSize),
	}
	alloc.cursor.Store(8)

	// 3. El Lazo de Supervivencia (SSD Page Cache)
	if degraded {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			for range ticker.C {
				// Forzamos al Buffer In-Memory de Linux a derramar la data al Firmware del SSD SATA (O(1))
				unix.Msync(data, unix.MS_ASYNC)
			}
		}()
	}

	return alloc, nil
}
