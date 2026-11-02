//go:build windows

package cli

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// platformDrimeSyncRoots reads the folders Drime's app registered with
// Windows as cloud-file sync roots, under SyncRootManager. Each entry names
// its provider; Drime's says Drime.
func platformDrimeSyncRoots() []string {
	const manager = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, manager, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, manager+`\`+n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		display, _, _ := sk.GetStringValue("DisplayNameResource")
		icon, _, _ := sk.GetStringValue("IconResource")
		sk.Close()
		if !strings.EqualFold(strings.TrimSpace(display), "Drime") && !strings.Contains(strings.ToLower(icon), `\drime\`) {
			continue
		}
		roots, err := registry.OpenKey(registry.LOCAL_MACHINE, manager+`\`+n+`\UserSyncRoots`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		users, _ := roots.ReadValueNames(-1)
		for _, u := range users {
			if p, _, err := roots.GetStringValue(u); err == nil && p != "" {
				out = append(out, p)
			}
		}
		roots.Close()
	}
	return out
}
