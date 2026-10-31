package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// The panel's second copy: `r2b mirror setup`, `r2b mirror` and `r2b mirror
// auto`, which had no screen at all. Setting it up checks and saves the
// second destination exactly as the command does; copying runs mirror.Run
// here, in the background, so the panel can show how far it has got.

// destLabel names a destination the way the panel does: "Drime · r2backup",
// or the bucket on Cloudflare R2.
func destLabel(c creds.Credentials) string {
	if c.IsDrime() {
		root := c.DrimeRoot
		if root == "" {
			root = "r2backup"
		}
		return "Drime · " + root
	}
	return c.Label() + " · " + c.Bucket
}

// samePlace reports whether two destinations are the same storage, which a
// mirror must not be: copying a place onto itself, with deletes on, is a way
// to lose it.
func samePlace(a, b creds.Credentials) bool {
	if a.IsDrime() != b.IsDrime() {
		return false
	}
	if a.IsDrime() {
		return strings.TrimSpace(a.DrimeToken) == strings.TrimSpace(b.DrimeToken) &&
			a.DrimeWorkspace == b.DrimeWorkspace && a.DrimeRoot == b.DrimeRoot
	}
	ea, eb := a.S3(), b.S3()
	return strings.EqualFold(strings.TrimSpace(ea.URL), strings.TrimSpace(eb.URL)) && ea.Region == eb.Region &&
		strings.TrimSpace(a.Bucket) == strings.TrimSpace(b.Bucket)
}

// mirrorJob is the one copy the panel is running or has just run.
type mirrorJob struct {
	mu      sync.Mutex
	reverse bool
	to      string
	p       mirror.Progress
	done    bool
	rep     *mirror.Report
	err     string
	cancel  context.CancelFunc
}

type mirrorStatusOut struct {
	Idle      bool    `json:"idle"`
	Reverse   bool    `json:"reverse"`
	To        string  `json:"to,omitempty"`
	Percent   float64 `json:"percent"`
	Files     string  `json:"files,omitempty"`
	BytesDone string  `json:"bytesDone,omitempty"`
	BytesAll  string  `json:"bytesAll,omitempty"`
	Done      bool    `json:"done"`
	Error     string  `json:"error,omitempty"`
	Copied    int     `json:"copied"`
	Removed   int     `json:"removed"`
	Unchanged int     `json:"unchanged"`
	Failed    int     `json:"failed"`
	Bytes     string  `json:"bytes,omitempty"`
	Held      int     `json:"held"`
}

func (j *mirrorJob) status() mirrorStatusOut {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := mirrorStatusOut{Reverse: j.reverse, To: j.to, Done: j.done, Error: j.err}
	if j.p.FilesTotal > 0 {
		st.Files = progress.FormatCount(int64(j.p.FilesDone)) + " of " + progress.FormatCount(int64(j.p.FilesTotal))
	}
	if j.p.BytesTotal > 0 {
		st.Percent = float64(j.p.BytesDone) / float64(j.p.BytesTotal) * 100
		st.BytesDone, st.BytesAll = progress.FormatBytes(j.p.BytesDone), progress.FormatBytes(j.p.BytesTotal)
	} else if j.p.FilesTotal > 0 {
		st.Percent = float64(j.p.FilesDone) / float64(j.p.FilesTotal) * 100
	}
	if r := j.rep; r != nil {
		st.Copied, st.Removed, st.Unchanged, st.Failed = r.Copied, r.Deleted, r.Unchanged, len(r.Failures)
		st.Bytes, st.Held = progress.FormatBytes(r.Bytes), r.SkippedDeletes
		if j.done && j.err == "" {
			st.Percent = 100
		}
	}
	return st
}

type mirrorInfoOut struct {
	Main      string          `json:"main"`
	Set       bool            `json:"set"`
	Dest      string          `json:"dest,omitempty"`
	Provider  string          `json:"provider,omitempty"` // drime or r2, the mirror's
	Suggest   string          `json:"suggest"`            // the other kind from the main one
	Auto      bool            `json:"auto"`
	Last      string          `json:"last,omitempty"`
	Job       mirrorStatusOut `json:"job"`
	HaveSets  bool            `json:"haveFolders"`
	Connected bool            `json:"connected"`
}

