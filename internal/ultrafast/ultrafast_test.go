package ultrafast

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/relay"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

func TestEnableSetsUpTheWorkerInTheUsersAccount(t *testing.T) {
	cf := newFakeCF(t)
	s := storeAt(t)
	st, err := Enable(context.Background(), s, " cf-token\n", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if st.AccountID != "acct1" || !strings.HasPrefix(st.Script, "r2backup-relay-") {
		t.Fatalf("state = %+v", st)
	}
	// No workers.dev address before: one was made.
	if !strings.HasPrefix(cf.subdomain, "r2b-") || st.URL != "https://"+st.Script+"."+cf.subdomain+".workers.dev" {
		t.Fatalf("url %q, subdomain %q", st.URL, cf.subdomain)
	}
	sc, ok := cf.script("acct1", st.Script)
	if !ok || !bytes.Equal(sc.module, relay.Script) {
		t.Fatal("the relay this binary carries was not uploaded")
	}
	if sc.secret != st.Secret || len(st.Secret) != 64 {
		t.Fatalf("secret bound %q, saved %q", sc.secret, st.Secret)
	}
	if sc.meta["compatibility_date"] != compatibilityDate {
		t.Fatalf("metadata = %v", sc.meta)
	}
	if !cf.enabled["acct1/"+st.Script] {
		t.Fatal("workers.dev address not turned on")
	}
	got, on, err := s.Load()
	if err != nil || !on || got.Secret != st.Secret || got.Token != "cf-token" {
		t.Fatalf("saved = %+v on=%v err=%v", got, on, err)
	}
}

func TestEnableUsesAnExistingWorkersDevAddress(t *testing.T) {
	cf := newFakeCF(t)
	cf.subdomain = "mine"
	st, err := Enable(context.Background(), storeAt(t), "cf-token", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(st.URL, ".mine.workers.dev") {
		t.Fatalf("url = %s", st.URL)
	}
	for _, c := range cf.calls {
		if strings.HasPrefix(c, "PUT") && strings.HasSuffix(c, "/workers/subdomain") {
			t.Fatal("changed the account's existing workers.dev name")
		}
	}
}

func TestEnableAsksWhichAccount(t *testing.T) {
	cf := newFakeCF(t)
	cf.accounts = append(cf.accounts, Account{ID: "acct2", Name: "Work"})
	_, err := Enable(context.Background(), storeAt(t), "cf-token", "", Options{APIBase: cf.srv.URL})
	var need *NeedAccount
	if !errors.As(err, &need) || len(need.Accounts) != 2 {
		t.Fatalf("err = %v", err)
	}
	st, err := Enable(context.Background(), storeAt(t), "cf-token", "acct2", Options{APIBase: cf.srv.URL})
	if err != nil || st.AccountID != "acct2" || st.AccountName != "Work" {
		t.Fatalf("st = %+v err = %v", st, err)
	}
}

func TestEnableWithABadToken(t *testing.T) {
	cf := newFakeCF(t)
	s := storeAt(t)
	_, err := Enable(context.Background(), s, "wrong", "", Options{APIBase: cf.srv.URL})
	if !errors.Is(err, ErrBadToken) {
		t.Fatalf("err = %v", err)
	}
	if s.On() {
		t.Fatal("saved a setup that failed")
	}
	if _, err := Enable(context.Background(), s, "  ", "", Options{APIBase: cf.srv.URL}); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

func TestDisableDeletesTheWorkerAndForgets(t *testing.T) {
	cf := newFakeCF(t)
	s := storeAt(t)
	st, err := Enable(context.Background(), s, "cf-token", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Disable(context.Background(), s, Options{APIBase: cf.srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cf.script("acct1", st.Script); ok {
		t.Fatal("the Worker is still in the account")
	}
	if s.On() {
		t.Fatal("token and password not forgotten")
	}
}

func TestDisableForgetsEvenWhenTheDeleteFails(t *testing.T) {
	cf := newFakeCF(t)
	s := storeAt(t)
	st, err := Enable(context.Background(), s, "cf-token", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cf.deleteErr = 500
	_, err = Disable(context.Background(), s, Options{APIBase: cf.srv.URL})
	if err == nil || !strings.Contains(err.Error(), st.Script) {
		t.Fatalf("err = %v, want it to name the Worker to delete by hand", err)
	}
	if s.On() {
		t.Fatal("token kept after turning off")
	}
}

func TestDisableWhenAlreadyGone(t *testing.T) {
	cf := newFakeCF(t)
	s := storeAt(t)
	st, err := Enable(context.Background(), s, "cf-token", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cf.mu.Lock()
	delete(cf.scripts, "acct1/"+st.Script) // deleted in the dashboard
	cf.mu.Unlock()
	if _, err := Disable(context.Background(), s, Options{APIBase: cf.srv.URL}); err != nil {
		t.Fatalf("a Worker already deleted is not an error: %v", err)
	}
}

// --- copying ------------------------------------------------------------------

type rig struct {
	src   *linkSource
	dst   *remote.Client
	relay *fakeRelay
	cf    *fakeCF
	st    State
	store *Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dst, cleanup := minio.Start(t)
	t.Cleanup(cleanup)
	cf := newFakeCF(t)
	r := &rig{src: newLinkSource(t), dst: dst, cf: cf, store: storeAt(t)}
	st, err := Enable(context.Background(), r.store, "cf-token", "", Options{APIBase: cf.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	r.relay = newFakeRelay(t, st.Secret)
	r.st = st
	return r
}

func (r *rig) copier(minSize int64) *Copier {
	c := NewCopier(r.st, r.store, Options{APIBase: r.cf.srv.URL, RelayURL: r.relay.srv.URL})
	c.MinSize = minSize
	c.PartSize = 5 << 20
	return c
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*seed + seed
	}
	return b
}

func readAll(t *testing.T, s remote.Store, key string) ([]byte, remote.Metadata) {
	t.Helper()
	obj, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b, obj.Metadata
}

const pre = "machines/drime-mirror/Laptop/current/"

func TestUltraFastCopiesCloudToCloud(t *testing.T) {
	r := newRig(t)
	mtime := time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)
	big := pattern(11<<20+333, 3) // three 5 MiB parts
	small := pattern(1000, 5)
	r.src.add(pre+"video.mp4", big, remote.Metadata{ModTime: mtime, Mode: 0o644, Kind: remote.KindFile})
	r.src.add(pre+"notes.txt", small, remote.Metadata{ModTime: mtime, Mode: 0o600, Kind: remote.KindFile})
	dst := bundle.New(r.dst, bundle.Options{Pack: false})

	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: dst, Prefixes: []string{""}, Delete: true, Relay: r.copier(1)})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 2 || rep.Relayed != 2 || len(rep.Failures) != 0 || rep.RelayStopped != nil {
		t.Fatalf("report = %+v (failures %v)", rep, rep.Failures)
	}
	if r.src.gets.Load() != 0 {
		t.Fatalf("%d files came through this computer", r.src.gets.Load())
	}
	if n := r.relay.copies.Load(); n != 4 {
		t.Fatalf("relay copies = %d, want 3 parts + 1 small file", n)
	}
	got, md := readAll(t, r.dst, pre+"video.mp4")
	if !bytes.Equal(got, big) || !md.ModTime.Equal(mtime) || md.Size != int64(len(big)) {
		t.Fatalf("big file: equal=%v md=%+v", bytes.Equal(got, big), md)
	}
	got, md = readAll(t, r.dst, pre+"notes.txt")
	if !bytes.Equal(got, small) || md.Mode != 0o600 {
		t.Fatalf("small file: equal=%v md=%+v", bytes.Equal(got, small), md)
	}

	// A second run has nothing to do.
	rep, err = mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: dst, Prefixes: []string{""}, Delete: true, Relay: r.copier(1)})
	if err != nil || rep.Copied != 0 || rep.Unchanged != 2 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
}

func TestUltraFastHybridWithPacking(t *testing.T) {
	r := newRig(t)
	scope := "machines/drime-mirror/Laptop/"
	big := pattern(6<<20, 7)
	r.src.add(scope+"current/big.bin", big, remote.Metadata{ModTime: time.Unix(10, 0), Mode: 0o644})
	for i := 0; i < 5; i++ {
		r.src.add(scope+"current/small"+string(rune('a'+i)), pattern(2000+i, byte(i+1)), remote.Metadata{ModTime: time.Unix(10, 0), Mode: 0o644})
	}
	dst := bundle.New(r.dst, bundle.Options{Pack: true})

	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: dst, Prefixes: []string{scope}, Delete: true,
		Relay: r.copier(bundle.DefaultSmallMax + 1)})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 6 || rep.Relayed != 1 || rep.RelayFellBack != 0 || len(rep.Failures) != 0 {
		t.Fatalf("report = %+v (failures %v)", rep, rep.Failures)
	}
	if r.src.gets.Load() != 5 {
		t.Fatalf("%d small files came through this computer, want 5", r.src.gets.Load())
	}
	if r.src.links.Load() != 1 {
		t.Fatalf("asked for %d download links, want only the big file's", r.src.links.Load())
	}
	// A fresh view of the destination, from what is stored, has them all:
	// the big file as an object of its own, the small ones packed.
	again := bundle.New(r.dst, bundle.Options{Pack: true})
	list, err := again.List(context.Background(), scope)
	if err != nil || len(list) != 6 {
		t.Fatalf("listing %d entries: %v", len(list), err)
	}
	got, _ := readAll(t, again, scope+"current/big.bin")
	if !bytes.Equal(got, big) {
		t.Fatal("big file differs")
	}
	raw, err := r.dst.List(context.Background(), scope+".bundles/packs/")
	if err != nil || len(raw) != 1 {
		t.Fatalf("want the small files in one pack, got %d packs: %v", len(raw), err)
	}
}

func TestUltraFastFallsBackWhenTheAllowanceRunsOut(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 4; i++ {
		r.src.add(pre+"f"+string(rune('a'+i)), pattern(3000, byte(i+2)), remote.Metadata{ModTime: time.Unix(10, 0)})
	}
	r.relay.status.Store(429)
	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: r.dst, Prefixes: []string{""}, Relay: r.copier(1), Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 4 || rep.Relayed != 0 || rep.RelayFellBack != 1 || !errors.Is(rep.RelayStopped, mirror.ErrRelayStopped) {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.RelayStopped.Error(), "allowance") {
		t.Fatalf("reason = %v", rep.RelayStopped)
	}
}

func TestUltraFastRetriesAPartOnce(t *testing.T) {
	r := newRig(t)
	data := pattern(11<<20, 9)
	r.src.add(pre+"big", data, remote.Metadata{ModTime: time.Unix(10, 0)})
	r.relay.failN.Store(1)
	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: r.dst, Prefixes: []string{""}, Relay: r.copier(1)})
	if err != nil || rep.Relayed != 1 || rep.RelayFellBack != 0 {
		t.Fatalf("report = %+v err = %v", rep, err)
	}
	got, _ := readAll(t, r.dst, pre+"big")
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestUltraFastWrongSecretStopsAndFallsBack(t *testing.T) {
	r := newRig(t)
	r.src.add(pre+"a", pattern(100, 1), remote.Metadata{ModTime: time.Unix(10, 0)})
	r.relay.secret.Store(strings.Repeat("z", 64))
	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: r.dst, Prefixes: []string{""}, Relay: r.copier(1)})
	if err != nil || rep.Copied != 1 || rep.Relayed != 0 || rep.RelayStopped == nil {
		t.Fatalf("report = %+v err = %v", rep, err)
	}
	if !strings.Contains(rep.RelayStopped.Error(), "password") {
		t.Fatalf("reason = %v", rep.RelayStopped)
	}
}

