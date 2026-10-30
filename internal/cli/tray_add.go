package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/tray"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's add-a-folder sheet and the parts of a folder's settings that
// change what it is: its name, where it is, and how long deleted files are
// kept. Adding is `r2b add --name --retention` and the terminal's add form;
// the rest are `rename`, `relink` and the retention that only `add` could
// set before. Each goes through the same dashboard method or set store.

// retentionChoices are the lengths of time the panel offers for keeping
// deleted files. Anything else set elsewhere is shown as it is and kept.
var retentionChoices = map[int]bool{7: true, 14: true, 30: true, 60: true, 90: true, 180: true, 365: true}

// suggestName is the name a new folder gets unless someone changes it: its
// own name, or for a whole drive, the drive letter -- filepath.Base(`D:\`)
// is `\`, which is not a name.
func suggestName(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if base == "" || base == "." || base == string(filepath.Separator) || base == "/" {
		if v := strings.TrimSuffix(filepath.VolumeName(root), ":"); v != "" {
			return "Drive " + strings.ToUpper(v)
		}
		return "Backup"
	}
	if err := sets.ValidName(base); err != nil {
		return "Backup"
	}
	return base
}

// toggleExclude leaves path out of a folder's backup, or puts it back, and
// returns the new list. Rules made redundant by the change are dropped, so
// the list stays as short as what it says. It is the one rule both a folder
// already backed up and one about to be added use.
func toggleExclude(excludes []string, path string, exclude bool) ([]string, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil, errors.New("choose a file or folder")
	}
	if up := ruleAbove(excludes, path); up != "" {
		return nil, fmt.Errorf("it is left out because %s is. Change that one instead", up)
	}
	next := []string{}
	for _, ex := range excludes {
		if ex == path || strings.HasPrefix(ex, path+"/") {
			continue // replaced by this change, either way
		}
		next = append(next, ex)
	}
	if exclude {
		next = append(next, path)
	}
	sort.Strings(next)
	return next, nil
}

func cleanExcludes(in []string) []string {
	out := []string{}
	for _, ex := range in {
		if ex = strings.Trim(ex, "/"); ex != "" {
			out = append(out, ex)
		}
	}
	sort.Strings(out)
	return out
}

type addCheckOut struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Exists    bool   `json:"exists"`
	Overlaps  string `json:"overlaps,omitempty"`
	Scheduled bool   `json:"scheduled"`
	CanSched  bool   `json:"canSchedule"`
}

// nameProblem says what is wrong with a name for a new folder, or "".
func (t *trayBackend) nameProblem(name, except string) string {
	if err := sets.ValidName(name); err != nil {
		return "That name will not work: " + err.Error() + "."
	}
	if name != except {
		if _, err := t.d.app.sets.Get(name); err == nil {
			return "There is already a folder called " + name + "."
		}
	}
	return ""
}

// addCall serves the add sheet and a folder's name, place and retention.
func (t *trayBackend) addCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "addCheck":
		var a struct {
			Path string `json:"path"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		root, err := filepath.Abs(a.Path)
		if err != nil {
			return nil, true, err
		}
		res := addCheckOut{Path: root, Name: suggestName(root)}
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			res.Exists = true
		}
		if other, ok := t.d.Overlaps(root); ok {
			res.Overlaps = other
		}
		// A name already taken gets a number, so the suggestion is one that
		// works rather than one that fails on the first press.
		for i := 2; t.nameProblem(res.Name, "") != "" && i < 100; i++ {
			res.Name = fmt.Sprintf("%s %d", suggestName(root), i)
		}
		t.mu.Lock()
		res.Scheduled, res.CanSched = t.last.ov.Scheduled, t.last.ov.SchedulerAvailable
		t.mu.Unlock()
		return res, true, nil

	case "addFolder":
		var a struct {
			Path      string   `json:"path"`
			Name      string   `json:"name"`
			Excludes  []string `json:"excludes"`
			Retention int      `json:"retention"`
			Schedule  bool     `json:"schedule"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		a.Name = strings.TrimSpace(a.Name)
		if p := t.nameProblem(a.Name, ""); p != "" {
			return nil, true, errors.New(p)
		}
		if info, err := os.Stat(a.Path); err != nil || !info.IsDir() {
			return nil, true, fmt.Errorf("%s is not a folder on this computer", a.Path)
		}
		// AddRequest's retention: 0 keeps nothing, which is a choice the
		// sheet offers by name; anything else is days.
		if a.Retention < 0 {
			a.Retention = 0
		}
		if err := t.d.Add(ctx, ui.AddRequest{Name: a.Name, Root: a.Path, Excludes: cleanExcludes(a.Excludes), Retention: a.Retention}); err != nil {
			return nil, true, err
		}
		t.invalidate()
		res := map[string]any{"name": a.Name, "backingUp": true}
		if a.Schedule {
			if err := t.d.Schedule(ctx, 30, false); err != nil {
				res["scheduleError"] = tray.Plain(err.Error())
			}
		}
		if err := t.BackUp(a.Name); err != nil {
			// Added all the same: the next run, scheduled or by hand, backs
			// it up. Said so rather than reported as a failure to add.
			res["backingUp"] = false
			res["why"] = tray.Plain(err.Error())
		}
		return res, true, nil

	case "excludeToggle":
		var a struct {
			Excludes []string `json:"excludes"`
			Path     string   `json:"path"`
			Exclude  bool     `json:"exclude"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		next, err := toggleExclude(cleanExcludes(a.Excludes), a.Path, a.Exclude)
		return next, true, err

	case "folderRename":
		var a struct {
			Name string `json:"name"`
			To   string `json:"to"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		a.To = strings.TrimSpace(a.To)
		if a.To == a.Name {
			return nil, true, nil
		}
		if p := t.nameProblem(a.To, a.Name); p != "" {
			return nil, true, errors.New(p)
		}
		if why := t.busyReason(); why != "" {
			return nil, true, fmt.Errorf("%s is running. Try again when it has finished", why)
		}
		err := t.d.Rename(ctx, a.Name, a.To)
		t.invalidate()
		return nil, true, err

	case "folderRelink":
		var a struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		if info, err := os.Stat(a.Path); err != nil || !info.IsDir() {
			return nil, true, fmt.Errorf("there is no folder at %s", a.Path)
		}
		err := t.d.Relink(ctx, a.Name, a.Path)
		t.mu.Lock()
		delete(t.local, a.Name)
		t.mu.Unlock()
		t.invalidate()
		return nil, true, err

	case "folderRetention":
		var a struct {
			Name string `json:"name"`
			Days int    `json:"days"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		s, err := t.d.app.sets.Get(a.Name)
		if err != nil {
			return nil, true, err
		}
		if !retentionChoices[a.Days] && a.Days != s.RetentionDays {
			return nil, true, errors.New("choose how long to keep deleted files")
		}
		// Turning the trash off is not offered here: files already in it
		// would stop being listed, and so stop being recoverable, while
		// still being stored. Only a new folder can start without one.
		s.RetentionDays = a.Days
		err = t.d.app.sets.Update(s)
		t.invalidate()
		return nil, true, err

	case "backupStop":
		return nil, true, t.stop()
	}
	return nil, false, nil
}
