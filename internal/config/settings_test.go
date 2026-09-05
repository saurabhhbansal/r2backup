package config

import (
	"os"
	"path/filepath"
	"testing"
)

func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvDataDir, dir)
	return dir
}

// A fresh install has no settings file, and that is not an error -- it is the
// starting state of every machine.
func TestLoadSettingsMissingFileIsTheDefault(t *testing.T) {
	withDataDir(t)
	s, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings on a fresh install: %v", err)
	}
	if s != (Settings{}) {
		t.Errorf("settings = %+v, want the zero value", s)
	}
	if s.BudgetUSD != 0 {
		t.Errorf("BudgetUSD = %v, want 0 (no limit)", s.BudgetUSD)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	withDataDir(t)
	if err := SaveSettings(Settings{BudgetUSD: 12.50}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.BudgetUSD != 12.50 {
		t.Errorf("BudgetUSD = %v, want 12.50", got.BudgetUSD)
	}
}

func TestSaveSettingsOverwrites(t *testing.T) {
	withDataDir(t)
	if err := SaveSettings(Settings{BudgetUSD: 5}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := SaveSettings(Settings{BudgetUSD: 20}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.BudgetUSD != 20 {
		t.Errorf("BudgetUSD = %v, want the second write, 20", got.BudgetUSD)
	}
}

// Clearing the limit has to actually clear it. omitempty drops the field, and
// a reader must take its absence as "no limit" rather than keeping the old
// one.
func TestSavingZeroClearsTheLimit(t *testing.T) {
	withDataDir(t)
	if err := SaveSettings(Settings{BudgetUSD: 30}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := SaveSettings(Settings{}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.BudgetUSD != 0 {
		t.Errorf("BudgetUSD = %v, want 0 after clearing", got.BudgetUSD)
	}
}

// Unreadable settings must not read back as "no limit set" -- failing open on
// a spending limit is the one behaviour this file exists to prevent.
func TestCorruptSettingsAreAnError(t *testing.T) {
	dir := withDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, settingsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(); err == nil {
		t.Error("corrupt settings loaded as the default; a limit must not fail open")
	}
}

// A rename-into-place leaves no temporary files behind.
func TestSaveSettingsLeavesNoTempFiles(t *testing.T) {
	dir := withDataDir(t)
	if err := SaveSettings(Settings{BudgetUSD: 1}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != settingsFileName {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}

func TestSettingsPathIsInsideTheDataDir(t *testing.T) {
	dir := withDataDir(t)
	path, err := SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	if want := filepath.Join(dir, settingsFileName); path != want {
		t.Errorf("SettingsPath = %q, want %q", path, want)
	}
}
