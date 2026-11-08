package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// DownloadArchive fetches one platform's release archive and checks it
// against the release's checksums.txt before returning it. progress, when not
// nil, is called as bytes arrive with how many have come so far and the total
// (0 when the server does not say).
//
// It is Fetch for a caller that needs more than the one binary -- the Windows
// installer wants the launcher too -- and wants to show a progress bar.
func DownloadArchive(ctx context.Context, rel *Release, platform string, progress func(done, total int64)) (data []byte, url string, err error) {
	url, ok := rel.Assets[platform]
	if !ok {
		return nil, "", fmt.Errorf("release %s has no build for %s", rel.Version, platform)
	}
	if rel.Checksums == "" {
		// The installer never installs something it cannot check.
		return nil, "", fmt.Errorf("release %s publishes no checksums, so it cannot be verified", rel.Version)
	}
	sums, err := get(ctx, rel.Checksums, 1<<20)
	if err != nil {
		return nil, "", fmt.Errorf("fetch checksums: %w", err)
	}
	name := path.Base(url)
	want, err := checksumFor(string(sums), name)
	if err != nil {
		return nil, "", err
	}
	archive, err := getWithProgress(ctx, url, maxArchive, progress)
	if err != nil {
		return nil, "", err
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return nil, "", fmt.Errorf("checksum mismatch for %s: the download was not what it claimed to be, so nothing was installed", name)
	}
	return archive, url, nil
}

// ZipFile returns one file from a .zip archive by its base name, trying each
// name in order. ok is false when none is there.
func ZipFile(archive []byte, names ...string) (data []byte, ok bool, err error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, false, fmt.Errorf("open release archive: %w", err)
	}
	for _, want := range names {
		for _, f := range zr.File {
			if path.Base(f.Name) != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, false, err
			}
			b, err := io.ReadAll(io.LimitReader(rc, maxArchive))
			rc.Close()
			if err != nil {
				return nil, false, err
			}
			return b, true, nil
		}
	}
	return nil, false, nil
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	fn    func(done, total int64)
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	// A few updates a second is plenty for a progress bar, and calling back
	// on every 32 KB read would flood whatever draws it.
	if now := time.Now(); err != nil || now.Sub(p.last) >= 80*time.Millisecond {
		p.last = now
		p.fn(p.done, p.total)
	}
	return n, err
}

func getWithProgress(ctx context.Context, url string, limit int64, progress func(done, total int64)) ([]byte, error) {
	if progress == nil {
		return get(ctx, url, limit)
	}
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
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	pr := &progressReader{r: io.LimitReader(resp.Body, limit), total: total, fn: progress}
	return io.ReadAll(pr)
}
