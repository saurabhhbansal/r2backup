package remote

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// The metadata keys stored on every object. They are already lower-case:
// S3 (and R2) lower-case metadata keys in every response regardless of how
// a client sent them, so a value written under "MTime" comes back under
// "mtime" -- writing the constants lower-case to begin with keeps ToS3's
// output identical to what a subsequent Head or Get will read.
const (
	metaKeyMTime   = "mtime"
	metaKeyMode    = "mode"
	metaKeySize    = "size"
	metaKeySymlink = "symlink"
	// metaKeyKind records what the object represents.
	//
	// Without it, an empty-directory marker and a genuine zero-byte file are
	// indistinguishable on the wire, and restore has to guess from the mode
	// bits -- which turns an empty executable file (touch run.sh; chmod +x)
	// into a directory. An object should say what it is rather than leave the
	// reader inferring it.
	metaKeyKind = "kind"
)

// Kind is what an object represents. It is written explicitly so restore never
// has to infer it.
type Kind string

const (
	KindFile     Kind = "file"
	KindSymlink  Kind = "symlink"
	KindEmptyDir Kind = "dir"
)

// Metadata is what a restore needs to put a file back exactly as it was:
// its modification time, its permission bits, its size (carried alongside
// the object's own ContentLength as a cross-check against a truncated
// upload), and -- only when the entry is a symlink rather than a regular
// file -- the link's target.
type Metadata struct {
	ModTime time.Time
	Mode    fs.FileMode
	Size    int64
	// Symlink is the link target. Leave it empty for a regular file; ToS3
	// only writes the "symlink" key when this is non-empty.
	Symlink string
	// Kind says what the object represents. Empty means an older object
	// written before this field existed; readers fall back to inference.
	Kind Kind
}

// ToS3 renders Metadata as the map PutObjectInput.Metadata (and
// CreateMultipartUploadInput.Metadata) expect.
func (m Metadata) ToS3() map[string]string {
	out := map[string]string{
		// RFC3339 with nanoseconds: a plain RFC3339 truncates to the
		// second, which is not enough resolution to tell two rapid saves
		// of the same file apart.
		metaKeyMTime: m.ModTime.Format(time.RFC3339Nano),
		metaKeyMode:  strconv.FormatUint(uint64(m.Mode.Perm()), 10),
		metaKeySize:  strconv.FormatInt(m.Size, 10),
	}
	if m.Symlink != "" {
		out[metaKeySymlink] = m.Symlink
	}
	if m.Kind != "" {
		out[metaKeyKind] = string(m.Kind)
	}
	return out
}

// MetadataFromS3 parses the map returned by HeadObject/GetObject back into
// Metadata.
//
// Lookups here are case-insensitive on purpose. ToS3 already writes
// lower-case keys, so a real round trip through S3/R2 would work with a
// verbatim match -- but nothing guarantees every caller (or every
// S3-compatible server) preserves that, and the cost of tolerating other
// casings is a single map pass.
func MetadataFromS3(m map[string]string) (Metadata, error) {
	lower := make(map[string]string, len(m))
	for k, v := range m {
		lower[strings.ToLower(k)] = v
	}

	var out Metadata
	if v, ok := lower[metaKeyMTime]; ok && v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return Metadata{}, fmt.Errorf("remote: metadata: parse mtime %q: %w", v, err)
		}
		out.ModTime = t
	}
	if v, ok := lower[metaKeyMode]; ok && v != "" {
		mode, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Metadata{}, fmt.Errorf("remote: metadata: parse mode %q: %w", v, err)
		}
		out.Mode = fs.FileMode(mode)
	}
	if v, ok := lower[metaKeySize]; ok && v != "" {
		size, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Metadata{}, fmt.Errorf("remote: metadata: parse size %q: %w", v, err)
		}
		out.Size = size
	}
	out.Symlink = lower[metaKeySymlink]
	out.Kind = Kind(lower[metaKeyKind])
	return out, nil
}
