package account

import (
	"os"
	"runtime"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/config"
)

func withTempDataDir(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
}

func TestLoadTokenWhenNoneSaved(t *testing.T) {
	withTempDataDir(t)

	token, err := LoadToken()
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty when nothing has been saved", token)
	}
}

func TestSaveAndLoadToken(t *testing.T) {
	withTempDataDir(t)

	if err := SaveToken("a-session-token"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	got, err := LoadToken()
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got != "a-session-token" {
		t.Errorf("token = %q, want a-session-token", got)
	}
}

func TestSaveTokenModeIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file mode bits don't apply on Windows")
	}
	withTempDataDir(t)

	if err := SaveToken("a-session-token"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	path, err := TokenPath()
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %v, want 0600", perm)
	}
}

func TestClearToken(t *testing.T) {
	withTempDataDir(t)

	if err := SaveToken("a-session-token"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if err := ClearToken(); err != nil {
		t.Fatalf("ClearToken: %v", err)
	}
	got, err := LoadToken()
	if err != nil {
		t.Fatalf("LoadToken after clear: %v", err)
	}
	if got != "" {
		t.Errorf("token = %q, want empty after ClearToken", got)
	}

	// Clearing an already-clear token must not error.
	if err := ClearToken(); err != nil {
		t.Errorf("ClearToken on already-absent token: %v", err)
	}
}
