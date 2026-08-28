package remote

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMetadataRoundTrip(t *testing.T) {
	want := Metadata{
		ModTime: time.Date(2026, 3, 14, 9, 26, 53, 589793238, time.UTC),
		Mode:    0o640,
		Size:    123456,
		Symlink: "",
	}

	got, err := MetadataFromS3(want.ToS3())
	if err != nil {
		t.Fatalf("MetadataFromS3: %v", err)
	}
	if !got.ModTime.Equal(want.ModTime) {
		t.Errorf("ModTime = %v, want %v", got.ModTime, want.ModTime)
	}
	if got.Mode != want.Mode {
		t.Errorf("Mode = %v, want %v", got.Mode, want.Mode)
	}
	if got.Size != want.Size {
		t.Errorf("Size = %d, want %d", got.Size, want.Size)
	}
	if got.Symlink != "" {
		t.Errorf("Symlink = %q, want empty", got.Symlink)
	}
}

func TestMetadataRoundTripSymlink(t *testing.T) {
	want := Metadata{
		ModTime: time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC),
		Mode:    0o777,
		Size:    0,
		Symlink: "../target/of/link.txt",
	}

	got, err := MetadataFromS3(want.ToS3())
	if err != nil {
		t.Fatalf("MetadataFromS3: %v", err)
	}
	if got.Symlink != want.Symlink {
		t.Errorf("Symlink = %q, want %q", got.Symlink, want.Symlink)
	}
}

// TestMetadataFromS3LowercasesKeys guards the exact failure mode described
// in the package docs: S3-compatible services lower-case metadata keys in
// their responses regardless of how a client sent them, so MetadataFromS3
// must not assume the keys it receives match ToS3's casing verbatim.
func TestMetadataFromS3LowercasesKeys(t *testing.T) {
	mixedCase := map[string]string{
		"Mtime":   "2026-08-28T12:00:00.123456789Z",
		"MODE":    strconv.FormatUint(uint64(fs.FileMode(0o755).Perm()), 10),
		"Size":    "42",
		"SymLink": "target.txt",
	}

	got, err := MetadataFromS3(mixedCase)
	if err != nil {
		t.Fatalf("MetadataFromS3: %v", err)
	}

	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-08-28T12:00:00.123456789Z")
	if !got.ModTime.Equal(wantTime) {
		t.Errorf("ModTime = %v, want %v", got.ModTime, wantTime)
	}
	if got.Mode != fs.FileMode(0o755) {
		t.Errorf("Mode = %v, want %v", got.Mode, fs.FileMode(0o755))
	}
	if got.Size != 42 {
		t.Errorf("Size = %d, want 42", got.Size)
	}
	if got.Symlink != "target.txt" {
		t.Errorf("Symlink = %q, want %q", got.Symlink, "target.txt")
	}
}

func TestMetadataToS3KeysAreLowercase(t *testing.T) {
	m := Metadata{Symlink: "x"}
	for k := range m.ToS3() {
		if k != strings.ToLower(k) {
			t.Errorf("ToS3 produced non-lowercase key %q", k)
		}
	}
}

func TestMetadataToS3OmitsEmptySymlink(t *testing.T) {
	m := Metadata{ModTime: time.Now(), Mode: 0o644, Size: 10}
	out := m.ToS3()
	if _, ok := out[metaKeySymlink]; ok {
		t.Errorf("ToS3 wrote a %q key for a non-symlink entry", metaKeySymlink)
	}
}

func TestMetadataFromS3BadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"bad mtime": {"mtime": "not-a-time"},
		"bad mode":  {"mode": "not-a-number"},
		"bad size":  {"size": "not-a-number"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := MetadataFromS3(m); err == nil {
				t.Errorf("MetadataFromS3(%v): want error, got nil", m)
			}
		})
	}
}
