package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if !reflect.DeepEqual(s, Settings{}) {
		t.Errorf("settings = %+v, want the zero value", s)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	withDataDir(t)
	if err := SaveSettings(Settings{MachineName: "laptop"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.MachineName != "laptop" {
		t.Errorf("MachineName = %q, want laptop", got.MachineName)
	}
}

func TestSaveSettingsOverwrites(t *testing.T) {
	withDataDir(t)
	if err := SaveSettings(Settings{MachineName: "a"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := SaveSettings(Settings{MachineName: "b"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.MachineName != "b" {
		t.Errorf("MachineName = %q, want the second write, b", got.MachineName)
	}
}

// A settings file from an older version still holds a spending limit. It
// loads, the limit is ignored, and the rest of the file is kept.
func TestOldBudgetSettingsLoadAndAreIgnored(t *testing.T) {
	dir := withDataDir(t)
	old := `{"budget_usd": 12.5, "budget_resumed_month": "2026-09", "machine_name": "laptop", "mirror_auto": true}`
	if err := os.WriteFile(filepath.Join(dir, settingsFileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("an old settings file with a budget did not load: %v", err)
	}
	if got.MachineName != "laptop" || !got.MirrorAuto {
		t.Errorf("the rest of the old file was lost: %+v", got)
	}
	if err := SaveSettings(got); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "budget") {
		t.Errorf("the old limit was written back:\n%s", data)
	}
}

// Unreadable settings are an error, not the defaults.
func TestCorruptSettingsAreAnError(t *testing.T) {
	dir := withDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, settingsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(); err == nil {
		t.Error("corrupt settings loaded as the default")
	}
}

// A rename-into-place leaves no temporary files behind.
func TestSaveSettingsLeavesNoTempFiles(t *testing.T) {
	dir := withDataDir(t)
	if err := SaveSettings(Settings{MachineName: "x"}); err != nil {
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
