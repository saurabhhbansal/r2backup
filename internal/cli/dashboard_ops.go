package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// Everything in this file is an operation the interface used to close itself
// for. Each one goes through the same package the matching command goes
// through -- backup.Run, restore.Run, sets.Store, account.Client -- so the two
// front doors cannot drift into doing different things.

func (d *dashboard) Scan(ctx context.Context, root string) (*scan.Result, error) {
	return scan.Walk(ctx, scan.Options{Root: root})
}

func (d *dashboard) Add(ctx context.Context, req ui.AddRequest) error {
	// Connected first, like `r2b add`. Otherwise the whole flow -- browse,
	// scan, pick, name -- succeeds on a machine with no credentials and only
	// the backup afterwards fails, telling the user to go and run a command.
	a, err := d.connected(ctx)
	if err != nil {
		return err
	}
	if err := sets.ValidName(req.Name); err != nil {
		return fmt.Errorf("%q: %w", req.Name, err)
	}
	if _, err := a.sets.Get(req.Name); err == nil {
		return fmt.Errorf("a folder called %q is already being backed up", req.Name)
	}
	root, err := filepath.Abs(req.Root)
	if err != nil {
		return err
	}
	s := sets.Set{
		Name: req.Name, Root: root, Machine: machineName(),
		Prefix:        "machines/" + machineName() + "/" + req.Name,
		Excludes:      req.Excludes,
		RetentionDays: req.Retention,
	}
	// The flag's default is DefaultRetentionDays, so 0 can only mean the
	// user asked for it. Say that in the one way sets.Add will not mistake
	// for "unset".
	if req.Retention <= 0 {
		s.RetentionDays = sets.RetentionDisabled
	}
	return a.sets.Add(s)
}

// Overlaps reports another folder already covering root, so the interface can
// say so before anything is uploaded.
//
// Overlapping folders are allowed -- each carries its own retention, so
// wanting one is reasonable -- but every file in the overlap is stored under
// two prefixes and paid for twice on every run that touches it. `r2b add`
// says this once, and on a tool whose whole argument is the operations
// budget, the interface has to as well.
func (d *dashboard) Overlaps(root string) (string, bool) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	other, ok := d.app.sets.Overlapping(abs)
	return other.Name, ok
}

func (d *dashboard) SetExcludes(ctx context.Context, name string, excludes []string) error {
	a := d.app
	s, err := a.sets.Get(name)
	if err != nil {
		return err
	}
	s.Excludes = excludes
	return a.sets.Update(s)
}

func (d *dashboard) Rename(ctx context.Context, from, to string) error {
	a := d.app
	// The index is keyed by set name too. It moves first: that is one bbolt
	// transaction and cannot half-happen, and if the set store then refuses
	// the new name the index goes back. The other order has no recovery --
	// it is what left a renamed set reading an empty index and re-uploading
	// everything.
	if err := a.index.RenameSet(from, to); err != nil {
		return err
	}
	if err := a.sets.Rename(from, to); err != nil {
		if back := a.index.RenameSet(to, from); back != nil {
			return fmt.Errorf("%w (and the index could not be put back under %q: %v)", err, from, back)
		}
		return err
	}
	return nil
}

func (d *dashboard) Relink(ctx context.Context, name, newRoot string) error {
	a := d.app
	return a.sets.Relink(name, newRoot)
}

// uiRestoreObserver adapts restore.Observer to the interface's callbacks.
type uiRestoreObserver struct {
	phase func(string)
	snap  func(progress.Snapshot)
	set   string
}

func (o *uiRestoreObserver) Phase(p restore.Phase, r *restore.Report) {
	switch p {
	case restore.PhaseListing:
		o.phase("listing " + o.set)
	case restore.PhasePlanning:
		o.phase(fmt.Sprintf("planning · %s files · %s",
			progress.FormatCount(r.ListedFiles), progress.FormatBytes(r.ListedBytes)))
	case restore.PhaseDownloading:
		o.phase("restoring into " + r.Target)
	}
}

func (o *uiRestoreObserver) Progress(s progress.Snapshot) { o.snap(s) }

