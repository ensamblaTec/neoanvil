//go:build !linux

package astx

import (
	"fmt"
	"os"
)

func CreateShadowFile(prefix, suffix string, content []byte) (string, func(), error) {
	tmpFile, err := os.CreateTemp("", prefix+"*"+suffix)
	if err != nil {
		return "", func() {}, fmt.Errorf("os.CreateTemp failed: %w", err)
	}
	defer tmpFile.Close()

	if _, err := tmpFile.Write(content); err != nil {
		os.Remove(tmpFile.Name())
		return "", func() {}, fmt.Errorf("failed writing to temp file: %w", err)
	}

	path := tmpFile.Name()

	cleanupFn := func() {
		os.Remove(path)
	}

	return path, cleanupFn, nil
}
