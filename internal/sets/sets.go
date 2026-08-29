// Package sets records which folders are backed up and how.
//
// Stored as one JSON file written by atomic rename, not a database: this is a
// handful of records a person can read and repair with a text editor, and a
// backup tool should not put its own configuration behind something that can
// corrupt.
package sets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the state a set is in.
type Status string

const (
	// StatusOK means the last run finished and nothing needs a decision.
	StatusOK Status = "ok"
	// StatusNeedsAttention means a run stopped on something only a person can
	// resolve -- most importantly, a root folder that has vanished. A
	// scheduled run never prompts, so it parks the set here instead and
	// touches nothing until someone looks.
	StatusNeedsAttention Status = "needs-attention"
)

// DefaultRetentionDays is how long overwritten and deleted objects stay in
// trash. Deletes are free on R2 and the copy that fills trash is one Class A
// operation per changed file, which for ordinary churn sits far inside the
// million-per-month free tier.
const DefaultRetentionDays = 30

// DefaultIntervalMinutes is how often the OS scheduler runs a set.
const DefaultIntervalMinutes = 30

// Set is one folder under backup.
type Set struct {
	// Name is what the user calls it, and what they type on the command line.
	// It can be changed freely.
	Name string `json:"name"`

	// Prefix is the object-key prefix in the bucket. It is assigned when the
	// set is created and never changes afterwards, even when Name does.
	//
	// Object storage has no rename: moving a prefix means copying every object
	// and deleting every original -- 61,204 operations for a set of that size,
	// to achieve a cosmetic change. So renaming is display-only by default and
	// the remote move is an explicit, costed opt-in. Tying identity to
	// something the user can change was also how the predecessor orphaned data
	// in the bucket.
	Prefix string `json:"prefix"`

	// Root is the absolute path on this machine.
	Root string `json:"root"`

	// Machine is the name this computer is filed under in the bucket.
	Machine string `json:"machine"`

	// Excludes are relative keys, or directory prefixes, left out of the
	// backup. The picker starts with everything selected, so this is empty
	// until the user unchecks something.
	Excludes []string `json:"excludes,omitempty"`

	RetentionDays   int `json:"retention_days"`
	IntervalMinutes int `json:"interval_minutes"`

	Status     Status    `json:"status"`
	StatusNote string    `json:"status_note,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	LastRunAt    time.Time `json:"last_run_at,omitzero"`
	LastRunFiles int64     `json:"last_run_files,omitempty"`
	LastRunBytes int64     `json:"last_run_bytes,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// TrashEnabled reports whether overwritten objects are kept.
func (s *Set) TrashEnabled() bool { return s.RetentionDays > 0 }

// Excluded reports whether a relative key falls under any exclude rule. A rule
// naming a directory excludes everything beneath it.
func (s *Set) Excluded(key string) bool {
	for _, ex := range s.Excludes {
		if key == ex || strings.HasPrefix(key, ex+"/") {
			return true
		}
	}
	return false
}

var (
	// ErrNotFound means no set goes by that name.
	ErrNotFound = errors.New("no such set")
	// ErrExists means a set of that name is already recorded.
	ErrExists = errors.New("a set with that name already exists")
	// ErrBadName means the name cannot be used as a set name. See ValidName.
	ErrBadName = errors.New("that name cannot be used for a set")
)

// Store holds every set, backed by one JSON file.
type Store struct {
	mu   sync.RWMutex
	path string
	sets []Set
}

// Open reads the store, creating an empty one if the file is not there.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read sets from %q: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.sets); err != nil {
		return nil, fmt.Errorf("parse sets in %q: %w", path, err)
	}
	return s, nil
}

// List returns every set, ordered by name.
func (s *Store) List() []Set {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Set, len(s.sets))
	copy(out, s.sets)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the set of that name.
func (s *Store) Get(name string) (Set, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, set := range s.sets {
		if set.Name == name {
			return set, nil
		}
	}
	return Set{}, fmt.Errorf("%q: %w", name, ErrNotFound)
}

// Add records a new set. Prefix is derived from the name at this moment and
// then fixed for the life of the set.
// MaxNameLen caps a set name. A key is the set's prefix plus the relative
// path of a file inside it, and S3 caps a key at 1024 bytes -- so every
// character spent on the name is one a deeply nested file cannot have.
const MaxNameLen = 100

// ValidName reports why a name cannot be a set name, or nil.
//
// The name is not just a label: `add` builds the set's remote prefix out of
// it ("machines/<machine>/<name>"), so whatever goes in here becomes part of
// every object key for that set, forever -- the prefix is the set's identity
// and is deliberately never rewritten afterwards. Nothing checked it. A set
// added as "../escape" was accepted, reported "Added", exited 0, and then
// failed every single upload for the rest of its life with
//
//	XMinioInvalidResourceName: Resource name contains bad components
//
// because "machines/pc/../escape/current/a.txt" is not a name a store will
// take. A backup that can never work must not be creatable, and certainly
// not with a success code.
//
// A "/" is refused for the same reason from the other direction: it would
// quietly turn one name into several prefix levels, and two different sets
// could then be given overlapping prefixes without either name looking
// unusual.
func ValidName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: it is empty", ErrBadName)
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("%w: it starts or ends with a space", ErrBadName)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: it is %d characters and the limit is %d, because the name is part of every object key for the set",
			ErrBadName, len(name), MaxNameLen)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q is a path component, not a name", ErrBadName, name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: it contains a slash, and the name becomes part of the set's location in the bucket", ErrBadName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: it contains a control character", ErrBadName)
		}
	}
	return nil
}

