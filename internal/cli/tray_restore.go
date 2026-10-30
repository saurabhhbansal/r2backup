package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's restore: pick a backed-up folder, browse what is in it, pick
// files and folders (or take everything), pick where they go, watch it run.
// Each step is a request the page makes; the restore itself is d.Restore, the
// same code r2b restore and the terminal's Restore key run.

// treeNode is one file or folder in a backed-up set, built from one listing
// of the bucket so browsing costs nothing after the first look.
type treeNode struct {
	dir   bool
	size  int64 // a file's size; a folder's total
	files int   // files at or below this folder
	mod   time.Time
	kids  map[string]*treeNode
}

type cachedTree struct {
	root *treeNode
	at   time.Time
}

const treeTTL = 5 * time.Minute

func buildTree(keys []string, sizes []int64, mods []time.Time) *treeNode {
	root := &treeNode{dir: true, kids: map[string]*treeNode{}}
	for i, key := range keys {
		parts := strings.Split(key, "/")
		n := root
		n.files++
		n.size += sizes[i]
		for j, part := range parts {
			last := j == len(parts)-1
			kid, ok := n.kids[part]
			if !ok {
				kid = &treeNode{dir: !last}
				if kid.dir {
					kid.kids = map[string]*treeNode{}
				}
				n.kids[part] = kid
			}
			if last {
				kid.size, kid.mod, kid.files = sizes[i], mods[i], 1
				break
			}
			kid.files++
			kid.size += sizes[i]
			n = kid
		}
	}
	return root
}

func (n *treeNode) at(dir string) *treeNode {
	cur := n
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		next, ok := cur.kids[part]
		if !ok || !next.dir {
			return nil
		}
		cur = next
	}
	return cur
}

// restoreTree is the set's files as a tree, listed from the bucket once and
// kept for a few minutes. refresh forces a new listing.
func (t *trayBackend) restoreTree(ctx context.Context, set, machine string, refresh bool) (*treeNode, error) {
	cacheKey := machine + "\x00" + set
	t.mu.Lock()
	c, ok := t.trees[cacheKey]
	t.mu.Unlock()
	if ok && !refresh && time.Since(c.at) < treeTTL {
		return c.root, nil
	}
	a, err := t.d.connected(ctx)
	if err != nil {
		return nil, err
	}
	s, err := resolveRestoreSet(ctx, a, set, machine)
	if err != nil {
		return nil, err
	}
	prefix, err := restore.SourcePrefix(s, machine)
	if err != nil {
		return nil, err
	}
	cp := restore.CurrentPrefix(prefix)
	entries, err := a.client.List(ctx, cp)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	sizes := make([]int64, 0, len(entries))
	mods := make([]time.Time, 0, len(entries))
	for _, e := range entries {
		rel := strings.TrimPrefix(e.Key, cp)
		if rel == "" || rel == e.Key || strings.HasSuffix(rel, "/") {
			continue
		}
		keys, sizes, mods = append(keys, rel), append(sizes, e.Size), append(mods, e.ModTime)
	}
	root := buildTree(keys, sizes, mods)
	t.mu.Lock()
	if t.trees == nil {
		t.trees = map[string]cachedTree{}
	}
	t.trees[cacheKey] = cachedTree{root, time.Now()}
	t.mu.Unlock()
	return root, nil
}

type restoreSetOut struct {
	Name       string `json:"name"`
	Machine    string `json:"machine"`
	Here       bool   `json:"here"`
	Root       string `json:"root,omitempty"`
	RootExists bool   `json:"rootExists"`
}

type treeEntryOut struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Files int    `json:"files"`
	Mod   string `json:"mod,omitempty"`
}

type treeOut struct {
	Dir     string         `json:"dir"`
	Entries []treeEntryOut `json:"entries"`
	Files   int            `json:"files"`
	Size    int64          `json:"size"`
}

// restoreJob is the one restore the panel is running or has just run.
type restoreJob struct {
	mu      sync.Mutex
	set     string
	to      string
	phase   string
	snap    progress.Snapshot
	done    bool
	result  ui.RestoreResult
	err     string
	cancel  context.CancelFunc
	started time.Time
}

type restoreStatus struct {
	Idle      bool    `json:"idle"`
	Set       string  `json:"set,omitempty"`
	Phase     string  `json:"phase,omitempty"`
	Percent   float64 `json:"percent"`
	BytesDone string  `json:"bytesDone,omitempty"`
	BytesAll  string  `json:"bytesAll,omitempty"`
	ETA       string  `json:"eta,omitempty"`
	Done      bool    `json:"done"`
	Error     string  `json:"error,omitempty"`
	Files     int     `json:"files"`
	Bytes     string  `json:"bytes,omitempty"`
	Target    string  `json:"target,omitempty"`
	Skipped   int     `json:"skipped"`
}

