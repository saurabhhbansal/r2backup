//go:build windows

package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPlaceExeReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	to := filepath.Join(dir, "r2b.exe")
	if err := os.WriteFile(to, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := placeExe([]byte("new"), to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); string(b) != "new" {
		t.Fatalf("got %q", b)
	}
	if fileExists(to + ".new") {
		t.Fatal("left the staging file behind")
	}
}

func TestPlaceExeReplacesARunningProgram(t *testing.T) {
	dir := t.TempDir()
	to := filepath.Join(dir, "r2b.exe")
	src, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE"))
	if err != nil {
		t.Skip("no ping.exe to run:", err)
	}
	if err := os.WriteFile(to, src, 0o755); err != nil {
		t.Fatal(err)
	}
	// A running program cannot be overwritten on Windows, only renamed.
	cmd := exec.Command(to, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := placeExe([]byte("new"), to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(to); string(b) != "new" {
		t.Fatalf("got %q", b)
	}
	if !fileExists(to + ".old") {
		t.Fatal("the running copy was not moved aside")
	}
}

func TestEmbeddedIconIsUsable(t *testing.T) {
	if iconHandle(32) == 0 {
		t.Fatal("the embedded icon did not load")
	}
}
