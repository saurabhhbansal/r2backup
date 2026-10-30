package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's deleted files: what mirroring moved aside, kept for the folder's
// retention window. Listing and recovering are the terminal's Trash tab and
// restore --deleted; deleting for good is new here, and goes through the same
// trash package that expires old files on its own.

type trashRowOut struct {
	Set        string `json:"set"`
	Key        string `json:"key"`
	Size       int64  `json:"size"`
	Deleted    string `json:"deleted"`
	Exact      bool   `json:"exact"`
	Expires    string `json:"expires"`
	Root       string `json:"root,omitempty"`
	RootExists bool   `json:"rootExists"`
}

// trashRows lists what is recoverable across every folder on this computer
// whose set keeps a trash, newest first.
func (t *trayBackend) trashRows(ctx context.Context, only string) ([]trashRowOut, error) {
	a, err := t.d.connected(ctx)
	if err != nil {
		return nil, err
	}
	_ = a.sets.Reload()
	tr := trash.New(a.client, trash.Clock{})
	rows := []trashRowOut{}
	for _, s := range a.sets.List() {
		if (only != "" && s.Name != only) || !s.TrashEnabled() {
			continue
		}
		entries, err := tr.List(ctx, s.Prefix, s.RetentionDays)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		exists := false
		if info, err := os.Stat(s.Root); err == nil && info.IsDir() {
			exists = true
		}
		for _, e := range entries {
			rows = append(rows, trashRowOut{
				Set: s.Name, Key: e.RelPath, Size: e.Size,
				Deleted: e.TrashedOn.Format(time.RFC3339), Exact: e.TrashedOnExact,
				Expires: e.ExpiresOn.Format(time.RFC3339),
				Root:    s.Root, RootExists: exists,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Deleted > rows[j].Deleted })
	return rows, nil
}

// trashDelete removes the trashed copies of one path, or all of a folder's
// trash when key is empty, and from the second copy too when where says so.
// It returns how many objects went.
func (t *trayBackend) trashDelete(ctx context.Context, set, key string, where deleteWhere) (int, error) {
	a, err := t.d.connected(ctx)
	if err != nil {
		return 0, err
	}
	s, err := a.sets.Get(set)
	if err != nil {
		return 0, err
	}
	entries, err := trash.New(a.client, trash.Clock{}).List(ctx, s.Prefix, s.RetentionDays)
	if err != nil {
		return 0, err
	}
	var keys []string
	for _, e := range entries {
		if key == "" || e.RelPath == key {
			keys = append(keys, e.TrashKey)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	if err := a.client.DeleteBatch(ctx, keys); err != nil {
		return 0, err
	}
	// The second copy holds the same keys: a copy is key for key.
	err = t.alsoFromSecond(ctx, where, func(s remote.Store) error { return s.DeleteBatch(ctx, keys) })
	return len(keys), err
}

// trashCall serves the panel's deleted-files requests.
func (t *trayBackend) trashCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "trashList":
		rows, err := t.trashRows(ctx, "")
		return rows, true, err

	case "trashRecover":
		var a struct {
			Set string `json:"set"`
			Key string `json:"key"`
			To  string `json:"to"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if a.Set == "" || a.Key == "" {
			return nil, true, errors.New("choose a file to recover")
		}
		res, err := t.d.Restore(ctx, ui.RestoreRequest{Set: a.Set, To: a.To, Deleted: a.Key, Overwrite: true},
			func(string) {}, func(progress.Snapshot) {})
		if err != nil {
			return nil, true, err
		}
		return map[string]any{"target": res.Target, "files": res.Files}, true, nil

	case "trashDelete":
		var a struct {
			Set   string `json:"set"`
			Key   string `json:"key"`
			Where string `json:"where"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if a.Set == "" || a.Key == "" {
			return nil, true, errors.New("choose a file to delete")
		}
		n, err := t.trashDelete(ctx, a.Set, a.Key, deleteWhere(a.Where))
		return map[string]int{"deleted": n}, true, err

	case "trashEmpty":
		var a struct {
			Set   string `json:"set"`
			Where string `json:"where"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		names := []string{a.Set}
		if a.Set == "" {
			// every folder's trash
			rows, err := t.trashRows(ctx, "")
			if err != nil {
				return nil, true, err
			}
			seen := map[string]bool{}
			names = names[:0]
			for _, r := range rows {
				if !seen[r.Set] {
					seen[r.Set] = true
					names = append(names, r.Set)
				}
			}
		}
		total := 0
		for _, name := range names {
			n, err := t.trashDelete(ctx, name, "", deleteWhere(a.Where))
			total += n
			if err != nil {
				return map[string]int{"deleted": total}, true, err
			}
		}
		return map[string]int{"deleted": total}, true, nil
	}
	return nil, false, nil
}
