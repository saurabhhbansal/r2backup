package index

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

// placesKey holds, per set, which storage that set's records describe. See
// Place.
var placesKey = []byte("places")

// Place returns which storage set's records were made against, as last
// recorded by SetPlace, or "" when nothing was recorded.
//
// A record says "this file is already in storage", which is only true of the
// storage it was uploaded to. When backups start going somewhere else (S3
// becomes where backups go, a key file with other storage is imported, setup
// picks other storage), the records no longer say anything about where the
// files are, and a backup that trusted them would never send a file the new
// storage lacks. backup.Run compares this against where it is about to write
// and checks the records against storage when they differ.
func (db *DB) Place(set string) (string, error) {
	bolt, err := db.handle()
	if err != nil {
		return "", err
	}
	var place string
	err = bolt.View(func(tx *bbolt.Tx) error {
		byName, err := readPlaces(tx)
		if err != nil {
			return err
		}
		place = byName[set]
		return nil
	})
	return place, err
}

// SetPlace records which storage set's records were made against. An empty
// place forgets it.
func (db *DB) SetPlace(set, place string) error {
	bolt, err := db.handle()
	if err != nil {
		return err
	}
	return bolt.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucketName)
		if meta == nil {
			return errors.New("index: meta bucket missing (index not opened via Open)")
		}
		byName, err := readPlaces(tx)
		if err != nil {
			return err
		}
		if byName[set] == place {
			return nil
		}
		if place == "" {
			delete(byName, set)
		} else {
			byName[set] = place
		}
		data, err := json.Marshal(byName)
		if err != nil {
			return fmt.Errorf("index: encode places: %w", err)
		}
		return meta.Put(placesKey, data)
	})
}

func readPlaces(tx *bbolt.Tx) (map[string]string, error) {
	byName := map[string]string{}
	meta := tx.Bucket(metaBucketName)
	if meta == nil {
		return byName, nil
	}
	if data := meta.Get(placesKey); data != nil {
		// Unreadable bookkeeping is treated as nothing recorded: the worst
		// case is one check of the records against storage.
		_ = json.Unmarshal(data, &byName)
		if byName == nil {
			byName = map[string]string{}
		}
	}
	return byName, nil
}