func TestUltraFastReplacesAnOutOfDateWorker(t *testing.T) {
	r := newRig(t)
	r.src.add(pre+"a", pattern(100, 1), remote.Metadata{ModTime: time.Unix(10, 0)})
	r.relay.version.Store("0")
	r.cf.onUpload = func(name, secret string) {
		if name == r.st.Script && secret == r.st.Secret {
			r.relay.version.Store(relay.Version)
		}
	}
	rep, err := mirror.Run(context.Background(), mirror.Options{Src: r.src, Dst: r.dst, Prefixes: []string{""}, Relay: r.copier(1)})
	if err != nil || rep.Relayed != 1 {
		t.Fatalf("report = %+v err = %v", rep, err)
	}
	st, _, _ := r.store.Load()
	if st.Version != relay.Version {
		t.Fatalf("saved version %q", st.Version)
	}
}

func TestReadyWaitsForANewAddress(t *testing.T) {
	r := newRig(t)
	o := Options{RelayURL: r.relay.srv.URL}
	st := r.st
	st.URL = r.relay.srv.URL
	if err := Ready(context.Background(), st, time.Second, o); err != nil {
		t.Fatal(err)
	}
	st.Secret = "wrong"
	if err := Ready(context.Background(), st, 0, o); err == nil {
		t.Fatal("a relay refusing the password counted as ready")
	}
}
