package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/ultrafast"
)

// Ultra fast mode (experimental): Drime files copied to S3 storage cloud to
// cloud by a Worker in the user's own Cloudflare account. One switch covers
// the second copy and Drime folders alike -- both are Drime to S3 storage.
// See internal/ultrafast.

// ultraOptions points Ultra fast mode somewhere else. Tests set it.
var ultraOptions ultrafast.Options

// ultraReadyWait is how long turning it on waits for the Worker to answer.
var ultraReadyWait = 45 * time.Second

// relayFor is the relay for a copy from src to dst, or nil when Ultra fast
// mode is off or the copy is not Drime to S3 storage.
func relayFor(a *app, src, dst creds.Credentials) mirror.Relay {
	if a.ultra == nil || !src.IsDrime() || !dst.IsS3() {
		return nil
	}
	st, on, err := a.ultra.Load()
	if err != nil || !on {
		return nil
	}
	c := ultrafast.NewCopier(st, a.ultra, ultraOptions)
	if dst.Bundle {
		// Hybrid: files a pack takes are packed here, as always.
		c.MinSize = bundle.DefaultSmallMax + 1
	}
	return c
}

// mirrorRelay is the relay for the second copy, in the direction asked for.
func mirrorRelay(a *app, reverse bool) mirror.Relay {
	main, err := a.creds.Load()
	if err != nil {
		return nil
	}
	second, err := a.mirror.Load()
	if err != nil {
		return nil
	}
	if reverse {
		return relayFor(a, second, main)
	}
	return relayFor(a, main, second)
}

// relayReason is why Ultra fast mode stopped, in words.
func relayReason(err error) string {
	return strings.TrimPrefix(err.Error(), mirror.ErrRelayStopped.Error()+": ")
}

// relayLines is what a report says about Ultra fast mode, if it took part.
func relayLines(r *mirror.Report) []string {
	var out []string
	if r.Relayed > 0 {
		out = append(out, fmt.Sprintf("Ultra fast mode: %d file(s), %s, went cloud to cloud.", r.Relayed, progress.FormatBytes(r.RelayedBytes)))
	}
	switch {
	case r.RelayStopped != nil:
		out = append(out, "Ultra fast mode stopped: "+relayReason(r.RelayStopped)+". The rest went through this PC.")
	case r.RelayFellBack > 0:
		out = append(out, fmt.Sprintf("%d file(s) went through this PC because the Worker could not copy them.", r.RelayFellBack))
	}
	return out
}

// ultraInfoOut is Ultra fast mode's state, for the panel and `r2b mirror ultra`.
type ultraInfoOut struct {
	On        bool   `json:"on"`
	Account   string `json:"account,omitempty"`
	Worker    string `json:"worker,omitempty"`
	Since     string `json:"since,omitempty"`
	Note      string `json:"note"`
	TokenHelp string `json:"tokenHelp"`
	// Applies is whether this computer copies from Drime to S3 storage at
	// all: a second copy that way, or Drime folders.
	Applies bool `json:"applies"`
	// Accounts is set when a token reaches several accounts and one has to
	// be chosen.
	Accounts []ultrafast.Account `json:"accounts,omitempty"`
	// Warming is set right after turning on, when the Worker's new address
	// does not answer yet.
	Warming bool `json:"warming,omitempty"`
}

func ultraInfo(a *app) ultraInfoOut {
	out := ultraInfoOut{Note: ultrafast.Note, TokenHelp: ultrafast.TokenHelp}
	if st, on, err := a.ultra.Load(); err == nil && on {
		out.On = true
		out.Account = st.AccountName
		if out.Account == "" {
			out.Account = st.AccountID
		}
		out.Worker = st.Script
		out.Since = time.Unix(st.Since, 0).Format(time.RFC3339)
	}
	main, mErr := a.creds.Load()
	second, sErr := a.mirror.Load()
	out.Applies = mErr == nil && sErr == nil && main.IsDrime() && second.IsS3()
	if !out.Applies {
		if _, err := s3Destination(a); err == nil {
			if _, err := drimeSourceCreds(a); err == nil {
				out.Applies = true
			}
		}
	}
	return out
}

// ultraOn sets Ultra fast mode up with token. A token reaching several
// accounts answers with them in Accounts and sets nothing up, so the caller
// can ask which.
func ultraOn(ctx context.Context, a *app, token, account string) (ultraInfoOut, error) {
	st, err := ultrafast.Enable(ctx, a.ultra, token, account, ultraOptions)
	var need *ultrafast.NeedAccount
	if errors.As(err, &need) {
		out := ultraInfo(a)
		out.Accounts = need.Accounts
		return out, nil
	}
	if err != nil {
		return ultraInfo(a), err
	}
	out := ultraInfo(a)
	if err := ultrafast.Ready(ctx, st, ultraReadyWait, ultraOptions); err != nil {
		out.Warming = true
	}
	return out, nil
}

