package cli

import (
	"context"
	"fmt"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/drime"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// openStore builds the client for whichever storage c is for. resume may be
// nil, for a store only read from.
func openStore(ctx context.Context, c creds.Credentials, resume remote.ResumeStore) (remote.Store, error) {
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
	client, err := remote.New(ctx, remote.Config{
		AccountID:       c.AccountID,
		Endpoint:        c.Endpoint,
		Bucket:          c.Bucket,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		Resume:          resume,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to R2: %w", err)
	}
	return client, nil
}

// providerLabel names c's storage for a person.
func providerLabel(c creds.Credentials) string {
	if c.IsDrime() {
		return "Drime"
	}
	return "Cloudflare R2"
}
