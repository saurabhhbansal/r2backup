package schedule

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func validEntry() Entry {
	return Entry{
		Name:       "r2backup-default",
		Interval:   30 * time.Minute,
		BinaryPath: "/usr/local/bin/r2backup",
		Args:       []string{"run", "--all"},
	}
}

func TestEntryValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(e Entry) Entry
		wantErr bool
	}{
		{"valid entry passes", func(e Entry) Entry { return e }, false},
		{"empty name rejected", func(e Entry) Entry { e.Name = ""; return e }, true},
		{"empty binary path rejected", func(e Entry) Entry { e.BinaryPath = ""; return e }, true},
		{"zero interval rejected", func(e Entry) Entry { e.Interval = 0; return e }, true},
		{"negative interval rejected", func(e Entry) Entry { e.Interval = -time.Minute; return e }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(validEntry()).validate()
			if tc.wantErr && err == nil {
				t.Fatalf("validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}

func TestUnsupportedError(t *testing.T) {
	err := unsupportedError("install", "r2backup-default")
	if err == nil {
		t.Fatal("unsupportedError returned nil")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("unsupportedError(...) = %v, want it to wrap ErrUnsupported so callers can errors.Is() against it", err)
	}
	if want := runtime.GOOS; !contains(err.Error(), want) {
		t.Errorf("unsupportedError(...) = %q, want it to name the actual GOOS %q so a bug report says what platform failed", err.Error(), want)
	}
}

func TestSanitizeUnitName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"alnum and dash pass through", "r2backup-default", "r2backup-default"},
		{"space becomes dash", "my backup", "my-backup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeUnitName(tc.in); got != tc.want {
				t.Errorf("sanitizeUnitName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	t.Run("slash never survives (would break a filename or unit path)", func(t *testing.T) {
		got := sanitizeUnitName("docs/photos")
		if containsRune(got, '/') {
			t.Errorf("sanitizeUnitName(%q) = %q, still contains a slash", "docs/photos", got)
		}
	})
	t.Run("empty input never produces empty output", func(t *testing.T) {
		if got := sanitizeUnitName(""); got == "" {
			t.Error("sanitizeUnitName(\"\") returned an empty string; every platform needs a non-empty name")
		}
	})
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestXMLEscapeText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"a & b", "a &amp; b"},
		{"<tag>", "&lt;tag&gt;"},
	}
	for _, tc := range cases {
		if got := xmlEscapeText(tc.in); got != tc.want {
			t.Errorf("xmlEscapeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultRunnerExecutesAndCaptures(t *testing.T) {
	out, err := defaultRunner(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("defaultRunner(echo hello) error: %v", err)
	}
	if got := string(out); got != "hello\n" {
		t.Errorf("defaultRunner(echo hello) output = %q, want %q", got, "hello\n")
	}
}

func TestDefaultRunnerSurfacesFailure(t *testing.T) {
	_, err := defaultRunner(context.Background(), "false")
	if err == nil {
		t.Fatal("defaultRunner(false) returned nil error, want the nonzero exit surfaced")
	}
}
