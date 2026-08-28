package minio

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// binaryName is "minio" everywhere except Windows, which needs the ".exe"
// suffix both in the cached filename and in the release URL.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "minio.exe"
	}
	return "minio"
}

// downloadURL returns the release URL for this platform's MinIO server
// binary. MinIO publishes a fixed URL per OS/arch under dl.min.io that
// always resolves to their current stable build -- there is no version
// segment to pin in that path.
func downloadURL() (string, error) {
	var osName string
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		osName = runtime.GOOS
	default:
		return "", fmt.Errorf("unsupported OS for the minio test binary: %s", runtime.GOOS)
	}

	var arch string
	switch runtime.GOARCH {
	case "amd64", "arm64":
		arch = runtime.GOARCH
	default:
		return "", fmt.Errorf("unsupported architecture for the minio test binary: %s", runtime.GOARCH)
	}

	return fmt.Sprintf("https://dl.min.io/server/minio/release/%s-%s/%s", osName, arch, binaryName()), nil
}

// cacheDir returns where downloaded MinIO binaries are kept across test
// runs, so a full test suite run does not re-download a ~100MB binary
// every time.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "r2backup-test"), nil
}

// ensureBinary returns the path to a usable MinIO server binary, downloading
// it into cacheDir if it is not already there.
func ensureBinary() (string, error) {
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

	url, err := downloadURL()
	if err != nil {
		return "", err
	}
	if err := downloadFile(url, path); err != nil {
		return "", err
	}
	return path, nil
}

// downloadFile fetches url and atomically installs it at dest with the
// executable bit set. It writes to a temp file in the same directory first
// and renames into place, so a failed or interrupted download never leaves
// a corrupt binary at dest for the next test run to trip over.
func downloadFile(url, dest string) (err error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".minio-download-*")
	if err != nil {
		return fmt.Errorf("create temp file for minio download: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if _, err = io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("save minio download: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("save minio download: %w", err)
	}
	if err = os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("make minio binary executable: %w", err)
	}
	if err = os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install minio binary: %w", err)
	}
	return nil
}
