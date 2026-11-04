package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// What someone is using r2backup for, asked right after getting started
// (with an account or without one). It decides which destinations setup asks
// for, and it travels with the keys to the person's other computers.
//
//   - purposeBoth: r2backup backs up to Drime (the primary backup) and keeps
//     a secondary backup on S3 storage.
//   - purposeCopy: Drime's own app backs up to Drime; r2backup copies chosen
//     Drime folders to S3 storage (the secondary backup only). The S3
//     storage is kept where backups go (a.creds), so what is copied there can
//     be browsed and restored like any backup; the Drime token is a.source,
//     and the folders are the Drime folders (mirror_sources.go).
//   - purposeS3: back up straight to S3 storage, no Drime. No longer offered
//     to new computers; a computer set up for it keeps working as it did.
const (
	purposeBoth = "both"
	purposeCopy = "copy"
	purposeS3   = "s3"
)

// The wording both the panel and `r2b setup` use for the question.
const (
	purposeQuestion  = "What are you using r2backup for?"
	purposeBothTitle = "Primary and secondary backup"
	purposeBothText  = "Back up to Drime, with a secondary backup on S3 storage."
	purposeCopyTitle = "Secondary backup only"
	purposeCopyText  = "You back up to Drime with Drime’s app. r2backup copies those backups to S3 storage."
	purposeS3Title   = "S3 storage only"
	purposeS3Text    = "Back up straight to S3 storage, without Drime."
)

// What a purpose still needs before it holds.
const (
	needDrime   = "drime"   // Drime first: getting started, or S3 storage here becoming the secondary backup
	needS3      = "s3"      // S3 storage for backups (or the Drime copies) to go to
	needSecond  = "second"  // a secondary backup
	needFolders = "folders" // Drime folders to copy (saved: the purpose holds already)
)

// parsePurpose accepts the stored values and a few spellings people type.
func parsePurpose(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "both", "primary", "primary+second", "primary+secondary", "drime", "drime+s3", "second-copy":
		return purposeBoth, true
	case "copy", "secondary", "secondary-only", "secondary-backup-only", "drime-app", "copy-drime":
		return purposeCopy, true
	case "s3", "s3-only", "s3only", "only-s3":
		return purposeS3, true
	}
	return "", false
}

// purposeTitle names a purpose the way the question does.
func purposeTitle(p string) string {
	switch p {
	case purposeBoth:
		return purposeBothTitle
	case purposeCopy:
		return purposeCopyTitle
	case purposeS3:
		return purposeS3Title
	}
	return ""
}

// purposeText is a purpose's one-line description, as the question gives it.
func purposeText(p string) string {
	switch p {
	case purposeBoth:
		return purposeBothText
	case purposeCopy:
		return purposeCopyText
	case purposeS3:
		return purposeS3Text
	}
	return ""
}

// savePurpose records the purpose on this computer.
func savePurpose(p string) error {
	s, err := config.LoadSettings()
	if err != nil {
		return err
	}
	if s.Purpose == p {
		return nil
	}
	s.Purpose = p
	return config.SaveSettings(s)
}

// storedPurpose is what was chosen on this computer, "" for nothing yet.
func storedPurpose() string {
	s, err := config.LoadSettings()
	if err != nil {
		return ""
	}
	p, _ := parsePurpose(s.Purpose)
	return p
}

// inferPurpose reads a purpose off the destinations, for a computer set up
// before the question existed or keys saved by an older copy: a secondary
// backup, or backups going to Drime, is the first; S3 storage alone is S3
// storage only.
func inferPurpose(primary, second *creds.Credentials) string {
	switch {
	case primary == nil:
		return ""
	case second != nil || primary.IsDrime():
		return purposeBoth
	default:
		return purposeS3
	}
}

// inferLocalPurpose is inferPurpose on this computer, where S3 storage alone
// with Drime folders being copied to it is a secondary backup only.
func inferLocalPurpose(primary, second *creds.Credentials) string {
	p := inferPurpose(primary, second)
	if p == purposeS3 {
		if st, err := config.LoadSettings(); err == nil && len(st.MirrorSources) > 0 {
			return purposeCopy
		}
	}
	return p
}

