package cli

import (
	"context"
	"errors"
	"os"
	"time"
)

// Updates and the account, for the panel. A newer release used to be
// mentioned only after a command finished in a terminal, so someone who only
// ever uses the taskbar app never heard of one; the account's sign-out and
// "save my keys for the next computer" had dashboard methods and no screen.

// trayUpdateEvery is how often the taskbar app asks whether there is a newer
// release on its own account: the same once a day as the command line.
const trayUpdateEvery = updateCheckEvery

// trayUpdateOnOpen is the least time between two checks made because the
// panel was opened. Opening the panel is when someone is there to say yes to
// an update, so it asks then; the gap keeps a burst of opens from spending
// GitHub's 60-requests-an-hour allowance for unsigned-in callers.
const trayUpdateOnOpen = 5 * time.Minute

// maybeCheckUpdate starts a check for a newer release when the last one was
// more than a day ago. It never waits on the network: State calls it, and
// State must be cheap.
func (t *trayBackend) maybeCheckUpdate(now time.Time) {
	t.checkUpdateIfOlder(now, trayUpdateEvery)
}

// checkUpdateIfOlder starts a background check when the last one is at least
// every old. It never waits on the network.
func (t *trayBackend) checkUpdateIfOlder(now time.Time, every time.Duration) {
	if !t.checkUpdates || os.Getenv(EnvNoUpdateCheck) != "" {
		return
	}
	t.mu.Lock()
	due := now.Sub(t.updateCheck) >= every
	if due {
		t.updateCheck = now
	}
	t.mu.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if v, err := t.d.CheckUpdate(ctx); err == nil {
			t.mu.Lock()
			t.update = v
			t.mu.Unlock()
		}
	}()
}

// updateCall serves the panel's update and account requests that are not
// sign-in itself.
func (t *trayBackend) updateCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "panelOpened":
		// The panel was just opened: look for a newer release so the page
		// can offer it while someone is there to answer.
		t.checkUpdateIfOlder(time.Now(), trayUpdateOnOpen)
		return nil, true, nil

	case "updateCheck":
		v, err := t.d.CheckUpdate(ctx)
		if err != nil {
			return nil, true, errors.New("could not check for an update: " + err.Error())
		}
		t.mu.Lock()
		t.update, t.updateCheck = v, time.Now()
		t.mu.Unlock()
		return map[string]string{"version": v, "current": Version}, true, nil

	case "updateApply":
		if why := t.busyReason(); why != "" {
			return nil, true, errors.New(why + " is running. Update when it has finished")
		}
		dl, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		v, err := t.d.ApplyUpdate(dl)
		if err != nil {
			return nil, true, errors.New("could not update, and nothing was changed: " + err.Error())
		}
		t.mu.Lock()
		t.update = ""
		t.mu.Unlock()
		// The taskbar app notices its program file was replaced and starts
		// the new one by itself (see tray.watchForUpdate).
		return map[string]string{"version": v}, true, nil

	}
	return nil, false, nil
}