// ultraOff deletes the Worker and forgets the token. An error still means
// it is off; it says the Worker has to be deleted by hand.
func ultraOff(ctx context.Context, a *app) (ultraInfoOut, error) {
	_, err := ultrafast.Disable(ctx, a.ultra, ultraOptions)
	return ultraInfo(a), err
}

func printUltraInfo(out io.Writer, in ultraInfoOut) {
	if !in.On {
		fmt.Fprintln(out, "Ultra fast mode is off. Turn it on: r2b mirror ultra on")
		return
	}
	fmt.Fprintf(out, "Ultra fast mode is on (experimental): Worker %s in the Cloudflare account %s.\n", in.Worker, in.Account)
	if !in.Applies {
		fmt.Fprintln(out, "This computer does not copy from Drime to S3 storage yet, so it is not used.")
	}
}

func newMirrorUltraCmd(opts *Options) *cobra.Command {
	var account string
	var yes bool
	cmd := &cobra.Command{
		Use:   "ultra [on|off]",
		Short: "Ultra fast mode (experimental): copy Drime to S3 storage cloud to cloud",
		Long: "Ultra fast mode copies Drime files to S3 storage through a Worker in your own\n" +
			"Cloudflare account, so nothing goes through this PC. It applies to the second\n" +
			"copy and to Drime folders.\n\n" +
			"  r2b mirror ultra        is it on?\n" +
			"  r2b mirror ultra on     explain it, then set it up with a Cloudflare API token\n" +
			"  r2b mirror ultra off    delete the Worker and forget the token",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if len(args) == 0 {
				printUltraInfo(opts.Out, ultraInfo(a))
				return nil
			}
			switch strings.ToLower(args[0]) {
			case "on":
				return runUltraOn(cmd.Context(), a, opts, account, yes)
			case "off":
				if _, err := ultraOff(cmd.Context(), a); err != nil {
					fmt.Fprintln(opts.Out, "Ultra fast mode is off and the token forgotten.")
					return err
				}
				fmt.Fprintln(opts.Out, "Ultra fast mode is off. The Worker is deleted from your Cloudflare account and the token forgotten.")
				return nil
			}
			return fmt.Errorf("say on or off, not %q", args[0])
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "the Cloudflare account ID, when the token reaches more than one")
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to how it works without being asked")
	return cmd
}

func runUltraOn(ctx context.Context, a *app, opts *Options, account string, yes bool) error {
	if !interactive() && opts.In == nil {
		return errors.New("turning Ultra fast mode on needs a terminal, to read how it works and paste the token")
	}
	p := newPrompter(opts)
	fmt.Fprintln(p.out, ultrafast.Note)
	fmt.Fprintln(p.out)
	if !yes {
		ok, err := confirm(p, "Turn Ultra fast mode on? [y/N]", false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(p.out, "Left off.")
			return nil
		}
	}
	fmt.Fprintln(p.out, ultrafast.TokenHelp)
	token, err := p.secret("Cloudflare API token")
	if err != nil {
		return err
	}
	fmt.Fprintln(p.out, "Setting up the Worker in your Cloudflare account...")
	info, err := ultraOn(ctx, a, token, account)
	if err != nil {
		return err
	}
	if len(info.Accounts) > 0 {
		fmt.Fprintln(p.out, "This token reaches more than one Cloudflare account:")
		for i, acc := range info.Accounts {
			fmt.Fprintf(p.out, "  %d. %s (%s)\n", i+1, acc.Name, acc.ID)
		}
		choice, err := p.askRequired("Which one? (number)")
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(choice))
		if err != nil || n < 1 || n > len(info.Accounts) {
			return fmt.Errorf("%q is not one of the numbers", choice)
		}
		if info, err = ultraOn(ctx, a, token, info.Accounts[n-1].ID); err != nil {
			return err
		}
	}
	fmt.Fprintf(p.out, "Ultra fast mode is on: Worker %s in %s.\n", info.Worker, info.Account)
	if info.Warming {
		fmt.Fprintln(p.out, "Its new address can take a few minutes to start working. Until then, copies go through this PC.")
	}
	if !info.Applies {
		fmt.Fprintln(p.out, "It is used when copying from Drime to S3 storage: set up a second copy (r2b mirror setup) or a Drime folder (r2b mirror drime add).")
	}
	return nil
}
