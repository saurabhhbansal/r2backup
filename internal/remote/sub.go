package remote

import (
	"context"
	"io"
	"strings"
)

// Sub is a view of a Store with every key under prefix: Sub(s, "a/b/") reads
// and writes "x" as "a/b/x", and lists keys with "a/b/" taken off. It is how
// a folder from one store is copied into a chosen place in another.
func Sub(s Store, prefix string) Store {
	if prefix == "" {
		return s
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &sub{s: s, p: prefix}
}

type sub struct {
	s Store
	p string
}

func (v *sub) k(key string) string { return v.p + key }

func (v *sub) trim(list []ListEntry) []ListEntry {
	out := make([]ListEntry, 0, len(list))
	for _, e := range list {
		if strings.HasPrefix(e.Key, v.p) {
			e.Key = e.Key[len(v.p):]
			out = append(out, e)
		}
	}
	return out
}

func (v *sub) Put(ctx context.Context, in PutInput) error {
	in.Key = v.k(in.Key)
	return v.s.Put(ctx, in)
}

func (v *sub) Get(ctx context.Context, key string) (*Object, error) { return v.s.Get(ctx, v.k(key)) }

func (v *sub) Head(ctx context.Context, key string) (*HeadResult, error) {
	return v.s.Head(ctx, v.k(key))
}

func (v *sub) List(ctx context.Context, prefix string) ([]ListEntry, error) {
	list, err := v.s.List(ctx, v.k(prefix))
	if err != nil {
		return nil, err
	}
	return v.trim(list), nil
}

func (v *sub) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	return v.s.ListPrefixes(ctx, v.k(prefix))
}

func (v *sub) Copy(ctx context.Context, src, dst string) error {
	return v.s.Copy(ctx, v.k(src), v.k(dst))
}

func (v *sub) Move(ctx context.Context, src, dst string) error {
	if mv, ok := v.s.(Mover); ok {
		return mv.Move(ctx, v.k(src), v.k(dst))
	}
	if err := v.s.Copy(ctx, v.k(src), v.k(dst)); err != nil {
		return err
	}
	return v.s.Delete(ctx, v.k(src))
}

func (v *sub) Delete(ctx context.Context, key string) error { return v.s.Delete(ctx, v.k(key)) }

func (v *sub) DeleteBatch(ctx context.Context, keys []string) error {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = v.k(k)
	}
	return v.s.DeleteBatch(ctx, full)
}

func (v *sub) DeletePrefix(ctx context.Context, prefix string) (PrefixDeletion, error) {
	return v.s.DeletePrefix(ctx, v.k(prefix))
}

func (v *sub) AbandonStaleUploads(ctx context.Context) (int, error) {
	return v.s.AbandonStaleUploads(ctx)
}

func (v *sub) ListMultipartUploads(ctx context.Context, prefix string) ([]MultipartUpload, error) {
	return v.s.ListMultipartUploads(ctx, v.k(prefix))
}

func (v *sub) AbortUpload(ctx context.Context, key, uploadID string) error {
	return v.s.AbortUpload(ctx, key, uploadID)
}

func (v *sub) Bucket() string { return v.s.Bucket() }

func (v *sub) Flush(ctx context.Context) error { return FlushStore(ctx, v.s) }

func (v *sub) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if rr, ok := v.s.(RangeReader); ok {
		return rr.GetRange(ctx, v.k(key), off, n)
	}
	obj, err := v.s.Get(ctx, v.k(key))
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, obj.Body, off); err != nil {
		obj.Body.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(obj.Body, n), obj.Body}, nil
}

var (
	_ Store       = (*sub)(nil)
	_ Mover       = (*sub)(nil)
	_ Flusher     = (*sub)(nil)
	_ RangeReader = (*sub)(nil)
)
