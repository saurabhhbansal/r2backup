package minio

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// minioModule is the MinIO server release the tests run against, built from
// source. MinIO stopped publishing community binaries in late 2025: the
// dl.min.io URLs this harness used to download from answer 410 Gone, and the
// GitHub releases carry source only. A Go module version cannot move under
// us the way a "latest" download URL did, so this is also a pin.
//
// RELEASE.2025-10-15T17-29-55Z.
const minioModule = "github.com/minio/minio@v0.0.0-20251015172955-9e49d5e7a648"

// EnvMinioBinary names a MinIO server binary to use instead of building one.
const EnvMinioBinary = "R2BACKUP_MINIO_BIN"

// binaryName is "minio" everywhere except Windows, which needs the ".exe"
// suffix.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "minio.exe"
	}
	return "minio"
}

// cacheDir returns where the MinIO binary is kept across test runs, so a
// full test suite run does not rebuild a ~150MB binary every time.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "r2backup-test"), nil
}

// ensureBinary returns the path to a usable MinIO server binary, building it
// into cacheDir if it is not already there.
func ensureBinary() (string, error) {
	if bin := os.Getenv(EnvMinioBinary); bin != "" {
		return bin, nil
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create minio cache dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, binaryName())
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return path, nil
	}
	if err := buildBinary(dir, path); err != nil {
		return "", err
	}
	return path, nil
}

// buildBinary installs MinIO with `go install` into a directory of its own,
// then renames it to dest. It takes a few minutes the first time on a
// machine and is cached after that (CI caches dir across runs). Packages
// testing in parallel may each build it; the rename means none sees half a
// binary.
func buildBinary(dir, dest string) error {
	gobin, err := os.MkdirTemp(dir, ".minio-build-*")
	if err != nil {
		return fmt.Errorf("create minio build dir: %w", err)
	}
	defer os.RemoveAll(gobin)
	cmd := exec.Command("go", "install", minioModule)
	cmd.Env = append(os.Environ(), "GOBIN="+gobin, "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s: %w\n%s", minioModule, err, out)
	}
	if err := os.Rename(filepath.Join(gobin, binaryName()), dest); err != nil {
		if _, serr := os.Stat(dest); serr == nil {
			return nil // another package's build got there first
		}
		return fmt.Errorf("install minio binary: %w", err)
	}
	return nil
}
