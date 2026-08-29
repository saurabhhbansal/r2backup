package e2e

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
)

// Deleting a torn object is the right call -- corrupt bytes that look valid are
// worse than an absent file, because a restore hands them back without a word.
// But it leaves a window: the key is now empty, and if a good version had been
// sitting there, the question is whether it is still recoverable.
//
// It is, and this proves it rather than assuming it. A run trashes the outgoing
// version BEFORE it uploads the replacement, so the previous copy is already in
// trash by the time the torn write is discarded. The failure is reported, the
// old version can be recovered, and the next run re-uploads. What must never
// happen is all three going wrong at once: a silent success, a corrupt object,
// and no way back.
func TestATornUploadLeavesThePreviousVersionRecoverable(t *testing.T) {
	const relPath = "important.bin"
	const size = 2 << 20

	original := bytes.Repeat([]byte{0xA1}, size)

	var h *harness
	var armed bool
	h = newHarnessWithClient(t, "TornRecovery", withPutHook(relPath, func(attempt int) {
		// Only start tearing once the good version is safely stored.
		if !armed {
			return
		}
		newContent := bytes.Repeat([]byte{byte(0xE0 + attempt)}, size)
		path := filepath.Join(h.root, relPath)
		if err := os.WriteFile(path, newContent, 0o644); err != nil {
			t.Errorf("mutate during attempt %d: %v", attempt, err)
			return
		}
		stamp := time.Now().Add(time.Duration(attempt+1) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Errorf("chtimes during attempt %d: %v", attempt, err)
		}
	}))

	// 1. Store a good version.
	path := filepath.Join(h.root, relPath)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	first := h.backupRun(t, withTrash(h))
	if first.Uploaded != 1 || !first.Succeeded() {
		t.Fatalf("first run did not store the file cleanly: %+v", first.Failures)
	}

	// 2. Change it so the next run wants to replace it, then make every
	//    upload attempt tear.
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xB2}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	armed = true

	second := h.backupRun(t, withTrash(h))

	// 3. The run must report the failure rather than claim success.
	if second.Succeeded() {
		t.Fatal("a file that tore on every attempt was reported as a successful upload")
	}
	var named bool
	for _, f := range second.Failures {
		if strings.Contains(f.Key, relPath) {
			named = true
		}
	}
	if !named {
		t.Errorf("the failure did not name %q: %+v", relPath, second.Failures)
	}

	// 4. Nothing torn is live at the key.
	if _, err := h.client.Head(context.Background(), h.currentPrefix()+"/"+relPath); err == nil {
		t.Error("a torn object is still live at the key; a restore would hand back corrupt bytes")
	}

	// 5. And the version that was good is still recoverable from trash. This
	//    is the part that makes deleting the torn object safe rather than
	//    merely tidy.
	entries, err := h.client.List(context.Background(), h.set.Prefix+"/trash/")
	if err != nil {
		t.Fatal(err)
	}
	var recovered bool
	for _, e := range entries {
		obj, err := h.client.Get(context.Background(), e.Key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, original) {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatalf("the previous good version is not recoverable from trash (%d trash objects).\n"+
			"Discarding the torn write would then be data loss rather than a safe refusal.", len(entries))
	}
}

func withTrash(h *harness) func(*backup.Options) {
	return func(o *backup.Options) {
		o.Trash = backup.NewTrash(h.client, h.set.RetentionDays)
	}
}
