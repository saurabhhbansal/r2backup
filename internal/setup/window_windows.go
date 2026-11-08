//go:build windows

package setup

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"

	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
)

//go:embed setup.html
var pageHTML string

//go:embed mark.png
var markPNG []byte

//go:embed wordmark.png
var wordmarkPNG []byte

var (
	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pShowWindow             = user32.NewProc("ShowWindow")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pReleaseCapture         = user32.NewProc("ReleaseCapture")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pSetWindowPos           = user32.NewProc("SetWindowPos")
	pLoadCursorW            = user32.NewProc("LoadCursorW")
	pLoadImageW             = user32.NewProc("LoadImageW")
	pGetDpiForSystem        = user32.NewProc("GetDpiForSystem")
	pSetDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSystemParametersInfoW  = user32.NewProc("SystemParametersInfoW")
	pDwmSetWindowAttribute  = dwmapi.NewProc("DwmSetWindowAttribute")
	pDwmExtendFrame         = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
	pCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	pGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	pCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	pLookupIconIdFromDirEx  = user32.NewProc("LookupIconIdFromDirectoryEx")
)

const (
	wmDestroy     = 0x0002
	wmSize        = 0x0005
	wmActivate    = 0x0006
	wmClose       = 0x0010
	wmNCCalcSize  = 0x0083
	wmNCLButton   = 0x00A1
	wmSysCommand  = 0x0112
	wmApp         = 0x8000
	htCaption     = 2
	scMinimize    = 0xF020
	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsClipChild   = 0x02000000
	wsExAppWindow = 0x00040000
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msgT struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type rect struct{ Left, Top, Right, Bottom int32 }

// Mode is what the window is for.
type Mode string

const (
	ModeInstall   Mode = "install"
	ModeUninstall Mode = "uninstall"
)

type window struct {
	mode   Mode
	hwnd   uintptr
	web    *edge.Chromium
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	queue   []func()
	rel     *selfupdate.Release
	working bool
	removed bool
}

var win *window

// Run shows the installer window and returns when it is closed.
func Run(mode Mode) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr, _, _ := procCoInitializeEx.Call(0, 2); int32(hr) < 0 {
		return errors.New("could not start COM")
	}
	_, _, _ = pSetDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &window{mode: mode, ctx: ctx, cancel: cancel}
	win = w

	inst, _, _ := pGetModuleHandleW.Call(0)
	cls, _ := windows.UTF16PtrFromString("r2backupSetup")
	cursor, _, _ := pLoadCursorW.Call(0, 32512)
	brush, _, _ := pCreateSolidBrush.Call(0x000F0D0D) // #0d0d0f, the page's own ground
	icon := iconHandle(256)
	small := iconHandle(16)
	wc := wndClassEx{
		WndProc: windowsCallback, Instance: windows.Handle(inst), Cursor: windows.Handle(cursor),
		Background: windows.Handle(brush), ClassName: cls, Icon: icon, IconSm: small,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	_, _, _ = pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	dpi, _, _ := pGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	width, height := int32(820*dpi/96), int32(540*dpi/96)
	var work rect
	_, _, _ = pSystemParametersInfoW.Call(0x0030 /* SPI_GETWORKAREA */, 0, uintptr(unsafe.Pointer(&work)), 0)
	x := work.Left + (work.Right-work.Left-width)/2
	y := work.Top + (work.Bottom-work.Top-height)/2

	title := "r2backup Setup"
	if mode == ModeUninstall {
		title = "Remove r2backup"
	}
	w.hwnd, _, _ = pCreateWindowExW.Call(wsExAppWindow, uintptr(unsafe.Pointer(cls)), wstr(title),
		wsOverlapped|wsCaption|wsSysMenu|wsMinimizeBox|wsClipChild,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, inst, 0)
	if w.hwnd == 0 {
		return errors.New("could not create the window")
	}
	// Dark, rounded, and with the system's own shadow: the frame is taken
	// away in WM_NCCALCSIZE, and a sliver of it is kept in DWM's hands so
	// Windows still draws the shadow and the corner rounding.
	dark := uint32(1)
	_, _, _ = pDwmSetWindowAttribute.Call(w.hwnd, 20, uintptr(unsafe.Pointer(&dark)), 4) // DWMWA_USE_IMMERSIVE_DARK_MODE
	corner := uint32(2)
	_, _, _ = pDwmSetWindowAttribute.Call(w.hwnd, 33, uintptr(unsafe.Pointer(&corner)), 4) // DWMWA_WINDOW_CORNER_PREFERENCE: round
	border := uint32(0x00302A2A)
	_, _, _ = pDwmSetWindowAttribute.Call(w.hwnd, 34, uintptr(unsafe.Pointer(&border)), 4) // DWMWA_BORDER_COLOR
	margins := [4]int32{0, 0, 1, 0}
	_, _, _ = pDwmExtendFrame.Call(w.hwnd, uintptr(unsafe.Pointer(&margins)))
	// The first WM_NCCALCSIZE came during CreateWindowEx, before w.hwnd was
	// known; ask again so the frame really goes.
	_, _, _ = pSetWindowPos.Call(w.hwnd, 0, 0, 0, 0, 0, 0x0001|0x0002|0x0004|0x0010|0x0020) // NOSIZE|NOMOVE|NOZORDER|NOACTIVATE|FRAMECHANGED

	w.web = edge.NewChromium()
	w.web.DataPath = filepath.Join(os.TempDir(), "r2backup-setup-webview")
	w.web.MessageCallback = w.onMessage
	if !w.web.Embed(w.hwnd) {
		_, _, _ = pDestroyWindow.Call(w.hwnd)
		return errNoWebView
	}
	if s, err := w.web.GetSettings(); err == nil {
		_ = s.PutAreDefaultContextMenusEnabled(false)
		_ = s.PutAreDevToolsEnabled(os.Getenv("R2BACKUP_SETUP_DEBUG") != "")
		_ = s.PutIsStatusBarEnabled(false)
		_ = s.PutIsZoomControlEnabled(false)
	}
	w.web.Resize()
	w.web.NavigateToString(page())
	_, _, _ = pShowWindow.Call(w.hwnd, 5) // SW_SHOW
	// The browser was created while its window was hidden, so it starts
	// invisible until told otherwise.
	_ = w.web.Show()
	w.web.Resize()
	_, _, _ = pSetForegroundWindow.Call(w.hwnd)
	w.web.Focus()

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	if w.mode == ModeUninstall && w.removed {
		RemoveInstallDirOnExit()
	}
	return nil
}

var errNoWebView = errors.New("Microsoft Edge WebView2 is not available. It comes with Windows 11; on Windows 10 install it from https://go.microsoft.com/fwlink/p/?LinkId=2124703")

// ErrNoWebView reports whether err means WebView2 is missing.
func ErrNoWebView(err error) bool { return errors.Is(err, errNoWebView) }

func page() string {
	b64 := base64.StdEncoding.EncodeToString
	return strings.NewReplacer(
		"{{MARK}}", "data:image/png;base64,"+b64(markPNG),
		"{{WORDMARK}}", "data:image/png;base64,"+b64(wordmarkPNG),
	).Replace(pageHTML)
}

// iconHandle makes an icon of about the given size from the embedded .ico.
func iconHandle(size int) windows.Handle {
	if len(iconICO) < 6 {
		return 0
	}
	off, _, _ := pLookupIconIdFromDirEx.Call(uintptr(unsafe.Pointer(&iconICO[0])), 1, uintptr(size), uintptr(size), 0)
	if off == 0 || int(off) >= len(iconICO) {
		return 0
	}
	// The directory entry for that image says how long it is.
	count := int(iconICO[4]) | int(iconICO[5])<<8
	length := 0
	for i := 0; i < count; i++ {
		e := iconICO[6+16*i:]
		o := int(e[12]) | int(e[13])<<8 | int(e[14])<<16 | int(e[15])<<24
		if uintptr(o) == off {
			length = int(e[8]) | int(e[9])<<8 | int(e[10])<<16 | int(e[11])<<24
		}
	}
	if length == 0 {
		return 0
	}
	h, _, _ := pCreateIconFromResource.Call(uintptr(unsafe.Pointer(&iconICO[off])), uintptr(length), 1, 0x00030000, uintptr(size), uintptr(size), 0)
	return windows.Handle(h)
}

var windowsCallback = windows.NewCallback(wndProc)

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	w := win
	if w != nil && hwnd == w.hwnd {
		switch msg {
		case wmNCCalcSize:
			if wp != 0 {
				return 0 // the whole window is client area
			}
		case wmSize:
			if w.web != nil {
				w.web.Resize()
			}
			return 0
		case wmActivate:
			if wp&0xffff != 0 && w.web != nil {
				w.web.Focus()
			}
		case wmApp:
			w.mu.Lock()
			q := w.queue
			w.queue = nil
			w.mu.Unlock()
			for _, f := range q {
				f()
			}
			return 0
		case wmClose:
			if w.busy() {
				return 0 // not halfway through putting files in place
			}
			_, _, _ = pDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			w.cancel()
			_, _, _ = pPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (w *window) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.working
}

// post runs f on the window's thread, the only one WebView2 may be used from.
func (w *window) post(f func()) {
	w.mu.Lock()
	w.queue = append(w.queue, f)
	w.mu.Unlock()
	_, _, _ = pPostMessageW.Call(w.hwnd, wmApp, 0, 0)
}

// emit calls a function on the page with one JSON argument.
func (w *window) emit(fn string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.post(func() { w.web.Eval("ui." + fn + "(" + string(b) + ")") })
}

type message struct {
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args"`
}

// onMessage runs on the window's thread.
func (w *window) onMessage(raw string) {
	var m message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return
	}
	switch m.Cmd {
	case "ready":
		w.emit("init", map[string]any{
			"mode": w.mode, "installed": Installed(), "dir": InstallDir(),
		})
		if w.mode == ModeInstall {
			go w.findLatest()
		}
	case "drag":
		_, _, _ = pReleaseCapture.Call()
		_, _, _ = pPostMessageW.Call(w.hwnd, wmNCLButton, htCaption, 0)
	case "minimize":
		_, _, _ = pPostMessageW.Call(w.hwnd, wmSysCommand, scMinimize, 0)
	case "close":
		_, _, _ = pPostMessageW.Call(w.hwnd, wmClose, 0, 0)
	case "retry":
		go w.findLatest()
	case "open":
		shellOpen(strings.Trim(string(m.Args), `"`))
	case "install":
		var opt Options
		_ = json.Unmarshal(m.Args, &opt)
		w.start(func() error {
			w.mu.Lock()
			rel := w.rel
			w.mu.Unlock()
			if rel == nil {
				return errors.New("the newest release is not known yet")
			}
			return Install(w.ctx, rel, opt, w.progress)
		})
	case "uninstall":
		var opt struct {
			RemoveData bool `json:"removeData"`
		}
		_ = json.Unmarshal(m.Args, &opt)
		w.start(func() error {
			err := Uninstall(w.ctx, opt.RemoveData, w.progress)
			if err == nil {
				w.mu.Lock()
				w.removed = true
				w.mu.Unlock()
			}
			return err
		})
	case "launch":
		if err := Launch(); err != nil {
			w.emit("toast", err.Error())
			return
		}
		_, _, _ = pPostMessageW.Call(w.hwnd, wmClose, 0, 0)
	}
}

func (w *window) start(job func() error) {
	w.mu.Lock()
	if w.working {
		w.mu.Unlock()
		return
	}
	w.working = true
	w.mu.Unlock()
	go func() {
		err := job()
		w.mu.Lock()
		w.working = false
		w.mu.Unlock()
		if err != nil {
			w.emit("failed", err.Error())
			return
		}
		w.emit("finished", map[string]any{"installed": Installed()})
	}()
}

func (w *window) progress(step string, frac float64) {
	w.emit("progress", map[string]any{"step": step, "frac": frac})
}

func (w *window) findLatest() {
	ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
	defer cancel()
	rel, err := Latest(ctx)
	if err != nil {
		w.emit("latestFailed", err.Error())
		return
	}
	w.mu.Lock()
	w.rel = rel
	w.mu.Unlock()
	w.emit("latest", map[string]any{"version": strings.TrimPrefix(rel.Version, "v"), "notes": rel.Notes})
}

func shellOpen(url string) {
	if !strings.HasPrefix(url, "https://") {
		return
	}
	shell32 := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")
	_, _, _ = shell32.Call(0, wstr("open"), wstr(url), 0, 0, 1)
}
