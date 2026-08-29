//go:build windows

package creds

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// dpapiProtector encrypts with the Windows Data Protection API, tied to the
// current user account. Another user on the same machine -- and anyone who
// walks off with the drive -- cannot read the result.
//
// CRYPTPROTECT_UI_FORBIDDEN is set because this runs from a scheduled task
// with no interactive desktop: without it, a prompt would appear on a session
// nobody is looking at and the backup would hang instead of running.
type dpapiProtector struct{}

func (dpapiProtector) protect(plaintext []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(plaintext))}
	if len(plaintext) > 0 {
		in.Data = &plaintext[0]
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafePointer(out.Data))))
	return copyBlob(out), nil
}

func (dpapiProtector) unprotect(ciphertext []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(ciphertext))}
	if len(ciphertext) > 0 {
		in.Data = &ciphertext[0]
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafePointer(out.Data))))
	return copyBlob(out), nil
}

func (dpapiProtector) name() string    { return "Windows DPAPI (this user account)" }
func (dpapiProtector) protected() bool { return true }

func newProtector() protector { return dpapiProtector{} }
