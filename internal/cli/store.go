package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/drime"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// openStore builds the client for whichever storage c is for, wrapped so
// bundled backups read and write correctly (see internal/bundle). c.Bundle
// decides whether new writes are bundled; backups bundled before are read
// either way, and one never bundled passes straight through. resume may be
// nil, for a store only read from.
func openStore(ctx context.Context, c creds.Credentials, resume remote.ResumeStore) (remote.Store, error) {
	base, err := openBaseStore(ctx, c, resume)
	if err != nil {
		return nil, err
	}
	return bundle.New(base, bundle.Options{Pack: c.Bundle}), nil
}

// baseStore is the store under any bundling, for what only it can answer
// (Drime's space usage, say).
func baseStore(s remote.Store) remote.Store {
	if b, ok := s.(*bundle.Store); ok {
		return b.Base()
	}
	return s
}

// testStore lets tests hand back an in-memory store for a set of
// credentials. It is nil outside tests.
var testStore func(creds.Credentials) (remote.Store, bool)

// openBaseStore builds the client itself.
func openBaseStore(ctx context.Context, c creds.Credentials, resume remote.ResumeStore) (remote.Store, error) {
	return openBase(ctx, c, resume, false)
}

// openExistingDrime opens a Drime folder only if it is already there: a
// folder to read from, never one to create from a typo.
func openExistingDrime(ctx context.Context, c creds.Credentials) (remote.Store, error) {
	return openBase(ctx, c, nil, true)
}

func openBase(ctx context.Context, c creds.Credentials, resume remote.ResumeStore, existing bool) (remote.Store, error) {
	if testStore != nil {
		if s, ok := testStore(c); ok {
			return s, nil
		}
	}
	if c.IsDrime() {
		client, err := drime.New(ctx, drime.Config{
			Token:       c.DrimeToken,
			WorkspaceID: c.DrimeWorkspace,
			Root:        c.DrimeRoot,
			Existing:    existing,
			BaseURL:     c.Endpoint,
			Resume:      resume,
		})
		if errors.Is(err, drime.ErrFolderNotFound) {
			return nil, fmt.Errorf("there is no folder %q in Drime. Check the name, including capitals", c.DrimeRoot)
		}
		if err != nil {
			return nil, fmt.Errorf("connect to Drime: %w", err)
		}
		return client, nil
	}
	ep := c.S3()
	client, err := remote.New(ctx, remote.Config{
		Endpoint:        ep.URL,
		Region:          ep.Region,
		VirtualHost:     ep.VirtualHost,
		Bucket:          c.Bucket,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		Resume:          resume,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.Label(), err)
	}
	return client, nil
}

// closeStores closes every store given except keep (the app's own client,
// which app.close closes). See remote.CloseStore.
func closeStores(keep remote.Store, stores ...remote.Store) {
	for _, s := range stores {
		if s == nil || s == keep {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_ = remote.CloseStore(ctx, s)
		cancel()
	}
}

// providerLabel names c's storage for a person.
func providerLabel(c creds.Credentials) string { return c.Label() }
