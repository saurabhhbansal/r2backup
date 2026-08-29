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
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

func TestChecksumForFindsTheRightLine(t *testing.T) {
	contents := strings.Join([]string{
		"aaaa  r2backup_1.0.0_linux_amd64.tar.gz",
		"bbbb *r2backup_1.0.0_windows_amd64.zip",
		"cccc  r2backup_1.0.0_darwin_arm64.tar.gz",
	}, "\n")
	for name, want := range map[string]string{
		"r2backup_1.0.0_linux_amd64.tar.gz":  "aaaa",
		"r2backup_1.0.0_windows_amd64.zip":   "bbbb",
		"r2backup_1.0.0_darwin_arm64.tar.gz": "cccc",
	} {
		got, err := checksumFor(contents, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("checksumFor(%s) = %q, want %q", name, got, want)
		}
	}
	if _, err := checksumFor(contents, "not_listed.tar.gz"); err == nil {
		t.Error("an unlisted asset should error rather than install unverified")
	}
}

// TestFetchRefusesATamperedArchive is the point of the whole checksum path: a
// download that is not what the release says it is must never reach the disk
// the binary is executed from.
func TestFetchRefusesATamperedArchive(t *testing.T) {
	name := "r2backup_1.0.0_" + Platform() + archiveExt()
	honest := []byte("the real binary")
	tampered := []byte("something else entirely")

	var archive []byte
	if runtime.GOOS == "windows" {
		archive = zipped(t, "r2backup.exe", tampered)
	} else {
		archive = tarGz(t, "r2backup", tampered)
	}
	// The checksum published is for the honest artifact, not this one.
	sum := sha256.Sum256(honest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()

	rel := &Release{
		Version:   "v1.0.0",
		Assets:    map[string]string{Platform(): srv.URL + "/" + name},
		Checksums: srv.URL + "/checksums.txt",
	}
	if _, err := Fetch(context.Background(), rel); err == nil {
		t.Fatal("Fetch accepted an archive whose checksum did not match")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should name the mismatch, got: %v", err)
	}
}

func TestFetchExtractsTheBinaryFromAVerifiedArchive(t *testing.T) {
	name := "r2backup_1.0.0_" + Platform() + archiveExt()
	body := []byte("#!/bin/sh\necho hello\n")

	var archive []byte
	if runtime.GOOS == "windows" {
		archive = zipped(t, "r2backup.exe", body)
	} else {
		archive = tarGz(t, "r2backup", body)
	}
	sum := sha256.Sum256(archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()

	rel := &Release{
		Version:   "v1.0.0",
		Assets:    map[string]string{Platform(): srv.URL + "/" + name},
		Checksums: srv.URL + "/checksums.txt",
	}
	r, err := Fetch(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	if got.String() != string(body) {
		t.Errorf("extracted %q, want %q", got.String(), body)
	}
}

func TestFetchRejectsAnArchiveWithoutTheBinary(t *testing.T) {
	name := "r2backup_1.0.0_" + Platform() + archiveExt()
	var archive []byte
	if runtime.GOOS == "windows" {
		archive = zipped(t, "README", []byte("nope"))
	} else {
		archive = tarGz(t, "README", []byte("nope"))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()
	rel := &Release{Version: "v1.0.0", Assets: map[string]string{Platform(): srv.URL + "/" + name}}
	if _, err := Fetch(context.Background(), rel); err == nil {
		t.Fatal("an archive with no r2backup binary should error")
	}
}

func TestFetchWithNoBuildForThisPlatform(t *testing.T) {
	rel := &Release{Version: "v1.0.0", Assets: map[string]string{"plan9_386": "http://example.invalid"}}
	if _, err := Fetch(context.Background(), rel); err == nil {
		t.Fatal("a release with no build for this platform should error clearly")
	}
}

// TestAssetNameMatchesGoreleaser guards the one thing that silently breaks
// updates: if the archive naming in .goreleaser.yaml and AssetName here ever
// drift apart, the updater stops finding its own releases.
func TestAssetNameMatchesGoreleaser(t *testing.T) {
	// .goreleaser.yaml: r2backup_{{ .Version }}_{{ .Os }}_{{ .Arch }}
	if got := AssetName("v2.5.1", "linux", "amd64"); got != "r2backup_2.5.1_linux_amd64.tar.gz" {
		t.Errorf("got %q; .goreleaser.yaml produces r2backup_2.5.1_linux_amd64.tar.gz", got)
	}
	if got := AssetName("v2.5.1", "windows", "arm64"); got != "r2backup_2.5.1_windows_arm64.zip" {
		t.Errorf("got %q; .goreleaser.yaml produces r2backup_2.5.1_windows_arm64.zip", got)
	}
}

func archiveExt() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// TestFetchExtractsEitherBinaryName is the one thing the r2backup -> r2b
// rename could have broken silently. An installed copy looks inside a
// downloaded archive for a file by name, so the *old* version has to be able
// to open the *new* archive -- and the way to find that out the hard way is a
// release that every existing installation reports as "no binary in the
// archive". Both spellings extract, in both directions.
func TestFetchExtractsEitherBinaryName(t *testing.T) {
	for _, inArchive := range []string{"r2b", "r2backup"} {
		t.Run(inArchive, func(t *testing.T) {
			member := inArchive
			if runtime.GOOS == "windows" {
				member += ".exe"
			}
			name := "r2backup_1.0.0_" + Platform() + archiveExt()
			body := []byte("binary bytes for " + inArchive)

			var archive []byte
			if runtime.GOOS == "windows" {
				archive = zipped(t, member, body)
			} else {
				archive = tarGz(t, member, body)
			}
			sum := sha256.Sum256(archive)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "checksums.txt") {
					fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
					return
				}
				w.Write(archive)
			}))
			defer srv.Close()

			r, err := Fetch(context.Background(), &Release{
				Version:   "v1.0.0",
				Assets:    map[string]string{Platform(): srv.URL + "/" + name},
				Checksums: srv.URL + "/checksums.txt",
			})
			if err != nil {
				t.Fatalf("archive containing %q: %v", member, err)
			}
			var got bytes.Buffer
			if _, err := got.ReadFrom(r); err != nil {
				t.Fatal(err)
			}
			if got.String() != string(body) {
				t.Errorf("extracted %q, want %q", got.String(), body)
			}
		})
	}
}
