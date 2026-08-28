package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// The index is only correct if the caller learns exactly which keys landed.
// Counts alone leave two wrong choices after a partial failure: record
// everything planned (and never retry the failures) or record nothing (and
// re-upload the whole set next run). These tests hold that line.

func collect(t *testing.T) (opts func(*Options), uploaded *[]string, moved *[][2]string) {
	t.Helper()
	var mu sync.Mutex
	up := &[]string{}
	mv := &[][2]string{}
	return func(o *Options) {
		o.OnUploaded = func(e scan.Entry, etag string) {
			mu.Lock()
			defer mu.Unlock()
			*up = append(*up, e.Key)
		}
		o.OnMoved = func(from, to string, size int64) {
			mu.Lock()
			defer mu.Unlock()
			*mv = append(*mv, [2]string{from, to})
		}
	}, up, mv
}

func TestOnUploadedFiresForEveryKeyThatLanded(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 12)
	up := newFakeUploader()

	with, got, _ := collect(t)
	o := Options{Root: "", Uploader: up, FS: fsys}
	with(&o)

	res, err := New(o).Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != res.Uploaded {
		t.Fatalf("OnUploaded fired %d times but Result.Uploaded is %d; the index and the summary would disagree",
			len(*got), res.Uploaded)
	}
	seen := map[string]bool{}
	for _, k := range *got {
		if seen[k] {
			t.Errorf("OnUploaded fired twice for %q; the index would double-count", k)
		}
		seen[k] = true
	}
	for _, u := range p.Uploads {
		if !seen[u.Key] {
			t.Errorf("OnUploaded never fired for %q, which the plan says was uploaded", u.Key)
		}
	}
}

func TestOnUploadedNeverFiresForAFailure(t *testing.T) {
	// The important one. A key that failed must not reach the index, or it is
	// recorded as backed up and never tried again.
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 6)
	up := newFakeUploader()
	failing := p.Uploads[2].Key
	up.putErr = map[string]error{failing: errors.New("503 slow down")}

	with, got, _ := collect(t)
	o := Options{Root: "", Uploader: up, FS: fsys}
	with(&o)

	res, err := New(o).Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range *got {
		if k == failing {
			t.Fatalf("OnUploaded fired for %q, which failed to upload.\n"+
				"Recording it would mark a file as backed up when it is not in the bucket.", failing)
		}
	}
	if len(*got) != 5 {
		t.Errorf("OnUploaded fired %d times, want 5 -- the other five must still be recorded", len(*got))
	}
	if len(res.Failed) != 1 {
		t.Errorf("Failed = %v, want exactly the one failure", res.Failed)
	}
}

func TestOnUploadedSkipsAFileThatCouldNotBeOpened(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 4)
	locked := p.Uploads[1].Key
	fsys.openErr[locked] = errors.New("locked by another process")

	with, got, _ := collect(t)
	o := Options{Root: "", Uploader: newFakeUploader(), FS: fsys}
	with(&o)

	if _, err := New(o).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, k := range *got {
		if k == locked {
			t.Fatalf("a file that could never be read was reported as uploaded: %q", locked)
		}
	}
	if len(*got) != 3 {
		t.Errorf("OnUploaded fired %d times, want 3", len(*got))
	}
}

func TestCallbacksAreOptional(t *testing.T) {
	// Leaving them nil must not panic; the engine is usable without them.
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 3)
	if _, err := New(Options{Root: "", Uploader: newFakeUploader(), FS: fsys}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}