func (t *trayBackend) mirrorInfo() (mirrorInfoOut, error) {
	a := t.d.app
	out := mirrorInfoOut{Suggest: "r2", Job: mirrorStatusOut{Idle: true}}
	main, err := a.creds.Load()
	if err == nil {
		out.Main, out.Connected = destLabel(main), main.Valid() == nil
		if !main.IsDrime() {
			out.Suggest = "drime"
		}
	} else if !errors.Is(err, creds.ErrNotFound) {
		return out, err
	}
	if mc, err := a.mirror.Load(); err == nil {
		out.Set, out.Dest, out.Provider = true, destLabel(mc), mc.Kind()
	}
	if s, err := config.LoadSettings(); err == nil {
		out.Auto = s.MirrorAuto && out.Set
		var last int64
		for _, v := range s.MirrorLast {
			if v > last {
				last = v
			}
		}
		if last > 0 {
			out.Last = time.Unix(last, 0).Format(time.RFC3339)
		}
	}
	_ = a.sets.Reload()
	out.HaveSets = len(a.sets.List()) > 0
	t.mu.Lock()
	if t.mj != nil {
		out.Job = t.mj.status()
	}
	t.mu.Unlock()
	return out, nil
}

func (t *trayBackend) mirrorSetup(ctx context.Context, c creds.Credentials, auto bool) error {
	a := t.d.app
	main, err := a.creds.Load()
	if err != nil {
		return errors.New("connect your storage first: Settings, then Storage")
	}
	if err := c.Valid(); err != nil {
		return err
	}
	if samePlace(main, c) {
		return errors.New("that is where your backups already go. A second copy has to be somewhere else")
	}
	store, err := openStore(ctx, c, nil)
	if err != nil {
		return err
	}
	if _, err := store.List(ctx, "r2backup/"); err != nil {
		return errors.New("could not reach it with those details: " + err.Error())
	}
	if err := a.mirror.Save(c); err != nil {
		return err
	}
	return setMirrorAuto(auto)
}

