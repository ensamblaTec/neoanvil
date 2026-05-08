// Test-only helpers used by archive_test.go's path-traversal probe.
// Kept in a separate file so the production archive.go doesn't expose
// these constructors.

package brain

import (
	"archive/tar"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
)

func zstdNewWriterForTest(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w)
}

func tarNewWriterForTest(w io.Writer) *tar.Writer {
	return tar.NewWriter(w)
}

func tarHeaderRegular(name string, data []byte) *tar.Header {
	return &tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(data)),
		ModTime:  time.Unix(0, 0),
		Typeflag: tar.TypeReg,
	}
}