func (j *restoreJob) status() restoreStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := restoreStatus{Set: j.set, Phase: j.phase, Percent: j.snap.Percent, Done: j.done, Error: j.err,
		Files: j.result.Files, Target: j.result.Target, Skipped: j.result.Skipped}
	if j.snap.BytesTotal > 0 {
		st.BytesDone, st.BytesAll = progress.FormatBytes(j.snap.BytesDone), progress.FormatBytes(j.snap.BytesTotal)
	}
	if j.snap.ETAKnown {
		st.ETA = j.snap.ETA.Round(time.Second).String()
	}
	if j.done && j.err == "" {
		st.Bytes = progress.FormatBytes(j.result.Bytes)
		st.Percent = 100
	}
	return st
}

// onlyPattern turns a picked path into an --only pattern that means exactly
// it: a folder is everything under it, and a file whose name has glob
// characters has them escaped so it matches itself and nothing else.
func onlyPattern(path string, dir bool) string {
	path = strings.Trim(path, "/")
	if dir {
		return path + "/**"
	}
	if !strings.ContainsAny(path, `*?[\`) {
		return path
	}
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// restoreCall serves the panel's restore requests. ok reports whether the
// method was one of them.
func (t *trayBackend) restoreCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "restoreSets":
		a, err := t.d.connected(ctx)
		if err != nil {
			return nil, true, err
		}
		rows, err := t.d.RemoteSets(ctx)
		if err != nil {
			return nil, true, err
		}
		res := make([]restoreSetOut, 0, len(rows))
		for _, r := range rows {
			o := restoreSetOut{Name: r.Name, Machine: r.Machine, Here: r.Here}
			if r.Here {
				if s, err := a.sets.Get(r.Name); err == nil && s.Root != "" {
					o.Root = s.Root
					if info, err := os.Stat(s.Root); err == nil && info.IsDir() {
						o.RootExists = true
					}
				}
			}
			res = append(res, o)
		}
		return res, true, nil

	case "restoreList":
		var a struct {
			Set     string `json:"set"`
			Machine string `json:"machine"`
			Dir     string `json:"dir"`
			Refresh bool   `json:"refresh"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		root, err := t.restoreTree(ctx, a.Set, a.Machine, a.Refresh)
		if err != nil {
			return nil, true, err
		}
		n := root.at(a.Dir)
		if n == nil {
			return nil, true, fmt.Errorf("%q is not in this backup", a.Dir)
		}
		res := treeOut{Dir: strings.Trim(a.Dir, "/"), Entries: []treeEntryOut{}, Files: n.files, Size: n.size}
		for name, k := range n.kids {
			e := treeEntryOut{Name: name, Dir: k.dir, Size: k.size, Files: k.files}
			if !k.dir && !k.mod.IsZero() {
				e.Mod = k.mod.Format(time.RFC3339)
			}
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

	case "restoreStart":
		var a struct {
			Set       string `json:"set"`
			Machine   string `json:"machine"`
			To        string `json:"to"`
			Overwrite bool   `json:"overwrite"`
			Verify    bool   `json:"verify"`
			Items     []struct {
				Path string `json:"path"`
				Dir  bool   `json:"dir"`
			} `json:"items"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if a.Set == "" {
			return nil, true, errors.New("choose a folder to restore")
		}
		req := ui.RestoreRequest{Set: a.Set, Machine: a.Machine, To: a.To, Overwrite: a.Overwrite, Verify: a.Verify}
		for _, it := range a.Items {
			req.OnlyAny = append(req.OnlyAny, onlyPattern(it.Path, it.Dir))
		}
		return nil, true, t.startRestore(req)

	case "restoreStatus":
		t.mu.Lock()
		j := t.rj
		t.mu.Unlock()
		if j == nil {
			return restoreStatus{Idle: true}, true, nil
		}
		return j.status(), true, nil

	case "restoreCancel":
		t.mu.Lock()
		j := t.rj
		t.mu.Unlock()
		if j != nil && j.cancel != nil {
			j.cancel()
		}
		return nil, true, nil

	case "restoreDismiss":
		t.mu.Lock()
		if t.rj != nil && t.rj.done {
			t.rj = nil
		}
		t.mu.Unlock()
		return nil, true, nil
	}
	return nil, false, nil
}

func (t *trayBackend) startRestore(req ui.RestoreRequest) error {
	t.mu.Lock()
	if t.rj != nil && !t.rj.done {
		t.mu.Unlock()
		return errors.New("a restore is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &restoreJob{set: req.Set, to: req.To, phase: "starting", cancel: cancel, started: time.Now()}
	t.rj = j
	t.mu.Unlock()

	go func() {
		defer cancel()
		res, err := t.d.Restore(ctx, req,
			func(p string) { j.mu.Lock(); j.phase = p; j.mu.Unlock() },
			func(s progress.Snapshot) { j.mu.Lock(); j.snap = s; j.mu.Unlock() })
		j.mu.Lock()
		j.done, j.result = true, res
		if err != nil {
			j.err = err.Error()
			if errors.Is(ctx.Err(), context.Canceled) {
				j.err = "Stopped before it finished."
			}
		}
		j.mu.Unlock()
	}()
	return nil
}
