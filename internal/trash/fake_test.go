package trash

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// fakeBackend is an in-memory stand-in for Backend. It exists so unit
// tests exercise trash's own logic -- key layout, collision handling,
// retention math, concurrency, cancellation -- without a real bucket,
// while still letting a test assert exactly which operations trash
// issued (e.g. that Move only ever calls Copy, never Get or Put -- which
// this fake, matching Backend, does not even expose a way to call).
type fakeBackend struct {
	mu sync.Mutex

	// objects holds every key currently "in the bucket", keyed by object
	// key, mapped to its fake size.
	objects map[string]int64

	copyCalls        int
	deleteCalls      int
	deleteBatchCalls int
	listCalls        int
	headCalls        int

	// panicOnCurrentDelete, when true, panics if Delete or DeleteBatch is
	// ever asked to remove a key containing "/current/". This is what
	// proves Prune structurally cannot touch the live tree, rather than
	// merely observing that it didn't happen to in one run.
	panicOnCurrentDelete bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: make(map[string]int64)}
}

// seed records an object as already present, as if a prior write had put
// it there -- typically a live object Move is about to trash.
func (f *fakeBackend) seed(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = size
}

func (f *fakeBackend) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeBackend) Copy(ctx context.Context, srcKey, dstKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyCalls++
	size, ok := f.objects[srcKey]
	if !ok {
		return fmt.Errorf("fake: copy: source %q does not exist", srcKey)
	}
	f.objects[dstKey] = size
	return nil
}

func (f *fakeBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.panicOnCurrentDelete && strings.Contains(key, "/current/") {
		panic("fake: Delete called on a live key: " + key)
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeBackend) DeleteBatch(ctx context.Context, keys []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteBatchCalls++
	for _, k := range keys {
		if f.panicOnCurrentDelete && strings.Contains(k, "/current/") {
			panic("fake: DeleteBatch called on a live key: " + k)
		}
	}
	for _, k := range keys {
		delete(f.objects, k)
	}
	return nil
}

func (f *fakeBackend) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	var out []remote.ListEntry
	for k, size := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeBackend) Head(ctx context.Context, key string) (*remote.HeadResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls++
	size, ok := f.objects[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.HeadResult{Size: size}, nil
}
