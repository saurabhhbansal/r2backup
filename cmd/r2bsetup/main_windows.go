//go:build windows

// Command r2bsetup is the Windows installer: a window that downloads the
// newest r2backup release from GitHub, checks it against the published
// checksums, and installs it for this user. Run with --uninstall it removes
// r2backup again; Installed apps runs it that way.
package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/saurabhhbansal/r2backup/internal/setup"
)

func main() {
	mode := setup.ModeInstall
	for _, a := range os.Args[1:] {
		if a == "--uninstall" || a == "/uninstall" {
			mode = setup.ModeUninstall
		}
	}
	if err := setup.Run(mode); err != nil {
		msg := "r2backup Setup could not start.\n\n" + err.Error()
		if setup.ErrNoWebView(err) {
			msg = "r2backup Setup needs Microsoft Edge WebView2, which comes with Windows 11.\n\nOn Windows 10, install the WebView2 Runtime from Microsoft, then run Setup again:\nhttps://go.microsoft.com/fwlink/p/?LinkId=2124703"
		}
		text, _ := windows.UTF16PtrFromString(msg)
		title, _ := windows.UTF16PtrFromString("r2backup Setup")
		box := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")
		_, _, _ = box.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
		os.Exit(1)
	}
}
