// Package index is the local cache of what has already been uploaded for a
// set. A run consults it to decide what changed without asking the bucket
// anything -- the predecessor to this tool hashed 60,000 files to find that 3
// had changed, and that took two hours and forty-three minutes. Everything
// here exists to make that check a bbolt lookup instead.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// Kind mirrors scan.Kind. It is redeclared here, rather than imported, so
// this package's on-disk format does not silently shift if scan's enum ever
// does; the two are kept in step by callers translating at the boundary.
type Kind uint8

const (
	KindFile Kind = iota
	KindSymlink
	KindEmptyDir
)

// Record is what the index remembers about one object already uploaded for a
// set.
type Record struct {
	Key string `json:"key"` // relative, forward-slashed, NFC -- see scan.Key.
	// Size and ModTime are what Changed compares a freshly scanned file
	// against. ModTime is Unix nanoseconds, not time.Time: a time.Time
	// carries a monotonic reading and a *Location that JSON round-trips
	// unreliably, and an int64 is what makes the JSON encoding of a Record
	// stable across Go versions and platforms.
	Size       int64  `json:"size"`
	ModTime    int64  `json:"mod_time"`
	ETag       string `json:"etag"`
	UploadedAt int64  `json:"uploaded_at"` // Unix nanoseconds.
	Kind       Kind   `json:"kind"`
	Target     string `json:"target,omitempty"` // symlink target; empty otherwise.
}

// ModTimeTolerance is how far a stored modtime and a freshly observed one may
// differ before Changed calls it a change. FAT and exFAT store timestamps at
// 2-second granularity while NTFS uses 100ns, so a file that has not been
// touched can still read back with an mtime shifted by up to 2s after being
// copied onto, or read from, one of those filesystems. Without this
// tolerance every file on such a volume looks changed on every single run,
// forever.
const ModTimeTolerance = 2 * time.Second

// FreeTierOpsPerMonth is Cloudflare R2's free allowance of Class A
// operations per calendar month.
const FreeTierOpsPerMonth = 1_000_000

// ErrNotFound is returned by Get when the set or the key inside it does not
// exist, so callers can tell "never uploaded" apart from a real failure
// instead of getting a zero-value Record back either way.
var ErrNotFound = errors.New("index: record not found")

// ErrLocked is joined into the error Open (or Acquire, its on-demand
// cousin) returns when bbolt's exclusive file lock is already held and the
// 5s wait timed out. It exists so a caller can tell "someone else has it"
// apart from every other way opening a file can fail, without depending on
// bbolt.ErrTimeout directly -- and so the message reaching a user names a
// cause they can act on ("another copy of r2b is already running") rather
// than the bare word "timeout", which was the whole complaint this fixes.
var ErrLocked = errors.New("index: locked by another process")

// Changed reports whether a file needs re-uploading, given the record last
// stored for it and what a fresh stat just observed.
func Changed(rec Record, size int64, modTime time.Time) bool {
	if rec.Size != size {
		return true
	}
	diff := modTime.Sub(time.Unix(0, rec.ModTime))
	if diff < 0 {
		diff = -diff
	}
	return diff > ModTimeTolerance
}

var (
	setsBucketName = []byte("sets")
	metaBucketName = []byte("meta")
	opsKey         = []byte("ops")
	// prunedKey holds, per set, the UTC calendar day its trash was last
	// swept. See ClaimDailyPrune.
	prunedKey = []byte("pruned")
)

// DB is a bbolt-backed store, isolated per set, safe for concurrent use from
// multiple goroutines the way bbolt itself is: one writer at a time, any
// number of concurrent readers.
//
// It is also safe for concurrent use by more than one *caller* of Acquire at
// once, sharing the one underlying bbolt handle between them -- see Acquire.
type DB struct {
	path string
	mu   sync.Mutex
	bolt *bbolt.DB // nil unless refs > 0; guarded by mu.
	refs int       // how many Acquires (Open's included) have not Released yet.

	// Now stands in for time.Now so the op-counter's month rollover can be
	// tested without waiting for a real month to turn over. Set it (if at
	// all) once, before any concurrent use begins; it is read but never
	// written by DB's own methods.
	Now func() time.Time
}

// New returns a DB for path with nothing open yet: no directory is created,
// no file is touched, no lock is taken, until the first Acquire. This is the
// on-demand half of DB -- see Acquire -- for a caller that may go long
// stretches with nothing to do with the index and wants to hold no OS
// resource for it during those stretches. Open, below, is this plus one
// Acquire, for the ordinary case of a caller that wants a ready-to-use DB
// immediately and will Close it exactly once when done.
func New(path string) *DB {
	return &DB{path: path, Now: time.Now}
}

