//go:build windows

package winconsole

import "unsafe"

// unsafePtr keeps the single unsafe.Pointer conversion this package needs in
// one named place, rather than inline in the syscall.
func unsafePtr(p *uint32) unsafe.Pointer { return unsafe.Pointer(p) }
