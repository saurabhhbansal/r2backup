package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// L3: `edit` on the command line has always saved the chosen excludes and
// then backed up right away -- unlike the dashboard's `e`, which only saves
// and tells you to press `b`. That difference is a product decision and
// stays; what does not is that the command line used to start a backup, which
// spends operations and a real transfer, without saying so first. finishEdit
// is the half of `edit` after the picker closes, split out (the same reason
// askForNewRoot stands apart from offerRelink) so this can be driven without
// a terminal or a real picker.
//
// This exercises finishEdit against a real MinIO bucket rather than a fake:
// the thing the finding was actually about is what a real run prints and in
// what order, not what one helper does with its side effects stubbed out.
func TestEditAnnouncesTheBackupBeforeItStarts(t *testing.T) {
	var cfg remote.Config
	_, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	defer cleanup()
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := sets.Set{
		Name: "Photos", Root: root, Machine: "testpc",
		Prefix: "machines/testpc/Photos", RetentionDays: 30,
	}
	seedAppForRemoveTest(t, cfg, set) // saves creds and adds the set; closed before returning

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	ctx := context.Background()
	if err := a.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var out bytes.Buffer
	opts := &Options{Out: &out, Err: &out}
	// A chosen exclude set that differs from the set's current one (empty),
	// so finishEdit takes the "something changed" branch instead of
	// returning early on "No change." -- the branch that leads to a backup.
	if err := finishEdit(ctx, a, opts, set, []string{"node_modules"}); err != nil {
		t.Fatalf("finishEdit: %v\noutput:\n%s", err, out.String())
	}

	got := out.String()
	const announce = "Backing up now to apply the change."
	if !strings.Contains(got, announce) {
		t.Fatalf("edit started a backup without announcing it first; output:\n%s", got)
	}
	// The announcement is the whole point of the fix: it must come before
	// anything a completed run prints, not merely appear somewhere in the
	// transcript.
	announceAt := strings.Index(got, announce)
	summaryAt := strings.Index(got, set.Name+":")
	if summaryAt == -1 {
		t.Fatalf("the backup this test drove never reported a summary; output:\n%s", got)
	}
	if announceAt > summaryAt {
		t.Fatalf("the backup was summarised before it was announced; output:\n%s", got)
	}

	// The excludes must actually have been saved -- finishEdit's other job,
	// which the announcement must not have displaced.
	updated, err := a.sets.Get(set.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Excludes) != 1 || updated.Excludes[0] != "node_modules" {
		t.Errorf("excludes = %v, want [node_modules]", updated.Excludes)
	}
}

// `edit` requires a terminal because the picker needs one; that guard runs
// before anything else in the command, including the announcement above, so
// a scheduled or scripted invocation -- which has no terminal -- must still
// return promptly with a plain error instead of blocking on a prompt nobody
// can answer. This is the "unattended path" half of L3: the fix adds a
// message, not a question, so there is nothing here for --yes/--no to
// resolve and nothing that can hang.
func TestEditRefusesUnattendedWithoutHanging(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	var out, errOut bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &errOut})
	root.SetOut(&out)
	root.SetErr(&errOut)
	// The set need not exist: the interactive check runs first and must
	// fail before anything looks it up.
	root.SetArgs([]string{"edit", "Photos"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("edit succeeded with no terminal attached, which means it tried to open the picker")
		}
		if !strings.Contains(err.Error(), "needs a terminal") {
			t.Errorf("error = %q, want it to say a terminal is needed", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("edit did not return promptly with no terminal attached -- it must not block on a prompt")
	}
}
