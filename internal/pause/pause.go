// Package pause lets a person hold a running backup and let it carry on later,
// from the taskbar panel or another terminal, whether the run was started
// there or by the scheduler.
//
// It is a file, for the same reason progress is: the run and whoever is asking
// it to wait are separate processes with no connection between them. The
// marker names the process it is meant for. A run only obeys a marker that
// carries its own pid, so a marker left behind -- by a crash, by the computer
// being switched off while a run was paused -- never holds a later run. A new
// run always starts unpaused; it also clears whatever marker it finds, and so
// does the end of every run.
//
// Pausing is cooperative and between pieces of work. Files and parts already
// on the wire finish; nothing new starts until the run is resumed. No
// connection is cut in the middle of a file.
package pause

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// FileName is the marker's name, beside the progress file in the data
// directory.
const FileName = "pause.json"

// Path is the marker that goes with a progress file.
func Path(progressPath string) string {
	return filepath.Join(filepath.Dir(progressPath), FileName)
}

// marker is what the file holds.
type marker struct {
	PID int       `json:"pid"`
	At  time.Time `json:"at"`
}

// Set asks the run with this pid to pause.
func Set(path string, pid int) error {
	data, err := json.Marshal(marker{PID: pid, At: time.Now()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pause-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Clear removes the marker, resuming whatever run it was holding. A marker
// that is not there is not an error.
func Clear(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// started is when this process began. A marker written before it cannot have
// been meant for it, whatever pid it names: pids are reused, and after a
// restart the number in a marker left by a paused run can easily belong to the
// next one.
var started = time.Now()

// For reports whether the marker asks the run with this pid -- this process,
// when it is the one asking -- to pause.
func For(path string, pid int) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return false
	}
	var m marker
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	return m.PID == pid && (pid != os.Getpid() || !m.At.Before(started))
}

// Gate is what a run consults between pieces of work.
type Gate interface {
	// Paused reports whether the run has been asked to hold.
	Paused() bool
	// Wait returns once the run is not paused, or with ctx's error if ctx
	// ends first.
	Wait(ctx context.Context) error
}

type ctxKey struct{}

// NewContext carries g to everything the run calls, so the engine's workers
// and the multipart senders can wait without each being handed it.
func NewContext(ctx context.Context, g Gate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, g)
}

// FromContext returns the gate ctx carries, or nil.
func FromContext(ctx context.Context) Gate {
	g, _ := ctx.Value(ctxKey{}).(Gate)
	return g
}

// Wait holds until the run ctx belongs to is not paused. Without a gate it
// returns at once (with ctx's error, if ctx has already ended).
func Wait(ctx context.Context) error {
	if g := FromContext(ctx); g != nil {
		return g.Wait(ctx)
	}
	return ctx.Err()
}

// PollEvery is how often a Watcher looks at the marker. A stat and a small
// read: cheap enough to do twice a second for as long as a run lasts.
const PollEvery = 500 * time.Millisecond

// Watcher is a Gate driven by the marker file.
type Watcher struct {
	path string
	pid  int

	paused atomic.Bool
	mu     sync.Mutex
	resume chan struct{} // closed when the run resumes; replaced on each pause
	stop   chan struct{}
	once   sync.Once
}

// Watch starts polling path for a marker naming pid. Stop it with Close.
func Watch(path string, pid int, every time.Duration) *Watcher {
	if every <= 0 {
		every = PollEvery
	}
	w := &Watcher{path: path, pid: pid, resume: make(chan struct{}), stop: make(chan struct{})}
	close(w.resume) // not paused
	w.poll()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				w.poll()
			}
		}
	}()
	return w
}

func (w *Watcher) poll() { w.set(For(w.path, w.pid)) }

// set changes the state, waking anything waiting when the run resumes.
func (w *Watcher) set(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.paused.Load() == on {
		return
	}
	w.paused.Store(on)
	if on {
		w.resume = make(chan struct{})
	} else {
		close(w.resume)
	}
}

// Paused reports whether the run is being held.
func (w *Watcher) Paused() bool { return w.paused.Load() }

// Wait blocks while the run is paused.
func (w *Watcher) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.mu.Lock()
		ch := w.resume
		w.mu.Unlock()
		select {
		case <-ch:
			if !w.paused.Load() {
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-w.stop:
			return ctx.Err()
		}
	}
}

// Close stops polling and releases anything waiting.
func (w *Watcher) Close() {
	w.once.Do(func() { close(w.stop) })
}
