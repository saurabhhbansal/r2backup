package ultrafast

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/relay"
)

// partsAtOnce is how many parts of one file the Worker copies at a time.
const partsAtOnce = 4

// linkMargin is how long a download link must still work for a part to be
// started with it; one closer to expiring is asked for again.
const linkMargin = 10 * time.Minute

// Copier is a mirror.Relay: it copies files through the Worker.
type Copier struct {
	// MinSize is the smallest file offered to the Worker. A destination
	// that packs small files sets it above the largest file a pack takes,
	// so those come to this computer to be packed without a wasted
	// request.
	MinSize int64
	// PartSize is how big each part of a big file is; zero is
	// remote.DefaultLinkPartSize.
	PartSize int64

	rc    *relayClient
	store *Store
	state State
	opts  Options

	once     sync.Once
	readyErr error
}

// NewCopier is a Copier for the relay in st. store, when set, is where an
// upgraded relay's version is recorded.
func NewCopier(st State, store *Store, o Options) *Copier {
	if o.RelayURL != "" {
		st.URL = o.RelayURL
	}
	return &Copier{MinSize: 1, rc: newRelayClient(st.URL, st.Secret, o.RelayHTTP), store: store, state: st, opts: o}
}

// ready checks, once a run, that the Worker answers and is the relay this
// binary carries, putting the new one up when it is not.
func (c *Copier) ready(ctx context.Context) error {
	c.once.Do(func() {
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		v, err := c.rc.health(hctx)
		if err != nil {
			c.readyErr = asStop(err)
			return
		}
		if v == relay.Version {
			return
		}
		if _, err := upgrade(ctx, c.store, c.state, c.opts); err != nil {
			c.readyErr = stopped("the Worker is out of date and could not be replaced: %v", err)
			return
		}
		if v, err = c.rc.health(hctx); err != nil || v != relay.Version {
			c.readyErr = stopped("the Worker was replaced but does not answer as the new one yet")
		}
	})
	return c.readyErr
}

// asStop makes any error stop the relay for the run: what fails a health
// check would fail every file.
func asStop(err error) error {
	if errors.Is(err, mirror.ErrRelayStopped) {
		return err
	}
	return stopped("the Worker does not answer (%v)", err)
}

// flatHeader is the one-value-per-name form the relay takes.
func flatHeader(h map[string][]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

// Copy copies key from src to dst through the Worker.
func (c *Copier) Copy(ctx context.Context, src, dst remote.Store, key string, size int64, onBytes func(int64)) error {
	lr, okR := src.(remote.LinkReader)
	lw, okW := dst.(remote.LinkWriter)
	if !okR || !okW || size < c.MinSize || size < 1 {
		return remote.ErrNoLink
	}
	if err := c.ready(ctx); err != nil {
		return err
	}
	link, err := lr.DownloadLink(ctx, key)
	if err != nil {
		return err
	}
	if link.Size != size {
		return fmt.Errorf("%s changed in Drime while being copied (%d bytes, listed as %d)", key, link.Size, size)
	}
	plan, err := lw.UploadLinks(ctx, remote.LinkUpload{
		Key: key, Size: size, Metadata: link.Metadata, ContentType: link.ContentType, PartSize: c.PartSize,
	})
	if err != nil {
		return err
	}
	etags, err := c.sendParts(ctx, lr, key, link, plan, onBytes)
	if err != nil {
		_ = lw.AbandonUpload(context.WithoutCancel(ctx), plan)
		return err
	}
	if err := lw.FinishUpload(ctx, plan, etags); err != nil {
		_ = lw.AbandonUpload(context.WithoutCancel(ctx), plan)
		return err
	}
	return nil
}

// sendParts has the Worker copy every part of plan, a few at a time.
func (c *Copier) sendParts(ctx context.Context, lr remote.LinkReader, key string, link *remote.DownloadLink,
	plan *remote.UploadPlan, onBytes func(int64)) ([]string, error) {
	var (
		mu    sync.Mutex
		cur   = link
		first error
	)
	// fresh is a download link good for long enough to start a part with.
	fresh := func() (*remote.DownloadLink, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Until(cur.Expires) > linkMargin {
			return cur, nil
		}
		l, err := lr.DownloadLink(ctx, key)
		if err != nil {
			return nil, err
		}
		cur = l
		return cur, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	etags := make([]string, len(plan.Parts))
	sem := make(chan struct{}, partsAtOnce)
	var wg sync.WaitGroup
	for i, part := range plan.Parts {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, part remote.UploadLink) {
			defer wg.Done()
			defer func() { <-sem }()
			etag, err := c.sendPart(ctx, fresh, plan, part)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			etags[i] = etag
			onBytes(part.Length)
		}(i, part)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return etags, nil
}

// sendPart copies one part, trying a second time when the first fails for a
// reason other than the relay being finished for the run.
func (c *Copier) sendPart(ctx context.Context, fresh func() (*remote.DownloadLink, error), plan *remote.UploadPlan, part remote.UploadLink) (string, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var l *remote.DownloadLink
		if l, err = fresh(); err != nil {
			return "", err
		}
		req := copyRequest{
			Src:    copySource{URL: l.URL},
			Dst:    copyDest{URL: part.URL, Headers: flatHeader(part.Header)},
			Length: part.Length,
		}
		if len(plan.Parts) > 1 {
			start, end := part.Offset, part.Offset+part.Length-1
			req.Src.Start, req.Src.End = &start, &end
		}
		var etag string
		etag, err = c.rc.copy(ctx, req)
		if err == nil {
			return etag, nil
		}
		if ctx.Err() != nil || errors.Is(err, mirror.ErrRelayStopped) {
			return "", err
		}
	}
	return "", err
}

var _ mirror.Relay = (*Copier)(nil)
