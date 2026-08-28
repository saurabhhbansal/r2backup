package backup_test

import (
	"errors"
	"os"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
)

func touch(path string, t time.Time) error { return os.Chtimes(path, t, t) }

func renameDir(from, to string) error { return os.Rename(from, to) }

func isRootMissing(err error) bool { return errors.Is(err, backup.ErrRootMissing) }
