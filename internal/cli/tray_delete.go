package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Deleting from the panel. Every delete goes to where backups go, and -- when
// the person chooses it -- to the second copy as well, in the same request:
// without that, the second copy keeps a deleted folder forever, because a
// copy never removes a whole folder the source no longer lists.

// deleteWhere is the panel's answer to "delete it from where?".
type deleteWhere string

const (
	wherePrimary deleteWhere = "primary"
	whereBoth    deleteWhere = "both"
)

func (w deleteWhere) both() bool { return w == whereBoth }

// secondStore opens the second copy, or returns nil when there is none.
func (t *trayBackend) secondStore(ctx context.Context) (remote.Store, error) {
	c, err := t.d.app.mirror.Load()
	if errors.Is(err, creds.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := openStore(ctx, c, nil)
	if err != nil {
		return nil, fmt.Errorf("could not open the second copy: %w", err)
	}
	return s, nil
}

// alsoFromSecond runs del against the second copy when where asks for it.
// The error says which copy it is about, since the first one has already
// gone by the time this runs.
func (t *trayBackend) alsoFromSecond(ctx context.Context, where deleteWhere, del func(remote.Store) error) error {
	if !where.both() {
		return nil
	}
	s, err := t.secondStore(ctx)
	if err != nil || s == nil {
		return err
	}
	if err := del(s); err != nil {
		return fmt.Errorf("deleted from your backups, but not from the second copy: %w", err)
	}
	return remote.FlushStore(ctx, s)
}

// deleteStoredBackup deletes one backup that no folder on this computer
// uses: one another computer made, or one left behind when a folder was
// removed and its backup kept.
func (t *trayBackend) deleteStoredBackup(ctx context.Context, machine, name string, where deleteWhere) (string, error) {
	a, err := t.d.connected(ctx)
	if err != nil {
		return "", err
	}
	found, err := discoverBackups(ctx, a)
	if err != nil {
		return "", err
	}
	me := machineName()
	for _, f := range found {
		if f.Machine != machine || f.Name != name {
			continue
		}
		if f.Machine == me {
			for _, s := range a.sets.List() {
				if s.Prefix == f.Prefix {
					return "", fmt.Errorf("%q is backed up by this computer. Stop backing it up from its folder settings", s.Name)
				}
			}
		}
		scope := f.Prefix + "/"
		gone, err := a.client.DeletePrefix(ctx, scope)
		if err != nil {
			return "", err
		}
		if err := remote.FlushStore(ctx, a.client); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("Deleted %s (%s).", countOf(int64(gone.Objects), "file", "files"), progress.FormatBytes(gone.Bytes))
		err = t.alsoFromSecond(ctx, where, func(s remote.Store) error {
			_, err := s.DeletePrefix(ctx, scope)
			return err
		})
		return msg, err
	}
	return "", errors.New("that backup is not there any more")
}

// deleteCall serves deleting a backup from the panel's list of backups.
func (t *trayBackend) deleteCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "storedDelete":
		var a struct {
			Machine string `json:"machine"`
			Name    string `json:"name"`
			Where   string `json:"where"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if why := t.busyReason(); why != "" {
			return nil, true, fmt.Errorf("%s is running. Try again when it has finished", why)
		}
		msg, err := t.deleteStoredBackup(ctx, a.Machine, a.Name, deleteWhere(a.Where))
		t.invalidate()
		return map[string]string{"message": msg}, true, err
	}
	return nil, false, nil
}
