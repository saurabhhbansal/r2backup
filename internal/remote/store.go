package remote

import (
	"context"
	"errors"
)

// Store is everything r2backup asks of the place backups are kept.
//
// It is the R2 client's own method set, written down as an interface so a
// second kind of storage can stand in for it. The shape is S3's -- flat
// "/"-separated keys, prefix listing, server-side copy -- because that is
// the model every caller was built against, and a store that is not S3
// underneath (Drime, a folder tree addressed by ids) does the translating
// itself rather than every caller learning a second model.
//
// *Client satisfies it unchanged; see the assertion at the bottom.
type Store interface {
	// Put writes one object, in parts where it is large enough to need them.
	Put(ctx context.Context, in PutInput) error
	// Get streams an object and the metadata a restore needs. A missing key
	// is an error wrapping ErrNotFound.
	Get(ctx context.Context, key string) (*Object, error)
	// Head is Get without the body.
	Head(ctx context.Context, key string) (*HeadResult, error)

	// List returns every object under prefix, recursively.
	List(ctx context.Context, prefix string) ([]ListEntry, error)
	// ListPrefixes returns the immediate child "folders" under prefix,
	// trimmed of the prefix and the trailing "/".
	ListPrefixes(ctx context.Context, prefix string) ([]string, error)

	// Copy duplicates srcKey to dstKey on the server, never through this
	// machine.
	Copy(ctx context.Context, srcKey, dstKey string) error
	Delete(ctx context.Context, key string) error
	DeleteBatch(ctx context.Context, keys []string) error
	DeletePrefix(ctx context.Context, prefix string) (PrefixDeletion, error)

	// AbandonStaleUploads gives up on unfinished large uploads older than
	// ResumeMaxAge.
	AbandonStaleUploads(ctx context.Context) (int, error)
	// ListMultipartUploads reports uploads the store itself says are open
	// under prefix -- as far as it can say. A store that cannot list them
	// returns what it knows from the local record instead.
	ListMultipartUploads(ctx context.Context, prefix string) ([]MultipartUpload, error)
	AbortUpload(ctx context.Context, key, uploadID string) error

	// Bucket names where this store points, for showing to a person.
	Bucket() string
}

// Mover is a Store that can move an object on the server more cheaply than
// copying it and deleting the original. Drime's copy duplicates the file's
// content and takes seconds a file; its move only changes which folder the
// file is in. R2 has no move, so it does not implement this, and callers fall
// back to Copy.
type Mover interface {
	// Move puts srcKey's object at dstKey, replacing anything there, and
	// srcKey no longer exists afterwards.
	Move(ctx context.Context, srcKey, dstKey string) error
}

// Flusher is a Store that holds writes back and needs telling when a run is
// over. Drime keeps file dates and permissions in one manifest per set
// rather than on each file, and Flush is when that manifest is written.
//
// A Store that writes everything as it goes -- R2 -- does not implement it,
// and callers check with a type assertion: see FlushStore.
type Flusher interface {
	Flush(ctx context.Context) error
}

// FlushStore flushes s if it holds anything back, and is a no-op otherwise.
func FlushStore(ctx context.Context, s Store) error {
	if f, ok := s.(Flusher); ok {
		return f.Flush(ctx)
	}
	return nil
}

// Checkpointer is a Flusher that can put what it holds back on record in the
// middle of a run, without letting go of what it has read the way Flush does.
//
// A run uses it to note finished files in the local index as it goes, so a
// backup cut off by a crash, a kill or a shutdown keeps what it finished. A
// file is only noted once the store has it on record, and for a store that
// holds writes back (bundling's packs and index, Drime's manifest of dates)
// that is after a checkpoint.
type Checkpointer interface {
	// Pending reports whether anything a write has returned for is not on
	// record yet.
	Pending() bool
	// Checkpoint puts everything written so far on record.
	Checkpoint(ctx context.Context) error
}