func (d *dashboard) Restore(ctx context.Context, req ui.RestoreRequest, phase func(string), snap func(progress.Snapshot)) (ui.RestoreResult, error) {
	a, err := d.connected(ctx)
	if err != nil {
		return ui.RestoreResult{}, err
	}
	// Local record first, bucket second -- the same lookup `r2b restore`
	// makes, so a computer that has never run add can restore here too.
	s, err := resolveRestoreSet(ctx, a, req.Set, req.Machine)
	if err != nil {
		return ui.RestoreResult{}, err
	}
	rep, err := restore.Run(ctx, restore.Options{
		Set: s, Client: a.client, Target: req.To, Only: req.Only,
		SourceMachine: req.Machine, Deleted: req.Deleted,
		Overwrite: req.Overwrite, Verify: req.Verify,
		Observer: &uiRestoreObserver{phase: phase, snap: snap, set: s.Name},
	})
	if err != nil {
		if errors.Is(err, restore.ErrNoTarget) {
			return ui.RestoreResult{}, errors.New("that folder is not on this computer, so it needs a destination")
		}
		return ui.RestoreResult{}, err
	}
	// A restore that found nothing must not read as a restore that worked.
	// This is the worst possible answer to "is my data there?", which is why
	// the command errors on it -- and why the interface has to as well, since
	// its result line is a muted notice that nobody reads twice.
	if rep.ListedFiles == 0 {
		return ui.RestoreResult{}, fmt.Errorf("nothing is stored for %q, so nothing was restored", s.Name)
	}
	if req.Only != "" && rep.Downloaded == 0 && rep.SkippedExisting == 0 && len(rep.Failures) == 0 {
		return ui.RestoreResult{}, fmt.Errorf(
			"%q matched none of the %s files stored for %s, so nothing was restored.\n"+
				"  A bare name takes everything under it (docs), and * does not cross a / (use docs/**)",
			req.Only, progress.FormatCount(rep.ListedFiles), s.Name)
	}
	if n := len(rep.Failures); n > 0 {
		return ui.RestoreResult{}, fmt.Errorf("%s restored, but %d file(s) did not: %v",
			progress.FormatCount(int64(rep.Downloaded)), n, rep.Failures[0].Err)
	}
	if n := len(rep.VerifyMismatches); n > 0 {
		return ui.RestoreResult{}, fmt.Errorf("%d file(s) did NOT match after writing, starting with %s",
			n, rep.VerifyMismatches[0])
	}
	return ui.RestoreResult{
		Files: rep.Downloaded, Bytes: rep.Bytes, Target: rep.Target,
		Skipped: rep.SkippedExisting, Failed: len(rep.Failures),
	}, nil
}

// Objects is `r2b ls <set>`: what is stored, and how big each one is.
//
// Read from the local index, which is the record of what was uploaded, so
// opening this list costs nothing. Largest first, because the question people
// actually bring to it is which files are worth the bill.
func (d *dashboard) Objects(ctx context.Context, name string) ([]ui.ObjectRow, error) {
	recs, err := d.app.index.All(name)
	if err != nil {
		return nil, err
	}
	rows := make([]ui.ObjectRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, ui.ObjectRow{Key: r.Key, Size: r.Size})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Size != rows[j].Size {
			return rows[i].Size > rows[j].Size
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, nil
}

// RemoteSets lists every backup in the bucket, this computer's and everyone
// else's.
//
// It is what makes `restore --machine` reachable without a command. A machine
// that has just signed in has working credentials and an empty sets.json, so
// the Folders tab is empty and there is nothing on screen naming the data that
// is sitting in the bucket -- which is the whole reason someone signs in on a
// second computer. This asks the bucket.
func (d *dashboard) RemoteSets(ctx context.Context) ([]ui.RemoteSet, error) {
	a, err := d.connected(ctx)
	if err != nil {
		return nil, err
	}
	found, err := discoverBackups(ctx, a)
	if err != nil {
		return nil, err
	}
	me := machineName()
	out := make([]ui.RemoteSet, 0, len(found))
	for _, f := range found {
		here := false
		if f.Machine == me {
			if _, err := a.sets.Get(f.Name); err == nil {
				here = true
			}
		}
		out = append(out, ui.RemoteSet{Name: f.Name, Machine: f.Machine, Here: here})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// --- account ---

func (d *dashboard) Account(ctx context.Context) (ui.AccountView, error) {
	var v ui.AccountView
	token, err := account.LoadToken()
	if err != nil {
		return v, err
	}
	if token == "" {
		v.Reachable = true
		return v, nil
	}
	c := accountClient()
	// Devices doubles as the "is this session still good" check: it is the
	// cheapest authenticated call, and a token that has expired has to read
	// as signed out rather than as an error the user cannot act on.
	devices, err := c.ListDevices(ctx, token)
	if err != nil {
		if errors.Is(err, account.ErrUnauthorized) {
			_ = account.ClearToken()
			v.Reachable = true
			return v, nil
		}
		v.Err = "Could not reach the account service: " + err.Error()
		return v, nil
	}
	v.SignedIn, v.Reachable = true, true
	v.Email = account.EmailFromToken(token)
	me := machineName()
	for _, dev := range devices {
		v.Devices = append(v.Devices, ui.DeviceView{
			Name: dev.DeviceName, OS: dev.OS,
			LastSeen: time.UnixMilli(dev.LastSeen), This: dev.DeviceName == me,
		})
	}
	if _, err := c.GetVault(ctx, token); err == nil {
		v.VaultStored = true
	} else if !errors.Is(err, account.ErrNotFound) {
		v.Err = err.Error()
	}
	return v, nil
}

func (d *dashboard) SignInStart(ctx context.Context, email string) error {
	return accountClient().RequestCode(ctx, email)
}

func (d *dashboard) SignInVerify(ctx context.Context, email, code string) error {
	token, err := accountClient().Verify(ctx, email, code)
	if err != nil {
		if errors.Is(err, account.ErrInvalidCode) {
			return errors.New("that code is not right, or it has expired")
		}
		return err
	}
	if err := account.SaveToken(token); err != nil {
		return err
	}
	_ = accountClient().RegisterDevice(ctx, token, machineName(), runtime.GOOS)
	return nil
}

func (d *dashboard) SignOut(ctx context.Context) error { return account.ClearToken() }

func (d *dashboard) UnlockVault(ctx context.Context, password string) error {
	token, err := requireAccountToken()
	if err != nil {
		return err
	}
	vault, err := accountClient().GetVault(ctx, token)
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			return errors.New("no credentials are stored for this account yet")
		}
		return err
	}
	plain, err := account.Decrypt(password, vault)
	if err != nil {
		// Worth spelling out: "wrong password" on a backup tool reads like
		// lost data and is not. It guards the stored copy of the keys, not
		// one byte of anyone's files.
		return errors.New("that password does not open the saved credentials. Nothing is lost if you have " +
			"forgotten it -- it protects the stored keys, not your files. Press k to enter your R2 keys instead")
	}
	var c creds.Credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return fmt.Errorf("the saved credentials could not be read: %w", err)
	}
	a := d.app
	if err := a.creds.Save(c); err != nil {
		return err
	}
	return checkBucketReachable(ctx, a)
}

