//go:build windows

package tray

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/saurabhhbansal/r2backup/internal/config"
)

//go:embed panel.html
var panelHTML string

// The panel's size in 96-DPI pixels; scaled for the screen it opens on.
const panelW, panelH = 380, 580

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pSetWindowPos                  = user32.NewProc("SetWindowPos")
	pSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	pAllowSetForegroundWindow      = user32.NewProc("AllowSetForegroundWindow")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pPostThreadMessageW            = user32.NewProc("PostThreadMessageW")
	pPostQuitMessage               = user32.NewProc("PostQuitMessage")
	pMonitorFromRect               = user32.NewProc("MonitorFromRect")
	pMonitorFromPoint              = user32.NewProc("MonitorFromPoint")
	pGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	pGetCursorPos                  = user32.NewProc("GetCursorPos")
	pEnumWindows                   = user32.NewProc("EnumWindows")
	pGetWindowThreadProcessID      = user32.NewProc("GetWindowThreadProcessId")
	pGetClassNameW                 = user32.NewProc("GetClassNameW")
	pMessageBoxW                   = user32.NewProc("MessageBoxW")
	pGetDpiForMonitor              = shcore.NewProc("GetDpiForMonitor")
	pSHAppBarMessage               = shell32.NewProc("SHAppBarMessage")
	pShellNotifyIconGetRect        = shell32.NewProc("Shell_NotifyIconGetRect")
	pGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pLoadCursorW                   = user32.NewProc("LoadCursorW")
	pDwmSetWindowAttribute         = dwmapi.NewProc("DwmSetWindowAttribute")
	pCreateSolidBrush              = windows.NewLazySystemDLL("gdi32.dll").NewProc("CreateSolidBrush")
	pCoCreateInstance              = ole32.NewProc("CoCreateInstance")
	pCoInitializeEx                = ole32.NewProc("CoInitializeEx")
	pShellExecuteW                 = shell32.NewProc("ShellExecuteW")
	pGetCurrentThreadID            = kernel32.NewProc("GetCurrentThreadId")
)

