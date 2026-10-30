package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
)

// Renaming this computer. The name is also where new folders are filed in
// storage ("machines/<name>/<folder>"), so a rename only changes where new
// folders go: folders already backed up keep the location they were created
// with, exactly as renaming a folder does. Earlier names are remembered so
// their backups still read as this computer's.

// maxMachineNameLen matches the longest hostname DNS allows.
const maxMachineNameLen = 63

// validMachineName says, in plain words, why name cannot be a computer name.
func validMachineName(name string) error {
	switch {
	case name == "":
		return errors.New("enter a name")
	case len(name) > maxMachineNameLen:
		return fmt.Errorf("use %d characters or fewer", maxMachineNameLen)
	case name == "." || name == "..":
		return errors.New("choose another name")
	case strings.ContainsAny(name, `/\`):
		return errors.New("a name can’t contain a slash")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("a name can’t contain control characters")
		}
	}
	return nil
}

// isThisMachine reports whether backups filed under name were made by this
// computer, under its current name or an earlier one.
func isThisMachine(name string) bool {
	if name == machineName() {
		return true
	}
	s, err := config.LoadSettings()
	return err == nil && slices.Contains(s.MachineNamesBefore, name)
}

// renameMachine gives this computer a new name, refusing one another
// computer already uses: two computers filing folders under the same name
// would write over each other's backups.
func (t *trayBackend) renameMachine(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if err := validMachineName(name); err != nil {
		return err
	}
	if os.Getenv("R2BACKUP_MACHINE") != "" {
		return errors.New("this computer’s name is set by R2BACKUP_MACHINE")
	}
	old := machineName()
	if name == old {
		return nil
	}

	token, _ := account.LoadToken()
	if token != "" {
		if devices, err := accountClient().ListDevices(ctx, token); err == nil {
			for _, d := range devices {
				if d.DeviceName == name && !isThisMachine(name) {
					return fmt.Errorf("another computer on your account is called %s", name)
				}
			}
		}
	}
	if a, err := t.d.connected(ctx); err == nil {
		if found, err := discoverBackups(ctx, a); err == nil {
			for _, f := range found {
				if f.Machine == name && !isThisMachine(name) {
					return fmt.Errorf("backups from another computer called %s are already in your storage", name)
				}
			}
		}
	}

	s, err := config.LoadSettings()
	if err != nil {
		return err
	}
	s.MachineNamesBefore = slices.DeleteFunc(s.MachineNamesBefore, func(n string) bool { return n == name || n == old })
	s.MachineNamesBefore = append(s.MachineNamesBefore, old)
	s.MachineName = name
	if name == hostName() {
		s.MachineName = "" // back to following the hostname
	}
	if err := config.SaveSettings(s); err != nil {
		return err
	}

	if token != "" {
		// The rename is made on this computer either way; the account's list
		// catches up the next time this computer signs in.
		_ = accountClient().RenameDevice(ctx, token, old, name, runtime.GOOS)
	}
	t.invalidate()
	return nil
}
