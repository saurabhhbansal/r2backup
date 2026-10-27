package drime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// DefaultRoot is the folder, at the top of the Drime drive, that everything
// r2backup writes goes under.
const DefaultRoot = "r2backup"

// Config configures a Client.
type Config struct {
	// Token is a Drime API token, made in the web app under
	// Settings > Developer.
	Token string
	// WorkspaceID picks a shared workspace. Empty is the personal drive.
	WorkspaceID string
	// Root is the top-level folder backups go in. Empty is DefaultRoot.
	Root string

	// BaseURL overrides DefaultBaseURL. Tests point it at a fake server.
	BaseURL    string
	HTTPClient *http.Client

	// Resume is where unfinished large uploads are recorded, as for R2.
	Resume remote.ResumeStore

	// Threshold and PartSize override the multipart defaults. Zero means
	// the default.
	Threshold int64
	PartSize  int64

	// MaxAttempts overrides how many times a request is tried. Zero is six.
	MaxAttempts int
}

// Client is a remote.Store backed by a Drime drive.
type Client struct {
	api       *api
	root      string
	tree      *tree
	meta      *manifests
	resume    remote.ResumeStore
	threshold int64
	partSize  int64
}

var (
	_ remote.Store   = (*Client)(nil)
	_ remote.Flusher = (*Client)(nil)
	_ remote.Mover   = (*Client)(nil)
)

// New connects to Drime and finds (or makes) the backup root folder. It makes
// network calls, so a bad token fails here rather than at the first upload.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("drime: an API token is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 0} // per-request deadlines come from ctx
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = 6
	}
	root := strings.Trim(cfg.Root, "/")
	if root == "" {
		root = DefaultRoot
	}
	c := &Client{
		api:       &api{base: base, token: strings.TrimSpace(cfg.Token), workspace: cfg.WorkspaceID, hc: hc, maxAttempts: attempts},
		root:      root,
		resume:    cfg.Resume,
		threshold: cfg.Threshold,
		partSize:  cfg.PartSize,
	}
	if c.threshold <= 0 {
		c.threshold = defaultThreshold
	}
	if c.partSize <= 0 {
		c.partSize = defaultPartSize
	}
	c.meta = newManifests(c)

	rootID, err := c.findRoot(ctx)
	if err != nil {
		return nil, err
	}
	c.tree = newTree(c.api, rootID)
	return c, nil
}

// findRoot walks Config.Root from the top of the drive, creating what is
// missing. Root may be nested ("Backups/r2backup").
func (c *Client) findRoot(ctx context.Context) (string, error) {
	parent := ""
	for _, name := range strings.Split(c.root, "/") {
		items, err := c.api.listFolder(ctx, parent, true)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
				return "", errors.New("drime: the API token was refused. Make a new one in Drime under Settings > Developer")
			}
			return "", err
		}
		id := ""
		for _, it := range items {
			if decodeName(it.Name) == name {
				id = it.ID.String()
				break
			}
		}
		if id == "" {
			f, err := c.api.createFolder(ctx, parent, encodeName(name))
			if err != nil {
				return "", err
			}
			id = f.ID.String()
		}
		parent = id
	}
	return parent, nil
}

// Bucket names the place backups go, for showing to a person.
func (c *Client) Bucket() string { return "Drime: " + c.root }

// SpaceUsage reports how full the account is.
func (c *Client) SpaceUsage(ctx context.Context) (SpaceUsage, error) {
	return c.api.spaceUsage(ctx)
}

// isManifest reports whether key is a set's manifest, which is bookkeeping
// rather than backed-up data and so never listed.
func isManifest(key string) bool {
	_, rel, ok := setScope(key)
	return ok && rel == ManifestName
}

func notFound(op, key string) error {
	return fmt.Errorf("drime: %s %q: %w", op, key, remote.ErrNotFound)
}

// metadataFor returns key's recorded metadata, or what Drime itself knows
// when nothing was recorded: the upload time and the size.
func (c *Client) metadataFor(ctx context.Context, key string, it item) (remote.Metadata, error) {
	md, ok, err := c.meta.lookup(ctx, key)
	if err != nil {
		return remote.Metadata{}, err
	}
	if ok {
		return md, nil
	}
	return remote.Metadata{ModTime: it.UpdatedAt, Mode: 0o644, Size: it.FileSize, Kind: remote.KindFile}, nil
}