func (t *trayBackend) startMirror(everything, reverse bool) error {
	t.mu.Lock()
	if t.mj != nil && !t.mj.done {
		t.mu.Unlock()
		return errors.New("a copy is already running")
	}
	t.mu.Unlock()

	a := t.d.app
	_ = a.sets.Reload()
	var list []sets.Set
	prefixes := []string{"machines/"}
	if !everything {
		list = a.sets.List()
		if len(list) == 0 {
			return errors.New("add a folder first")
		}
		prefixes = nil
		for _, s := range list {
			prefixes = append(prefixes, s.KeyScope())
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Connected under the dashboard's lock, so mirrorEnds below finds the
	// client already there rather than building one beside a backup's.
	if _, err := t.d.connected(ctx); err != nil {
		cancel()
		return err
	}
	src, dst, srcName, dstName, err := mirrorEnds(ctx, a, reverse)
	if err != nil {
		cancel()
		return err
	}
	if srcName == dstName && src.Bucket() == dst.Bucket() {
		cancel()
		return errors.New("the second copy is the same place as your backups")
	}
	j := &mirrorJob{reverse: reverse, to: dstName, cancel: cancel}
	t.mu.Lock()
	t.mj = j
	t.mu.Unlock()

	go func() {
		defer cancel()
		rep, err := mirror.Run(ctx, mirror.Options{
			Src: src, Dst: dst, Prefixes: prefixes,
			// Copying back never removes anything from where backups go:
			// it is for getting files back, and a mirror that is behind
			// must not be able to take them away.
			Delete: !reverse,
			Progress: func(p mirror.Progress) {
				j.mu.Lock()
				j.p = p
				j.mu.Unlock()
			},
		})
		j.mu.Lock()
		j.done, j.rep = true, rep
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			j.err = "Stopped before it finished. The next copy picks up from there."
		case err != nil:
			j.err = err.Error()
		}
		j.mu.Unlock()
		if err == nil && rep != nil && len(rep.Failures) == 0 && !reverse {
			recordMirrored(list, everything)
		}
	}()
	return nil
}

// mirrorCall serves the panel's second-copy requests.
func (t *trayBackend) mirrorCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "mirrorInfo":
		res, err := t.mirrorInfo()
		return res, true, err

	case "mirrorSetup":
		var a struct {
			destArgs
			Auto bool `json:"auto"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return nil, true, t.mirrorSetup(ctx, credsFromKeys(a.keys()), a.Auto)

	// Drime folders another app backs up to, copied to S3 storage. See
	// mirror_sources.go.
	case "drimeSources":
		return t.drimeSources(), true, nil

	case "drimeSourceAdd":
		var a struct {
			Token     string `json:"token"` // only when none is saved yet
			Folder    string `json:"folder"`
			Name      string `json:"name"`
			Workspace string `json:"workspace"`
			Auto      bool   `json:"auto"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		err := addDrimeSource(ctx, t.d.app, a.Token, config.MirrorSource{
			Name: a.Name, Folder: a.Folder, Workspace: a.Workspace, Auto: a.Auto})
		if err == nil && a.Token != "" {
			_ = t.pushKeys(ctx)
		}
		return t.drimeSources(), true, err

	case "drimeSourceRemove":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return t.drimeSources(), true, removeDrimeSource(a.Name)

	// Copy one Drime folder (or all, with no name) now, in the background;
	// mirrorStatus follows it like any other copy.
	case "drimeSourceRun":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return nil, true, t.startDrimeSources(a.Name)

	case "mirrorAuto":
		var a struct {
			On bool `json:"on"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if a.On && !t.d.app.mirror.Exists() {
			return nil, true, errors.New("set up a second copy first")
		}
		return nil, true, setMirrorAuto(a.On)

	case "mirrorRun":
		var a struct {
			Everything bool `json:"everything"`
			Reverse    bool `json:"reverse"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return nil, true, t.startMirror(a.Everything, a.Reverse)

	case "mirrorStatus":
		t.mu.Lock()
		j := t.mj
		t.mu.Unlock()
		if j == nil {
			return mirrorStatusOut{Idle: true}, true, nil
		}
		return j.status(), true, nil

	case "mirrorCancel":
		t.mu.Lock()
		j := t.mj
		t.mu.Unlock()
		if j != nil && j.cancel != nil {
			j.cancel()
		}
		return nil, true, nil

	case "mirrorDismiss":
		t.mu.Lock()
		if t.mj != nil && t.mj.done {
			t.mj = nil
		}
		t.mu.Unlock()
		return nil, true, nil

	case "mirrorForget":
		t.mu.Lock()
		running := t.mj != nil && !t.mj.done
		t.mu.Unlock()
		if running {
			return nil, true, errors.New("a copy is running. Stop it first")
		}
		if err := setMirrorAuto(false); err != nil {
			return nil, true, err
		}
		return nil, true, t.d.app.mirror.Delete()
	}
	return nil, false, nil
}

type drimeSourceOut struct {
	Name    string `json:"name"`
	Folder  string `json:"folder"`
	Auto    bool   `json:"auto"`
	LastRun string `json:"lastRun,omitempty"`
}

type drimeSourcesOut struct {
	Sources []drimeSourceOut `json:"sources"`
	// Dest is the S3 storage they are copied to; DestErr says why there is
	// none yet.
	Dest    string `json:"dest,omitempty"`
	DestErr string `json:"destErr,omitempty"`
	// HaveToken is whether a Drime token to read with is on this computer.
	HaveToken bool `json:"haveToken"`
}

func (t *trayBackend) drimeSources() drimeSourcesOut {
	a := t.d.app
	out := drimeSourcesOut{Sources: []drimeSourceOut{}}
	if c, err := s3Destination(a); err == nil {
		out.Dest = destLabel(c)
	} else {
		out.DestErr = err.Error()
	}
	_, err := drimeSourceCreds(a)
	out.HaveToken = err == nil
	if st, err := config.LoadSettings(); err == nil {
		for _, s := range st.MirrorSources {
			o := drimeSourceOut{Name: s.Name, Folder: s.Folder, Auto: s.Auto}
			if s.LastRun > 0 {
				o.LastRun = time.Unix(s.LastRun, 0).Format(time.RFC3339)
			}
			out.Sources = append(out.Sources, o)
		}
	}
	return out
}

func (t *trayBackend) startDrimeSources(name string) error {
	t.mu.Lock()
	if t.mj != nil && !t.mj.done {
		t.mu.Unlock()
		return errors.New("a copy is already running")
	}
	t.mu.Unlock()
	a := t.d.app
	dest, err := s3Destination(a)
	if err != nil {
		return err
	}
	var names []string
	label := "Drime folders"
	if name != "" {
		names, label = []string{name}, "Drime folder "+name
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &mirrorJob{to: label + " → " + destLabel(dest), cancel: cancel}
	t.mu.Lock()
	t.mj = j
	t.mu.Unlock()
	go func() {
		defer cancel()
		results, err := runDrimeSources(ctx, a, names, false, nil, func(_ string, p mirror.Progress) {
			j.mu.Lock()
			j.p = p
			j.mu.Unlock()
		})
		total := &mirror.Report{}
		for _, r := range results {
			if r.Err != nil && err == nil {
				err = fmt.Errorf("%s: %w", r.Name, r.Err)
			}
			if r.Report != nil {
				total.Copied += r.Report.Copied
				total.Deleted += r.Report.Deleted
				total.Unchanged += r.Report.Unchanged
				total.Bytes += r.Report.Bytes
				total.Failures = append(total.Failures, r.Report.Failures...)
				total.SkippedDeletes += r.Report.SkippedDeletes
			}
		}
		j.mu.Lock()
		j.done, j.rep = true, total
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			j.err = "Stopped before it finished. The next copy picks up from there."
		case err != nil:
			j.err = err.Error()
		}
		j.mu.Unlock()
	}()
	return nil
}
