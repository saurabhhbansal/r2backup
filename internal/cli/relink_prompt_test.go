package cli

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// A path arrives at a prompt the way a person produces one, not the way a
// terminal argument does. Windows Explorer's "Copy as path" wraps it in
// double quotes, and a pasted path routinely carries whitespace or a trailing
// separator. Answering any of those with "no such directory" would send the
// user back to the command line, which is the entire thing this prompt
// exists to avoid.
func TestCleanPastedPathAcceptsWhatPeopleActuallyPaste(t *testing.T) {
	cases := []struct{ in, want string }{
		{`D:\Photos\2026`, `D:\Photos\2026`},
		{`  D:\Photos\2026  `, `D:\Photos\2026`},
		{"\"D:\\Photos\\2026\"\n", `D:\Photos\2026`},
		{`"D:\Photos\2026"`, `D:\Photos\2026`},
		{`'/home/me/photos'`, `/home/me/photos`},
		{`/home/me/photos/`, `/home/me/photos`},
		{`D:\Photos\`, `D:\Photos`},
		{"\n", ``},
		{`   `, ``},
		{`""`, ``},
	}
	for _, c := range cases {
		if got := cleanPastedPath(c.in); got != c.want {
			t.Errorf("cleanPastedPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A drive root and a filesystem root are paths, not paths with a stray
// separator on the end. Trimming them leaves "C:" (which names a drive's
// working directory, not its root) or "" (which names nothing at all).
func TestCleanPastedPathKeepsARootARoot(t *testing.T) {
	if got := cleanPastedPath(`C:\`); got != `C:\` {
		t.Errorf("cleanPastedPath(`C:\\`) = %q, want `C:\\`", got)
	}
	if runtime.GOOS != "windows" {
		if got := cleanPastedPath(`/`); got != `/` {
			t.Errorf("cleanPastedPath(`/`) = %q, want `/`", got)
		}
	}
}

// The prompt must never appear where nobody can answer it. A scheduled run
// has no terminal, and --yes/--no both mean nobody is watching; neither can
// supply a path in any case. A hidden task blocking on a question would back
// nothing up and say nothing until the machine was rebooted.
func TestRelinkIsNeverOfferedWhenNobodyCanAnswer(t *testing.T) {
	for _, o := range []*Options{{Yes: true}, {No: true}, {Yes: true, No: true}} {
		if o.Decision() == Ask {
			t.Fatalf("Options{Yes:%v No:%v} reports Ask; offerRelink would prompt an unattended run",
				o.Yes, o.No)
		}
	}
}

// The conversation itself: what a user types, and what happens because of it.
func TestAskForNewRootDrivesTheWholeConversation(t *testing.T) {
	t.Run("a pasted Windows path relinks and the run continues", func(t *testing.T) {
		var out bytes.Buffer
		var got []string
		ok := askForNewRoot(&out, strings.NewReader("\"D:\\Photos\\2026\"\n"), func(p string) error {
			got = append(got, p)
			return nil
		})
		if !ok {
			t.Fatal("a valid path did not relink")
		}
		if len(got) != 1 || got[0] != `D:\Photos\2026` {
			t.Errorf("relink called with %q, want the unquoted path", got)
		}
		if !strings.Contains(out.String(), "Nothing has to be uploaded again") {
			t.Errorf("the user is not told the relink was free:\n%s", out.String())
		}
	})

	t.Run("pressing Enter leaves it alone", func(t *testing.T) {
		var out bytes.Buffer
		called := 0
		if askForNewRoot(&out, strings.NewReader("\n"), func(string) error { called++; return nil }) {
			t.Error("an empty answer relinked something")
		}
		if called != 0 {
			t.Errorf("relink was called %d times on an empty answer", called)
		}
	})

	t.Run("a wrong path is re-asked, not a dead end", func(t *testing.T) {
		var out bytes.Buffer
		var got []string
		ok := askForNewRoot(&out, strings.NewReader("/nope\n/home/me/photos\n"), func(p string) error {
			got = append(got, p)
			if p == "/nope" {
				return errors.New("no such directory")
			}
			return nil
		})
		if !ok {
			t.Fatal("the second, correct answer did not relink")
		}
		if len(got) != 2 {
			t.Fatalf("relink attempts = %v, want the bad path then the good one", got)
		}
		if !strings.Contains(out.String(), "no such directory") {
			t.Errorf("the user is not told why the first path failed:\n%s", out.String())
		}
	})

	t.Run("it gives up rather than spinning", func(t *testing.T) {
		var out bytes.Buffer
		calls := 0
		// Every answer rejected, and more answers than attempts allowed.
		if askForNewRoot(&out, strings.NewReader("/a\n/b\n/c\n/d\n/e\n"), func(string) error {
			calls++
			return errors.New("no such directory")
		}) {
			t.Error("reported success having never relinked")
		}
		if calls > 3 {
			t.Errorf("asked %d times; the loop is not bounded", calls)
		}
	})

	t.Run("EOF after a bad path does not re-prompt into nothing", func(t *testing.T) {
		var out bytes.Buffer
		calls := 0
		// No trailing newline: ReadString returns the text and io.EOF at once.
		if askForNewRoot(&out, strings.NewReader("/nope"), func(string) error {
			calls++
			return errors.New("no such directory")
		}) {
			t.Error("reported success on a rejected path")
		}
		if calls != 1 {
			t.Errorf("relink attempted %d times, want 1: there was nothing left to read", calls)
		}
	})
}
