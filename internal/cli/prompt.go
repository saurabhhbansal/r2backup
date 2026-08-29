package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// prompter asks the questions `setup` needs answered.
//
// It exists because setup went from three questions to a conversation, and
// the two habits the old code had do not survive that. It created a fresh
// bufio.Reader per question, which is safe for exactly one question: a
// bufio.Reader may pull more than one line out of the file descriptor and
// then be discarded along with everything it over-read, so the second
// question silently loses the answer to the third. And it read secrets
// straight from os.Stdin's file descriptor, which cannot be driven by a test.
// One reader, held for the whole conversation, and one seam for the echo-off
// read, fixes both.
type prompter struct {
	out io.Writer
	in  *bufio.Reader
	// hideEcho is false when the input is not a terminal -- a test driving
	// setup through Options.In -- in which case a secret is read like any
	// other line rather than by turning echo off on a descriptor that has no
	// echo to turn off.
	hideEcho bool
}

func newPrompter(opts *Options) *prompter {
	if opts.In != nil {
		return &prompter{out: opts.Out, in: bufio.NewReader(opts.In)}
	}
	return &prompter{out: opts.Out, in: bufio.NewReader(os.Stdin), hideEcho: true}
}

// ask puts a question and returns the trimmed answer.
func (p *prompter) ask(label string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", label)
	line, err := p.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// askRequired repeats a question until it gets a non-empty answer, so a
// stray enter costs a keystroke rather than the whole command.
func (p *prompter) askRequired(label string) (string, error) {
	for {
		v, err := p.ask(label)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
		fmt.Fprintln(p.out, "  (this one is needed)")
	}
}

// secret reads an answer without echoing it, when there is a terminal to
// stop echoing on.
func (p *prompter) secret(label string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", label)
	if !p.hideEcho {
		line, err := p.in.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil && line == "" {
			return "", err
		}
		return line, nil
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
