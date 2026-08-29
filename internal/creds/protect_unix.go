//go:build !windows

package creds

// filePermissionProtector stores the secret as-is and relies on the file being
// 0600 in a 0700 directory.
//
// It is deliberately honest about being weak: protected() returns false, so
// `r2backup status` can tell the user what is actually guarding their key
// rather than implying encryption that is not happening. The Keychain and
// libsecret backends land with the account work.
type filePermissionProtector struct{}

func (filePermissionProtector) protect(b []byte) ([]byte, error)   { return b, nil }
func (filePermissionProtector) unprotect(b []byte) ([]byte, error) { return b, nil }
func (filePermissionProtector) name() string                       { return "file permissions (0600)" }
func (filePermissionProtector) protected() bool                    { return false }

func newProtector() protector { return filePermissionProtector{} }
