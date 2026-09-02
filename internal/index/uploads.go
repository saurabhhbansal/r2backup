package index

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// Unfinished uploads.
//
// A large file goes up in parts, and the server keeps the parts it has
// accepted under an upload id until the whole thing is completed or
// abandoned. This is where that id lives between runs, so a nine-gigabyte
// file interrupted at eight gigabytes carries on from eight rather than
// starting again at zero.
//
// It is in the index rather than a file of its own because the two answer the
// same question -- what does the bucket already have -- and because bbolt
// gives a write here the same crash safety every other record gets. A process
// killed mid-part must not leave a half-written note about where it was.
//
// PendingUpload is declared here rather than imported from internal/remote,
// for the same reason Kind is declared here rather than imported from
// internal/scan: this is an on-disk format, and it must not silently change
// shape because a type somewhere else did. internal/backup translates.

// PendingPart is one part the server has accepted.
type PendingPart struct {
	Number int32  `json:"n"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// PendingUpload is a multipart upload that was started and not finished.
type PendingUpload struct {
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
	PartSize int64  `json:"part_size"`
	// Size and ModTime are the file as it was when the upload began. Parts
	// already sent describe those bytes, so a file that no longer matches
	// cannot be resumed into.
	Size      int64         `json:"size"`
	ModTime   int64         `json:"mod_time"`
	Parts     []PendingPart `json:"parts"`
	StartedAt int64         `json:"started_at"`
}

var uploadsBucketName = []byte("uploads")

// PendingUploadFor returns the unfinished upload recorded for key.
func (db *DB) PendingUploadFor(key string) (PendingUpload, bool, error) {
	var u PendingUpload
	var found bool
	bolt, err := db.handle()
	if err != nil {
		return PendingUpload{}, false, err
	}
	err = bolt.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(uploadsBucketName)
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			// A record that cannot be read is a record that cannot be
			// resumed from, which is the same answer as having none. The
			// upload starts again rather than the backup failing over a
			// bookkeeping file.
			return nil
		}
		found = true
		return nil
	})
	if err != nil {
		return PendingUpload{}, false, fmt.Errorf("index: read pending upload %q: %w", key, err)
	}
	return u, found, nil
}

// SavePendingUpload records where a multipart upload has got to.
func (db *DB) SavePendingUpload(u PendingUpload) error {
	if u.Key == "" {
		return fmt.Errorf("index: pending upload has no key")
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("index: encode pending upload %q: %w", u.Key, err)
	}
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(uploadsBucketName)
		if err != nil {
			return err
		}
		return b.Put([]byte(u.Key), raw)
	})
	if err != nil {
		return fmt.Errorf("index: save pending upload %q: %w", u.Key, err)
	}
	return nil
}

// ForgetPendingUpload drops the record, on completion or on abandonment.
func (db *DB) ForgetPendingUpload(key string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(uploadsBucketName)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("index: forget pending upload %q: %w", key, err)
	}
	return nil
}

// AllPendingUploads lists every unfinished upload, oldest first.
func (db *DB) AllPendingUploads() ([]PendingUpload, error) {
	var out []PendingUpload
	bolt, err := db.handle()
	if err != nil {
		return nil, err
	}
	err = bolt.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(uploadsBucketName)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, raw []byte) error {
			var u PendingUpload
			if err := json.Unmarshal(raw, &u); err != nil {
				return nil // skip a record nothing can use
			}
			out = append(out, u)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("index: list pending uploads: %w", err)
	}
	return out, nil
}

// PendingBytes is how many bytes of unfinished uploads the bucket is already
// holding, and how many files they belong to.
//
// This is what lets the interface say "3.1 GB of 4 GB already sent" about
// work that was interrupted, instead of showing nothing until the next run
// gets going.
func (db *DB) PendingBytes() (done, total int64, files int, err error) {
	all, err := db.AllPendingUploads()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, u := range all {
		for _, p := range u.Parts {
			done += p.Size
		}
		total += u.Size
	}
	return done, total, len(all), nil
}

// DropSetUploads forgets unfinished uploads whose key sits under prefix.
//
// Called when a set is removed: its records would otherwise sit in the index
// forever, and the sweep would keep asking the bucket about an upload for a
// folder nobody is backing up any more.
func (db *DB) DropSetUploads(prefix string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(uploadsBucketName)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		var doomed [][]byte
		for k, _ := c.Seek([]byte(prefix)); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			doomed = append(doomed, append([]byte(nil), k...))
		}
		for _, k := range doomed {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("index: drop pending uploads under %q: %w", prefix, err)
	}
	return nil
}

func hasPrefix(k []byte, prefix string) bool {
	if len(k) < len(prefix) {
		return false
	}
	return string(k[:len(prefix)]) == prefix
}

// Age is how long ago this upload was started.
func (u PendingUpload) Age(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, u.StartedAt))
}
