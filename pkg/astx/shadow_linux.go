//go:build linux

package astx

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func CreateShadowFile(prefix, suffix string, content []byte) (string, func(), error) {
	fd, err := unix.MemfdCreate(prefix, 0)
	if err != nil {
		return "", func() {}, fmt.Errorf("memfd_create failed: %w", err)
	}

	for len(content) > 0 {
		n, writeErr := unix.Write(fd, content)
		if writeErr != nil {
			unix.Close(fd)
			return "", func() {}, fmt.Errorf("failed writing to memfd: %w", writeErr)
		}
		content = content[n:]
	}

	path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fd)

	cleanupFn := func() {
		unix.Close(fd)
	}

	return path, cleanupFn, nil
}
