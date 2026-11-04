package drime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFoldersBrowsesByPath(t *testing.T) {
	f := newFake(t)
	backups := f.add("", "Backups", itemTypeFolder, nil)
	f.add("", "Photos", itemTypeFolder, nil)
	f.add("", "notes.txt", "file", []byte("x"))
	laptop := f.add(backups.ID.String(), "Laptop", itemTypeFolder, nil)
	f.add(backups.ID.String(), "desktop", itemTypeFolder, nil)
	f.add(laptop.ID.String(), "a.txt", "file", []byte("a"))
	cfg := Config{Token: f.token, BaseURL: f.srv.URL}
	ctx := context.Background()

	got, err := Folders(ctx, cfg, "")
	if err != nil || !reflect.DeepEqual(got, []string{"Backups", "Photos"}) {
		t.Fatalf("top = %v, %v", got, err)
	}
	got, err = Folders(ctx, cfg, "/Backups/")
	if err != nil || !reflect.DeepEqual(got, []string{"desktop", "Laptop"}) {
		t.Fatalf("Backups = %v, %v", got, err)
	}
	got, err = Folders(ctx, cfg, "Backups/Laptop")
	if err != nil || len(got) != 0 {
		t.Fatalf("Laptop = %v, %v", got, err)
	}
	if _, err := Folders(ctx, cfg, "Backups/Nope"); !errors.Is(err, ErrFolderNotFound) {
		t.Fatalf("a missing folder: %v", err)
	}
	if f.calls["POST /api/v1/folders"] != 0 {
		t.Fatal("browsing made a folder")
	}
	if _, err := Folders(ctx, Config{Token: "wrong", BaseURL: f.srv.URL}, ""); err == nil {
		t.Fatal("a wrong token listed folders")
	}
}

func TestWorkspacesListsTheOwnDriveFirst(t *testing.T) {
	f := newFake(t)
	cfg := Config{Token: f.token, BaseURL: f.srv.URL}
	ctx := context.Background()
	// No workspaces endpoint: the own drive alone.
	got, err := Workspaces(ctx, cfg)
	if err != nil || len(got) != 1 || got[0].ID != "" {
		t.Fatalf("without workspaces = %v, %v", got, err)
	}
	f.workspaces = []map[string]any{{"id": 7, "name": "Team"}, {"id": 9, "name": ""}}
	got, err = Workspaces(ctx, cfg)
	want := []Workspace{{"", "My workspace"}, {"7", "Team"}, {"9", "Workspace 9"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("workspaces = %v, %v", got, err)
	}
	// Drime leaves the own workspace's name null; its app shows the first
	// name, and so does this.
	f.firstName = "Ada"
	if got, _ = Workspaces(ctx, cfg); got[0].Name != "Ada’s workspace" {
		t.Fatalf("own workspace unnamed = %q", got[0].Name)
	}
	named := "Studio"
	f.personal = &named
	if got, _ = Workspaces(ctx, cfg); got[0].Name != "Studio" {
		t.Fatalf("own workspace renamed = %q", got[0].Name)
	}
	if _, err := Workspaces(ctx, Config{Token: "wrong", BaseURL: f.srv.URL}); err == nil {
		t.Fatal("a wrong token listed workspaces")
	}
}
