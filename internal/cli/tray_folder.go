package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's folder settings: what a folder includes, stopping a backup of
// it, and what the whole thing costs. Choosing what is included is the
// terminal's `edit` picker; stopping is `remove`; cost is the Cost tab. Each
// goes through the same set store and dashboard methods those use.

const localTreeTTL = 2 * time.Minute

// localTree is the folder's files as they are on disk now, scanned once and
// kept briefly, so opening folders in the picker costs nothing after the first.
func (t *trayBackend) localTree(ctx context.Context, s sets.Set, refresh bool) (*treeNode, error) {
	t.mu.Lock()
	c, ok := t.local[s.Name]
	t.mu.Unlock()
	if ok && !refresh && time.Since(c.at) < localTreeTTL {
		return c.root, nil
	}
	res, err := t.d.Scan(ctx, s.Root)
	if err != nil {
		if errors.Is(err, scan.ErrRootMissing) {
			return nil, fmt.Errorf("%s is not on this computer any more", s.Root)
		}
		return nil, err
	}
	var keys []string
	var sizes []int64
	var mods []time.Time
	for _, e := range res.Entries {
		if e.Kind != scan.KindFile {
			continue
		}
		keys, sizes, mods = append(keys, e.Key), append(sizes, e.Size), append(mods, e.ModTime)
	}
	root := buildTree(keys, sizes, mods)
	t.mu.Lock()
	if t.local == nil {
		t.local = map[string]cachedTree{}
	}
	t.local[s.Name] = cachedTree{root, time.Now()}
	t.mu.Unlock()
	return root, nil
}

type folderEntryOut struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Files int    `json:"files"`
	// State is "on" (backed up), "off" (left out) or "partial" (a folder
	// with something inside left out). Inherited is true when it is left out
	// only because a folder above it is, which can only be changed up there.
	State     string `json:"state"`
	Inherited string `json:"inherited,omitempty"`
}

type folderListOut struct {
	Dir      string           `json:"dir"`
	Entries  []folderEntryOut `json:"entries"`
	Excluded int              `json:"excluded"`
}

func hasPrefixRule(excludes []string, dir string) bool {
	for _, ex := range excludes {
		if strings.HasPrefix(ex, dir+"/") {
			return true
		}
	}
	return false
}

// ruleAbove names the rule that leaves path out through a folder above it,
// or "" when it is left out by its own rule or not at all.
func ruleAbove(excludes []string, path string) string {
	for _, ex := range excludes {
		if ex != path && strings.HasPrefix(path, ex+"/") {
			return ex
		}
	}
	return ""
}

func entryState(s sets.Set, path string, dir bool) (state, inherited string) {
	if s.Excluded(path) {
		return "off", ruleAbove(s.Excludes, path)
	}
	if dir && hasPrefixRule(s.Excludes, path) {
		return "partial", ""
	}
	return "on", ""
}

// setExcluded leaves path out of the folder's backup, or puts it back, and
// saves. Rules made redundant by the change are dropped, so the list stays
// as short as what it says.
func (t *trayBackend) setExcluded(name, path string, exclude bool) error {
	_ = t.d.app.sets.Reload()
	s, err := t.d.app.sets.Get(name)
	if err != nil {
		return err
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return errors.New("choose a file or folder")
	}
	if up := ruleAbove(s.Excludes, path); up != "" {
		return fmt.Errorf("it is left out because %s is. Change that one instead", up)
	}
	var next []string
	for _, ex := range s.Excludes {
		if ex == path || strings.HasPrefix(ex, path+"/") {
			continue // replaced by this change, either way
		}
		next = append(next, ex)
	}
	if exclude {
		next = append(next, path)
	}
	sort.Strings(next)
	return t.d.SetExcludes(context.Background(), name, next)
}

// busyReason says what stops a change that needs the index right now, or "".
func (t *trayBackend) busyReason() string {
	t.mu.Lock()
	child, busy := t.child != nil, t.busy
	t.mu.Unlock()
	if child {
		return busy
	}
	var ov ui.Overview
	if applyLiveRun(&ov, time.Now()) {
		return "a backup of " + ov.Running
	}
	return ""
}

