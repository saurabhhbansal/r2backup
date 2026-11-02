package minio

import "testing"

// TestBinary builds (or finds) the MinIO server the integration tests use.
// CI runs it on its own before the suite, so the build happens once rather
// than in every package that starts a server at the same moment.
func TestBinary(t *testing.T) {
	if _, err := ensureBinary(); err != nil {
		unavailable(t, err)
	}
}