// Open opens (creating if necessary) the index at path, along with any
// missing parent directories.
func Open(path string) (*DB, error) {
	db := New(path)
	if err := db.Acquire(); err != nil {
		return nil, err
	}
	return db, nil
}

// Acquire checks this DB out for one more concurrent user, opening the
// underlying file -- and taking bbolt's exclusive lock -- if this is the
// first one currently checked out. Every successful Acquire must be matched
// by exactly one Release.
//
// This is what lets one *DB be shared by several concurrent users inside a
// single long-lived process -- which bbolt itself allows -- while still
// giving up the OS file lock the moment none of them are using it, so a
// second r2b process (a scheduled backup, `status`, `ls`) is not locked out
// just because that first process happens to still be running with nothing
// to do. See internal/cli's dashboard struct for the caller this exists for,
// and why a per-call Open/Close there was tried first and made things worse,
// not better.
func (db *DB) Acquire() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.refs == 0 {
		if err := db.open(); err != nil {
			return err
		}
	}
	db.refs++
	return nil
}

// Release gives back one Acquire (or the implicit one Open made). Once every
// acquirer has released, the underlying file is closed and bbolt's lock let
// go; a later Acquire reopens it. Releasing more times than were acquired is
// a caller bug, not a state this DB can be argued back out of, so it is a
// no-op rather than a panic or a negative count.
func (db *DB) Release() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.refs == 0 {
		return nil
	}
	db.refs--
	if db.refs > 0 || db.bolt == nil {
		return nil
	}
	b := db.bolt
	db.bolt = nil
	if err := b.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	return nil
}

// open does the actual bbolt.Open and bucket bootstrap. Called with mu held,
// by Acquire when refs is rising from zero.
func (db *DB) open() error {
	if dir := filepath.Dir(db.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("open index at %q: %w", db.path, err)
		}
	}
	b, err := bbolt.Open(db.path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			// A bare "timeout" told a user nothing they could do anything
			// about. This names the actual cause -- there is exactly one way
			// this lock is already held, which is another copy of r2b -- and
			// the fact that this process gave up rather than sitting there,
			// which it did, silently, for 5s, before this error existed.
			return fmt.Errorf("open index at %q: another copy of r2b is already running "+
				"(a backup, or the dashboard), and this one stopped rather than wait for it: %w",
				db.path, errors.Join(ErrLocked, err))
		}
		return fmt.Errorf("open index at %q: %w", db.path, err)
	}
	err = b.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(setsBucketName); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(metaBucketName)
		return err
	})
	if err != nil {
		b.Close()
		return fmt.Errorf("open index at %q: %w", db.path, err)
	}
	db.bolt = b
	return nil
}

// handle returns the bbolt handle every other method actually reads and
// writes through, synchronized against Acquire/Release/Close so a state
// change already in flight is never read half-way through.
//
// It errors rather than returning nil when nobody currently holds this DB
// open -- calling one of these methods without a matching Acquire (or the
// implicit one Open makes) is a caller bug, and finding that out from a
// returned error is a lot cheaper than finding it out from a nil-pointer
// panic inside bbolt.
func (db *DB) handle() (*bbolt.DB, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.bolt == nil {
		return nil, errors.New("index: used while not open (missing Acquire or Open)")
	}
	return db.bolt, nil
}

// Close closes the underlying bbolt file if it is currently open, and resets
// the use count to zero regardless of how many Acquires are outstanding.
//
// It exists for a caller with no matching Acquire to give back one at a
// time -- which is every direct caller of Open, and internal/cli's
// app.close() at the end of every command -- so shutting down cleanly never
// requires counting how many Acquires happened first. A later Acquire on the
// same *DB reopens it: Close is "give it all back now", not "this DB is
// spent".
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refs = 0
	if db.bolt == nil {
		return nil
	}
	b := db.bolt
	db.bolt = nil
	if err := b.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	return nil
}

func (db *DB) now() time.Time {
	if db.Now != nil {
		return db.Now()
	}
	return time.Now()
}

// setBucket resolves the nested bucket that holds set's records. With
// create false it is a read-only lookup (nil, nil when the set has never
// been written); with create true it must run inside an Update transaction.
func setBucket(tx *bbolt.Tx, set string, create bool) (*bbolt.Bucket, error) {
	sets := tx.Bucket(setsBucketName)
	if sets == nil {
		return nil, errors.New("index: sets bucket missing (index not opened via Open)")
	}
	if create {
		return sets.CreateBucketIfNotExists([]byte(set))
	}
	return sets.Bucket([]byte(set)), nil
}

