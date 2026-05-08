package telemetry

import "os"

// testMkdirAll is a thin wrapper so the test file doesn't need to
// directly import os (keeps the production code's import surface clean).
// [Épica 230.E]
func testMkdirAll(dir string) error { return os.MkdirAll(dir, 0755) }
