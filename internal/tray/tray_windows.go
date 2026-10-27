//go:build windows

package tray

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pSetWindowPos                  = user32.NewProc("SetWindowPos")
	pSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pPostThreadMessageW            = user32.NewProc("PostThreadMessageW")
	pPostQuitMessage               = user32.NewProc("PostQuitMessage")
	pSystemParametersInfoW         = user32.NewProc("SystemParametersInfoW")
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
	spiWorkArea  = 0x0030
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

	visible  atomic.Bool
	ready    atomic.Bool
	lastHide atomic.Int64 // unix nanos
	kick     chan struct{}
	lastKind string
}

// the one panel this process has, for the window procedure.
var current *panel

// Run shows the tray icon and runs until Quit is chosen or ctx ends. A second
// copy started while one is running asks the first to open its panel and
// returns at once.
func Run(ctx context.Context, be Backend) error {
	name, _ := windows.UTF16PtrFromString(`Local\r2backup-tray`)
	mutex, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		signalOpen()
		return nil
	}
	if err != nil {
		return fmt.Errorf("tray: %w", err)
	}
	defer windows.CloseHandle(mutex)

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
	go func() { <-ctx.Done(); systray.Quit() }()

	systray.Run(p.onReady, func() { cancel() })
	return nil
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
	// A dark background, so the window never shows through to the desktop
	// for the moment before the page draws.
	brush, _, _ := pCreateSolidBrush.Call(0x00141212) // 0x00BBGGRR: #121214
	wc := wndClassEx{
		Style: csDropShadow, WndProc: windows.NewCallback(wndProc), Background: windows.Handle(brush),
		Instance: inst, Cursor: windows.Handle(cursor), ClassName: cls,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	_, _, _ = pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	w, h := p.size()
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

func (p *panel) size() (int, int) {
	dpi, _, _ := pGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	return panelW * int(dpi) / 96, panelH * int(dpi) / 96
}

func (p *panel) show() {
	p.post(func() {
		w, h := p.size()
		var wa rect
		_, _, _ = pSystemParametersInfoW.Call(spiWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0)
		gap := int32(12 * w / panelW)
		x := wa.Right - int32(w) - gap
		y := wa.Bottom - int32(h) - gap
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
			path, err := pickFolder()
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
	case "dashboard":
		p.hide()
		go p.do(func() (string, error) { return "", p.be.OpenDashboard() })
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
	case "quit":
		p.cancel()
	}
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
		msg = "Could not do that: " + err.Error()
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
		st.Status = Status{Kind: KindAttention, Title: "Could not read r2backup's state", Detail: err.Error()}
	}
	st.Busy = p.be.Busy()
	st.Login = startAtLogin()

	if st.Status.Kind != p.lastKind {
		p.lastKind = st.Status.Kind
		systray.SetIcon(Icon(st.Status.Kind))
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
	systray.SetIcon(Icon(KindPending))
	systray.SetTitle("r2backup")
	systray.SetTooltip("r2backup")
	systray.SetOnTapped(p.toggle)

	open := systray.AddMenuItem("Open r2backup", "Show the panel")
	now := systray.AddMenuItem("Back up now", "Back up every folder")
	dash := systray.AddMenuItem("Open r2b…", "The full interface: restore, trash and more")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Scheduled backups still run")
	go func() {
		for {
			select {
			case <-open.ClickedCh:
				p.show()
			case <-now.ClickedCh:
				go p.do(func() (string, error) { return "Backing up every folder.", p.be.BackUp("") })
			case <-dash.ClickedCh:
				go p.do(func() (string, error) { return "", p.be.OpenDashboard() })
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

const openEventName = `Local\r2backup-tray-open`

func signalOpen() {
	name, _ := windows.UTF16PtrFromString(openEventName)
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return
	}
	_ = windows.SetEvent(h)
	_ = windows.CloseHandle(h)
}

// watchOpenRequests opens the panel when another copy is started, which is
// what someone means by starting it again.
func (p *panel) watchOpenRequests() {
	name, _ := windows.UTF16PtrFromString(openEventName)
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

const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "r2backup"
)

// launchCommand is what Windows runs at sign-in: the windowless launcher
// beside this program, so no console window appears.
func launchCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if w := filepath.Join(filepath.Dir(exe), "r2bw.exe"); fileExists(w) {
		return `"` + w + `" tray`, nil
	}
	return `"` + exe + `" tray --hidden`, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func startAtLogin() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setStartAtLogin(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	cmd, err := launchCommand()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValue, cmd)
}

// pickFolder shows Windows' own folder picker -- the same dialog Explorer
// uses -- and returns the chosen path, or "" if it was cancelled.
func pickFolder() (string, error) {
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		path, err := pickFolderSTA()
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

func pickFolderSTA() (string, error) {
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
	title, _ := windows.UTF16PtrFromString("Choose a folder to back up")
	vtbl(dlg, 17, uintptr(unsafe.Pointer(title))) // SetTitle
	ok, _ := windows.UTF16PtrFromString("Back up this folder")
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