func (s *Store) Add(set Set) error {
	if err := ValidName(set.Name); err != nil {
		return fmt.Errorf("%q: %w", set.Name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.sets {
		if existing.Name == set.Name {
			return fmt.Errorf("%q: %w", set.Name, ErrExists)
		}
	}
	if set.Prefix == "" {
		set.Prefix = set.Name
	}
	if set.RetentionDays == 0 {
		set.RetentionDays = DefaultRetentionDays
	}
	if set.IntervalMinutes == 0 {
		set.IntervalMinutes = DefaultIntervalMinutes
	}
	if set.Status == "" {
		set.Status = StatusOK
	}
	if set.CreatedAt.IsZero() {
		set.CreatedAt = time.Now()
	}
	s.sets = append(s.sets, set)
	return s.save()
}

// Overlapping returns an existing set whose root contains root, or is
// contained by it, and whether there was one.
//
// Nothing stops a user adding a folder that is already inside a set they
// added -- and it is a legitimate thing to want, since each set carries its
// own retention and schedule. But every file in the overlap is then stored
// twice, under two prefixes, and paid for twice in Class A operations on
// every run that touches it. That is worth one line of warning on a tool
// whose whole argument is the operations budget. It is a note, not a prompt:
// "no second prompts" is a design decision, and this does not become one.
//
// Comparison is on cleaned absolute paths, case-sensitively. On a
// case-insensitive volume that can miss an overlap spelled differently --
// which is the harmless direction for a warning to fail in, unlike claiming
// an overlap that is not there.
func (s *Store) Overlapping(root string) (Set, bool) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Set{}, false
	}
	abs = filepath.Clean(abs)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, existing := range s.sets {
		other := filepath.Clean(existing.Root)
		if abs == other || under(abs, other) || under(other, abs) {
			return existing, true
		}
	}
	return Set{}, false
}

// under reports whether child sits inside parent. It compares whole path
// components, so "/home/me/docs-old" is not inside "/home/me/docs".
func under(child, parent string) bool {
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// Update replaces a set, matched by name.
func (s *Store) Update(set Set) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sets {
		if s.sets[i].Name == set.Name {
			s.sets[i] = set
			return s.save()
		}
	}
	return fmt.Errorf("%q: %w", set.Name, ErrNotFound)
}

// Rename changes the display name only. The bucket prefix is untouched, so
// nothing is copied and nothing is spent. Moving the prefix as well is a
// separate, costed operation the caller performs deliberately.
func (s *Store) Rename(from, to string) error {
	if err := ValidName(to); err != nil {
		return fmt.Errorf("%q: %w", to, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if from == to {
		return nil
	}
	for _, set := range s.sets {
		if set.Name == to {
			return fmt.Errorf("%q: %w", to, ErrExists)
		}
	}
	for i := range s.sets {
		if s.sets[i].Name == from {
			s.sets[i].Name = to
			return s.save()
		}
	}
	return fmt.Errorf("%q: %w", from, ErrNotFound)
}

// Relink points a set at a new root without re-uploading anything. This is the
// answer to a folder that was renamed or moved on disk -- the objects in the
// bucket are still correct, only the local path changed.
func (s *Store) Relink(name, newRoot string) error {
	abs, err := filepath.Abs(newRoot)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", newRoot, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("relink %q to %q: %w", name, abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("relink %q: %q is not a directory", name, abs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sets {
		if s.sets[i].Name == name {
			s.sets[i].Root = abs
			s.sets[i].Status = StatusOK
			s.sets[i].StatusNote = ""
			return s.save()
		}
	}
	return fmt.Errorf("%q: %w", name, ErrNotFound)
}

// MarkNeedsAttention parks a set for a person to look at. Used when a run hits
// something only a human can decide and no human is present -- a scheduled run
// at 3am must never guess.
func (s *Store) MarkNeedsAttention(name, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sets {
		if s.sets[i].Name == name {
			s.sets[i].Status = StatusNeedsAttention
			s.sets[i].StatusNote = note
			return s.save()
		}
	}
	return fmt.Errorf("%q: %w", name, ErrNotFound)
}

// Remove forgets a set locally. It does not touch the bucket: deleting a
// backup because someone stopped tracking a folder is never the safe reading.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sets {
		if s.sets[i].Name == name {
			s.sets = append(s.sets[:i], s.sets[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("%q: %w", name, ErrNotFound)
}

// save writes the file by atomic rename, so an interrupted write can never
// leave a truncated set list behind.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.sets, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sets: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create directory for %q: %w", s.path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sets-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file beside %q: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %q: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace %q: %w", s.path, err)
	}
	return nil
}
