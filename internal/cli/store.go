package cli

import (
	"context"
	"fmt"

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

// openBaseStore builds the client itself.
func openBaseStore(ctx context.Context, c creds.Credentials, resume remote.ResumeStore) (remote.Store, error) {
	if c.IsDrime() {
		client, err := drime.New(ctx, drime.Config{
			Token:       c.DrimeToken,
			WorkspaceID: c.DrimeWorkspace,
			Root:        c.DrimeRoot,
			BaseURL:     c.Endpoint,
			Resume:      resume,
		})
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

// providerLabel names c's storage for a person.
func providerLabel(c creds.Credentials) string { return c.Label() }
