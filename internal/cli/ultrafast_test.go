package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/ultrafast"
	"github.com/saurabhhbansal/r2backup/relay"
)

// ultraWorld points Ultra fast mode at a fake Cloudflare API and a fake
// relay that only answers /health. It records the Cloudflare calls.
func ultraWorld(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	var secret string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(403)
			return
		}
		var result any
		switch {
		case r.URL.Path == "/accounts":
			result = []map[string]string{{"id": "acct1", "name": "Personal"}}
		case strings.HasSuffix(r.URL.Path, "/workers/subdomain"):
			result = map[string]string{"subdomain": "mine"}
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/workers/scripts/"):
			_ = r.ParseMultipartForm(1 << 20)
			var meta struct {
				Bindings []struct{ Text string } `json:"bindings"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("metadata")), &meta)
			mu.Lock()
			secret = meta.Bindings[0].Text
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}))
	t.Cleanup(cf.Close)
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := secret != "" && r.Header.Get("Authorization") == "Bearer "+secret
		mu.Unlock()
		if !ok {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"version":"` + relay.Version + `"}`))
	}))
	t.Cleanup(rl.Close)
	ultraOptions = ultrafast.Options{APIBase: cf.URL, RelayURL: rl.URL}
	ultraReadyWait = time.Second
	t.Cleanup(func() { ultraOptions = ultrafast.Options{}; ultraReadyWait = 45 * time.Second })
	return &calls
}

func TestUltraOnExplainsFirstAndCanBeDeclined(t *testing.T) {
	parityWorld(t, true)
	calls := ultraWorld(t)
	opts := conversation("n")
	if _, err := r2b(t, opts, "mirror", "ultra", "on"); err != nil {
		t.Fatal(err)
	}
	out := said(opts)
	if !strings.Contains(out, "nothing goes through this PC") || !strings.Contains(out, "Workers Scripts > Edit") ||
		!strings.Contains(out, "With packing on") || !strings.Contains(out, "Left off.") {
		t.Fatalf("output:\n%s", out)
	}
	if len(*calls) != 0 {
		t.Fatalf("called Cloudflare after being told no: %v", *calls)
	}
	a, _ := openApp()
	defer a.close()
	if a.ultra.On() {
		t.Fatal("turned on anyway")
	}
}

func TestUltraOnAndOff(t *testing.T) {
	parityWorld(t, true)
	calls := ultraWorld(t)
	opts := conversation("y", "cf-token")
	if _, err := r2b(t, opts, "mirror", "ultra", "on"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if !strings.Contains(said(opts), "Ultra fast mode is on: Worker r2backup-relay-") || strings.Contains(said(opts), "few minutes") {
		t.Fatalf("output:\n%s", said(opts))
	}
	out, err := r2b(t, nil, "mirror", "ultra")
	if err != nil || !strings.Contains(out, "is on (experimental)") || !strings.Contains(out, "Personal") {
		t.Fatalf("%q %v", out, err)
	}

	if _, err := r2b(t, nil, "mirror", "ultra", "off"); err != nil {
		t.Fatal(err)
	}
	deleted := false
	for _, c := range *calls {
		if strings.HasPrefix(c, "DELETE /accounts/acct1/workers/scripts/r2backup-relay-") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("the Worker was not deleted: %v", *calls)
	}
	a, _ := openApp()
	defer a.close()
	if a.ultra.On() {
		t.Fatal("still on after off")
	}
}

func TestUltraOnWithAWrongToken(t *testing.T) {
	parityWorld(t, true)
	ultraWorld(t)
	opts := conversation("y", "nope")
	_, err := r2b(t, opts, "mirror", "ultra", "on")
	if err == nil || !strings.Contains(err.Error(), "Workers Scripts > Edit") {
		t.Fatalf("err = %v", err)
	}
}

func TestRelayIsOnlyForDrimeToS3(t *testing.T) {
	parityWorld(t, true)
	ultraWorld(t)
	a, _ := openApp()
	defer a.close()
	drime := creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}
	packed := b2("second")
	packed.Bundle = true

	if relayFor(a, drime, packed) != nil {
		t.Fatal("a relay while Ultra fast mode is off")
	}
	if _, err := ultraOn(t.Context(), a, "cf-token", ""); err != nil {
		t.Fatal(err)
	}
	r, ok := relayFor(a, drime, packed).(*ultrafast.Copier)
	if !ok || r.MinSize != bundle.DefaultSmallMax+1 {
		t.Fatalf("packing destination: relay %v", r)
	}
	r, ok = relayFor(a, drime, b2("second")).(*ultrafast.Copier)
	if !ok || r.MinSize != 1 {
		t.Fatalf("destination without packing: relay %v", r)
	}
	if relayFor(a, b2("main"), b2("second")) != nil {
		t.Fatal("a relay from S3 storage")
	}
	if relayFor(a, b2("main"), drime) != nil {
		t.Fatal("a relay to Drime")
	}
	// This world backs up to B2, so the second copy is not Drime to S3.
	if mirrorRelay(a, false) != nil {
		t.Fatal("a relay for a second copy from B2")
	}
}

func TestReportSaysWhatWentCloudToCloud(t *testing.T) {
	var b bytes.Buffer
	printMirrorReport(&b, &mirror.Report{Copied: 3, Relayed: 2, RelayedBytes: 3 << 30, RelayFellBack: 1,
		RelayStopped: errorString(mirror.ErrRelayStopped.Error() + ": your Cloudflare account's free Worker allowance for today is used up")}, false)
	out := b.String()
	if !strings.Contains(out, "Ultra fast mode: 2 file(s), 3 GB, went cloud to cloud.") ||
		!strings.Contains(out, "Ultra fast mode stopped: your Cloudflare account's free Worker allowance for today is used up. The rest went through this PC.") {
		t.Fatalf("report:\n%s", out)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