// Put uploads one file, replacing whatever was stored at key.
//
// Drime will not overwrite: uploading a name that exists gives a second file
// beside it. So the new copy goes up first and the old one is deleted only
// once it has landed -- an interruption between the two leaves two copies,
// which lookup resolves to the newer, rather than none.
func (c *Client) Put(ctx context.Context, in remote.PutInput) error {
	if in.Key == "" || strings.HasSuffix(in.Key, "/") {
		return fmt.Errorf("drime: put: bad key %q", in.Key)
	}
	if in.Size < 0 {
		return fmt.Errorf("drime: put %q: negative size %d", in.Key, in.Size)
	}
	dp, leaf := path.Split(in.Key)
	d, err := c.tree.resolve(ctx, dp, true)
	if err != nil {
		return fmt.Errorf("drime: put %q: %w", in.Key, err)
	}
	old, err := c.tree.all(ctx, in.Key)
	if err != nil {
		return fmt.Errorf("drime: put %q: %w", in.Key, err)
	}

	var it item
	if in.Size > c.threshold {
		it, err = c.uploadLarge(ctx, in.Key, d.id, leaf, in)
	} else {
		it, err = c.uploadSmall(ctx, d.id, leaf, in.Body, in.Size, in.ContentType, in.Progress)
	}
	if err != nil {
		return err
	}

	if err := c.replaceWith(ctx, in.Key, leaf, it, old); err != nil {
		return err
	}

	md := in.Metadata
	md.Size = in.Size
	if scope, err := c.meta.set(ctx, in.Key, md); err != nil {
		return err
	} else if scope != "" {
		// A long run saves its dates every few minutes, so an interruption
		// loses at most that much. A failure here is not the upload's
		// failure; the final flush tries again.
		_ = c.meta.flush(ctx, scope)
	}
	return nil
}

// replaceWith makes it the only entry at key: the old entries go, and if Drime
// renamed the new one to dodge them ("name (1)") it gets its name back.
func (c *Client) replaceWith(ctx context.Context, key, leaf string, it item, old []item) error {
	if len(old) > 0 {
		ids := make([]string, 0, len(old))
		gone := map[string]bool{}
		for _, o := range old {
			if o.ID != it.ID {
				ids = append(ids, o.ID.String())
				gone[o.ID.String()] = true
			}
		}
		if err := c.api.deleteEntries(ctx, ids, true); err != nil && !isNotFound(err) {
			return fmt.Errorf("drime: replace %q: remove old copy: %w", key, err)
		}
		c.tree.forgetIDs(gone)
	}
	if decodeName(it.Name) != leaf {
		renamed, err := c.api.rename(ctx, it.ID.String(), encodeName(leaf))
		if err != nil {
			return fmt.Errorf("drime: replace %q: rename: %w", key, err)
		}
		if renamed.ID != "" {
			it = renamed
		} else {
			it.Name = encodeName(leaf)
		}
	}
	c.tree.put(key, it)
	return nil
}

// Get streams key's content with its metadata.
func (c *Client) Get(ctx context.Context, key string) (*remote.Object, error) {
	it, ok, err := c.tree.lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("drime: get %q: %w", key, err)
	}
	if !ok {
		return nil, notFound("get", key)
	}
	md, err := c.metadataFor(ctx, key, it)
	if err != nil {
		return nil, fmt.Errorf("drime: get %q: %w", key, err)
	}
	resp, err := c.api.download(ctx, it)
	if err != nil {
		if isNotFound(err) {
			return nil, notFound("get", key)
		}
		return nil, fmt.Errorf("drime: get %q: %w", key, err)
	}
	size := it.FileSize
	if resp.ContentLength >= 0 {
		size = resp.ContentLength
	}
	return &remote.Object{
		Body:        resp.Body,
		Metadata:    md,
		Size:        size,
		ContentType: it.Mime,
	}, nil
}

// Head is Get without the body.
func (c *Client) Head(ctx context.Context, key string) (*remote.HeadResult, error) {
	it, ok, err := c.tree.lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("drime: head %q: %w", key, err)
	}
	if !ok {
		return nil, notFound("head", key)
	}
	md, err := c.metadataFor(ctx, key, it)
	if err != nil {
		return nil, fmt.Errorf("drime: head %q: %w", key, err)
	}
	return &remote.HeadResult{Size: it.FileSize, LastModified: it.UpdatedAt, Metadata: md}, nil
}

// List returns every file under prefix. The prefix is matched as bytes, as S3
// does, but only the folder it names is walked.
func (c *Client) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	start := prefix
	if !strings.HasSuffix(start, "/") {
		start = path.Dir(start)
		if start == "." {
			start = ""
		}
	}
	files, err := c.tree.walk(ctx, start)
	if err != nil {
		return nil, fmt.Errorf("drime: list %q: %w", prefix, err)
	}
	out := make([]remote.ListEntry, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f.key, prefix) || isManifest(f.key) {
			continue
		}
		out = append(out, remote.ListEntry{Key: f.key, Size: f.it.FileSize, ModTime: f.it.UpdatedAt})
	}
	return out, nil
}

