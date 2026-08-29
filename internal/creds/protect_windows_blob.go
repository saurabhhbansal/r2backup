//go:build windows

package creds

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// copyBlob copies a DPAPI output blob into Go-managed memory before the
// Windows allocation is freed.
func copyBlob(b windows.DataBlob) []byte {
	if b.Size == 0 || b.Data == nil {
		return nil
	}
	src := unsafe.Slice(b.Data, b.Size)
	out := make([]byte, b.Size)
	copy(out, src)
	return out
}

func unsafePointer(p *byte) unsafe.Pointer { return unsafe.Pointer(p) }
