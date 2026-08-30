// Package runstate is how a running backup tells anyone watching what it is
// doing, and how finished runs are remembered.
//
// A file rewritten by atomic rename, not a socket. A scheduled run and a
// terminal typing `r2backup status --watch` are separate processes with no
// connection between them, and the predecessor's IPC layer is where a
// surprising number of its bugs lived -- a status that disagreed with reality,
// a server that was not listening, a client that timed out mid-question. A file
// cannot disagree with itself: the watcher reads whatever the last complete
// write left, or it reads nothing.
package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Live is what a run in progress publishes, roughly once a second.
type Live struct {
	Set       string    `json:"set"`
	PID       int       `json:"pid"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
	FilesDone  int64 `json:"files_done"`
	FilesTotal int64 `json:"files_total"`

	ByteRate float64 `json:"byte_rate"`
	FileRate float64 `json:"file_rate"`

	ETASeconds float64 `json:"eta_seconds"`
	ETAKnown   bool    `json:"eta_known"`
}

// Stale reports whether this file is describing a run that is no longer
// happening. A killed process leaves its last update behind forever, and
// presenting that as live progress would be exactly the "stored status is a
// claim, not an answer" problem the predecessor had with device presence.
//
// Two independent checks, because either alone is wrong: the timestamp catches
// a process that is wedged, and the PID catches one that died between writes.
func (l *Live) Stale(now time.Time) bool {
	if now.Sub(l.UpdatedAt) > 15*time.Second {
		return true
	}
	return !processAlive(l.PID)
}

// ReadInterrupted returns the run that was going when this machine last
// stopped, if there was one.
//
// A run publishes progress about once a second and deletes the file when it
// finishes -- on success and on failure alike. So a file still sitting there
// whose process is gone did not finish and was not allowed to say why: the
// machine was shut down, the lid was closed, the process was killed. That is
// a different thing from "no run", and it used to read as the same thing,
// because every reader checked Stale and then ignored what it found.
//
// It is the whole basis of telling the user their upload is paused rather
// than showing them nothing.
func ReadInterrupted(path string, now time.Time) (*Live, bool) {
	l, err := ReadLive(path)
	if err != nil {
		return nil, false
	}
	// Still running, and this is not what that means.
	if !l.Stale(now) {
		return nil, false
	}
	// A dead process is the clear case. The other is a file nothing has
	// touched for far longer than a run ever goes quiet: a live run rewrites
	// this every second, so minutes of silence means it is not coming back,
	// whatever the pid says.
	//
	// Both are needed because pids are reused. After a reboot the number
	// written here can easily belong to something else entirely, and asking
	// only whether *a* process holds it would answer "still running" about a
	// run that ended when the machine did.
	if processAlive(l.PID) && now.Sub(l.UpdatedAt) < abandonedAfter {
		return nil, false
	}
	return l, true
}

// abandonedAfter is how long a progress file may go untouched before it is
// read as an interrupted run rather than a live one, regardless of what its
// pid resolves to now. A run writes every second; this is two orders of
// magnitude more patience than that.
const abandonedAfter = 2 * time.Minute

// Past is a finished run, kept so `status` can say what happened without
// asking the network anything.
type Past struct {
	Set        string    `json:"set"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   float64   `json:"duration_seconds"`

	Uploaded  int   `json:"uploaded"`
	Moved     int   `json:"moved"`
	Deleted   int   `json:"deleted"`
	Unchanged int   `json:"unchanged"`
	Bytes     int64 `json:"bytes"`

	Operations int `json:"operations"`

	// Problems and Failures are counts plus a few examples. The full lists
	// live in the run log; status needs enough to tell the user something is
	// wrong and where to look.
	Problems   int      `json:"problems"`
	Failures   int      `json:"failures"`
	Collisions int      `json:"collisions"`
	Examples   []string `json:"examples,omitempty"`

	Error string `json:"error,omitempty"`
}

// OK reports whether the run finished with nothing needing attention.
func (p *Past) OK() bool {
	return p.Error == "" && p.Failures == 0 && p.Collisions == 0
}

// WriteLive publishes progress atomically. A reader either sees the previous
// complete file or the new complete file, never half of either.
func WriteLive(path string, l Live) error {
	l.UpdatedAt = time.Now()
	if l.PID == 0 {
		l.PID = os.Getpid()
	}
	return writeJSON(path, l)
}

// ReadLive returns the current run, or ErrNoRun when there is not one.
func ReadLive(path string) (*Live, error) {
	var l Live
	if err := readJSON(path, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ClearLive removes the progress file at the end of a run.
func ClearLive(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear progress file %q: %w", path, err)
	}
	return nil
}

// ErrNoRun means no run is in progress, or none has ever finished.
var ErrNoRun = errors.New("no run recorded")

// History is the last N finished runs, newest first.
type History struct {
	Runs []Past `json:"runs"`
}

// MaxHistory is how many finished runs are kept. Enough to see a pattern,
// small enough that the file stays trivial to read and rewrite.
const MaxHistory = 50

// Record appends a finished run, keeping the newest MaxHistory.
func Record(path string, p Past) error {
	var h History
	if err := readJSON(path, &h); err != nil && !errors.Is(err, ErrNoRun) {
		return err
	}
	h.Runs = append(h.Runs, p)
	sort.Slice(h.Runs, func(i, j int) bool { return h.Runs[i].FinishedAt.After(h.Runs[j].FinishedAt) })
	if len(h.Runs) > MaxHistory {
		h.Runs = h.Runs[:MaxHistory]
	}
	return writeJSON(path, h)
}

// ReadHistory returns finished runs, newest first. An absent file is an empty
// history, not an error: a tool that has never run is a normal state.
func ReadHistory(path string) (*History, error) {
	var h History
	if err := readJSON(path, &h); err != nil {
		if errors.Is(err, ErrNoRun) {
			return &History{}, nil
		}
		return nil, err
	}
	return &h, nil
}

// Last returns the most recent run for a set.
func (h *History) Last(set string) (Past, bool) {
	for _, r := range h.Runs {
		if r.Set == set {
			return r, true
		}
	}
	return Past{}, false
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create directory for %q: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runstate-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file beside %q: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ErrNoRun
	}
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if len(data) == 0 {
		return ErrNoRun
	}
	if err := json.Unmarshal(data, v); err != nil {
		// A torn or corrupt file is not worth failing a command over; the
		// caller is asking what happened, and "nothing recorded" is a better
		// answer than an error about JSON.
		return ErrNoRun
	}
	return nil
}
