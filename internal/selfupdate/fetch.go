package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"runtime"
	"strings"
	"time"
)

// maxArchive bounds what will be pulled into memory. A release archive is a
// few megabytes; anything wildly larger is not one of ours.
const maxArchive = 128 << 20

// Fetch downloads this platform's archive, verifies it against the release's
// published checksums, and returns the binary from inside it.
//
// The checksum is verified on the ARCHIVE before anything is unpacked. Checking
// after extraction would mean writing the contents of an unverified download to
// disk first, which is the wrong order for a file that is about to be executed.
func Fetch(ctx context.Context, rel *Release) (io.Reader, error) {
	url, ok := rel.Assets[Platform()]
	if !ok {
		return nil, fmt.Errorf("release %s has no build for %s", rel.Version, Platform())
	}
	archive, err := get(ctx, url, maxArchive)
	if err != nil {
		return nil, err
	}

	if rel.Checksums != "" {
		sums, err := get(ctx, rel.Checksums, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("fetch checksums: %w", err)
		}
		name := path.Base(url)
		want, err := checksumFor(string(sums), name)
		if err != nil {
			return nil, err
		}
		got := sha256.Sum256(archive)
		if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
			return nil, fmt.Errorf("checksum mismatch for %s: the download was not what it claimed to be, so nothing was replaced", name)
		}
	}

	bin, err := extract(archive, url)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(bin), nil
}

// checksumFor pulls one file's hash out of a checksums.txt.
func checksumFor(contents, name string) (string, error) {
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in checksums.txt", name)
}

// binaryNames is what the main binary may be called inside a release
// archive, newest spelling first.
//
// The command was renamed from r2backup to r2b, and the archive is the one
// place where that is not merely cosmetic: an installed copy looks inside a
// downloaded archive for a file by name, so a release that renames the file
// is a release the previous version cannot install. Both names are accepted
// so that is a one-time cost paid once, and never again on the next rename.
func binaryNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"r2b.exe", "r2backup.exe"}
	}
	return []string{"r2b", "r2backup"}
}

func wantedName(names []string) string { return names[0] }

func matches(base string, names []string) bool {
	for _, n := range names {
		if base == n {
			return true
		}
	}
	return false
}

// extract pulls the r2b binary out of a .zip or .tar.gz.
func extract(archive []byte, url string) ([]byte, error) {
	names := binaryNames()
	wanted := wantedName(names)

	if strings.HasSuffix(url, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("open release archive: %w", err)
		}
		for _, f := range zr.File {
			if !matches(path.Base(f.Name), names) {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxArchive))
		}
		return nil, fmt.Errorf("release archive contains no %s", wanted)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("release archive contains no %s", wanted)
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if matches(path.Base(h.Name), names) {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
