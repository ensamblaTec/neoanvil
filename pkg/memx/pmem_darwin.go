//go:build darwin

package memx

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Fallback Unix MAP local sin acelerador DAX para testeo de aritméticas RelPtr en M3
func MountPMEM(dbPath string, mmapSize int64, requireEncryption bool) (*BumpAllocator, error) {
	if mmapSize <= 0 {
		mmapSize = 1024 * 1024 * 10
	}

	file, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	file.Truncate(mmapSize)

	// Sólo MAP_SHARED existe en kernel XNU Apple Caches.
	flags := unix.MAP_SHARED

	data, err := unix.Mmap(int(file.Fd()), 0, int(mmapSize), unix.PROT_READ|unix.PROT_WRITE, flags)
	if err != nil {
		return nil, err
	}

	// Cursor starts at 8 to reserve relative pointer 0 as Nil evaluation
	alloc := &BumpAllocator{
		baseMap:  data,
		basePtr:  unsafe.Pointer(&data[0]),
		capacity: uint64(mmapSize),
	}
	alloc.cursor.Store(8)

	return alloc, nil
}
