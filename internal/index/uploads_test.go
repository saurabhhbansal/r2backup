package index

import (
	"testing"
	"time"
)

func pending(key string, started time.Time, parts ...PendingPart) PendingUpload {
	var size int64 = 100 << 20
	return PendingUpload{
		Key: key, UploadID: "upload-" + key, PartSize: 16 << 20,
		Size: size, ModTime: 1234, Parts: parts, StartedAt: started.UnixNano(),
	}
}

func TestAPendingUploadSurvivesAReopen(t *testing.T) {
	db := openTestDB(t)
	u := pending("machines/pc/Set/current/big.bin", time.Now(),
		PendingPart{Number: 1, ETag: "a", Size: 16 << 20},
		PendingPart{Number: 2, ETag: "b", Size: 16 << 20})
	if err := db.SavePendingUpload(u); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.PendingUploadFor(u.Key)
	if err != nil || !ok {
		t.Fatalf("PendingUploadFor: ok=%v err=%v", ok, err)
	}
	if got.UploadID != u.UploadID || got.PartSize != u.PartSize || got.Size != u.Size {
		t.Errorf("read back %+v, want %+v", got, u)
	}
	if len(got.Parts) != 2 || got.Parts[1].ETag != "b" {
		t.Errorf("parts came back as %+v", got.Parts)
	}
}

// The record is what a killed process leaves behind, so it is written after
// every part. Overwriting must replace the part list, not merge into it.
func TestSavingAgainReplacesTheParts(t *testing.T) {
	db := openTestDB(t)
	key := "k"
	if err := db.SavePendingUpload(pending(key, time.Now(), PendingPart{Number: 1, ETag: "a", Size: 5})); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePendingUpload(pending(key, time.Now(),
		PendingPart{Number: 1, ETag: "a", Size: 5},
		PendingPart{Number: 2, ETag: "b", Size: 5})); err != nil {
		t.Fatal(err)
	}
	got, _, err := db.PendingUploadFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(got.Parts))
	}
}

func TestAnUnknownKeyIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	_, ok, err := db.PendingUploadFor("never/seen")
	if err != nil {
		t.Fatalf("a key with no record should not be an error: %v", err)
	}
	if ok {
		t.Error("reported a record that was never written")
	}
}

func TestForgettingLeavesNothingBehind(t *testing.T) {
	db := openTestDB(t)
	if err := db.SavePendingUpload(pending("k", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetPendingUpload("k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.PendingUploadFor("k"); ok {
		t.Error("the record is still there")
	}
	// Forgetting what was never there is how a completed upload with no
	// record behaves, and must not be an error.
	if err := db.ForgetPendingUpload("k"); err != nil {
		t.Errorf("forgetting twice: %v", err)
	}
}

// What the interface reads to say "3.1 GB of 4 GB already sent" about work
// that was interrupted, without asking the bucket anything.
func TestPendingBytesAddsUpWhatIsAlreadyOnTheServer(t *testing.T) {
	db := openTestDB(t)
	if err := db.SavePendingUpload(pending("a", time.Now(),
		PendingPart{Number: 1, Size: 10}, PendingPart{Number: 2, Size: 20})); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePendingUpload(pending("b", time.Now(), PendingPart{Number: 1, Size: 5})); err != nil {
		t.Fatal(err)
	}
	done, total, files, err := db.PendingBytes()
	if err != nil {
		t.Fatal(err)
	}
	if done != 35 {
		t.Errorf("done = %d, want 35", done)
	}
	if want := int64(2 * (100 << 20)); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if files != 2 {
		t.Errorf("files = %d, want 2", files)
	}
}

// A removed set must not leave records behind: the sweep would keep asking
// the bucket about an upload for a folder nobody backs up any more.
func TestRemovingASetDropsItsPendingUploads(t *testing.T) {
	db := openTestDB(t)
	for _, k := range []string{
		"machines/pc/Photos/current/a.bin",
		"machines/pc/Photos/current/b.bin",
		"machines/pc/Docs/current/c.bin",
	} {
		if err := db.SavePendingUpload(pending(k, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DropSetUploads("machines/pc/Photos/"); err != nil {
		t.Fatal(err)
	}
	all, err := db.AllPendingUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Key != "machines/pc/Docs/current/c.bin" {
		t.Fatalf("left %+v, want only the Docs record", all)
	}
}

// A prefix must not match a set whose name merely starts with the same
// letters -- the same trap KeyScope exists to avoid elsewhere.
func TestDroppingOneSetDoesNotTakeItsNamesake(t *testing.T) {
	db := openTestDB(t)
	for _, k := range []string{
		"machines/pc/Photo/current/a.bin",
		"machines/pc/Photos/current/b.bin",
	} {
		if err := db.SavePendingUpload(pending(k, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DropSetUploads("machines/pc/Photo/"); err != nil {
		t.Fatal(err)
	}
	all, err := db.AllPendingUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Key != "machines/pc/Photos/current/b.bin" {
		t.Fatalf("left %+v, want only the Photos record", all)
	}
}

// PendingUploadsUnderPrefix is what a caller reads before DropSetUploads
// erases the very thing it needs: the upload ids to abort on the server. It
// must return the same records DropSetUploads would delete, and none of the
// ones a namesake prefix would wrongly catch.
func TestPendingUploadsUnderPrefixMatchesWhatDropSetUploadsWouldDelete(t *testing.T) {
	db := openTestDB(t)
	for _, k := range []string{
		"machines/pc/Photo/current/a.bin",
		"machines/pc/Photos/current/b.bin",
		"machines/pc/Photos/current/c.bin",
		"machines/pc/Docs/current/d.bin",
	} {
		if err := db.SavePendingUpload(pending(k, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.PendingUploadsUnderPrefix("machines/pc/Photos/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want the 2 under Photos/: %+v", len(got), got)
	}
	for _, u := range got {
		if u.Key == "machines/pc/Photo/current/a.bin" || u.Key == "machines/pc/Docs/current/d.bin" {
			t.Errorf("PendingUploadsUnderPrefix returned a record outside the prefix: %+v", u)
		}
	}

	// The records are still there afterwards -- this reads, it does not
	// drop, which is the entire reason it needs to exist as its own method
	// rather than everyone just calling DropSetUploads and losing the upload
	// ids in the same breath.
	all, err := db.AllPendingUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("PendingUploadsUnderPrefix must not have deleted anything, left %d records", len(all))
	}
}

func TestAgeIsMeasuredFromWhenTheUploadStarted(t *testing.T) {
	now := time.Now()
	u := pending("k", now.Add(-3*time.Hour))
	if got := u.Age(now).Round(time.Minute); got != 3*time.Hour {
		t.Errorf("Age = %s, want 3h", got)
	}
}
