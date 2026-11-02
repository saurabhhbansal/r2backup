package drime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

func TestDownloadLinkIsTheStorageRedirect(t *testing.T) {
	f := newFake(t)
	f.redirect = true
	c := newClient(t, f, nil)
	data := bytes.Repeat([]byte("0123456789"), 100)
	mtime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	put(t, c, set+"/current/a.bin", data, remote.Metadata{ModTime: mtime, Mode: 0o640})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	l, err := c.DownloadLink(context.Background(), set+"/current/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if l.Size != int64(len(data)) || !l.Metadata.ModTime.Equal(mtime) || l.Metadata.Mode != 0o640 {
		t.Fatalf("link = %+v", l)
	}
	if left := time.Until(l.Expires); left < 29*time.Minute || left > 31*time.Minute {
		t.Fatalf("link expires in %s, want about 30 minutes", left)
	}

	// The link works with no token, and a Range of it is a piece.
	req, _ := http.NewRequest(http.MethodGet, l.URL, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(got) != "0123456789" {
		t.Fatalf("range read: %s %q", resp.Status, got)
	}
}

func TestDownloadLinkWhenDrimeServesItself(t *testing.T) {
	f := newFake(t) // no redirect: the content comes straight back
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.bin", []byte("x"), remote.Metadata{})
	_, err := c.DownloadLink(context.Background(), set+"/current/a.bin")
	if !errors.Is(err, remote.ErrNoLink) {
		t.Fatalf("err = %v, want ErrNoLink", err)
	}
}

func TestDownloadLinkMissingKey(t *testing.T) {
	f := newFake(t)
	f.redirect = true
	c := newClient(t, f, nil)
	_, err := c.DownloadLink(context.Background(), set+"/current/none")
	if !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetStillWorksThroughTheRedirect(t *testing.T) {
	f := newFake(t)
	f.redirect = true
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("hello"), remote.Metadata{})
	got, _ := get(t, c, set+"/current/a.txt")
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}
