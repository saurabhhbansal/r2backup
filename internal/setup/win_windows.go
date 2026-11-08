//go:build windows

package setup

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
	procCoCreateInstance    = ole32.NewProc("CoCreateInstance")
	procCoInitializeEx      = ole32.NewProc("CoInitializeEx")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidShellLink = guid{0x00021401, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW  = guid{0x000214F9, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile = guid{0x0000010b, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// comCall calls method i of a COM object's vtable.
func comCall(obj unsafe.Pointer, i int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(i)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func wstr(s string) uintptr {
	p, _ := windows.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

// shortcut writes a .lnk file through the shell's own IShellLink, the same
// object Explorer uses.
func shortcut(lnk, target, args, workDir, icon, desc string) error {
	errc := make(chan error, 1)
	// COM wants a thread of its own, set up for it.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_, _, _ = procCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED
		var sl unsafe.Pointer
		hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, 1, /* CLSCTX_INPROC_SERVER */
			uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&sl)))
		if int32(hr) < 0 {
			errc <- fmt.Errorf("create shell link: 0x%08x", uint32(hr))
			return
		}
		defer comCall(sl, 2)                                     // Release
		comCall(sl, 20, wstr(target))                            // SetPath
		comCall(sl, 11, wstr(args))                              // SetArguments
		comCall(sl, 9, wstr(workDir))                            // SetWorkingDirectory
		comCall(sl, 7, wstr(desc))                               // SetDescription
		comCall(sl, 17, wstr(strings.TrimSuffix(icon, ",0")), 0) // SetIconLocation
		var pf unsafe.Pointer
		if hr := comCall(sl, 0, uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&pf))); int32(hr) < 0 {
			errc <- fmt.Errorf("shell link has no IPersistFile: 0x%08x", uint32(hr))
			return
		}
		defer comCall(pf, 2)
		if hr := comCall(pf, 6, wstr(lnk), 1); int32(hr) < 0 { // Save
			errc <- fmt.Errorf("save %s: 0x%08x", lnk, uint32(hr))
			return
		}
		errc <- nil
	}()
	return <-errc
}
