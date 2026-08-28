// Package trash keeps a safety net for a mirrored set: before a live
// object is overwritten or deleted, it is moved aside into
// <prefix>/trash/<date>/... with a server-side copy, so an accidental
// delete or a bad sync stays recoverable for as long as the set's
// retention window (sets.Set.RetentionDays) says it should, instead of
// being gone the moment the mirror changes.
//
// The bucket stays human-browsable on purpose: <prefix>/current/ is the
// live mirror and <prefix>/trash/<YYYY-MM-DD>/ is what was moved aside on
// that day, so a person poking around in R2's own console can find either
// without any tooling from this package.
package trash

import (
	"context"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Backend is what trash needs from the remote store: the same five
// operations remote.Client exposes, restated as an interface here rather
// than depended on as *remote.Client directly, so unit tests run against
// an in-process fake -- fast, deterministic, able to assert exactly which
// operations trash issued -- while integration tests still exercise the
// genuine implementation through test/minio.
type Backend interface {
	// Copy duplicates srcKey to dstKey with a single server-side
	// operation. Move and Restore both depend on this being a real
	// CopyObject and never a Get followed by a Put -- a
	// download-then-reupload would pull every trashed or restored byte
	// across the network for no reason, which is the entire cost this
	// package exists to avoid.
	Copy(ctx context.Context, srcKey, dstKey string) error
	Delete(ctx context.Context, key string) error
	DeleteBatch(ctx context.Context, keys []string) error
	List(ctx context.Context, prefix string) ([]remote.ListEntry, error)
	Head(ctx context.Context, key string) (*remote.HeadResult, error)
}

// Clock supplies the current time. Move stamps a trashed object with the
// day named by Now, and Prune measures age against it, so a retention
// test can pin "now" to an exact instant instead of depending on the real
// wall clock.
type Clock struct {
	// Now, if set, is called for the current time. The zero Clock falls
	// back to time.Now, so production code can pass Clock{}.
	Now func() time.Time
}

func (c Clock) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Trash moves a set's objects into, lists, restores from, and prunes its
// trash tree. It holds no mutable state of its own -- every call is
// independent of every other -- so one value can be shared across
// concurrent sets and goroutines freely.
type Trash struct {
	backend Backend
	clock   Clock
}

// New builds a Trash backed by backend, using clock for "now". Pass
// Clock{} to use the real wall clock.
func New(backend Backend, clock Clock) *Trash {
	return &Trash{backend: backend, clock: clock}
}
