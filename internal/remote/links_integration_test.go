package remote_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// sendLink does what the relay does with one upload link: a PUT of exactly
// Length bytes with the signed headers, answering the ETag.
func sendLink(t *testing.T, l remote.UploadLink, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, l.URL, bytes.NewReader(data[l.Offset:l.Offset+l.Length]))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range l.Header {
		req.Header[k] = v
	}
	req.ContentLength = l.Length
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT by link: %s: %s", resp.Status, b)
	}
	return resp.Header.Get("ETag")
}

func TestUploadByLinkCarriesMetadata(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mtime := time.Date(2026, 3, 4, 5, 6, 7, 890, time.UTC)
	data := []byte(strings.Repeat("cloud to cloud ", 1000))
	plan, err := c.UploadLinks(ctx, remote.LinkUpload{
		Key: "machines/drime-mirror/x/current/a.txt", Size: int64(len(data)),
		Metadata: remote.Metadata{ModTime: mtime, Mode: 0o600, Kind: remote.KindFile}, ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.UploadID != "" || len(plan.Parts) != 1 {
		t.Fatalf("a small file should be one PUT, got %d parts, upload id %q", len(plan.Parts), plan.UploadID)
	}
	if err := c.FinishUpload(ctx, plan, []string{sendLink(t, plan.Parts[0], data)}); err != nil {
		t.Fatal(err)
	}
	obj, err := c.Get(ctx, plan.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if !obj.Metadata.ModTime.Equal(mtime) || obj.Metadata.Mode != 0o600 || obj.Metadata.Size != int64(len(data)) {
		t.Fatalf("metadata = %+v", obj.Metadata)
	}
	if obj.ContentType != "text/plain" {
		t.Fatalf("content type = %q", obj.ContentType)
	}
}

func TestUploadByLinkInParts(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	// Three parts: S3's 5 MiB floor applies to every part but the last.
	const part = 5 << 20
	data := make([]byte, 2*part+12345)
	for i := range data {
		data[i] = byte(i * 7)
	}
	plan, err := c.UploadLinks(ctx, remote.LinkUpload{Key: "big.bin", Size: int64(len(data)), PartSize: part,
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.UploadID == "" || len(plan.Parts) != 3 {
		t.Fatalf("want 3 parts in a multipart upload, got %d (id %q)", len(plan.Parts), plan.UploadID)
	}
	if plan.Parts[2].Offset != 2*part || plan.Parts[2].Length != 12345 {
		t.Fatalf("last part = %+v", plan.Parts[2])
	}
	etags := make([]string, 3)
	// Out of order, as parallel relay calls finish.
	for _, i := range []int{2, 0, 1} {
		etags[i] = sendLink(t, plan.Parts[i], data)
	}
	if err := c.FinishUpload(ctx, plan, etags); err != nil {
		t.Fatal(err)
	}
	obj, err := c.Get(ctx, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestAbandonedUploadByLinkLeavesNothing(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	const part = 5 << 20
	plan, err := c.UploadLinks(ctx, remote.LinkUpload{Key: "gone.bin", Size: 2 * part, PartSize: part})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AbandonUpload(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ups, err := c.ListMultipartUploads(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range ups {
		if u.Key == "gone.bin" {
			t.Fatal(fmt.Sprintf("upload %s still open", u.UploadID))
		}
	}
}
