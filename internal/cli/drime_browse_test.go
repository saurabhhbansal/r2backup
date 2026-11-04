package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/drime"
)

// fakeDrimeTree is a Drime that only lists: folders by parent id, and one
// workspace. Workspace 5's top holds Team alone.
func fakeDrimeTree(t *testing.T) string {
	t.Helper()
	type entry struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		ParentID int    `json:"parent_id"`
	}
	own := map[string][]entry{
		"":  {{1, "Backups", "folder", 0}, {2, "Photos", "folder", 0}},
		"1": {{3, "Laptop", "folder", 1}},
		"3": {},
		"2": {},
	}
	team := map[string][]entry{"": {{9, "Team", "folder", 0}}, "9": {}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/drive/file-entries":
			tree := own
			if r.URL.Query().Get("workspaceId") == "5" {
				tree = team
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"current_page": 1, "last_page": 1, "data": tree[r.URL.Query().Get("folderId")]})
		case "/api/v1/me/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":5,"name":"Team"}]}`))
		default:
			t.Errorf("fake drime: unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPanelBrowsesDrimeAndMovesWhereBackupsGo(t *testing.T) {
	tb := trayForTest(t)
	a := tb.d.app
	base := fakeDrimeTree(t)
	if err := a.creds.Save(creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "tok", Endpoint: base}); err != nil {
		t.Fatal(err)
	}

	out, err := call(t, tb, "drimeWorkspaces", map[string]string{"which": "primary"})
	if err != nil {
		t.Fatal(err)
	}
	if ws := out.([]drime.Workspace); !reflect.DeepEqual(ws, []drime.Workspace{{ID: "", Name: "My drive"}, {ID: "5", Name: "Team"}}) {
		t.Fatalf("workspaces = %v", ws)
	}
	out, err = call(t, tb, "drimeFolders", map[string]string{"which": "primary", "path": "Backups"})
	if err != nil {
		t.Fatal(err)
	}
	if l := out.(drimeFolderList); l.Path != "Backups" || !reflect.DeepEqual(l.Folders, []string{"Laptop"}) {
		t.Fatalf("Backups = %+v", l)
	}
	out, err = call(t, tb, "drimeFolders", map[string]string{"which": "primary", "workspace": "5"})
	if err != nil || !reflect.DeepEqual(out.(drimeFolderList).Folders, []string{"Team"}) {
		t.Fatalf("workspace 5 = %+v, %v", out, err)
	}
	if _, err := call(t, tb, "drimeFolders", map[string]string{"which": "primary", "path": "Nope"}); err == nil {
		t.Fatal("a folder that is not there listed")
	}

	if _, err := call(t, tb, "setDrimeLocation", map[string]string{"folder": ""}); err == nil {
		t.Fatal("the top of the drive was taken")
	}
	out, err = call(t, tb, "setDrimeLocation", map[string]string{"workspace": "5", "folder": "/Team/"})
	if err != nil {
		t.Fatal(err)
	}
	if st := out.(*storeOut); st.Workspace != "5" || st.Folder != "Team" || st.Label != "Drime · Team" {
		t.Fatalf("setDrimeLocation = %+v", st)
	}
	if c, _ := a.creds.Load(); c.DrimeWorkspace != "5" || c.DrimeRoot != "Team" || c.DrimeToken != "tok" {
		t.Fatalf("saved %+v", c.Redacted())
	}

	// A new token for the same Drime keeps the place chosen.
	if c, ok := tb.drimeKeepingPlace(destArgs{Provider: "drime", Token: "tok"}); !ok || c.DrimeRoot != "Team" || c.DrimeWorkspace != "5" {
		t.Fatalf("a new token lost the place: %+v %v", c.Redacted(), ok)
	}
}

// A secondary backup only reads the Drime folders with a token of its own:
// it is checked, saved, and the browser lists with it.
func TestPanelDrimeSourceTokenAndBrowsing(t *testing.T) {
	tb := trayForTest(t)
	a := tb.d.app
	base := fakeDrimeTree(t)
	_ = a.creds.Save(b2Keys)
	// Pointed at the fake: the token the panel types talks to the same Drime.
	_ = a.source.Save(creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "old", Endpoint: base})

	if _, err := call(t, tb, "drimeSourceToken", map[string]string{"token": "wrong"}); err == nil {
		t.Fatal("a token Drime refuses was saved")
	}
	if _, err := call(t, tb, "drimeSourceToken", map[string]string{"token": "tok"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.source.Load(); c.DrimeToken != "tok" {
		t.Fatalf("source token %q", c.DrimeToken)
	}
	out, err := call(t, tb, "drimeFolders", map[string]string{"path": ""})
	if err != nil || !reflect.DeepEqual(out.(drimeFolderList).Folders, []string{"Backups", "Photos"}) {
		t.Fatalf("folders = %+v, %v", out, err)
	}
	if _, err := call(t, tb, "drimeFolders", map[string]string{"which": "primary"}); err == nil {
		t.Fatal("browsed where backups go, which is not Drime")
	}
}
