package remote_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// Removing a set. AbortUpload and ListMultipartUploads exist for one caller:
// `r2b remove`, which needs to stop the server billing for an upload nobody
// ever finished before the local record that names it is gone for good. See
// internal/cli's remove tests for the end-to-end version of this; these
// exercise the two new client methods directly against a real server.

// AbortUpload stops an upload the caller already knows about -- the ordinary
// case, where the index still has the record.
func TestAbortUploadStopsAnUploadTheCallerAlreadyKnowsAbout(t *testing.T) {
	store := newMemStore()
	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	defer cleanup()

	path, _ := bigFile(t)
	if err := putFile(t, client, "big/file.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	u, ok, err := store.Resumable("big/file.bin")
	if err != nil || !ok {
		t.Fatalf("the interrupted upload should have left a record: ok=%v err=%v", ok, err)
	}

	if err := client.AbortUpload(context.Background(), u.Key, u.UploadID); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}

	live, err := client.ListMultipartUploads(context.Background(), "big/")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("the server is still holding %+v after AbortUpload", live)
	}
}

// Aborting an upload the server has already let go of -- because this is a
// second attempt, or because it never existed under this id -- must not be
// reported as a failure: that state is exactly what an abort is trying to
// reach.
func TestAbortingAGoneUploadIsNotAnError(t *testing.T) {
	store := newMemStore()
	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	defer cleanup()

	path, _ := bigFile(t)
	if err := putFile(t, client, "big/file.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	u, _, _ := store.Resumable("big/file.bin")

	if err := client.AbortUpload(context.Background(), u.Key, u.UploadID); err != nil {
		t.Fatalf("first abort: %v", err)
	}
	if err := client.AbortUpload(context.Background(), u.Key, u.UploadID); err != nil {
		t.Fatalf("second abort on an already-gone upload should not error: %v", err)
	}
}

// ListMultipartUploads is the one way to find an upload the local index
// never recorded: it asks the bucket directly, which is what `remove
// --purge` needs to catch what a lost or hand-edited index would otherwise
// miss.
//
// A store is configured here, not omitted -- the tempting way to write
// "the index never knew about this" -- because Put falls back to the SDK's
// own manager.Uploader without one (see Put and this package's own doc
// comment), and that uploader aborts on any failure by itself. That would
// leave nothing on the server for this test to find, and would be testing
// the SDK's cleanup rather than ListMultipartUploads. So the record is made
// and then thrown away, standing in for a crash between
// CreateMultipartUpload and the first save of it, or an index that was
// simply lost -- the case this method exists to still catch.
func TestListMultipartUploadsFindsWhatTheIndexNeverRecorded(t *testing.T) {
	store := newMemStore()
	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	defer cleanup()

	path, _ := bigFile(t)
	if err := putFile(t, client, "big/untracked.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	if err := store.ForgetResumable("big/untracked.bin"); err != nil {
		t.Fatal(err)
	}
	if store.len() != 0 {
		t.Fatal("the record should be gone, standing in for one the index never had")
	}

	live, err := client.ListMultipartUploads(context.Background(), "big/")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Key != "big/untracked.bin" {
		t.Fatalf("ListMultipartUploads = %+v, want one upload for big/untracked.bin", live)
	}

	if err := client.AbortUpload(context.Background(), live[0].Key, live[0].UploadID); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	live, err = client.ListMultipartUploads(context.Background(), "big/")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("still listed after abort: %+v", live)
	}
}

// A prefix scopes ListMultipartUploads the same way it scopes List: an
// upload under a different prefix must not show up in another set's
// listing, or --purge would go around aborting uploads that belong to sets
// nobody asked it to touch.
func TestListMultipartUploadsIsScopedToItsPrefix(t *testing.T) {
	var cfg remote.Config
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	defer cleanup()

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
		o.Region = "auto"
	})
	for _, key := range []string{"machines/pc/A/current/f.bin", "machines/pc/B/current/g.bin"} {
		if _, err := raw.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String(cfg.Bucket), Key: aws.String(key),
		}); err != nil {
			t.Fatalf("start bare multipart upload for %q: %v", key, err)
		}
	}

	live, err := client.ListMultipartUploads(context.Background(), "machines/pc/A/")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Key != "machines/pc/A/current/f.bin" {
		t.Fatalf("ListMultipartUploads(\"machines/pc/A/\") = %+v, want only the A upload", live)
	}
}