// ListPrefixes returns the folders directly under prefix.
func (c *Client) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	names, err := c.tree.folders(ctx, strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, fmt.Errorf("drime: list %q: %w", prefix, err)
	}
	return names, nil
}

// Copy duplicates srcKey to dstKey on Drime's side.
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string) error {
	src, ok, err := c.tree.lookup(ctx, srcKey)
	if err != nil {
		return fmt.Errorf("drime: copy %q: %w", srcKey, err)
	}
	if !ok {
		return notFound("copy", srcKey)
	}
	dp, leaf := path.Split(dstKey)
	d, err := c.tree.resolve(ctx, dp, true)
	if err != nil {
		return fmt.Errorf("drime: copy to %q: %w", dstKey, err)
	}
	old, err := c.tree.all(ctx, dstKey)
	if err != nil {
		return fmt.Errorf("drime: copy to %q: %w", dstKey, err)
	}
	dup, err := c.api.duplicate(ctx, src.ID.String(), d.id)
	if err != nil {
		if isNotFound(err) {
			return notFound("copy", srcKey)
		}
		return fmt.Errorf("drime: copy %q to %q: %w", srcKey, dstKey, err)
	}
	if err := c.replaceWith(ctx, dstKey, leaf, dup, old); err != nil {
		return err
	}
	md, err := c.metadataFor(ctx, srcKey, src)
	if err != nil {
		return err
	}
	if scope, err := c.meta.set(ctx, dstKey, md); err != nil {
		return err
	} else if scope != "" {
		_ = c.meta.flush(ctx, scope)
	}
	return nil
}

// Move puts srcKey's file at dstKey without copying its content: Drime moves
// it between folders and renames it. This is what trash uses, because Drime's
// copy takes seconds a file where a move takes a fraction of one.
func (c *Client) Move(ctx context.Context, srcKey, dstKey string) error {
	src, ok, err := c.tree.lookup(ctx, srcKey)
	if err != nil {
		return fmt.Errorf("drime: move %q: %w", srcKey, err)
	}
	if !ok {
		return notFound("move", srcKey)
	}
	md, err := c.metadataFor(ctx, srcKey, src)
	if err != nil {
		return err
	}
	srcDir, _ := path.Split(srcKey)
	dp, leaf := path.Split(dstKey)
	d, err := c.tree.resolve(ctx, dp, true)
	if err != nil {
		return fmt.Errorf("drime: move to %q: %w", dstKey, err)
	}
	old, err := c.tree.all(ctx, dstKey)
	if err != nil {
		return fmt.Errorf("drime: move to %q: %w", dstKey, err)
	}
	// Whatever already sits at dstKey goes first, so the move cannot land
	// beside it and be renamed "name (1)" by Drime.
	if len(old) > 0 {
		var ids []string
		gone := map[string]bool{}
		for _, o := range old {
			if o.ID != src.ID {
				ids = append(ids, o.ID.String())
				gone[o.ID.String()] = true
			}
		}
		if err := c.api.deleteEntries(ctx, ids, true); err != nil && !isNotFound(err) {
			return fmt.Errorf("drime: move to %q: remove what was there: %w", dstKey, err)
		}
		c.tree.forgetIDs(gone)
	}
	if strings.Trim(srcDir, "/") != strings.Trim(dp, "/") {
		if err := c.api.move(ctx, src.ID.String(), d.id); err != nil {
			if isNotFound(err) {
				return notFound("move", srcKey)
			}
			return fmt.Errorf("drime: move %q to %q: %w", srcKey, dstKey, err)
		}
	}
	c.tree.forgetIDs(map[string]bool{src.ID.String(): true})
	if err := c.replaceWith(ctx, dstKey, leaf, src, nil); err != nil {
		return err
	}
	if err := c.meta.remove(ctx, []string{srcKey}); err != nil {
		return err
	}
	if scope, err := c.meta.set(ctx, dstKey, md); err != nil {
		return err
	} else if scope != "" {
		_ = c.meta.flush(ctx, scope)
	}
	return nil
}

// Delete removes key. A key that is not there is not an error, as on S3.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.DeleteBatch(ctx, []string{key})
}

// deleteChunk bounds how many ids go in one request.
const deleteChunk = 100

// DeleteBatch removes keys, skipping any that are not there.
func (c *Client) DeleteBatch(ctx context.Context, keys []string) error {
	var ids []string
	gone := map[string]bool{}
	for _, k := range keys {
		all, err := c.tree.all(ctx, k)
		if err != nil {
			return fmt.Errorf("drime: delete %q: %w", k, err)
		}
		for _, it := range all {
			ids = append(ids, it.ID.String())
			gone[it.ID.String()] = true
		}
	}
	for len(ids) > 0 {
		chunk := ids[:min(deleteChunk, len(ids))]
		ids = ids[len(chunk):]
		if err := c.api.deleteEntries(ctx, chunk, true); err != nil && !isNotFound(err) {
			return fmt.Errorf("drime: delete %d files: %w", len(chunk), err)
		}
	}
	c.tree.forgetIDs(gone)
	return c.meta.remove(ctx, keys)
}