func (d *dashboard) StoreVault(ctx context.Context, password string) error {
	token, err := requireAccountToken()
	if err != nil {
		return err
	}
	a, err := d.connected(ctx)
	if err != nil {
		return err
	}
	c, err := a.creds.Load()
	if err != nil {
		return errors.New("there are no credentials on this computer to save yet")
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// Checked before they are pushed, never after. Storing keys that do not
	// work hands every later machine the same broken setup, and the failure
	// surfaces there rather than here where it can still be corrected.
	if err := checkBucketReachable(ctx, a); err != nil {
		return err
	}
	vault, err := account.Encrypt(password, plain)
	if err != nil {
		return err
	}
	return accountClient().PutVault(ctx, token, vault)
}

func (d *dashboard) SaveKeys(ctx context.Context, k ui.Keys) error {
	a := d.app
	c := creds.Credentials{
		AccountID:       strings.TrimSpace(k.AccountID),
		AccessKeyID:     strings.TrimSpace(k.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(k.Secret),
		Bucket:          strings.TrimSpace(k.Bucket),
	}
	if err := c.Valid(); err != nil {
		return err
	}
	if err := a.creds.Save(c); err != nil {
		return err
	}
	// Checked before anything is built on them, so a typo is caught while it
	// is still on screen rather than at the first backup.
	return checkBucketReachable(ctx, a)
}

func checkBucketReachable(ctx context.Context, a *app) error {
	if err := a.connect(ctx); err != nil {
		return err
	}
	if _, err := a.client.List(ctx, "r2backup/"); err != nil {
		return fmt.Errorf("credentials saved, but the bucket could not be read: %w", err)
	}
	return nil
}

func requireAccountToken() (string, error) {
	token, err := account.LoadToken()
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("not signed in on this computer")
	}
	return token, nil
}

// --- update ---

func (d *dashboard) CheckUpdate(ctx context.Context) (string, error) {
	rel, err := selfupdate.Latest(ctx, Repo)
	if err != nil {
		return "", err
	}
	if !selfupdate.Newer(Version, rel.Version) {
		return "", nil
	}
	return rel.Version, nil
}

func (d *dashboard) ApplyUpdate(ctx context.Context) (string, error) {
	rel, err := selfupdate.Latest(ctx, Repo)
	if err != nil {
		return "", err
	}
	bin, err := selfupdate.Fetch(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := selfupdate.Apply(bin, ""); err != nil {
		return "", err
	}
	return rel.Version, nil
}
