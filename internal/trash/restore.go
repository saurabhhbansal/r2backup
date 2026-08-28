package trash

import (
	"context"
	"errors"
	"fmt"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Restore copies a trashed object back to its live key with a
// server-side Copy -- the same reason Move uses one: pulling a
// potentially large file through this process just to put it back where
// it came from would be needless network cost for a recovery that should
// be cheap and fast, the whole point of trashing by copy instead of by
// download in the first place.
//
// It Heads the trash key first so a caller gets a clear "not found" for a
// stale or mistyped entry -- one already pruned, say -- rather than
// whatever error CopyObject happens to produce for a missing source. Head
// is a Class B operation, so this does not add to the Class A cost of
// restoring.
func (t *Trash) Restore(ctx context.Context, prefix string, entry Entry) error {
	if _, err := t.backend.Head(ctx, entry.TrashKey); err != nil {
		if errors.Is(err, remote.ErrNotFound) {
			return fmt.Errorf("trash: restore %q: %w", entry.RelPath, remote.ErrNotFound)
		}
		return fmt.Errorf("trash: restore %q: %w", entry.RelPath, err)
	}

	dst := liveKey(prefix, entry.RelPath)
	if err := t.backend.Copy(ctx, entry.TrashKey, dst); err != nil {
		return fmt.Errorf("trash: restore %q: %w", entry.RelPath, err)
	}
	return nil
}
