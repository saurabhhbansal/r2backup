package cli

import (
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
//   - purposeBoth: back up to Drime and keep a second copy on S3-compatible
//     storage.
//   - purposeS3: back up straight to S3-compatible storage, no Drime.
const (
	purposeBoth = "both"
	purposeS3   = "s3"
)

// The wording both the panel and `r2b setup` use for the question.
const (
	purposeQuestion  = "What are you using r2backup for?"
	purposeBothTitle = "Drime, with a second copy"
	purposeBothText  = "Back up to Drime, and keep a second copy on S3-compatible storage: Cloudflare R2, Backblaze B2, Amazon S3 or another."
	purposeS3Title   = "S3 storage only"
	purposeS3Text    = "Back up straight to S3-compatible storage, without Drime."
)

// What a purpose still needs before it holds.
const (
	needDrime  = "drime"  // getting started: Drime first, then the second copy
	needS3     = "s3"     // S3 storage for backups to go to
	needSecond = "second" // a second copy
)

// parsePurpose accepts the stored values and a few spellings people type.
func parsePurpose(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "both", "primary", "primary+second", "drime", "drime+s3", "second-copy":
		return purposeBoth, true
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
// before the question existed or keys saved by an older copy: a second copy,
// or backups going to Drime, is the first; S3 storage alone is the second.
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

// currentPurpose is the purpose this computer works to: the one chosen, or
// the one its destinations show.
func currentPurpose(a *app) string {
	if p := storedPurpose(); p != "" {
		return p
	}
	k := localStoredKeys(a)
	return inferPurpose(k.Primary, k.Second)
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
	// Need: "drime" (getting started: Drime, then the second copy), "s3"
	// (S3 storage for backups), "second" (a second copy), or "".
	Need string `json:"need,omitempty"`
	// Moved names the S3 storage backups now go to, when switching to S3
	// storage only made the second copy the place backups go.
	Moved string `json:"moved,omitempty"`
}

// applyPurpose makes p this computer's purpose, changing destinations where
// that is safe and saying what to set up where it is not.
//
// Nothing set up yet: it is saved, and Need is what setup asks for next.
// Switching to the first with no second copy: Need is "second", and the
// purpose is saved once one is added (apply it again). Switching to S3 only:
// a Drime second copy is dropped (what is in Drime stays); backups going to
// Drime with an S3 second copy move to that S3 storage, and the Drime copy
// stays where it is; Drime alone needs S3 storage first.
func applyPurpose(a *app, p string) (purposeResult, error) {
	p, ok := parsePurpose(p)
	out := purposeResult{Purpose: p}
	if !ok {
		return out, errors.New("choose both (Drime with a second copy) or s3 (S3 storage only)")
	}
	k := localStoredKeys(a)
	if k.Primary == nil {
		out.Need = needDrime
		if p == purposeS3 {
			out.Need = needS3
		}
		out.Saved = true
		return out, savePurpose(p)
	}
	switch p {
	case purposeBoth:
		if k.Second == nil {
			out.Need = needSecond
			return out, nil
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

// forgetSecond stops keeping a second copy. What is there stays.
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
		Use:   "purpose [both|s3]",
		Short: "Show or change what r2backup is used for here",
		Long: "Without an argument, says what this computer is using r2backup for.\n\n" +
			"  r2b setup purpose both   " + purposeBothTitle + ": " + purposeBothText + "\n" +
			"  r2b setup purpose s3     " + purposeS3Title + ": " + purposeS3Text + "\n\n" +
			"Switching to s3 when backups go to Drime with an S3 second copy makes\n" +
			"that S3 storage where backups go; what is in Drime stays there.",
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
			if text := purposeS3Warning(a, args[0]); text != "" {
				ok, err := decide(opts, newPrompter(opts), "Use S3 storage only? "+text+"\nContinue? [y/N]", false)
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
			switch res.Need {
			case needSecond:
				// As the panel does: set the second copy up, then switch.
				if !interactive() && opts.In == nil {
					return errors.New("add a second copy first: r2b mirror setup, then run this again")
				}
				fmt.Fprintln(opts.Out, "First, the second copy.")
				if err := runMirrorSetup(cmd.Context(), a, opts, ""); err != nil {
					return err
				}
				if res, err = applyPurpose(a, args[0]); err != nil {
					return err
				}
				if res.Need != "" {
					return errors.New("add a second copy first: r2b mirror setup, then run this again")
				}
			case needS3:
				if res.Saved {
					break
				}
				return errors.New("backups go to Drime, with no S3 second copy to move to. Add one (r2b mirror setup) or connect S3 storage (r2b setup --keys --provider r2)")
			}
			if res.Moved != "" {
				fmt.Fprintf(opts.Out, "Backups go to %s now. What is in Drime stays there.\n", res.Moved)
			}
			if res.Saved && res.Need != "" {
				fmt.Fprintf(opts.Out, "%s. Next: r2b setup\n", purposeTitle(res.Purpose))
				return nil
			}
			fmt.Fprintf(opts.Out, "%s: %s\n", purposeTitle(res.Purpose), purposeLine(a, res.Purpose))
			_ = pushKeysQuietly(cmd, a)
			return nil
		},
	}
}

// purposeS3Warning is what the panel says before switching to S3 storage
// only, or "" when the switch moves or drops nothing (no Drime here).
func purposeS3Warning(a *app, to string) string {
	pp, ok := parsePurpose(to)
	if !ok || pp != purposeS3 || currentPurpose(a) == purposeS3 {
		return ""
	}
	k := localStoredKeys(a)
	p, s := k.Primary, k.Second
	if p != nil && !p.IsDrime() && !(s != nil && s.IsDrime()) {
		return ""
	}
	switch {
	case p != nil && p.IsDrime() && s != nil && !s.IsDrime():
		return "Backups go to " + destLabel(*s) + " from now on, with no second copy. What’s in Drime stays. " +
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
	line := "backups go to " + destLabel(*k.Primary)
	if k.Second != nil {
		line += ", second copy on " + destLabel(*k.Second)
	} else if p == purposeBoth {
		line += ", no second copy yet (r2b mirror setup)"
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