// currentPurpose is the purpose this computer works to: the one chosen, or
// the one its destinations show.
func currentPurpose(a *app) string {
	if p := storedPurpose(); p != "" {
		return p
	}
	k := localStoredKeys(a)
	return inferLocalPurpose(k.Primary, k.Second)
}

// adoptPurpose takes a purpose that came with keys from the account or a
// key file, unless this computer already chose one.
func adoptPurpose(p string) {
	if p, ok := parsePurpose(p); ok && storedPurpose() == "" {
		_ = savePurpose(p)
	}
}

// purposeResult says what applying a purpose did, or what it still needs.
type purposeResult struct {
	Purpose string `json:"purpose"`
	// Saved is false when Need names something to set up first.
	Saved bool `json:"saved"`
	// Need: "drime" (Drime first), "s3" (S3 storage), "second" (a secondary
	// backup), "folders" (Drime folders to copy; saved already), or "".
	Need string `json:"need,omitempty"`
	// Moved names the S3 storage backups now go to, when the switch made
	// the secondary backup the place backups go.
	Moved string `json:"moved,omitempty"`
}

// applyPurpose makes p this computer's purpose, changing destinations where
// that is safe and saying what to set up where it is not.
//
// Nothing set up yet: it is saved, and Need is what setup asks for next.
//
// Primary and secondary backup: with Drime and no secondary backup, Need is
// "second"; with S3 storage and nothing else, Need is "drime": Drime becomes
// the primary backup and the S3 storage the secondary (promoteToDrime). Apply
// it again once that is done.
//
// Secondary backup only: backups going to Drime with an S3 secondary backup
// move to that S3 storage, and the Drime token is kept to read the Drime
// folders with; Drime alone needs S3 storage first. Need is "folders" when no
// Drime folders are chosen yet.
//
// S3 storage only (older computers): a Drime secondary backup is dropped;
// backups going to Drime with an S3 secondary backup move to it.
func applyPurpose(a *app, p string) (purposeResult, error) {
	p, ok := parsePurpose(p)
	out := purposeResult{Purpose: p}
	if !ok {
		return out, errors.New("choose both (primary and secondary backup) or copy (secondary backup only)")
	}
	k := localStoredKeys(a)
	if k.Primary == nil {
		out.Need = needDrime
		if p != purposeBoth {
			out.Need = needS3
		}
		out.Saved = true
		return out, savePurpose(p)
	}
	switch p {
	case purposeBoth:
		if k.Second == nil {
			out.Need = needSecond
			if !k.Primary.IsDrime() {
				out.Need = needDrime
			}
			return out, nil
		}
	case purposeCopy:
		switch {
		case k.Primary.IsDrime() && k.Second != nil && k.Second.IsS3():
			if err := keepDrimeToRead(a, *k.Primary); err != nil {
				return out, err
			}
			if err := a.creds.Save(*k.Second); err != nil {
				return out, err
			}
			if err := forgetSecond(a); err != nil {
				return out, err
			}
			out.Moved = destLabel(*k.Second)
		case k.Primary.IsDrime():
			// The S3 storage connected next takes Drime's place: its token
			// is kept now, to read the Drime folders with.
			out.Need = needS3
			return out, keepDrimeToRead(a, *k.Primary)
		case k.Second != nil && k.Second.IsDrime():
			if err := keepDrimeToRead(a, *k.Second); err != nil {
				return out, err
			}
			if err := forgetSecond(a); err != nil {
				return out, err
			}
		}
		if st, err := config.LoadSettings(); err == nil && len(st.MirrorSources) == 0 {
			out.Need = needFolders
		}
	case purposeS3:
		switch {
		case k.Primary.IsS3() && k.Second != nil && k.Second.IsDrime():
			if err := forgetSecond(a); err != nil {
				return out, err
			}
		case k.Primary.IsDrime() && k.Second != nil && k.Second.IsS3():
			if err := a.creds.Save(*k.Second); err != nil {
				return out, err
			}
			if err := forgetSecond(a); err != nil {
				return out, err
			}
			out.Moved = destLabel(*k.Second)
		case k.Primary.IsDrime():
			out.Need = needS3
			return out, nil
		}
	}
	out.Saved = true
	return out, savePurpose(p)
}

