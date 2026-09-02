package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// app is everything a command needs, opened once and closed once.
//
// index is a handle rather than an already-open database: see checkoutIndex.
// It is created once per app and never replaced, so a *index.DB captured
// elsewhere -- connect's Resume store, below, is the one that matters --
// stays valid for the app's whole life no matter how many times the index
// is checked out and given back in between.
type app struct {
	dir    string
	sets   *sets.Store
	index  *index.DB
	creds  *creds.Store
	client *remote.Client
}

// openApp opens local state only. Commands that never touch the network --
// status, ls, rename -- stop here, so they work with no connection and cost
// nothing.
//
// The index itself is not opened here. index.New builds a handle that opens
// nothing -- no file, no directory, no bbolt lock -- until something actually
// calls checkoutIndex; a command that never touches the index (relink, for
// instance) now never takes bbolt's lock at all, and one that does holds it
// only while checked out. See checkoutIndex and the dashboard struct's
// comment in dashboard.go for why that distinction is the whole fix.
func openApp() (*app, error) {
	dir, err := config.EnsureDataDir()
	if err != nil {
		return nil, err
	}
	setsPath := filepath.Join(dir, "sets.json")
	st, err := sets.Open(setsPath)
	if err != nil {
		return nil, err
	}
	idxPath, err := config.IndexPath()
	if err != nil {
		return nil, err
	}
	return &app{
		dir:   dir,
		sets:  st,
		index: index.New(idxPath),
		creds: creds.Open(filepath.Join(dir, "credentials")),
	}, nil
}

// checkoutIndex checks out the index for one caller's use and returns a func
// to give it back. Call the func exactly once, generally via defer,
// regardless of how the checkout was used -- a long-running one (a backup
// holds it for its whole run) is exactly as valid as a single call.
//
// This is the on-demand half of the fix: a command process still holds the
// index open from its first checkout until app.close() at exit, which costs
// it nothing since it is not running alongside itself. The dashboard is the
// caller this exists for -- it is long-lived and mostly idle, so checking
// out only while a Load, a Backup or some other operation is actually
// touching the index is what lets a second r2b process (a scheduled backup,
// `status`, `ls`) get the lock in between, instead of finding the window
// merely being open enough to starve it for as long as it stays open.
func (a *app) checkoutIndex() (*index.DB, func(), error) {
	if err := a.index.Acquire(); err != nil {
		return nil, nil, err
	}
	return a.index, func() { _ = a.index.Release() }, nil
}

func (a *app) close() {
	if a.index != nil {
		_ = a.index.Close()
	}
}

// connect adds the R2 client. Kept separate from openApp so a command only
// pays for -- and only fails on -- what it actually needs.
func (a *app) connect(ctx context.Context) error {
	if a.client != nil {
		return nil
	}
	c, err := a.creds.Load()
	if err != nil {
		if errors.Is(err, creds.ErrNotFound) {
			return errors.New("no credentials on this machine yet. Run: r2b setup")
		}
		return err
	}
	client, err := remote.New(ctx, remote.Config{
		AccountID:       c.AccountID,
		Endpoint:        c.Endpoint,
		Bucket:          c.Bucket,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		// Attached here, once, so every command that reaches the bucket can
		// pick up a large upload where the last one stopped. Without it a
		// file interrupted partway starts again from the beginning, which on
		// a connection bad enough to interrupt it once may mean it never
		// finishes at all.
		//
		// a.index is safe to capture here even though connect runs long
		// before, and independently of, any particular checkout: it is the
		// same *index.DB for the app's whole life (see the app struct's
		// comment), and every place that actually reads or writes through
		// this Resume store -- all inside backup.Run's upload path -- runs
		// only while that same call's own checkoutIndex is held. If index
		// were ever replaced with a freshly-opened *index.DB per checkout
		// instead of reused, this captured pointer would go stale the moment
		// the first checkout after connect released it.
		Resume: backup.ResumeStoreFor(a.index),
	})
	if err != nil {
		return fmt.Errorf("connect to R2: %w", err)
	}
	a.client = client
	return nil
}

// machineName is what this computer is filed under in the bucket.
func machineName() string {
	if n := os.Getenv("R2BACKUP_MACHINE"); n != "" {
		return n
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this-computer"
	}
	return h
}

// resolveSets returns the named set, or all of them when no name is given.
func (a *app) resolveSets(name string) ([]sets.Set, error) {
	if name == "" {
		all := a.sets.List()
		if len(all) == 0 {
			return nil, errors.New("nothing is being backed up yet. Run: r2b add <folder>")
		}
		return all, nil
	}
	s, err := a.sets.Get(name)
	if err != nil {
		return nil, err
	}
	return []sets.Set{s}, nil
}