// ErrNoCheckpoint is what a Checkpointer wrapping another store returns when
// the store underneath cannot save mid-run. Nothing went wrong: what was
// written is on record once the run's final flush is done.
var ErrNoCheckpoint = errors.New("remote: this storage saves only at the end of a run")

// PendingStore reports whether s holds back anything already written. A
// Flusher that cannot say is assumed to.
func PendingStore(s Store) bool {
	if c, ok := s.(Checkpointer); ok {
		return c.Pending()
	}
	_, holds := s.(Flusher)
	return holds
}

// CheckpointStore puts what s holds back on record mid-run. ok is false for
// a store that holds writes back but cannot do that safely mid-run: its
// writes are on record only once the run's final flush is done.
func CheckpointStore(ctx context.Context, s Store) (ok bool, err error) {
	if c, is := s.(Checkpointer); is {
		if err := c.Checkpoint(ctx); err != nil {
			if errors.Is(err, ErrNoCheckpoint) {
				return false, nil
			}
			return true, err
		}
		return true, nil
	}
	if _, holds := s.(Flusher); holds {
		return false, nil
	}
	return true, nil
}

// EndpointOf is the URL s sends its requests to, or "" when it cannot say. A
// store wrapping another (bundling) answers for the one underneath. It is
// what a run checks to know whether the network has come back.
func EndpointOf(s Store) string {
	for i := 0; s != nil && i < 8; i++ {
		if e, ok := s.(interface{ Endpoint() string }); ok {
			return e.Endpoint()
		}
		b, ok := s.(interface{ Base() Store })
		if !ok {
			return ""
		}
		s = b.Base()
	}
	return ""
}

// ThrottlesOf is s's count of slow-down answers from storage, or nil when s
// does not keep one. A store wrapping another answers for the one
// underneath. The engine reads it to send fewer files at once.
func ThrottlesOf(s Store) func() int64 {
	for i := 0; s != nil && i < 8; i++ {
		if t, ok := s.(interface{ Throttles() int64 }); ok {
			return t.Throttles
		}
		b, ok := s.(interface{ Base() Store })
		if !ok {
			return nil
		}
		s = b.Base()
	}
	return nil
}

// CloseStore is for a Store that is done with: it saves anything held back,
// then lets go of what the store keeps on disk (a bundling store's cached
// packs are temporary files that otherwise stay behind). It is safe on any
// Store; one with nothing to save or let go of does nothing.
func CloseStore(ctx context.Context, s Store) error {
	if s == nil {
		return nil
	}
	err := FlushStore(ctx, s)
	if c, ok := s.(interface{ Close() }); ok {
		c.Close()
	}
	return err
}

// Provider names which kind of storage a Store is.
type Provider string

const (
	ProviderR2     Provider = "r2"
	ProviderDrime  Provider = "drime"
	ProviderB2     Provider = "b2"
	ProviderS3     Provider = "s3"
	ProviderCustom Provider = "s3-custom"
)

// Label is the provider's name as a person would write it.
func (p Provider) Label() string {
	switch p {
	case ProviderDrime:
		return "Drime"
	case ProviderB2:
		return "Backblaze B2"
	case ProviderS3:
		return "Amazon S3"
	case ProviderCustom:
		return "S3-compatible storage"
	default:
		return "Cloudflare R2"
	}
}

// ParseProvider reads a provider name typed on a command line, accepting the
// few spellings a person is likely to use.
func ParseProvider(s string) (Provider, bool) {
	switch s {
	case "r2", "R2", "cloudflare", "Cloudflare", "cf":
		return ProviderR2, true
	case "drime", "Drime", "DRIME":
		return ProviderDrime, true
	case "b2", "B2", "backblaze", "Backblaze":
		return ProviderB2, true
	case "s3", "S3", "aws", "AWS", "amazon":
		return ProviderS3, true
	case "s3-custom", "custom", "s3-compatible", "minio":
		return ProviderCustom, true
	}
	return "", false
}

var _ Store = (*Client)(nil)
