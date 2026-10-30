package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The default data directory keeps the name every installed machine's
// scheduled task already has. Renaming it would leave the old task running
// and a new one duplicating it.
func TestTheDefaultDataDirKeepsTheOriginalName(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	if got := InstanceName(); got != "r2backup" {
		t.Errorf("InstanceName() = %q with no override, want r2backup", got)
	}
	if args := DataDirArgs(); args != nil {
		t.Errorf("DataDirArgs() = %q with no override, want none", args)
	}
	// Naming the default directory explicitly is still the default.
	def, err := defaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDataDir, def)
	if got := InstanceName(); got != "r2backup" {
		t.Errorf("InstanceName() = %q for the default directory named explicitly, want r2backup", got)
	}
}

// Any other data directory is a separate r2backup and must neither take over
// the default one's scheduled task nor be run against the default directory.
func TestACustomDataDirGetsItsOwnNameAndCarriesItself(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv(EnvDataDir, a)
	nameA, argsA := InstanceName(), DataDirArgs()
	t.Setenv(EnvDataDir, b)
	nameB := InstanceName()

	if !strings.HasPrefix(nameA, "r2backup-") || nameA == nameB {
		t.Errorf("names %q and %q: want distinct r2backup-<hash> names", nameA, nameB)
	}
	if !slices.Equal(argsA, []string{"--data-dir", a}) {
		t.Errorf("DataDirArgs() = %q, want --data-dir %s", argsA, a)
	}
	// Stable: the same directory always gets the same name, or `schedule
	// --remove` could never find what `schedule` registered.
	t.Setenv(EnvDataDir, a)
	if InstanceName() != nameA {
		t.Error("the same directory got a different name the second time")
	}
}

func TestApplyDataDirArg(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"backup", "--data-dir", "/x/y"}, "/x/y"},
		{[]string{"--data-dir=/x/z", "tray"}, "/x/z"},
		{[]string{"backup"}, ""},
		{[]string{"add", "--", "--data-dir", "/not/a/flag"}, ""},
	} {
		t.Setenv(EnvDataDir, "")
		ApplyDataDirArg(c.args)
		if got := os.Getenv(EnvDataDir); got != c.want {
			t.Errorf("ApplyDataDirArg(%q): %s = %q, want %q", c.args, EnvDataDir, got, c.want)
		}
	}
}
