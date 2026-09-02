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

// forgetClient discards the cached client, if any, so the next call to
// connect rebuilds one from whatever credentials are on disk right now
// rather than handing back the client connect built the last time it ran.
// connect treats a.client != nil as proof the credentials it was built from
// still work, which is only true until something changes those credentials
// out from under it -- SaveKeys and UnlockVault do exactly that, and without
// this a client built from a mistyped secret key would keep answering for
// the account, rejecting every later attempt with the same
// SignatureDoesNotMatch the first one produced, until the process restarted.
//
// a.client is shared with the dashboard's worker goroutines (see the
// dashboard struct's comment in dashboard.go, which names connect as the one
// piece of app state that needs a lock), so a caller reachable from more
// than one goroutine must take that same lock before calling this -- see
// dashboard.forgetClient, which is the only caller that should reach here
// from the dashboard.
func (a *app) forgetClient() {
	a.client = nil
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

// abortSetUploads aborts, on the server, every unfinished multipart upload
// that removing a set would otherwise orphan, and reports how many it got
// to and how many it did not.
//
// It is shared by `r2b remove` and the interface's own Remove, which is why
// getting a connected client -- something the two callers already do
// differently, and for their own reasons -- comes in as a callback rather
// than this calling a.connect itself.
//
// It never returns an error. A file that never finished uploading is not
// part of anyone's backup -- there is no completed object for it, in any
// listing, purge or no purge -- so it is always safe to stop billing for it,
// and aborting it is not the destructive half of removal that --purge gates.
// But the abort itself can still fail on a bad connection, and that must not
// also fail the removal: dropping the index is the one action that makes an
// upload unreachable forever (see DropSetUploads), so a caller failing here
// would leave the set stuck half-removed with no way to retry, over a
// problem that has nothing to do with whether the set itself can go. What
// failed is counted instead, for the caller to tell the user about.
//
// full additionally asks the bucket itself what it has open under the
// prefix -- catching an upload the index never recorded, because a crash
// landed between CreateMultipartUpload and the first save of it -- which
// only --purge pays the extra request for; the default remove is already
// not guaranteed to see everything a lost record would have caught, and
// --purge is the one place already committed to enumerating the bucket.
func (a *app) abortSetUploads(ctx context.Context, idx *index.DB, s sets.Set, full bool, ensureConnected func() error) (aborted, failed int) {
	pending, err := idx.PendingUploadsUnderPrefix(s.KeyScope())
	if err != nil || (len(pending) == 0 && !full) {
		return 0, 0
	}
	if err := ensureConnected(); err != nil {
		// No connection, no abort. DropSetUploads runs regardless -- see the
		// callers -- so the local records go either way; only whether the
		// server side is reached depends on this.
		return 0, len(pending)
	}
	seen := make(map[string]bool, len(pending))
	for _, u := range pending {
		seen[u.Key+"\x00"+u.UploadID] = true
		if a.client.AbortUpload(ctx, u.Key, u.UploadID) != nil {
			failed++
			continue
		}
		aborted++
	}
	if !full {
		return aborted, failed
	}
	live, err := a.client.ListMultipartUploads(ctx, s.KeyScope())
	if err != nil {
		return aborted, failed
	}
	for _, u := range live {
		if seen[u.Key+"\x00"+u.UploadID] {
			continue // already aborted above
		}
		if a.client.AbortUpload(ctx, u.Key, u.UploadID) != nil {
			failed++
			continue
		}
		aborted++
	}
	return aborted, failed
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
