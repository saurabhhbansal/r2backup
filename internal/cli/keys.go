package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
)

// Using r2backup without an account is the first option offered: the storage
// keys stay on this computer, and an exported key file -- encrypted under a
// passphrase -- is how they reach another one. Signing in to the hosted
// account is the second option.

const (
	accountModeLocal   = "local"
	accountModeAccount = "account"
)

// setAccountMode records how this computer keeps its keys.
func setAccountMode(mode string) error {
	s, err := config.LoadSettings()
	if err != nil {
		return err
	}
	if s.AccountMode == mode {
		return nil
	}
	s.AccountMode = mode
	return config.SaveSettings(s)
}

// accountMode is how this computer keeps its keys: "account" whenever it is
// signed in, otherwise what was chosen ("local" or "").
func accountMode() string {
	if tok, err := account.LoadToken(); err == nil && tok != "" {
		return accountModeAccount
	}
	s, err := config.LoadSettings()
	if err != nil || s.AccountMode == accountModeAccount {
		return ""
	}
	return s.AccountMode
}

// exportKeys writes this computer's storage keys to path, sealed under
// passphrase. It refuses to overwrite a file that is there already.
func exportKeys(a *app, path, passphrase string) error {
	data, err := account.ExportKeys(passphrase, localStoredKeys(a))
	if err != nil {
		return err
	}
	if path == "" {
		return errors.New("choose where to save the key file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s is there already. Choose another name", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// importedKeys says what an import changed.
type importedKeys struct {
	Primary string `json:"primary,omitempty"`
	Second  string `json:"second,omitempty"`
	Source  string `json:"source,omitempty"`
}

// importKeys opens a key file and saves its keys on this computer, replacing
// any already here. Where backups go is checked before it is saved. A caller
// holding a connected client drops it afterwards (dashboard.forgetClient).
func importKeys(ctx context.Context, a *app, path, passphrase string) (importedKeys, error) {
	var out importedKeys
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	k, err := account.ImportKeys(passphrase, data)
	if errors.Is(err, account.ErrDecryptFailed) {
		return out, errors.New("that passphrase does not open this key file")
	}
	if err != nil {
		return out, err
	}
	if k.Primary != nil {
		if err := reachable(ctx, *k.Primary); err != nil {
			return out, fmt.Errorf("the key file's storage keys do not work: %w", err)
		}
		if err := a.creds.Save(*k.Primary); err != nil {
			return out, err
		}
		out.Primary = destLabel(*k.Primary)
	}
	if k.Second != nil {
		if err := a.mirror.Save(*k.Second); err != nil {
			return out, err
		}
		out.Second = destLabel(*k.Second)
	}
	if k.Source != nil {
		if err := a.source.Save(*k.Source); err != nil {
			return out, err
		}
		out.Source = destLabel(*k.Source)
	}
	if accountMode() == "" {
		_ = setAccountMode(accountModeLocal)
	}
	return out, nil
}

func newKeysCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Move storage keys to another computer without an account",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "export <file>",
		Short: "Save this computer's storage keys to an encrypted file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("keys export needs a terminal for the passphrase")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			p := newPrompter(opts)
			pass, err := p.secret(fmt.Sprintf("Passphrase for the file (%d+ characters)", account.MinKeyFilePassphrase))
			if err != nil {
				return err
			}
			again, err := p.secret("Again")
			if err != nil {
				return err
			}
			if pass != again {
				return errors.New("those did not match")
			}
			if err := exportKeys(a, args[0], pass); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Saved to %s. On the other computer: r2b keys import %s\n", args[0], filepath.Base(args[0]))
			fmt.Fprintln(opts.Out, "Anyone with this file and the passphrase can reach your backups. Delete it once it is imported.")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "import <file>",
		Short: "Use the storage keys in a file made with keys export",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("keys import needs a terminal for the passphrase")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			pass, err := newPrompter(opts).secret("Passphrase")
			if err != nil {
				return err
			}
			got, err := importKeys(cmd.Context(), a, args[0], pass)
			if err != nil {
				return err
			}
			if got.Primary != "" {
				fmt.Fprintf(opts.Out, "Backups go to %s.\n", got.Primary)
			}
			if got.Second != "" {
				fmt.Fprintf(opts.Out, "Second copy: %s.\n", got.Second)
			}
			if got.Source != "" {
				fmt.Fprintf(opts.Out, "Drime mirrored from: %s.\n", got.Source)
			}
			return nil
		},
	})
	return cmd
}
