package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// Removing a set must not leave its unfinished uploads on the server: they
// are billed, and they show up in no object listing, so once the local
// record of them is gone (DropSetUploads, called by both remove paths)
// nothing can ever find them again to stop the charge. These tests exercise
// `r2b remove` end to end -- through the real cobra command, against a real
// MinIO bucket -- rather than unit-testing abortSetUploads in isolation,
// because the thing the finding was actually about is what the command does
// when it is run, not what one helper function does in the middle of it.

// bareMultipartUpload starts a multipart upload directly against MinIO,
// bypassing remote.Client entirely -- which has no exported way to start one
// on its own, deliberately: production code only ever starts one as part of
// Put's resumable path. This is the same "talk to MinIO's S3 API directly"
// escape hatch test/minio's own createBucket uses, and it is exactly what a
// test needs here: an upload the app's own client had no hand in creating,
// standing in for one whose CreateMultipartUpload succeeded but whose first
// SavePendingUpload never landed (a crash between the two), which is the
// case `remove --purge` exists to still catch.
func bareMultipartUpload(t *testing.T, cfg remote.Config, key string) string {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	raw := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
		o.Region = "auto"
	})
	out, err := raw.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("start bare multipart upload: %v", err)
	}
	return aws.ToString(out.UploadId)
}

// testCredsFor turns the config a minio.StartWithConfig mutate callback
// captured into the Credentials shape the app actually stores on disk --
// the same conversion secondcomputer_test.go's bucketWithABackupInIt does.
func testCredsFor(cfg remote.Config) creds.Credentials {
	return creds.Credentials{
		AccountID:       "acct",
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		Bucket:          cfg.Bucket,
		Endpoint:        cfg.Endpoint,
	}
}

// seedAppForRemoveTest opens a fresh app under a temp data dir, saves creds
// reaching the given bucket, and records one set. It is closed before
// returning, exactly as TestTheDashboardSeesAndDoesRealWork closes its own
// setup app before opening the thing actually under test -- bbolt's lock is
// exclusive, and the real `remove` command opens its own app right after.
func seedAppForRemoveTest(t *testing.T, cfg remote.Config, set sets.Set) {
	t.Helper()
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()
	if err := a.creds.Save(testCredsFor(cfg)); err != nil {
		t.Fatalf("save creds: %v", err)
	}
	if err := a.sets.Add(set); err != nil {
		t.Fatalf("add set: %v", err)
	}
}

// runRemove runs the real `remove` command through cobra, the way a user's
// shell would invoke it, and returns what it printed.
func runRemove(t *testing.T, args ...string) (out string, err error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := NewRoot(&Options{Out: &stdout, Err: &stderr})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"remove"}, args...))
	err = root.Execute()
	return stdout.String(), err
}

// A pending upload the index knows about is exactly what DropSetUploads
// forgets today with nothing having aborted it first. After a plain
// `r2b remove` (no --purge -- this must not depend on the destructive flag,
// because a stray part is not part of anyone's backup either way), the
// server must no longer be holding it.
func TestRemoveAbortsAPendingUploadRecordedInTheIndex(t *testing.T) {
	var cfg remote.Config
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	defer cleanup()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	set := sets.Set{
		Name: "Big", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Big", RetentionDays: 30,
	}
	seedAppForRemoveTest(t, cfg, set)

	key := set.KeyScope() + "current/movie.bin"
	uploadID := bareMultipartUpload(t, cfg, key)

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	idx, release, err := a.checkoutIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SavePendingUpload(index.PendingUpload{
		Key: key, UploadID: uploadID, PartSize: 5 << 20,
		Size: 100 << 20, ModTime: time.Now().UnixNano(), StartedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	release()
	a.close()

	out, err := runRemove(t, "Big")
	if err != nil {
		t.Fatalf("remove: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "stopped 1 unfinished upload") {
		t.Errorf("output did not report the aborted upload:\n%s", out)
	}

	live, err := client.ListMultipartUploads(context.Background(), set.KeyScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("the server is still holding %+v after remove", live)
	}
}

// --purge additionally has to catch an upload the index never recorded --
// the case a lost or hand-edited index, or a crash between
// CreateMultipartUpload and the first save, leaves behind. Only --purge
// pays for the extra bucket listing this needs.
func TestPurgeAbortsAnUploadTheIndexNeverKnewAbout(t *testing.T) {
	var cfg remote.Config
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	defer cleanup()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	set := sets.Set{
		Name: "Big", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Big", RetentionDays: 30,
	}
	seedAppForRemoveTest(t, cfg, set)

	key := set.KeyScope() + "current/movie.bin"
	// Started directly against the bucket and never written to the index --
	// standing in for the upload no local record has any idea about.
	bareMultipartUpload(t, cfg, key)

	out, err := runRemove(t, "Big", "--purge")
	if err != nil {
		t.Fatalf("remove --purge: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "stopped 1 unfinished upload") {
		t.Errorf("output did not report the aborted upload:\n%s", out)
	}

	live, err := client.ListMultipartUploads(context.Background(), set.KeyScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("the server is still holding %+v after remove --purge", live)
	}
}

// An abort that fails -- here, because the bucket is simply gone by the time
// remove tries to reach it -- must not strand the set half-removed. The
// local records are the only thing that would let a retry ever find this
// upload again, so refusing to drop them over a network failure would not
// protect anything; it would only leave the user with a set they can
// neither use nor get rid of.
func TestRemoveSucceedsEvenWhenAbortFails(t *testing.T) {
	var cfg remote.Config
	cleanupCalled := false
	_, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	defer func() {
		if !cleanupCalled {
			cleanup()
		}
	}()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	set := sets.Set{
		Name: "Big", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Big", RetentionDays: 30,
	}
	seedAppForRemoveTest(t, cfg, set)

	key := set.KeyScope() + "current/movie.bin"
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	idx, release, err := a.checkoutIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SavePendingUpload(index.PendingUpload{
		Key: key, UploadID: "whatever-was-in-progress", PartSize: 5 << 20,
		Size: 100 << 20, ModTime: time.Now().UnixNano(), StartedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	release()
	a.close()

	// The server is gone before remove ever gets a chance to reach it, so
	// the abort this drives is a genuine network failure, not a manufactured
	// one.
	cleanup()
	cleanupCalled = true

	out, err := runRemove(t, "Big")
	if err != nil {
		t.Fatalf("remove should still succeed when the abort fails: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "could not be reached and may still be billed") {
		t.Errorf("output did not say the abort failed:\n%s", out)
	}

	a2, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a2.close()
	if _, err := a2.sets.Get("Big"); err == nil {
		t.Error("the set is still there; a failed abort should not have blocked removal")
	}
	idx2, release2, err := a2.checkoutIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	all, err := idx2.AllPendingUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("the local record should have gone with the set regardless: %+v", all)
	}
}