// Put stores one record for a set, replacing any existing record at the
// same key.
func (db *DB) Put(set string, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record %q for set %q: %w", rec.Key, set, err)
	}
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, true)
		if err != nil {
			return err
		}
		return b.Put([]byte(rec.Key), data)
	})
	if err != nil {
		return fmt.Errorf("put %q in set %q: %w", rec.Key, set, err)
	}
	return nil
}

// PutMany stores every record in one write transaction. This is the batch
// path: committing 60,000 records one transaction at a time is unusably
// slow, since bbolt fsyncs on every commit.
func (db *DB) PutMany(set string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, true)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			data, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("encode record %q: %w", rec.Key, err)
			}
			if err := b.Put([]byte(rec.Key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("put %d records in set %q: %w", len(recs), set, err)
	}
	return nil
}

// Get returns the record stored for key in set, or ErrNotFound if the set or
// the key does not exist.
func (db *DB) Get(set, key string) (Record, error) {
	var rec Record
	bolt, err := db.handle()
	if err != nil {
		return Record{}, err
	}
	err = bolt.View(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, false)
		if err != nil {
			return err
		}
		if b == nil {
			return ErrNotFound
		}
		data := b.Get([]byte(key))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("get %q from set %q: %w", key, set, err)
	}
	return rec, nil
}

// Delete removes key from set. Deleting a key that is not there, or from a
// set that has never been written, is not an error.
func (db *DB) Delete(set, key string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, false)
		if err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("delete %q from set %q: %w", key, set, err)
	}
	return nil
}

// DeleteMany removes every key in one write transaction.
func (db *DB) DeleteMany(set string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, false)
		if err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		for _, k := range keys {
			if err := b.Delete([]byte(k)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete %d keys from set %q: %w", len(keys), set, err)
	}
	return nil
}

// All returns every record stored for set, in bbolt's key order. A set that
// has never been written returns an empty slice, not an error.
func (db *DB) All(set string) ([]Record, error) {
	var recs []Record
	bolt, err := db.handle()
	if err != nil {
		return nil, err
	}
	err = bolt.View(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, false)
		if err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var rec Record
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode record %q: %w", k, err)
			}
			recs = append(recs, rec)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("iterate set %q: %w", set, err)
	}
	return recs, nil
}

// Count returns the number of records stored for set.
func (db *DB) Count(set string) (int, error) {
	var n int
	bolt, err := db.handle()
	if err != nil {
		return 0, err
	}
	err = bolt.View(func(tx *bbolt.Tx) error {
		b, err := setBucket(tx, set, false)
		if err != nil {
			return err
		}
		if b != nil {
			n = b.Stats().KeyN
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count set %q: %w", set, err)
	}
	return n, nil
}

// DropSet deletes every record for set. Dropping a set that was never
// written is not an error.
func (db *DB) DropSet(set string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		sets := tx.Bucket(setsBucketName)
		if sets == nil {
			return nil
		}
		err := sets.DeleteBucket([]byte(set))
		if err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("drop set %q: %w", set, err)
	}
	return nil
}

// RenameSet moves every record from one set name to another.
//
// The index is keyed by set name, and a set's name is the only thing
// `r2backup rename` changes -- so without this, renaming a set left its
// records stranded under the old key and the next backup read an empty index
// and re-uploaded the entire tree, to the very same prefix it was already
// stored under. That is the free-tier claim breaking on a cosmetic change.
//
// The whole move is one transaction: it either happens or it does not, and
// there is no window in which the records exist under both names or neither.
// Renaming onto a name that already holds records is refused rather than
// merged -- two sets' records in one bucket would make each look like the
// other had deleted half its files.
func (db *DB) RenameSet(from, to string) error {
	if from == to {
		return nil
	}
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		sets := tx.Bucket(setsBucketName)
		if sets == nil {
			return errors.New("index: sets bucket missing (index not opened via Open)")
		}
		src := sets.Bucket([]byte(from))
		if src == nil {
			// Nothing was ever recorded under that name. A set that has
			// never been backed up is renameable like any other.
			return nil
		}
		if sets.Bucket([]byte(to)) != nil {
			return fmt.Errorf("index already holds records for %q", to)
		}
		dst, err := sets.CreateBucket([]byte(to))
		if err != nil {
			return err
		}
		if err := src.ForEach(func(k, v []byte) error {
			if v == nil { // a nested bucket, which this layout never creates
				return nil
			}
			return dst.Put(k, v)
		}); err != nil {
			return err
		}
		return sets.DeleteBucket([]byte(from))
	})
	if err != nil {
		return fmt.Errorf("rename set %q to %q in the index: %w", from, to, err)
	}
	return nil
}