const (
	wmActivate   = 0x0006
	wmSize       = 0x0005
	wmClose      = 0x0010
	wmApp        = 0x8000
	wsPopup      = 0x80000000
	wsExToolWin  = 0x00000080
	wsExTopmost  = 0x00000008
	csDropShadow = 0x00020000
	swHide       = 0
	swShowNormal = 1
	swpShow      = 0x0040
	hwndTopmost  = ^uintptr(0) // (HWND)-1
	wmDpiChanged = 0x02E0
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

// panel is the tray app: the icon, the popup window and the WebView2 inside.
type panel struct {
	be     Backend
	ctx    context.Context
	cancel context.CancelFunc

	hwnd     uintptr
	web      *edge.Chromium
	uiThread uintptr

	mu    sync.Mutex
	queue []func()

	visible   atomic.Bool
	ready     atomic.Bool
	lastHide  atomic.Int64 // unix nanos
	updated   atomic.Bool  // the program file was replaced: start the new one on the way out
	kick      chan struct{}
	lastKind  string
	lastLight bool // the taskbar was light when the icon was last set
}

// the one panel this process has, for the window procedure.
var current *panel

// Run shows the tray icon and runs until Quit is chosen or ctx ends. A second
// copy started while one is running asks the first to open its panel and
// returns at once.
func Run(ctx context.Context, be Backend) error {
	// One copy per data directory: a second r2backup kept elsewhere gets
	// an icon of its own.
	name, _ := windows.UTF16PtrFromString(`Local\` + config.InstanceName() + `-tray`)
	mutex, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		signalOpen()
		return nil
	}
	if err != nil {
		return fmt.Errorf("tray: %w", err)
	}
	release := sync.OnceFunc(func() { _ = windows.CloseHandle(mutex) })
	defer release()

	_, _, _ = pSetProcessDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)

	ctx, cancel := context.WithCancel(ctx)
	p := &panel{be: be, ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1)}
	current = p

	uiErr := make(chan error, 1)
	go p.uiLoop(uiErr)
	if err := <-uiErr; err != nil {
		cancel()
		return err
	}
	go p.watchOpenRequests()
	go p.refreshLoop()
	go p.watchForUpdate()
	go func() { <-ctx.Done(); systray.Quit() }()

	systray.Run(p.onReady, func() { cancel() })
	if p.updated.Load() {
		// The lock goes first, or the new copy would find this one still
		// running and only ask it to open its panel.
		release()
		if err := relaunch(); err != nil {
			Fail(fmt.Errorf("restart after an update: %w", err))
		}
	}
	return nil
}

// watchForUpdate restarts the app when its program file is replaced --
// `r2b update` swaps it in place, and so does the installer -- so the icon
// does not go on running the old version until the next sign-in. Windows
// lets a running program be renamed but not overwritten, so the path this
// process was started from now holds the new file.
func (p *panel) watchForUpdate() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	started, err := os.Stat(exe)
	if err != nil {
		return
	}
	changed := func(fi os.FileInfo) bool {
		return !os.SameFile(started, fi) || !fi.ModTime().Equal(started.ModTime()) || fi.Size() != started.Size()
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		seen, err := os.Stat(exe)
		if err != nil || !changed(seen) {
			continue // mid-swap, or nothing new
		}
		// The installer copies the new file into place; wait for it to stop
		// growing before starting it.
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		again, err := os.Stat(exe)
		if err != nil || again.Size() != seen.Size() || !again.ModTime().Equal(seen.ModTime()) {
			continue
		}
		p.updated.Store(true)
		p.cancel()
		return
	}
}

// Fail tells someone why the taskbar app did not start. Started from the
// Start menu or at sign-in it has no console, so an error printed there goes
// nowhere and the icon just never appears; this puts it on screen and in
// tray-errors.log in the data directory.
func Fail(err error) {
	if err == nil {
		return
	}
	if dir, derr := config.DataDir(); derr == nil {
		if f, oerr := os.OpenFile(filepath.Join(dir, "tray-errors.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); oerr == nil {
			_, _ = fmt.Fprintf(f, "%s r2b tray: %v\n", time.Now().Format(time.RFC3339), err)
			_ = f.Close()
		}
	}
	text, _ := windows.UTF16PtrFromString("The r2backup taskbar app could not start.\n\n" + err.Error())
	title, _ := windows.UTF16PtrFromString("r2backup")
	const mbIconError, mbTopmost, mbSetForeground = 0x10, 0x40000, 0x10000
	_, _, _ = pMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)),
		mbIconError|mbTopmost|mbSetForeground)
}

// --- the window ------------------------------------------------------------

func (p *panel) uiLoop(started chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// WebView2 must live on a single-threaded COM apartment. The library
	// sets one up only on the program's main thread, which belongs to the
	// tray icon here.
	if hr, _, _ := pCoInitializeEx.Call(0, 2); int32(hr) < 0 { // COINIT_APARTMENTTHREADED
		started <- fmt.Errorf("tray: start COM: 0x%08x", uint32(hr))
		return
	}
	p.uiThread, _, _ = pGetCurrentThreadID.Call()

	var inst windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &inst)
	cls, _ := windows.UTF16PtrFromString("r2backupPanel")
	cursor, _, _ := pLoadCursorW.Call(0, 32512) // IDC_ARROW
	// The page's own background colour, so the window never flashes a
	// different one for the moment before the page draws.
	bg := uintptr(0x00141212) // 0x00BBGGRR: #121214
	if lightTheme() {
		bg = 0x00F8F6F6 // #f6f6f8
	}
	brush, _, _ := pCreateSolidBrush.Call(bg)
	wc := wndClassEx{
		Style: csDropShadow, WndProc: windows.NewCallback(wndProc), Background: windows.Handle(brush),
		Instance: inst, Cursor: windows.Handle(cursor), ClassName: cls,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	_, _, _ = pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	w, h := size(systemDPI())
	title, _ := windows.UTF16PtrFromString("r2backup")
	p.hwnd, _, _ = pCreateWindowExW.Call(wsExToolWin|wsExTopmost, uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(title)), wsPopup, 0, 0, uintptr(w), uintptr(h), 0, 0, uintptr(inst), 0)
	if p.hwnd == 0 {
		started <- errors.New("tray: could not create the panel window")
		return
	}
	// Windows 11 rounds a popup's corners only when asked.
	corner := uint32(2) // DWMWCP_ROUND
	_, _, _ = pDwmSetWindowAttribute.Call(p.hwnd, 33, uintptr(unsafe.Pointer(&corner)), 4)

	dir, err := config.EnsureDataDir()
	if err != nil {
		started <- err
		return
	}
	p.web = edge.NewChromium()
	p.web.DataPath = filepath.Join(dir, "webview")
	p.web.MessageCallback = p.onMessage
	if !p.web.Embed(p.hwnd) {
		started <- errors.New("tray: Microsoft Edge WebView2 is not available. It ships with Windows 11; on Windows 10 install it from https://go.microsoft.com/fwlink/p/?LinkId=2124703")
		return
	}
	if s, err := p.web.GetSettings(); err == nil {
		_ = s.PutAreDefaultContextMenusEnabled(false)
		_ = s.PutAreDevToolsEnabled(false)
		_ = s.PutIsStatusBarEnabled(false)
	}
	p.web.Resize()
	p.web.NavigateToString(panelHTML)
	started <- nil

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			return
		}
		if m.Message == wmApp && m.Hwnd == 0 {
			p.mu.Lock()
			q := p.queue
			p.queue = nil
			p.mu.Unlock()
			for _, f := range q {
				f()
			}
			continue
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	p := current
	if p != nil && hwnd == p.hwnd {
		switch msg {
		case wmActivate:
			// Clicking anywhere else closes the panel, as the clock and the
			// volume flyouts do.
			if wp&0xffff == 0 {
				p.hide()
			}
			return 0
		case wmSize:
			if p.web != nil {
				p.web.Resize()
			}
			return 0
		case wmClose:
			p.hide()
			return 0
		case wmDpiChanged:
			// show() has already sized the panel for the monitor it is
			// moving to; the size Windows suggests here is the old one
			// rescaled, so it is ignored.
			return 0
		}
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// post runs f on the window's thread, which is the only thread WebView2 may
// be called from.
func (p *panel) post(f func()) {
	p.mu.Lock()
	p.queue = append(p.queue, f)
	p.mu.Unlock()
	_, _, _ = pPostThreadMessageW.Call(p.uiThread, wmApp, 0, 0)
}

// size is the panel's size in pixels at a monitor's DPI.
func size(dpi uint32) (int32, int32) {
	if dpi == 0 {
		dpi = 96
	}
	return panelW * int32(dpi) / 96, panelH * int32(dpi) / 96
}

func systemDPI() uint32 {
	dpi, _, _ := pGetDpiForSystem.Call()
	return uint32(dpi)
}

func (p *panel) show() {
	p.post(func() {
		icon := trayIconRect()
		work, edge, dpi := whereToOpen(icon)
		w, h := size(dpi)
		gap := 12 * int32(dpi) / 96
		x, y := place(work, edge, icon, w, h, gap)
		_, _, _ = pSetWindowPos.Call(p.hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpShow)
		// The browser starts invisible because its window was hidden when it
		// was created; it is told to draw each time the panel opens.
		_ = p.web.Show()
		p.web.Resize()
		_, _, _ = pSetForegroundWindow.Call(p.hwnd)
		p.web.Focus()
		p.visible.Store(true)
		// Only now: a refresh that ran before this would have seen the
		// panel hidden, skipped drawing, and waited the hidden interval.
		p.refreshNow()
	})
}

// --- where the panel goes ----------------------------------------------------------

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

type appBarData struct {
	Size            uint32
	Wnd             uintptr
	CallbackMessage uint32
	Edge            uint32
	Rc              rect
	LParam          uintptr
}

type notifyIconIdentifier struct {
	Size uint32
	Wnd  uintptr
	ID   uint32
	GUID windows.GUID
}

const monitorDefaultToNearest = 2

// whereToOpen picks the monitor the panel opens on -- the one showing the
// icon, else the one with the taskbar, else the one under the pointer --
// and returns its work area, the edge its taskbar is on and its DPI.
func whereToOpen(icon rect) (work rect, edge int, dpi uint32) {
	abd := appBarData{}
	abd.Size = uint32(unsafe.Sizeof(abd))
	haveBar, _, _ := pSHAppBarMessage.Call(5, uintptr(unsafe.Pointer(&abd))) // ABM_GETTASKBARPOS
	var mon uintptr
	switch {
	case !icon.empty():
		mon, _, _ = pMonitorFromRect.Call(uintptr(unsafe.Pointer(&icon)), monitorDefaultToNearest)
	case haveBar != 0:
		mon, _, _ = pMonitorFromRect.Call(uintptr(unsafe.Pointer(&abd.Rc)), monitorDefaultToNearest)
	default:
		var pt struct{ X, Y int32 }
		_, _, _ = pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		// MonitorFromPoint takes the POINT by value, packed into one word.
		mon, _, _ = pMonitorFromPoint.Call(uintptr(uint32(pt.X))|uintptr(uint32(pt.Y))<<32, monitorDefaultToNearest)
	}
	mi := monitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if mon == 0 {
		return rect{}, edgeBottom, systemDPI()
	}
	if ok, _, _ := pGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok == 0 {
		return rect{}, edgeBottom, systemDPI()
	}
	edge = edgeOf(mi.Monitor, mi.Work)
	// Windows' own word on the edge, when the taskbar it describes is on
	// this monitor: an auto-hidden taskbar leaves the work area whole, so
	// the work area alone cannot say.
	if haveBar != 0 && abd.Edge <= edgeBottom {
		if barMon, _, _ := pMonitorFromRect.Call(uintptr(unsafe.Pointer(&abd.Rc)), monitorDefaultToNearest); barMon == mon {
			edge = int(abd.Edge)
		}
	}
	var dx, dy uint32
	if hr, _, _ := pGetDpiForMonitor.Call(mon, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); hr != 0 || dx == 0 { // MDT_EFFECTIVE_DPI
		dx = systemDPI()
	}
	return mi.Work, edge, dx
}

// trayIconRect is where the icon is on screen, or an empty rect when Windows
// will not say -- when the icon is tucked into the overflow area, say.
func trayIconRect() rect {
	hwnd := systrayWindow()
	if hwnd == 0 {
		return rect{}
	}
	// 100 is the id fyne.io/systray gives its one icon.
	id := notifyIconIdentifier{Wnd: hwnd, ID: 100}
	id.Size = uint32(unsafe.Sizeof(id))
	var r rect
	if hr, _, _ := pShellNotifyIconGetRect.Call(uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&r))); hr != 0 {
		return rect{}
	}
	return r
}

var (
	systrayHwnd   uintptr
	findSystrayCB = windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		var owner uint32
		_, _, _ = pGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if owner != uint32(os.Getpid()) {
			return 1
		}
		var buf [64]uint16
		n, _, _ := pGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if windows.UTF16ToString(buf[:n]) == "SystrayClass" {
			systrayHwnd = hwnd
			return 0
		}
		return 1
	})
)

// systrayWindow finds the hidden window fyne.io/systray owns the icon
// through, which the library does not expose: this process's top-level
// window of its class. Only the panel's thread calls it.
func systrayWindow() uintptr {
	if systrayHwnd == 0 {
		_, _, _ = pEnumWindows.Call(findSystrayCB, 0)
	}
	return systrayHwnd
}

func (p *panel) hide() {
	if !p.visible.Swap(false) {
		return
	}
	p.lastHide.Store(time.Now().UnixNano())
	_, _, _ = pShowWindow.Call(p.hwnd, swHide)
}

// toggle is what a click on the icon does. The click itself takes focus
// from an open panel, which hides it just before the click arrives -- so a
// panel hidden a moment ago stays hidden, and the click reads as "close".
func (p *panel) toggle() {
	if p.visible.Load() {
		p.post(p.hide)
		return
	}
	if time.Since(time.Unix(0, p.lastHide.Load())) < 400*time.Millisecond {
		return
	}
	p.show()
}

// --- talking to the page -----------------------------------------------------

type message struct {
	Cmd string `json:"cmd"`
	Arg string `json:"arg"`
}

func (p *panel) onMessage(raw string) {
	var m message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return
	}
	switch m.Cmd {
	case "ready":
		p.ready.Store(true)
		p.refreshNow()
	case "hide":
		p.hide()
	case "backup":
		go p.do(func() (string, error) {
			if err := p.be.BackUp(m.Arg); err != nil {
				return "", err
			}
			if m.Arg == "" {
				return "Backing up every folder.", nil
			}
			return "Backing up " + m.Arg + ".", nil
		})
	case "add":
		p.hide()
		go p.do(func() (string, error) {
			path, err := pickFolder("Choose a folder to back up", "Back up this folder")
			if err != nil || path == "" {
				return "", err
			}
			if err := p.be.AddFolder(path); err != nil {
				return "", err
			}
			p.show()
			return "Added " + filepath.Base(path) + ". Backing it up now.", nil
		})
	case "schedule":
		on, every := parseSchedule(m.Arg)
		go p.do(func() (string, error) {
			if err := p.be.SetSchedule(p.ctx, on, every); err != nil {
				return "", err
			}
			if on {
				return "Backing up automatically.", nil
			}
			return "Automatic backups are off.", nil
		})
	case "login":
		go p.do(func() (string, error) {
			on := m.Arg == "true"
			if err := setStartAtLogin(on); err != nil {
				return "", err
			}
			if on {
				return "r2backup will start with Windows.", nil
			}
			return "r2backup will not start with Windows.", nil
		})
	case "browser":
		p.hide()
		if u := p.be.StorageURL(); u != "" {
			shellOpen(u)
		}
	case "open":
		p.hide()
		if m.Arg != "" {
			shellOpen(m.Arg)
		}
	case "call":
		go p.call(m.Arg)
	case "quit":
		p.cancel()
	}
}

// callMsg is a request from the page that wants an answer: it names a method,
// carries its arguments as JSON, and is replied to by id.
type callMsg struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// call serves one request from the page. A few methods need the window or
// Windows' own dialogs and are answered here; the rest are the backend's.
func (p *panel) call(raw string) {
	var c callMsg
	if json.Unmarshal([]byte(raw), &c) != nil {
		return
	}
	var out any
	var err error
	switch c.Method {
	case "pickFolder":
		out, err = p.pickFolderFor(c.Args)
	case "cfSignIn":
		// The sign-in happens in the browser, which takes focus and so
		// hides the panel; it comes back when there is an answer to show.
		out, err = p.be.Call(p.ctx, c.Method, c.Args)
		p.show()
	default:
		out, err = p.be.Call(p.ctx, c.Method, c.Args)
	}
	reply := struct {
		ID    int    `json:"id"`
		OK    bool   `json:"ok"`
		Data  any    `json:"data,omitempty"`
		Error string `json:"error,omitempty"`
	}{ID: c.ID, OK: err == nil, Data: out}
	if err != nil {
		reply.Error = Plain(err.Error())
	}
	js, jerr := json.Marshal(reply)
	if jerr != nil {
		js, _ = json.Marshal(struct {
			ID    int    `json:"id"`
			Error string `json:"error"`
		}{c.ID, jerr.Error()})
	}
	p.post(func() { p.web.Eval("r2b.reply(" + string(js) + ")") })
	p.refreshNow()
}

// pickFolderFor shows the folder picker for the page, with its own wording.
// The panel closes itself when another window takes focus, so it is hidden
// while the picker is up and shown again after, with the page still as it was.
func (p *panel) pickFolderFor(args json.RawMessage) (any, error) {
	var a struct{ Title, Button string }
	_ = json.Unmarshal(args, &a)
	if a.Title == "" {
		a.Title = "Choose a folder"
	}
	if a.Button == "" {
		a.Button = "Choose"
	}
	p.hide()
	defer p.show()
	return pickFolder(a.Title, a.Button)
}

func parseSchedule(arg string) (bool, int) {
	state, n, _ := strings.Cut(arg, ":")
	every, err := strconv.Atoi(n)
	if err != nil || every <= 0 {
		every = 30
	}
	return state == "on", every
}

// do runs an action off the window thread and says how it went.
func (p *panel) do(f func() (string, error)) {
	msg, err := f()
	if err != nil {
		msg = "Could not do that: " + Plain(err.Error())
	}
	if msg != "" {
		p.toast(msg)
	}
	p.refreshNow()
}

func (p *panel) toast(msg string) {
	js, _ := json.Marshal(msg)
	p.post(func() {
		if p.ready.Load() {
			p.web.Eval("r2b.toast(" + string(js) + ")")
		}
	})
}

// --- keeping it current ----------------------------------------------------------

func (p *panel) refreshNow() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// refreshLoop reloads the state every two seconds while the panel is open,
// so a running backup's progress moves, and every fifteen while it is not,
// so the icon still changes colour when something needs a look.
func (p *panel) refreshLoop() {
	for {
		p.refresh()
		wait := 15 * time.Second
		if p.visible.Load() {
			wait = 2 * time.Second
		}
		select {
		case <-p.ctx.Done():
			return
		case <-p.kick:
		case <-time.After(wait):
		}
	}
}

func (p *panel) refresh() {
	st, err := p.be.State(p.ctx)
	if err != nil {
		st.Status = Status{Kind: KindAttention, Title: "Could not read r2backup's state", Detail: Plain(err.Error())}
	}
	st.Busy = p.be.Busy()
	st.Login = startAtLogin()

	if light := taskbarLight(); st.Status.Kind != p.lastKind || light != p.lastLight {
		p.lastKind, p.lastLight = st.Status.Kind, light
		systray.SetIcon(IconFor(st.Status.Kind, light))
	}
	tip := "r2backup — " + st.Status.Title
	if len(tip) > 120 {
		tip = tip[:120]
	}
	systray.SetTooltip(tip)

	if !p.visible.Load() || !p.ready.Load() {
		return
	}
	js, err := json.Marshal(st)
	if err != nil {
		return
	}
	p.post(func() { p.web.Eval("r2b.render(" + string(js) + ")") })
}

// --- the icon ---------------------------------------------------------------------

func (p *panel) onReady() {
	p.lastLight = taskbarLight()
	systray.SetIcon(IconFor(KindPending, p.lastLight))
	systray.SetTitle("r2backup")
	systray.SetTooltip("r2backup")
	systray.SetOnTapped(p.toggle)

	open := systray.AddMenuItem("Open r2backup", "Show the panel")
	now := systray.AddMenuItem("Back up now", "Back up every folder")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Scheduled backups still run")
	go func() {
		for {
			select {
			case <-open.ClickedCh:
				p.show()
			case <-now.ClickedCh:
				go p.do(func() (string, error) { return "Backing up every folder.", p.be.BackUp("") })
			case <-quit.ClickedCh:
				p.cancel()
				return
			case <-p.ctx.Done():
				return
			}
		}
	}()
	p.refreshNow()
}

// --- one copy at a time -------------------------------------------------------------

func openEventName() string { return `Local\` + config.InstanceName() + `-tray-open` }

func signalOpen() {
	name, _ := windows.UTF16PtrFromString(openEventName())
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return
	}
	// This copy was just started from the Start menu, so Windows lets it
	// take the foreground; the running copy, idle in the background, may
	// not. Passing the right on lets the panel come to the front with
	// focus, instead of opening behind whatever was in front -- where
	// Escape and clicking away would not close it.
	_, _, _ = pAllowSetForegroundWindow.Call(^uintptr(0)) // ASFW_ANY
	_ = windows.SetEvent(h)
	_ = windows.CloseHandle(h)
}

// watchOpenRequests opens the panel when another copy is started, which is
// what someone means by starting it again.
func (p *panel) watchOpenRequests() {
	name, _ := windows.UTF16PtrFromString(openEventName())
	h, err := windows.CreateEvent(nil, 0, 0, name)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	for p.ctx.Err() == nil {
		r, _ := windows.WaitForSingleObject(h, 1000)
		if r == windows.WAIT_OBJECT_0 {
			p.show()
		}
	}
}

// --- Windows bits -----------------------------------------------------------------

func shellOpen(target string) {
	verb, _ := windows.UTF16PtrFromString("open")
	t, _ := windows.UTF16PtrFromString(target)
	_, _, _ = pShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(t)), 0, 0, swShowNormal)
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// runValue names the sign-in entry: "r2backup" for the default data
// directory, as it always was, and its own name for any other.
func runValue() string { return config.InstanceName() }

// launcher is how this app is started with no console window: through the
// windowless launcher beside this program when there is one, else this
// program asked to hide its own. The arguments carry the data directory,
// since neither Windows at sign-in nor a restart passes on an environment
// variable reliably.
func launcher() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	args := append([]string{"tray"}, config.DataDirArgs()...)
	if w := filepath.Join(filepath.Dir(exe), "r2bw.exe"); fileExists(w) {
		return w, args, nil
	}
	return exe, append(args, "--hidden"), nil
}

// launchCommand is what Windows runs at sign-in.
func launchCommand() (string, error) {
	exe, args, err := launcher()
	if err != nil {
		return "", err
	}
	line := syscall.EscapeArg(exe)
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	return line, nil
}

// relaunch starts a new copy of the app, after an update.
func relaunch() error {
	exe, args, err := launcher()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Start()
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// lightTheme reports whether Windows is set to light mode for apps, which is
// what the page's prefers-color-scheme follows.
func lightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 1
}

// taskbarLight reports whether the taskbar itself is light, which Windows
// keeps apart from the apps setting: the tray icon follows this one.
func taskbarLight() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	return err == nil && v == 1
}

func startAtLogin() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue())
	return err == nil
}

func setStartAtLogin(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue()); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	cmd, err := launchCommand()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValue(), cmd)
}

// pickFolder shows Windows' own folder picker -- the same dialog Explorer
// uses -- and returns the chosen path, or "" if it was cancelled.
func pickFolder(title, okLabel string) (string, error) {
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		path, err := pickFolderSTA(title, okLabel)
		ch <- result{path, err}
	}()
	r := <-ch
	return r.path, r.err
}

var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

// comObject is the start of every COM object: a pointer to its method table.
type comObject struct{ methods *[32]uintptr }

// vtbl calls method index i of a COM object.
func vtbl(obj unsafe.Pointer, i int, args ...uintptr) uintptr {
	fn := (*comObject)(obj).methods[i]
	r, _, _ := syscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func pickFolderSTA(titleText, okText string) (string, error) {
	// S_OK and S_FALSE (already initialised on this thread) are both fine;
	// only a failure code, which is negative, is not.
	if hr, _, _ := pCoInitializeEx.Call(0, 2); int32(hr) < 0 { // COINIT_APARTMENTTHREADED
		return "", fmt.Errorf("start COM: 0x%08x", uint32(hr))
	}
	defer windows.CoUninitialize()

	var dlg unsafe.Pointer
	hr, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, 1, // CLSCTX_INPROC_SERVER
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
	if hr != 0 || dlg == nil {
		return "", fmt.Errorf("open the folder picker: 0x%08x", uint32(hr))
	}
	defer vtbl(dlg, 2) // Release

	const fosPickFolders, fosForceFileSystem = 0x20, 0x40
	vtbl(dlg, 9, fosPickFolders|fosForceFileSystem) // SetOptions
	title, _ := windows.UTF16PtrFromString(titleText)
	vtbl(dlg, 17, uintptr(unsafe.Pointer(title))) // SetTitle
	ok, _ := windows.UTF16PtrFromString(okText)
	vtbl(dlg, 18, uintptr(unsafe.Pointer(ok))) // SetOkButtonLabel

	if hr := vtbl(dlg, 3, 0); hr != 0 { // Show
		if uint32(hr) == 0x800704C7 { // cancelled
			return "", nil
		}
		return "", fmt.Errorf("folder picker: 0x%08x", uint32(hr))
	}
	var item unsafe.Pointer
	if hr := vtbl(dlg, 20, uintptr(unsafe.Pointer(&item))); hr != 0 || item == nil { // GetResult
		return "", fmt.Errorf("folder picker result: 0x%08x", uint32(hr))
	}
	defer vtbl(item, 2)
	var str *uint16
	const sigdnFileSysPath = 0x80058000
	if hr := vtbl(item, 5, sigdnFileSysPath, uintptr(unsafe.Pointer(&str))); hr != 0 || str == nil { // GetDisplayName
		return "", fmt.Errorf("folder picker path: 0x%08x", uint32(hr))
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(str))
	return windows.UTF16PtrToString(str), nil
}