// keepDrimeToRead keeps a Drime token for reading the Drime folders with,
// unless one is kept already.
func keepDrimeToRead(a *app, c creds.Credentials) error {
	if a.source.Exists() {
		return nil
	}
	return a.source.Save(creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: c.DrimeToken,
		DrimeWorkspace: c.DrimeWorkspace, Endpoint: c.Endpoint})
}

// promoteToDrime makes Drime (c) where backups go, and the S3 storage they
// went to the secondary backup, copied to after every backup. Drime is
// checked before anything changes.
func promoteToDrime(ctx context.Context, a *app, c creds.Credentials) error {
	if !c.IsDrime() {
		return errors.New("the primary backup goes to Drime")
	}
	if err := c.Valid(); err != nil {
		return err
	}
	old, err := a.creds.Load()
	if err != nil || old.IsDrime() {
		return errors.New("backups do not go to S3 storage on this computer")
	}
	if a.mirror.Exists() {
		return errors.New("there is a secondary backup already")
	}
	if err := secondCheck(ctx, c); err != nil {
		return err
	}
	if err := a.mirror.Save(old); err != nil {
		return err
	}
	if err := a.creds.Save(c); err != nil {
		_ = a.mirror.Delete()
		return err
	}
	return setMirrorAuto(true)
}

// forgetSecond stops keeping a secondary backup. What is there stays.
func forgetSecond(a *app) error {
	if err := setMirrorAuto(false); err != nil {
		return err
	}
	if err := a.mirror.Delete(); err != nil && !errors.Is(err, creds.ErrNotFound) {
		return err
	}
	return nil
}

// newPurposeCmd builds `setup purpose`: what this computer is using r2backup for,
// and changing it.
func newPurposeCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "purpose [both|copy]",
		Short: "Show or change what r2backup is used for here",
		Long: "Without an argument, says what this computer is using r2backup for.\n\n" +
			"  r2b setup purpose both   " + purposeBothTitle + ": " + purposeBothText + "\n" +
			"  r2b setup purpose copy   " + purposeCopyTitle + ": " + purposeCopyText + "\n\n" +
			"Switching to copy when backups go to Drime with an S3 secondary backup\n" +
			"makes that S3 storage where the copies go; what is in Drime stays there.\n" +
			"Choose the Drime folders to copy with r2b mirror drime add.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if len(args) == 0 {
				p := currentPurpose(a)
				if p == "" {
					fmt.Fprintln(opts.Out, "Not chosen yet. Run: r2b setup")
					return nil
				}
				fmt.Fprintf(opts.Out, "%s: %s\n", purposeTitle(p), purposeLine(a, p))
				return nil
			}
			if text := purposeWarning(a, args[0]); text != "" {
				title := "Use S3 storage only?"
				if pp, _ := parsePurpose(args[0]); pp == purposeCopy {
					title = "Use r2backup for a secondary backup only?"
				}
				ok, err := decide(opts, newPrompter(opts), title+" "+text+"\nContinue? [y/N]", false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(opts.Out, "Nothing changed.")
					return nil
				}
			}
			res, err := applyPurpose(a, args[0])
			if err != nil {
				return err
			}
			switch {
			case res.Need == needSecond:
				// As the panel does: set the secondary backup up, then switch.
				if !interactive() && opts.In == nil {
					return errors.New("add a secondary backup first: r2b mirror setup, then run this again")
				}
				fmt.Fprintln(opts.Out, "First, the secondary backup.")
				if err := runMirrorSetup(cmd.Context(), a, opts, ""); err != nil {
					return err
				}
				if res, err = applyPurpose(a, args[0]); err != nil {
					return err
				}
				if res.Need != "" {
					return errors.New("add a secondary backup first: r2b mirror setup, then run this again")
				}
			case res.Need == needDrime && !res.Saved:
				// As the panel does: Drime becomes the primary backup, and
				// the S3 storage here the secondary.
				old, _ := a.creds.Load()
				if !interactive() && opts.In == nil {
					return errors.New("connect Drime first, in a terminal: r2b setup purpose both")
				}
				fmt.Fprintf(opts.Out, "First Drime, the primary backup. %s becomes the secondary backup.\n", destLabel(old))
				c, err := askForKind(cmd.Context(), newPrompter(opts), creds.ProviderDrime)
				if err != nil {
					return err
				}
				if err := promoteToDrime(cmd.Context(), a, c); err != nil {
					return err
				}
				if res, err = applyPurpose(a, args[0]); err != nil {
					return err
				}
			case res.Need == needS3 && !res.Saved:
				return errors.New("backups go to Drime, with no S3 secondary backup to move to. Add one first (r2b mirror setup), then run this again")
			}
			if res.Moved != "" {
				fmt.Fprintf(opts.Out, "Backups go to %s now. What is in Drime stays there.\n", res.Moved)
			}
			if res.Saved && res.Need != "" && res.Need != needFolders {
				fmt.Fprintf(opts.Out, "%s. Next: r2b setup\n", purposeTitle(res.Purpose))
				return nil
			}
			fmt.Fprintf(opts.Out, "%s: %s\n", purposeTitle(res.Purpose), purposeLine(a, res.Purpose))
			_ = pushKeysQuietly(cmd, a)
			return nil
		},
	}
}

