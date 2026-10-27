package remote

import "context"

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

// Provider names which kind of storage a Store is.
type Provider string

const (
	ProviderR2    Provider = "r2"
	ProviderDrime Provider = "drime"
)

// Label is the provider's name as a person would write it.
func (p Provider) Label() string {
	switch p {
	case ProviderDrime:
		return "Drime"
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
	}
	return "", false
}

var _ Store = (*Client)(nil)
