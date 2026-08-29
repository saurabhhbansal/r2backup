package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// app is everything a command needs, opened once and closed once.
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
	db, err := index.Open(idxPath)
	if err != nil {
		return nil, err
	}
	return &app{
		dir:   dir,
		sets:  st,
		index: db,
		creds: creds.Open(filepath.Join(dir, "credentials")),
	}, nil
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
			return errors.New("no credentials on this machine yet. Run: r2backup setup")
		}
		return err
	}
	client, err := remote.New(ctx, remote.Config{
		AccountID:       c.AccountID,
		Endpoint:        c.Endpoint,
		Bucket:          c.Bucket,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
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
			return nil, errors.New("nothing is being backed up yet. Run: r2backup add <folder>")
		}
		return all, nil
	}
	s, err := a.sets.Get(name)
	if err != nil {
		return nil, err
	}
	return []sets.Set{s}, nil
}