// purposeWarning is what the panel says before switching away from Drime
// (to a secondary backup only, or S3 storage only), or "" when the switch
// moves or drops nothing.
func purposeWarning(a *app, to string) string {
	pp, ok := parsePurpose(to)
	if !ok || pp == purposeBoth || currentPurpose(a) == pp {
		return ""
	}
	k := localStoredKeys(a)
	p, s := k.Primary, k.Second
	if p != nil && !p.IsDrime() && !(s != nil && s.IsDrime()) {
		return ""
	}
	if pp == purposeCopy {
		switch {
		case p != nil && p.IsDrime() && s != nil && !s.IsDrime():
			return "r2backup stops backing up to Drime. " + destLabel(*s) + " keeps the secondary backup, and gets the Drime folders you choose. " +
				"What’s in Drime stays. If the copy there is behind, copy now first (r2b mirror)."
		case s != nil && s.IsDrime():
			return "Nothing more is copied to " + destLabel(*s) + ". What’s there stays."
		}
		return "Next, choose the S3 storage for the secondary backup. r2backup stops backing up to Drime. What’s in Drime stays."
	}
	switch {
	case p != nil && p.IsDrime() && s != nil && !s.IsDrime():
		return "Backups go to " + destLabel(*s) + " from now on, with no secondary backup. What’s in Drime stays. " +
			"If the copy there is behind, copy now first (r2b mirror)."
	case s != nil && s.IsDrime():
		return "Nothing more is copied to " + destLabel(*s) + ". What’s there stays."
	}
	return "Next, choose the S3 storage backups go to. What’s in Drime stays."
}

// purposeLine describes the destinations behind a purpose.
func purposeLine(a *app, p string) string {
	k := localStoredKeys(a)
	if k.Primary == nil {
		return "nothing connected yet"
	}
	if p == purposeCopy {
		line := "Drime folders copied to " + destLabel(*k.Primary)
		if st, err := config.LoadSettings(); err == nil && len(st.MirrorSources) == 0 {
			line += ", none chosen yet (r2b mirror drime add <folder>)"
		} else if err == nil {
			names := make([]string, 0, len(st.MirrorSources))
			for _, s := range st.MirrorSources {
				names = append(names, s.Folder)
			}
			line += ": " + strings.Join(names, ", ")
		}
		return line
	}
	line := "backups go to " + destLabel(*k.Primary)
	if k.Second != nil {
		line += ", secondary backup on " + destLabel(*k.Second)
	} else if p == purposeBoth {
		line += ", no secondary backup yet (r2b mirror setup)"
	}
	return line
}

// pushKeysQuietly saves this computer's keys, purpose included, to the
// account when signed in. Not signed in is not a failure.
func pushKeysQuietly(cmd *cobra.Command, a *app) error {
	token, key, err := accountSession()
	if err != nil {
		return nil
	}
	k := localStoredKeys(a)
	if k.Primary == nil {
		return nil
	}
	return accountClient().PutKeys(cmd.Context(), token, key, k)
}
