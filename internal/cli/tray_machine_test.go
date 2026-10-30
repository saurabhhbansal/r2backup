package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/account"
)

func TestValidMachineName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "tab\there", strings.Repeat("x", 64)} {
		if validMachineName(bad) == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"Office PC", "laptop-2", "Saurabh’s Desktop", strings.Repeat("x", 63)} {
		if err := validMachineName(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

// A rename changes where new folders are filed, and the earlier name still
// counts as this computer, so backups filed under it are not shown -- or
// offered for deletion -- as another computer's.
func TestRenamingThisComputerKeepsTheOldNameAsItsOwn(t *testing.T) {
	tb := trayForTest(t)
	t.Setenv("R2BACKUP_MACHINE", "")
	old := machineName()

	if _, err := call(t, tb, "renameMachine", map[string]string{"name": "  Office PC  "}); err != nil {
		t.Fatal(err)
	}
	if got := machineName(); got != "Office PC" {
		t.Fatalf("name is %q, want Office PC", got)
	}
	if !isThisMachine(old) || !isThisMachine("Office PC") {
		t.Fatalf("old name %q or new name no longer counts as this computer", old)
	}
	if isThisMachine("someone-else") {
		t.Fatal("an unrelated name counts as this computer")
	}

	// Back to the hostname: it follows the hostname again.
	if _, err := call(t, tb, "renameMachine", map[string]string{"name": old}); err != nil {
		t.Fatal(err)
	}
	if machineName() != old || !isThisMachine("Office PC") {
		t.Fatal("renaming back lost a name")
	}
}

func TestRenameIsRefusedWhileTheNameIsSetByTheEnvironment(t *testing.T) {
	tb := trayForTest(t) // sets R2BACKUP_MACHINE
	if _, err := call(t, tb, "renameMachine", map[string]string{"name": "Other"}); err == nil {
		t.Fatal("renamed a computer whose name the environment sets")
	}
}

// Two computers filed under one name would share a location in storage, so a
// name another computer on the account has is refused, and a rename tells
// the account which entry it replaces.
func TestRenameChecksTheAccountAndReplacesItsEntry(t *testing.T) {
	var mu sync.Mutex
	var posted []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/devices" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"device_name": "Laptop", "os": "windows", "last_seen": 1},
			}})
		case r.URL.Path == "/devices" && r.Method == http.MethodPost:
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			posted = append(posted, b)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tb := trayForTest(t)
	t.Setenv("R2BACKUP_MACHINE", "")
	t.Setenv(EnvAccountAPI, srv.URL)
	if err := account.SaveToken("fake.token.value"); err != nil {
		t.Fatal(err)
	}
	old := machineName()

	if _, err := call(t, tb, "renameMachine", map[string]string{"name": "Laptop"}); err == nil {
		t.Fatal("took the name of another computer on the account")
	}
	if _, err := call(t, tb, "renameMachine", map[string]string{"name": "Desk"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0]["device_name"] != "Desk" || posted[0]["previous_name"] != old {
		t.Fatalf("account was told %+v, want Desk replacing %q", posted, old)
	}
}
