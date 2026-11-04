package drime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Browsing a Drime drive, for choosing a folder by clicking through it rather
// than typing its path: the workspaces a token reaches, and the folders inside
// a folder. Nothing here creates anything.

// Workspace is a Drime workspace. ID "" is the person's own drive.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// browser is an api with no root folder found, for listing.
func browser(cfg Config) (*api, error) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, errors.New("drime: an API token is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	return &api{base: base, token: token, workspace: cfg.WorkspaceID, hc: hc, maxAttempts: attempts}, nil
}

// refused turns a token Drime will not take into words a person can act on.
func refused(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
		return errors.New("drime: the API token was refused. Make a new one in Drime under Settings > Developer")
	}
	return err
}

// Workspaces lists the workspaces the token reaches, the person's own
// workspace first. GET /me/workspaces lists the team ones and the personal
// one's name, which Drime leaves null until it is renamed; Drime's own app
// then calls it "<first name>'s workspace", and so does this.
func Workspaces(ctx context.Context, cfg Config) ([]Workspace, error) {
	cfg.WorkspaceID = ""
	a, err := browser(cfg)
	if err != nil {
		return nil, err
	}
	// The token is checked first, on a call every account answers, so a bad
	// token is an error and not an empty list of workspaces.
	if _, err := a.listFolder(ctx, "", true); err != nil {
		return nil, refused(err)
	}
	var res struct {
		Workspaces []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"workspaces"`
		Personal struct {
			Name *string `json:"name"`
		} `json:"personal"`
	}
	if err := a.call(ctx, http.MethodGet, "/me/workspaces", nil, nil, &res, nil); err != nil {
		return []Workspace{{ID: "", Name: a.personalName(ctx, "")}}, nil
	}
	own := ""
	if res.Personal.Name != nil {
		own = *res.Personal.Name
	}
	out := []Workspace{{ID: "", Name: a.personalName(ctx, own)}}
	for _, w := range res.Workspaces {
		if w.ID.String() == "" || w.ID.String() == "0" {
			continue
		}
		name := strings.TrimSpace(w.Name)
		if name == "" {
			name = "Workspace " + w.ID.String()
		}
		out = append(out, Workspace{ID: w.ID.String(), Name: name})
	}
	return out, nil
}

// personalName is the name to show for the person's own workspace: the one
// Drime has, else "<first name>'s workspace" from /users/me, else "My
// workspace".
func (a *api) personalName(ctx context.Context, name string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	var me struct {
		User struct {
			FirstName   string `json:"first_name"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	if err := a.call(ctx, http.MethodGet, "/users/me", nil, nil, &me, nil); err == nil {
		first := strings.TrimSpace(me.User.FirstName)
		if first == "" {
			if f := strings.Fields(me.User.DisplayName); len(f) > 0 {
				first = f[0]
			}
		}
		if first != "" {
			return first + "’s workspace"
		}
	}
	return "My workspace"
}

// Folders lists the names of the folders directly inside dir, a path from the
// top of the drive ("" for the top itself, "Backups/Laptop" for one inside),
// in cfg.WorkspaceID. A dir that is not there is ErrFolderNotFound.
func Folders(ctx context.Context, cfg Config, dir string) ([]string, error) {
	a, err := browser(cfg)
	if err != nil {
		return nil, err
	}
	parent := ""
	dir = strings.Trim(dir, "/")
	if dir != "" {
		for _, name := range strings.Split(dir, "/") {
			items, err := a.listFolder(ctx, parent, true)
			if err != nil {
				return nil, refused(err)
			}
			id := ""
			for _, it := range items {
				if decodeName(it.Name) == name {
					id = it.ID.String()
					break
				}
			}
			if id == "" {
				return nil, fmt.Errorf("%w: %s", ErrFolderNotFound, dir)
			}
			parent = id
		}
	}
	items, err := a.listFolder(ctx, parent, true)
	if err != nil {
		return nil, refused(err)
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		n := decodeName(it.Name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, nil
}
