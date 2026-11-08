package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveRelease serves one archive and a checksums.txt that lists sum for it.
func serveRelease(t *testing.T, archive []byte, sum string) *Release {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/r2backup_1.0.0_windows_amd64.zip", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  r2backup_1.0.0_windows_amd64.zip\n", sum)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Release{
		Version:   "v1.0.0",
		Assets:    map[string]string{"windows_amd64": srv.URL + "/r2backup_1.0.0_windows_amd64.zip"},
		Checksums: srv.URL + "/checksums.txt",
	}
}

func TestDownloadArchiveVerifiesAndReportsProgress(t *testing.T) {
	archive := zipped(t, "r2bw.exe", []byte("launcher"))
	got := sha256.Sum256(archive)
	rel := serveRelease(t, archive, hex.EncodeToString(got[:]))

	var last int64
	data, _, err := DownloadArchive(context.Background(), rel, "windows_amd64", func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(archive)) {
		t.Fatalf("progress ended at %d, want %d", last, len(archive))
	}
	b, ok, err := ZipFile(data, "r2b.exe", "r2bw.exe")
	if err != nil || !ok || string(b) != "launcher" {
		t.Fatalf("ZipFile = %q, %v, %v", b, ok, err)
	}
	if _, ok, _ := ZipFile(data, "missing.exe"); ok {
		t.Fatal("found a file that is not there")
	}
}

func TestDownloadArchiveRefusesATamperedArchive(t *testing.T) {
	rel := serveRelease(t, zipped(t, "r2b.exe", []byte("evil")), strings.Repeat("0", 64))
	if _, _, err := DownloadArchive(context.Background(), rel, "windows_amd64", nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
}

func TestDownloadArchiveRefusesAReleaseWithoutChecksums(t *testing.T) {
	rel := &Release{Version: "v1.0.0", Assets: map[string]string{"windows_amd64": "http://127.0.0.1:1/x.zip"}}
	if _, _, err := DownloadArchive(context.Background(), rel, "windows_amd64", nil); err == nil {
		t.Fatal("installed something it could not check")
	}
}