// opsState is the persisted shape of the operation counter: the calendar
// month it is counting for, and how many operations have landed in it.
type opsState struct {
	Month string `json:"month"` // "2006-01", UTC.
	Used  int64  `json:"used"`
}

func monthKey(t time.Time) string {
	return t.UTC().Format("2006-01")
}

func startOfNextMonth(t time.Time) time.Time {
	t = t.UTC()
	y, m, _ := t.Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}

// ClaimDailyPrune reports whether set's trash still needs sweeping today,
// and records today's date when it says yes -- so the answer is yes at most
// once per set per UTC day.
//
// This exists to hold two claims that pull against each other. Trash expires
// by the calendar, so a set that never changes still has to be swept or its
// trash is kept, and paid for, forever -- which is what happened while
// trash.Prune had no caller. But finding what to expire costs a
// ListObjectsV2, and "a run where nothing changed costs nothing" is this
// product's headline claim, with its own tests. Sweeping once a day rather
// than once a run keeps both: an unchanged tree is free every run but the
// first of the day, and the retention window is actually enforced.
//
// The claim is recorded before the sweep runs, not after. A sweep that fails
// is therefore not retried until tomorrow, which is the right way round: the
// alternative retries a failing List on every run all day, spending
// operations on an error, and old trash outstaying its window by a day costs
// storage, not data.
func (db *DB) ClaimDailyPrune(set string) (bool, error) {
	today := db.now().UTC().Format("2006-01-02")
	var due bool
	bolt, err := db.handle()
	if err != nil {
		return false, err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		if meta == nil {
			return errors.New("index: meta bucket missing (index not opened via Open)")
		}
		var byName map[string]string
		if data := meta.Get(prunedKey); data != nil {
			if err := json.Unmarshal(data, &byName); err != nil {
				// Unreadable bookkeeping must not stop a sweep; the worst
				// case of starting over is one extra listing.
				byName = nil
			}
		}
		if byName == nil {
			byName = map[string]string{}
		}
		if byName[set] == today {
			return nil
		}
		due = true
		byName[set] = today
		data, err := json.Marshal(byName)
		if err != nil {
			return fmt.Errorf("index: encode prune dates: %w", err)
		}
		return meta.Put(prunedKey, data)
	})
	if err != nil {
		return false, err
	}
	return due, nil
}

// ForgetDailyPrune drops set's sweep bookkeeping. Called when a set is
// removed, so a later set of the same name does not inherit a claim that
// makes it skip its first sweep.
func (db *DB) ForgetDailyPrune(set string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	return bolt.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		if meta == nil {
			return nil
		}
		data := meta.Get(prunedKey)
		if data == nil {
			return nil
		}
		var byName map[string]string
		if err := json.Unmarshal(data, &byName); err != nil {
			return meta.Delete(prunedKey)
		}
		if _, ok := byName[set]; !ok {
			return nil
		}
		delete(byName, set)
		out, err := json.Marshal(byName)
		if err != nil {
			return fmt.Errorf("index: encode prune dates: %w", err)
		}
		return meta.Put(prunedKey, out)
	})
}

// AddOps records n more Class A R2 operations against the current calendar
// month. r2backup performs every operation against R2 itself -- there is no
// billing API to poll -- so this local tally is the only way it can warn a
// user before they run past the free tier. If the stored counter belongs to
// an earlier month, it is zeroed first: the count is "operations this month",
// not a running total since install.
//
// This deviates from a bare `AddOps(n int)` signature: swallowing a bbolt
// write failure here would let the counter silently fall behind actual
// usage, defeating the point of tracking it, and returning an error is the
// only alternative to a panic.
//
// The month-rollover rule itself lives in addMonthly, shared with the Class B
// counter -- see internal/index/usage.go.
func (db *DB) AddOps(n int) error { return db.addMonthly(opsKey, n) }

// OpsThisMonth returns Class A operations counted so far in the current
// calendar month and when that count will next reset. It computes the
// rollover from the current time rather than trusting the stored month, so a
// query made after the month has turned -- with no AddOps call in between to
// trigger the reset itself -- still reports 0 rather than a stale total.
func (db *DB) OpsThisMonth() (used int, resetAt time.Time, err error) {
	return db.readMonthly(opsKey)
}
