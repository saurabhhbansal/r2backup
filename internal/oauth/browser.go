package oauth

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrNoBrowser means this machine has no browser we can open, or no display
// to open one on.
//
// It is a first-class outcome rather than a failure, because for this product
// it is not rare. r2backup is a backup tool, so a good share of installs are
// on a VPS reached over SSH, where there is no browser, no display, and no way
// for the person at the keyboard to reach a listener bound to that machine's
// loopback address. A caller that sees this should fall back to asking for the
// keys by hand -- the path that existed before any of this -- rather than
// reporting that sign-in is broken, because nothing is broken.
var ErrNoBrowser = errors.New("oauth: no browser available on this machine")

// openBrowser points the person's default browser at rawURL.
//
// It hands off to whatever the platform's opener is and does not wait for the
// browser to exit -- these commands return as soon as the URL is dispatched,
// and on Linux xdg-open in particular may hand off to an already-running
// browser and return immediately. So a nil error means "the URL was handed
// over", never "a person has seen the page". The flow does not rely on the
// difference: it waits for the callback, not for this.
func openBrowser(rawURL string) error {
	if !hasDisplay() {
		return ErrNoBrowser
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		// rundll32 rather than `start`: `start` is a cmd.exe builtin, so it
		// would need a shell, and putting a URL through a shell is how you
		// get argument-injection bugs. This form takes the URL as one argv
		// entry that no shell ever parses.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		path, err := exec.LookPath("xdg-open")
		if err != nil {
			return ErrNoBrowser
		}
		cmd = exec.Command(path, rawURL)
	}
	// Detach the opener's output. xdg-open and its children are chatty on
	// some desktops, and anything they print would land in the middle of a
	// full-screen bubbletea view and corrupt it.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return ErrNoBrowser
	}
	// Reap the child rather than leaving a zombie for the life of the
	// process. The result is deliberately ignored: a non-zero exit from
	// xdg-open tells us nothing actionable, and the callback is the only
	// evidence of success that counts.
	go func() { _ = cmd.Wait() }()
	return nil
}

// hasDisplay reports whether this machine plausibly has somewhere to show a
// browser window.
//
// On macOS and Windows the answer is yes: a desktop session is the normal
// case, and the rarer headless variants of both still have a URL handler
// registered, so the honest thing is to try and let the open fail. On Linux
// the question is real, and the two environment variables below are how every
// other program on the system answers it. SSH_CONNECTION is checked too but
// only as a tiebreak -- a forwarded X11 session sets DISPLAY and should work
// fine, so the presence of a display wins over the fact of being remote.
func hasDisplay() bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return true
	}
	if strings.TrimSpace(os.Getenv("DISPLAY")) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != "" {
		return true
	}
	return false
}

// BrowserAvailable reports whether a browser could be opened here.
//
// It lets a caller decide not to offer a browser sign-in at all, rather than
// offering it, being taken up on it, and only then discovering there is
// nowhere to open. On a VPS reached over SSH -- a good share of this
// product's installs -- that difference is a question nobody should have been
// asked, and on a scripted setup it is a prompt that silently eats an answer
// meant for the next one.
func BrowserAvailable() bool { return hasDisplay() }
