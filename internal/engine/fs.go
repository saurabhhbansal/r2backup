package engine

import (
	"io"
	"io/fs"
	"os"
)

// FileSystem is the local-disk access the engine needs to upload a regular
// file: open its bytes and stat it before and after the transfer.
//
// It is an interface -- rather than a bare call to os.Open/os.Stat -- for the
// same reason Uploader and Reporter are: tests exercise locked files,
// permission errors, and a file that mutates mid-upload without touching a
// real filesystem, and without racing real filesystem timestamp granularity
// to do it.
type FileSystem interface {
	Open(path string) (io.ReadCloser, error)
	Stat(path string) (fs.FileInfo, error)
}

// osFS is the default FileSystem, backed by the real filesystem.
type osFS struct{}

func (osFS) Open(path string) (io.ReadCloser, error) { return os.Open(path) }
func (osFS) Stat(path string) (fs.FileInfo, error)   { return os.Stat(path) }
