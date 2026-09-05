package index

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// This file holds what the index knows about usage rather than about files:
// the operation counters, how many bytes are stored, and a daily record of
// how that number moved. internal/cost turns these into money; nothing here
// knows a price, and nothing here should.

var (
	// classBKey counts the read operations -- GetObject, HeadObject --
	// which in practice means restores. It is a separate counter from
	// opsKey rather than a field beside it so that an index written by an
	// older build, which has no such key, reads back as zero instead of
	// failing to decode.
	classBKey = []byte("classb")

	// samplesKey holds this month's daily storage observations. See
	// RecordStorageSample for why a running total will not do.
	samplesKey = []byte("storage_samples")
)

// addMonthly adds n to a per-calendar-month counter stored under key,
// resetting it first if the stored month is not the current one.
//
// AddOps and AddClassBOps are both this function. It was worth extracting
// because the reset-on-rollover rule is the part that is easy to get wrong,
// and having one copy of it means a Class B counter cannot quietly acquire a
// different notion of when a month turns over than the Class A one has.
func (db *DB) addMonthly(key []byte, n int) error {
	if n == 0 {
		return nil
	}
	cur := monthKey(db.now())
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		var st opsState
		if data := meta.Get(key); data != nil {
			if err := json.Unmarshal(data, &st); err != nil {
				return fmt.Errorf("decode %s counter: %w", key, err)
			}
		}
		if st.Month != cur {
			st.Month = cur
			st.Used = 0
		}
		st.Used += int64(n)
		data, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return meta.Put(key, data)
	})
	if err != nil {
		return fmt.Errorf("add %d to the %s counter: %w", n, key, err)
	}
	return nil
}

// readMonthly returns a counter's value for the current month and when it
// next resets. Like OpsThisMonth, it works the rollover out from the clock
// rather than trusting the stored month, so a read taken after the month
// turned -- with no write in between to trigger the reset -- reports zero
// rather than last month's total.
func (db *DB) readMonthly(key []byte) (used int, resetAt time.Time, err error) {
	now := db.now()
	bolt, err := db.handle()
	if err != nil {
		return 0, time.Time{}, err
	}
	var st opsState
	txErr := bolt.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		data := meta.Get(key)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &st)
	})
	if txErr != nil {
		return 0, time.Time{}, fmt.Errorf("read the %s counter: %w", key, txErr)
	}
	if st.Month != monthKey(now) {
		return 0, startOfNextMonth(now), nil
	}
	return int(st.Used), startOfNextMonth(now), nil
}

// AddClassBOps records n more Class B (read) operations against this month.
//
// These are the cheap ones -- ten million free a month against one million
// Class A -- so this counter exists for completeness of the estimate rather
// than because anyone is likely to run past it. Counting them anyway means
// the figure shown to someone who has just restored a large folder matches
// what they will actually be charged.
func (db *DB) AddClassBOps(n int) error { return db.addMonthly(classBKey, n) }

// ClassBOpsThisMonth returns Class B operations counted this calendar month.
func (db *DB) ClassBOpsThisMonth() (used int, resetAt time.Time, err error) {
	return db.readMonthly(classBKey)
}