// DeletePrefix removes everything under prefix. A prefix ending in "/" that
// names a folder is deleted as one folder, in one request.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) (remote.PrefixDeletion, error) {
	objects, err := c.List(ctx, prefix)
	if err != nil {
		return remote.PrefixDeletion{}, err
	}
	var out remote.PrefixDeletion
	for _, o := range objects {
		out.Objects++
		out.Bytes += o.Size
	}
	if strings.HasSuffix(prefix, "/") && prefix != "/" {
		p := strings.TrimSuffix(prefix, "/")
		d, err := c.tree.resolve(ctx, p, false)
		if err != nil {
			return remote.PrefixDeletion{}, err
		}
		if d == nil {
			return out, nil
		}
		if err := c.api.deleteEntries(ctx, []string{d.id}, true); err != nil && !isNotFound(err) {
			return remote.PrefixDeletion{}, fmt.Errorf("drime: delete %q: %w", prefix, err)
		}
		c.tree.forgetDir(p)
		c.meta.forgetUnder(prefix)
		return out, nil
	}
	keys := make([]string, len(objects))
	for i, o := range objects {
		keys[i] = o.Key
	}
	if err := c.DeleteBatch(ctx, keys); err != nil {
		return remote.PrefixDeletion{}, err
	}
	return out, nil
}

// Flush writes the dates and permissions recorded since the last flush.
func (c *Client) Flush(ctx context.Context) error {
	return c.meta.flush(ctx, "")
}

// AbandonStaleUploads gives up on unfinished uploads older than
// remote.ResumeMaxAge. Drime cannot list open uploads, so only the ones this
// machine wrote down are found.
func (c *Client) AbandonStaleUploads(ctx context.Context) (int, error) {
	if c.resume == nil {
		return 0, nil
	}
	all, err := c.resume.AllResumable()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range all {
		if _, _, ok := unpackUploadID(u.UploadID); !ok {
			continue // not one of ours
		}
		if time.Since(time.Unix(0, u.StartedAt)) <= remote.ResumeMaxAge {
			continue
		}
		c.abandon(ctx, u)
		n++
	}
	return n, nil
}

// ListMultipartUploads reports this machine's record of unfinished uploads
// under prefix. Drime has no call to list them server-side.
func (c *Client) ListMultipartUploads(ctx context.Context, prefix string) ([]remote.MultipartUpload, error) {
	if c.resume == nil {
		return nil, nil
	}
	all, err := c.resume.AllResumable()
	if err != nil {
		return nil, err
	}
	var out []remote.MultipartUpload
	for _, u := range all {
		if _, _, ok := unpackUploadID(u.UploadID); !ok || !strings.HasPrefix(u.Key, prefix) {
			continue
		}
		out = append(out, remote.MultipartUpload{Key: u.Key, UploadID: u.UploadID, Initiated: time.Unix(0, u.StartedAt)})
	}
	return out, nil
}

// AbortUpload tells Drime to drop an unfinished upload's parts.
func (c *Client) AbortUpload(ctx context.Context, key, uploadID string) error {
	if err := c.abortUpload(ctx, uploadID); err != nil {
		return fmt.Errorf("drime: abort upload %q: %w", key, err)
	}
	return nil
}

// --- Manifest storage -------------------------------------------------------

func manifestKey(scope string) string { return scope + "/" + ManifestName }

func (c *Client) readManifest(ctx context.Context, scope string) (map[string]metaRecord, error) {
	key := manifestKey(scope)
	it, ok, err := c.tree.lookup(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]metaRecord{}, nil
	}
	resp, err := c.api.download(ctx, it)
	if err != nil {
		if isNotFound(err) {
			return map[string]metaRecord{}, nil
		}
		return nil, fmt.Errorf("drime: read %s: %w", key, err)
	}
	defer resp.Body.Close()
	entries, err := decodeManifest(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("drime: read %s: %w", key, err)
	}
	return entries, nil
}

func (c *Client) writeManifest(ctx context.Context, scope string, data []byte) error {
	key := manifestKey(scope)
	d, err := c.tree.resolve(ctx, scope, true)
	if err != nil {
		return err
	}
	old, err := c.tree.all(ctx, key)
	if err != nil {
		return err
	}
	it, err := c.uploadSmall(ctx, d.id, ManifestName, bytesReader(data), int64(len(data)), "application/gzip", nil)
	if err != nil {
		return err
	}
	return c.replaceWith(ctx, key, ManifestName, it, old)
}
