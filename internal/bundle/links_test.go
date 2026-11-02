package bundle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// linkMem is a memStore that takes uploads by link. Its "link" is the key;
// FinishUpload stands in for the relay having sent the bytes, which a test
// puts in pending first.
type linkMem struct {
	*memStore
	pending map[string][]byte
	plans   int
}

func newLinkMem() *linkMem { return &linkMem{memStore: newMem(), pending: map[string][]byte{}} }

func (m *linkMem) UploadLinks(ctx context.Context, in remote.LinkUpload) (*remote.UploadPlan, error) {
	m.plans++
	return &remote.UploadPlan{Key: in.Key, Size: in.Size, Parts: []remote.UploadLink{{URL: "mem://" + in.Key, Length: in.Size}}}, nil
}

func (m *linkMem) FinishUpload(ctx context.Context, plan *remote.UploadPlan, etags []string) error {
	return m.memStore.Put(ctx, remote.PutInput{Key: plan.Key, Body: bytes.NewReader(m.pending[plan.Key]), Size: plan.Size})
}

func (m *linkMem) AbandonUpload(ctx context.Context, plan *remote.UploadPlan) error { return nil }

func (m *linkMem) DownloadLink(ctx context.Context, key string) (*remote.DownloadLink, error) {
	o, err := m.memStore.Head(ctx, key)
	if err != nil {
		return nil, err
	}
	return &remote.DownloadLink{URL: "mem://" + key, Size: o.Size}, nil
}

const scope = "machines/pc/Docs/"

func TestPackingRefusesLinksForSmallFiles(t *testing.T) {
	ctx := context.Background()
	base := newLinkMem()
	s := New(base, Options{Pack: true, PackSize: 1 << 10, SmallMax: 100})
	_, err := s.UploadLinks(ctx, remote.LinkUpload{Key: scope + "current/small", Size: 100})
	if !errors.Is(err, remote.ErrNoLink) {
		t.Fatalf("a file bound for a pack got links: %v", err)
	}
	if base.plans != 0 {
		t.Fatal("asked the base store for links anyway")
	}
}

func TestLinkUploadIsOnTheIndex(t *testing.T) {
	ctx := context.Background()
	base := newLinkMem()
	s := New(base, Options{Pack: true, PackSize: 1 << 10, SmallMax: 100})
	key := scope + "current/big"
	data := bytes.Repeat([]byte("b"), 500)
	// A small file packed alongside, so the backup has an index.
	if err := s.Put(ctx, remote.PutInput{Key: scope + "current/small", Body: bytes.NewReader([]byte("s")), Size: 1}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.UploadLinks(ctx, remote.LinkUpload{Key: key, Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	base.pending[key] = data
	if err := s.FinishUpload(ctx, plan, []string{"e"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// A fresh Store, reading only what is stored, sees both.
	again := New(base, Options{Pack: true, PackSize: 1 << 10, SmallMax: 100})
	list, err := again.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, e := range list {
		sizes[e.Key] = e.Size
	}
	if sizes[key] != 500 || sizes[scope+"current/small"] != 1 {
		t.Fatalf("listing = %v", sizes)
	}
	obj, err := again.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestNotPackingGivesLinksForEverything(t *testing.T) {
	ctx := context.Background()
	base := newLinkMem()
	s := New(base, Options{Pack: false, SmallMax: 100})
	if _, err := s.UploadLinks(ctx, remote.LinkUpload{Key: scope + "current/tiny", Size: 3}); err != nil {
		t.Fatalf("not packing, a small file should get links: %v", err)
	}
}

func TestDownloadLinkRefusedForPackedFile(t *testing.T) {
	ctx := context.Background()
	base := newLinkMem()
	s := New(base, Options{Pack: true, PackSize: 1 << 10, SmallMax: 100})
	if err := s.Put(ctx, remote.PutInput{Key: scope + "current/small", Body: bytes.NewReader([]byte("s")), Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DownloadLink(ctx, scope+"current/small"); !errors.Is(err, remote.ErrNoLink) {
		t.Fatalf("a packed file got a link: %v", err)
	}
	if _, err := s.DownloadLink(ctx, scope+"current/none"); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestLinksNeedABaseThatHasThem(t *testing.T) {
	s := New(newMem(), Options{})
	if _, err := s.UploadLinks(context.Background(), remote.LinkUpload{Key: scope + "current/x", Size: 1 << 30}); !errors.Is(err, remote.ErrNoLink) {
		t.Fatalf("err = %v", err)
	}
}