// StoredBytes sums what the index believes is stored, across every set.
//
// "Believes" is doing real work in that sentence. This is r2backup's own
// record of what it uploaded, so it does not include another tool's objects
// in the same bucket, trash that has been moved aside but not yet expired, or
// the parts of an abandoned multipart upload -- all of which are on the bill.
// It is the cheap answer, correct for the common case; reconciling it against
// the bucket costs a ListObjectsV2 and belongs on a slower path.
//
// It decodes every record to read one field, which is wasteful in principle.
// In practice this is called when a dashboard refreshes, over an index that
// fits in memory, and the alternative -- a stored aggregate updated on every
// write -- is a second source of truth that can drift from the first.
func (db *DB) StoredBytes() (bytes int64, objects int64, err error) {
	bolt, err := db.handle()
	if err != nil {
		return 0, 0, err
	}
	txErr := bolt.View(func(tx *bbolt.Tx) error {
		sets := tx.Bucket(setsBucketName)
		if sets == nil {
			return nil
		}
		return sets.ForEachBucket(func(name []byte) error {
			set := sets.Bucket(name)
			if set == nil {
				return nil
			}
			return set.ForEach(func(_, data []byte) error {
				if data == nil {
					return nil
				}
				var rec Record
				if err := json.Unmarshal(data, &rec); err != nil {
					// One unreadable record must not zero the whole
					// figure. Skipping it makes the total slightly low,
					// which is the safe direction for something a
					// spending limit reads.
					return nil
				}
				// Symlinks and empty directories are stored as objects
				// with no body, so they cost operations but no storage.
				if rec.Kind == KindFile {
					bytes += rec.Size
				}
				objects++
				return nil
			})
		})
	})
	if txErr != nil {
		return 0, 0, fmt.Errorf("total the stored bytes: %w", txErr)
	}
	return bytes, objects, nil
}

// StorageSample is one observation of how much was stored at a moment.
type StorageSample struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
}

// samplesState is the persisted shape: the month being sampled, and the
// observations within it.
type samplesState struct {
	Month   string          `json:"month"`
	Samples []StorageSample `json:"samples"`
}

// RecordStorageSample notes that this many bytes were stored right now.
//
// A daily series rather than a single current figure, because R2 bills
// storage over time. Someone who adds a large folder on the 28th owes two
// days of storage on it, not a month's -- and a spending limit built on
// "current size, assumed all month" would stop their backups over money they
// have not been charged. See cost.AccrueGBMonths, which reads these.
//
// One sample per UTC day, timestamped at the start of that day, with a later
// reading on the same day replacing an earlier one. Days are the right
// granularity: finer would grow the record for accuracy nobody can see at
// two decimal places, and coarser would reintroduce the problem above.
func (db *DB) RecordStorageSample(bytes int64) error {
	if bytes < 0 {
		bytes = 0
	}
	now := db.now().UTC()
	cur := monthKey(now)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	bolt, err := db.handle()
	if err != nil {
		return err
	}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		var st samplesState
		if data := meta.Get(samplesKey); data != nil {
			if err := json.Unmarshal(data, &st); err != nil {
				// Unreadable history is not worth failing a backup over.
				// Starting the month's series again costs accuracy in the
				// estimate and nothing else.
				st = samplesState{}
			}
		}
		if st.Month != cur {
			st = samplesState{Month: cur}
		}
		replaced := false
		for i := range st.Samples {
			if st.Samples[i].At.Equal(day) {
				st.Samples[i].Bytes = bytes
				replaced = true
				break
			}
		}
		if !replaced {
			st.Samples = append(st.Samples, StorageSample{At: day, Bytes: bytes})
		}
		// A month cannot have more days than this; the guard is against a
		// clock that jumps rather than against normal use.
		if len(st.Samples) > 31 {
			st.Samples = st.Samples[len(st.Samples)-31:]
		}
		data, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return meta.Put(samplesKey, data)
	})
	if err != nil {
		return fmt.Errorf("record a storage sample: %w", err)
	}
	return nil
}

// StorageSamples returns this month's observations, oldest first. A month
// that has not been sampled yet returns none rather than an error -- that is
// the state of every fresh install on the first of the month.
func (db *DB) StorageSamples() ([]StorageSample, error) {
	cur := monthKey(db.now())
	bolt, err := db.handle()
	if err != nil {
		return nil, err
	}
	var st samplesState
	txErr := bolt.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		data := meta.Get(samplesKey)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &st)
	})
	if txErr != nil {
		return nil, fmt.Errorf("read the storage samples: %w", txErr)
	}
	if st.Month != cur {
		return nil, nil
	}
	return st.Samples, nil
}
