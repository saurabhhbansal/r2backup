package tray

import (
	"context"
	"encoding/json"
)

// Backend is what the panel asks of the rest of the program. internal/cli
// implements it on top of the same code the terminal interface uses, so a
// button here and a key there do exactly the same thing.
type Backend interface {
	// State is the panel's data right now. It must be cheap: the panel
	// asks for it every couple of seconds while it is open.
	State(ctx context.Context) (State, error)

	// BackUp starts a backup of one folder, or of all of them when name is
	// empty, in the background. It returns once the run has started.
	BackUp(name string) error
	// Busy names a command this panel started that has not finished.
	Busy() string

	// AddFolder starts backing up path, everything in it.
	AddFolder(path string) error
	// SetSchedule turns automatic backups on, every so many minutes, or off.
	SetSchedule(ctx context.Context, on bool, everyMinutes int) error

	// Call answers one request from the page that wants a reply -- connect
	// storage, list files to restore, and so on. The method is a name, the
	// arguments are the page's JSON, and the answer is whatever marshals for
	// the page. The page draws all of it: nothing here opens a terminal.
	Call(ctx context.Context, method string, args json.RawMessage) (any, error)

	// StorageURL is where the backups can be seen in a browser: Drime's web
	// app, or the bucket in Cloudflare's dashboard.
	StorageURL() string
}
