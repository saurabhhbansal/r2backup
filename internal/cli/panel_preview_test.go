package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/tray"
)

// TestPanelPreview serves the taskbar panel to an ordinary browser, wired to
// a real trayBackend on a scratch data directory and the fake account
// service, so every screen can be clicked through without Windows or any
// storage. It runs only when asked:
//
//	R2B_PANEL_PREVIEW=127.0.0.1:8765 go test ./internal/cli -run TestPanelPreview -timeout 0
//
// The account's email code is 123456. Storage is not reached: connecting
// saves the keys as typed, and copies run against a timer.
func TestPanelPreview(t *testing.T) {
	addr := os.Getenv("R2B_PANEL_PREVIEW")
	if addr == "" {
		t.Skip("set R2B_PANEL_PREVIEW=host:port to preview the panel")
	}
	tb := trayForTest(t)
	w, stop := newFakeWorker(t)
	defer stop()
	// The panel reads who is signed in from the session token itself.
	w.token = fakeToken(`{"sub":"me@example.com","exp":4000000000}`)
	page, err := os.ReadFile(filepath.Join("..", "tray", "panel.html"))
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	lastSaved := ""

	// The panel window is 380 by 580 (panelW, panelH in tray_windows.go).
	shim := `<style>html { width: 380px; height: 580px; }</style>
<script>
window.chrome = { webview: { postMessage: (m) => fetch("/msg", { method: "POST", body: m }).then((r) => r.text()).then((js) => { if (js) (0, eval)(js); }) } };
setInterval(() => fetch("/state").then((r) => r.json()).then((s) => window.r2b && r2b.render(s)), 1500);
</script>
`
	html := strings.Replace(string(page), "<script>", shim+"<script>", 1)

	state := func() string {
		st, err := tb.State(context.Background())
		if err != nil {
			return "{}"
		}
		b, _ := json.Marshal(st)
		return string(b)
	}
	reply := func(id int, out any, err error) string {
		r := map[string]any{"id": id, "ok": err == nil, "data": out}
		if err != nil {
			r["error"] = tray.Plain(err.Error())
		}
		b, _ := json.Marshal(r)
		return "r2b.reply(" + string(b) + ")"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(state()))
	})
	mux.HandleFunc("/msg", func(w http.ResponseWriter, r *http.Request) {
		var m struct{ Cmd, Arg string }
		_ = json.NewDecoder(r.Body).Decode(&m)
		switch m.Cmd {
		case "ready":
			_, _ = w.Write([]byte("r2b.render(" + state() + ")"))
		case "call":
			var c callPreview
			_ = json.Unmarshal([]byte(m.Arg), &c)
			out, err := previewCall(tb, c, scratch, &lastSaved)
			_, _ = w.Write([]byte(reply(c.ID, out, err)))
		default:
			t.Logf("panel sent %s %q", m.Cmd, m.Arg)
		}
	})
	t.Logf("panel preview on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		t.Fatal(err)
	}
}

type callPreview struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// previewCall answers a request the way the Windows panel would, except for
// what needs Windows' dialogs or real storage.
func previewCall(tb *trayBackend, c callPreview, scratch string, lastSaved *string) (any, error) {
	ctx := context.Background()
	a := tb.d.app
	switch c.Method {
	case "pickFolder":
		return scratch, nil
	case "pickFile":
		var d struct {
			Save bool   `json:"save"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(c.Args, &d)
		if d.Save {
			*lastSaved = filepath.Join(scratch, fmt.Sprintf("%d-%s", time.Now().UnixNano(), d.Name))
			return *lastSaved, nil
		}
		return *lastSaved, nil
	case "connect", "mirrorSetup":
		var d struct {
			destArgs
			Auto bool `json:"auto"`
		}
		_ = json.Unmarshal(c.Args, &d)
		k := credsFromKeys(d.keys())
		if err := k.Valid(); err != nil {
			return nil, err
		}
		if c.Method == "connect" {
			err := a.creds.Save(k)
			tb.d.forgetClient()
			tb.invalidate()
			return nil, err
		}
		if main, err := a.creds.Load(); err != nil {
			return nil, fmt.Errorf("choose where backups go first")
		} else if samePlace(main, k) {
			return nil, fmt.Errorf("that is where your backups already go. A second copy has to be somewhere else")
		}
		if err := a.mirror.Save(k); err != nil {
			return nil, err
		}
		return nil, setMirrorAuto(d.Auto)
	case "drimeSourceAdd":
		var d struct {
			Token, Folder, Name string
			Auto                bool
		}
		_ = json.Unmarshal(c.Args, &d)
		if d.Token != "" {
			_ = a.source.Save(credsFromKeys(destArgs{Provider: "drime", Token: d.Token}.keys()))
		}
		if _, err := s3Destination(a); err != nil {
			return tb.drimeSources(), err
		}
		if _, err := drimeSourceCreds(a); err != nil {
			return tb.drimeSources(), err
		}
		d.Folder = strings.Trim(d.Folder, "/ ")
		if d.Name == "" {
			d.Name = d.Folder[strings.LastIndex(d.Folder, "/")+1:]
		}
		st, _ := config.LoadSettings()
		st.MirrorSources = append(st.MirrorSources, config.MirrorSource{Name: d.Name, Folder: d.Folder, Auto: d.Auto})
		err := config.SaveSettings(st)
		return tb.drimeSources(), err
	case "drimeSourceRun", "mirrorRun":
		to := "Backblaze B2 · demo"
		if c.Method == "drimeSourceRun" {
			to = "Drime folders → demo"
		}
		ctx, cancel := context.WithCancel(context.Background())
		j := &mirrorJob{to: to, cancel: cancel}
		tb.mu.Lock()
		tb.mj = j
		tb.mu.Unlock()
		go func() {
			for i := 1; i <= 10; i++ {
				select {
				case <-ctx.Done():
					j.mu.Lock()
					j.done, j.err = true, "Stopped before it finished. The next copy picks up from there."
					j.mu.Unlock()
					return
				case <-time.After(400 * time.Millisecond):
				}
				j.mu.Lock()
				j.p = mirror.Progress{FilesDone: i * 12, FilesTotal: 120, BytesDone: int64(i) << 20, BytesTotal: 10 << 20}
				j.mu.Unlock()
			}
			j.mu.Lock()
			j.done, j.rep = true, &mirror.Report{Copied: 120, Bytes: 10 << 20, Unchanged: 30}
			j.mu.Unlock()
		}()
		return nil, nil
	}
	raw := c.Args
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return tb.Call(ctx, c.Method, raw)
}