type costOut struct {
	Provider       string  `json:"provider"`
	StoredBytes    int64   `json:"storedBytes"`
	StoredObjects  int64   `json:"storedObjects"`
	EstimatedUSD   float64 `json:"estimatedUSD"`
	ProjectedUSD   float64 `json:"projectedUSD"`
	StorageUSD     float64 `json:"storageUSD"`
	OperationsUSD  float64 `json:"operationsUSD"`
	WithinFreeTier bool    `json:"withinFreeTier"`
	OpsUsed        int     `json:"opsUsed"`
	OpsLimit       int     `json:"opsLimit"`
	BudgetUSD      float64 `json:"budgetUSD"`
	BudgetState    string  `json:"budgetState"`
}

func (t *trayBackend) cost(ctx context.Context) (costOut, error) {
	t.mu.Lock()
	ov, ok := t.last.ov, t.last.ok
	t.mu.Unlock()
	if !ok {
		_, fresh, err := t.d.Load(ctx)
		if err != nil {
			t.mu.Lock()
			ov, ok = t.last.ov, t.last.ov.Configured
			t.mu.Unlock()
			if !ok {
				return costOut{}, err
			}
		} else {
			ov = fresh
		}
	}
	return costOut{
		Provider: ov.Provider, StoredBytes: ov.StoredBytes, StoredObjects: ov.StoredObjects,
		EstimatedUSD: ov.EstimatedUSD, ProjectedUSD: ov.ProjectedUSD,
		StorageUSD: ov.StorageUSD, OperationsUSD: ov.OperationsUSD, WithinFreeTier: ov.WithinFreeTier,
		OpsUsed: ov.OpsUsed, OpsLimit: ov.OpsLimit, BudgetUSD: ov.BudgetUSD, BudgetState: ov.BudgetState,
	}, nil
}

// folderCall serves the panel's folder-settings and cost requests.
func (t *trayBackend) folderCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "folderList":
		var a struct {
			Name    string `json:"name"`
			Dir     string `json:"dir"`
			Refresh bool   `json:"refresh"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		s, err := t.d.app.sets.Get(a.Name)
		if err != nil {
			return nil, true, err
		}
		root, err := t.localTree(ctx, s, a.Refresh)
		if err != nil {
			return nil, true, err
		}
		n := root.at(a.Dir)
		if n == nil {
			return nil, true, fmt.Errorf("%q is not in this folder", a.Dir)
		}
		dir := strings.Trim(a.Dir, "/")
		res := folderListOut{Dir: dir, Entries: []folderEntryOut{}, Excluded: len(s.Excludes)}
		for name, k := range n.kids {
			p := name
			if dir != "" {
				p = dir + "/" + name
			}
			e := folderEntryOut{Name: name, Dir: k.dir, Size: k.size, Files: k.files}
			e.State, e.Inherited = entryState(s, p, k.dir)
			res.Entries = append(res.Entries, e)
		}
		sort.Slice(res.Entries, func(i, j int) bool {
			x, y := res.Entries[i], res.Entries[j]
			if x.Dir != y.Dir {
				return x.Dir
			}
			return strings.ToLower(x.Name) < strings.ToLower(y.Name)
		})
		return res, true, nil

	case "folderInfo":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		_ = t.d.app.sets.Reload()
		s, err := t.d.app.sets.Get(a.Name)
		if err != nil {
			return nil, true, err
		}
		exists := false
		if info, err := os.Stat(s.Root); err == nil && info.IsDir() {
			exists = true
		}
		return map[string]any{"name": s.Name, "root": s.Root, "rootExists": exists, "excluded": len(s.Excludes), "trash": s.TrashEnabled()}, true, nil

	case "folderExclude":
		var a struct {
			Name    string `json:"name"`
			Path    string `json:"path"`
			Exclude bool   `json:"exclude"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return nil, true, t.setExcluded(a.Name, a.Path, a.Exclude)

	case "folderRemove":
		var a struct {
			Name  string `json:"name"`
			Purge bool   `json:"purge"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if why := t.busyReason(); why != "" {
			return nil, true, fmt.Errorf("%s is running. Try again when it has finished", why)
		}
		_ = t.d.app.sets.Reload()
		err := t.d.Remove(ctx, a.Name, a.Purge)
		t.invalidate()
		return nil, true, err

	case "cost":
		c, err := t.cost(ctx)
		return c, true, err

	case "setBudget":
		var a struct {
			USD float64 `json:"usd"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		err := t.d.SetBudget(ctx, a.USD)
		t.invalidate()
		return nil, true, err

	case "resumeBudget":
		err := t.d.ResumeBudget(ctx)
		t.invalidate()
		return nil, true, err
	}
	return nil, false, nil
}
