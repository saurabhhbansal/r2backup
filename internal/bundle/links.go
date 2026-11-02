package bundle

import (
	"context"
	"fmt"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Links (see remote.LinkReader and remote.LinkWriter) go to the base store for
// objects of their own, and are refused for anything a pack holds or would
// hold: a pack is built on this computer, so a file bound for one has to come
// here first. That is Ultra fast mode's hybrid: big files go cloud to cloud,
// small ones are packed here as they always were.

// DownloadLink is the base store's link for an object of its own.
func (s *Store) DownloadLink(ctx context.Context, key string) (*remote.DownloadLink, error) {
	lr, ok := s.base.(remote.LinkReader)
	if !ok {
		return nil, remote.ErrNoLink
	}
	sp, rel, through, err := s.enter(ctx, key)
	if err != nil {
		return nil, err
	}
	if !through {
		e, known := sp.lookup(rel)
		sp.mu.Unlock()
		if !known {
			return nil, fmt.Errorf("bundle: link %q: %w", key, remote.ErrNotFound)
		}
		if e.packed() {
			return nil, remote.ErrNoLink
		}
	}
	return lr.DownloadLink(ctx, key)
}

// UploadLinks is the base store's, for a file that will be an object of its
// own: every file when this Store is not packing, and otherwise those over
// SmallMax.
func (s *Store) UploadLinks(ctx context.Context, in remote.LinkUpload) (*remote.UploadPlan, error) {
	lw, ok := s.base.(remote.LinkWriter)
	if !ok {
		return nil, remote.ErrNoLink
	}
	sp, _, through, err := s.enter(ctx, in.Key)
	if err != nil {
		return nil, err
	}
	if !through {
		sp.mu.Unlock()
		if s.opts.Pack && in.Size <= s.opts.SmallMax {
			return nil, remote.ErrNoLink
		}
	}
	return lw.UploadLinks(ctx, in)
}

// FinishUpload finishes the upload on the base store, then records the new
// object on the index as Put does for one sent directly.
func (s *Store) FinishUpload(ctx context.Context, plan *remote.UploadPlan, etags []string) error {
	lw, ok := s.base.(remote.LinkWriter)
	if !ok {
		return remote.ErrNoLink
	}
	if err := lw.FinishUpload(ctx, plan, etags); err != nil {
		return err
	}
	s.writes.Add(int64(len(plan.Parts)))
	sp, rel, through, err := s.enter(ctx, plan.Key)
	if err != nil || through {
		return err
	}
	defer sp.mu.Unlock()
	sp.remove(rel)
	sp.set(rel, entry{Size: plan.Size, Written: s.now()})
	return nil
}

// AbandonUpload is the base store's.
func (s *Store) AbandonUpload(ctx context.Context, plan *remote.UploadPlan) error {
	lw, ok := s.base.(remote.LinkWriter)
	if !ok {
		return remote.ErrNoLink
	}
	return lw.AbandonUpload(ctx, plan)
}

var (
	_ remote.LinkReader = (*Store)(nil)
	_ remote.LinkWriter = (*Store)(nil)
)
