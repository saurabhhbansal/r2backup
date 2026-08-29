// Package minio starts a real, local MinIO server for integration tests, so
// they run against actual S3 semantics -- pagination, multipart, server-side
// copy, metadata casing -- instead of a hand-rolled mock that only reflects
// back whatever the mock author assumed was true.
package minio

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

const (
	accessKeyID     = "minioadmin"
	secretAccessKey = "minioadminsecret"
	bucketName      = "r2backup-test"

	readyTimeout = 20 * time.Second
)

// Start launches a local MinIO server on a free port with a temporary data
// directory, waits for it to actually accept requests, creates a bucket,
// and returns a remote.Client configured against it plus a cleanup function
// that stops the server and removes its data directory. The caller must run
// the cleanup, typically via t.Cleanup or defer.
//
// If MinIO cannot be downloaded or started -- no network, an unsupported
// platform, a server that never comes up -- Start skips the test (t.Skip)
// with an explanation rather than failing it. These are integration tests
// meant to exercise a real server when one is available; its absence is an
// environment fact, not a defect in the code under test.
func Start(t testing.TB) (*remote.Client, func()) {
	t.Helper()
	return StartWithConfig(t, nil)
}

// StartWithConfig works like Start, but calls mutate on the remote.Config
// just before the client is built, so a caller can install its own
// HTTPClient -- e.g. a RoundTripper that hooks or throttles individual
// requests to make an otherwise-racy scenario (a file changing mid-upload,
// say) deterministic instead of timing-dependent. mutate must not change
// Endpoint, Bucket, or the credentials; it exists to layer behaviour onto
// the transport, not to repoint the client at something else. Pass nil for
// plain Start behaviour.
func StartWithConfig(t testing.TB, mutate func(*remote.Config)) (*remote.Client, func()) {
	t.Helper()

	bin, err := ensureBinary()
	if err != nil {
		t.Skipf("minio test harness unavailable: %v", err)
		return nil, nil
	}

	proc, err := startServer(bin)
	if err != nil {
		t.Skipf("minio test harness unavailable: %v", err)
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := createBucket(ctx, proc.endpoint); err != nil {
		proc.stop()
		t.Skipf("minio test harness unavailable: %v", err)
		return nil, nil
	}

	cfg := remote.Config{
		Endpoint:        proc.endpoint,
		Bucket:          bucketName,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	client, err := remote.New(context.Background(), cfg)
	if err != nil {
		proc.stop()
		t.Fatalf("minio test harness: build remote client: %v", err)
		return nil, nil
	}

	return client, proc.stop
}

// serverProc is a running MinIO server.
type serverProc struct {
	cmd      *exec.Cmd
	dataDir  string
	endpoint string
	output   *bytes.Buffer
}

func startServer(bin string) (*serverProc, error) {
	dataDir, err := os.MkdirTemp("", "r2backup-minio-data-")
	if err != nil {
		return nil, fmt.Errorf("create minio data dir: %w", err)
	}

	port, err := freePort()
	if err != nil {
		os.RemoveAll(dataDir)
		return nil, err
	}
	consolePort, err := freePort()
	if err != nil {
		os.RemoveAll(dataDir)
		return nil, err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	consoleAddr := fmt.Sprintf("127.0.0.1:%d", consolePort)

	cmd := exec.Command(bin, "server", dataDir, "--address", addr, "--console-address", consoleAddr)
	cmd.Env = append(os.Environ(),
		"MINIO_ROOT_USER="+accessKeyID,
		"MINIO_ROOT_PASSWORD="+secretAccessKey,
		"MINIO_BROWSER=off",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		os.RemoveAll(dataDir)
		return nil, fmt.Errorf("start minio server: %w", err)
	}

	proc := &serverProc{cmd: cmd, dataDir: dataDir, endpoint: "http://" + addr, output: &output}

	if err := waitReady(proc.endpoint, readyTimeout); err != nil {
		proc.stop()
		return nil, fmt.Errorf("minio server did not become ready: %w\noutput:\n%s", err, output.String())
	}

	return proc, nil
}

func (p *serverProc) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
	os.RemoveAll(p.dataDir)
}

// freePort asks the OS for an ephemeral port by binding to port 0 and
// reading back what it chose, then releases it immediately. There is a
// window between that release and MinIO binding the same port, but it is
// short enough in practice that this is the standard "find a free port"
// idiom rather than a real flakiness risk.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitReady polls MinIO's health endpoint until it answers 200 or timeout
// elapses. Sleeping a fixed duration and hoping is what this replaces: a
// slow CI box needs longer than a fast laptop, and a fixed sleep is either
// too short (flaky) or wastefully long on every run that didn't need it.
func waitReady(endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(endpoint + "/minio/health/live")
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health check returned %s", resp.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

// createBucket makes the test bucket using a plain S3 client built directly
// against the MinIO server. This deliberately does not go through
// remote.Client: bucket creation is not part of that package's API (r2backup
// never creates R2 buckets at runtime, only reads and writes within one), so
// the test harness talks to MinIO's S3 API on its own for this one step.
func createBucket(ctx context.Context, endpoint string) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	)
	if err != nil {
		return fmt.Errorf("load config for bucket setup: %w", err)
	}

	cli := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.Region = "auto"
	})

	if _, err := cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		return fmt.Errorf("create bucket %q: %w", bucketName, err)
	}
	return nil
}
