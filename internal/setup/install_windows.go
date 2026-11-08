//go:build windows

// Package setup is the Windows installer: a small window that downloads the
// newest release, checks it, and puts r2backup in place -- the same steps as
// scripts/install.ps1, with a face on them and an entry in Installed apps so
// it can be removed again.
package setup

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
)

// Repo is where releases come from.
const Repo = "saurabhhbansal/r2backup"

//go:embed r2backup.ico
var iconICO []byte

const (
	appName      = "r2backup"
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\r2backup`
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	setupName    = "r2backup-setup.exe"
)

// Options are the choices on the first screen.
type Options struct {
	StartAtLogin bool `json:"startAtLogin"`
	AddToPath    bool `json:"addToPath"`
}

// Progress reports a step by name and how far along the whole install is,
// from 0 to 1.
type Progress func(step string, frac float64)

// InstallDir is where r2backup goes: per user, so no administrator prompt.
func InstallDir() string {
	if d := os.Getenv("R2BACKUP_INSTALL_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", appName)
}

func dataDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), appName) }

// Installed is the version already in InstallDir, or "" when there is none.
func Installed() string {
	exe := filepath.Join(InstallDir(), "r2b.exe")
	if _, err := os.Stat(exe); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := hidden(ctx, exe, "--version").Output()
	if err != nil {
		return "installed"
	}
	// "r2b version 1.5.4"
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "installed"
	}
	return strings.TrimPrefix(f[len(f)-1], "v")
}

// Latest asks GitHub for the newest release.
func Latest(ctx context.Context) (*selfupdate.Release, error) {
	return selfupdate.Latest(ctx, Repo)
}

// Install downloads rel, checks it, and installs it.
func Install(ctx context.Context, rel *selfupdate.Release, opt Options, progress Progress) error {
	dir := InstallDir()
	platform := "windows_" + runtime.GOARCH

	progress("Downloading r2backup "+strings.TrimPrefix(rel.Version, "v"), 0.02)
	archive, _, err := selfupdate.DownloadArchive(ctx, rel, platform, func(done, total int64) {
		f := 0.0
		if total > 0 {
			f = float64(done) / float64(total)
		}
		progress(fmt.Sprintf("Downloading · %s of %s", mb(done), mb(total)), 0.02+0.68*f)
	})
	if err != nil {
		return err
	}

	progress("Checking the download", 0.72)
	main, ok, err := selfupdate.ZipFile(archive, "r2b.exe", "r2backup.exe")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the release has no r2b.exe")
	}
	launcher, hasLauncher, err := selfupdate.ZipFile(archive, "r2bw.exe", "r2backupw.exe")
	if err != nil {
		return err
	}

	progress("Closing r2backup", 0.78)
	wasRunning := closeTray(8 * time.Second)

	progress("Installing", 0.84)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := placeExe(main, filepath.Join(dir, "r2b.exe")); err != nil {
		return fmt.Errorf("install r2b.exe: %w", err)
	}
	if hasLauncher {
		if err := placeExe(launcher, filepath.Join(dir, "r2bw.exe")); err != nil {
			return fmt.Errorf("install r2bw.exe: %w", err)
		}
	}
	for _, old := range []string{"r2backup.exe", "r2backupw.exe"} {
		_ = os.Remove(filepath.Join(dir, old))
	}
	progress("Adding to the Start menu", 0.9)
	trayExe := filepath.Join(dir, "r2bw.exe")
	if !hasLauncher {
		trayExe = filepath.Join(dir, "r2b.exe")
	}
	// Both programs carry the app icon (see cmd/r2b/winres_gen.go).
	icon := trayExe + ",0"
	if err := shortcut(startMenuLink(), trayExe, "tray", dir, icon, "See how your backups are doing, and back up now"); err != nil {
		return fmt.Errorf("add the Start menu shortcut: %w", err)
	}

	progress("Finishing", 0.95)
	_ = setStartAtLogin(opt.StartAtLogin, trayExe)
	if opt.AddToPath {
		_ = addToPath(dir)
	}
	setupCopy := copySelf(dir)
	version := strings.TrimPrefix(rel.Version, "v")
	_ = registerUninstall(dir, version, icon, setupCopy, len(main)+len(launcher))

	// Re-point a scheduled task at the file that now exists. It never
	// creates one.
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	_ = hidden(sctx, filepath.Join(dir, "r2b.exe"), "schedule", "--repair").Run()
	cancel()

	if wasRunning {
		_ = Launch()
	}
	progress("Installed", 1)
	return nil
}

// Launch starts the taskbar app. A copy that is already running opens its
// panel instead.
func Launch() error {
	dir := InstallDir()
	exe := filepath.Join(dir, "r2bw.exe")
	if _, err := os.Stat(exe); err != nil {
		exe = filepath.Join(dir, "r2b.exe")
	}
	cmd := exec.Command(exe, "tray")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Uninstall removes r2backup from this computer. Backups already in storage
// are not touched. removeData also deletes the settings, keys and the index
// of what has been backed up.
func Uninstall(ctx context.Context, removeData bool, progress Progress) error {
	dir := InstallDir()
	progress("Closing r2backup", 0.1)
	closeTray(8 * time.Second)

	progress("Turning off automatic backups", 0.3)
	if exe := filepath.Join(dir, "r2b.exe"); fileExists(exe) {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_ = hidden(sctx, exe, "schedule", "--remove", "--yes").Run()
		cancel()
	}

	progress("Removing shortcuts", 0.5)
	_ = setStartAtLogin(false, "")
	_ = os.Remove(startMenuLink())
	_ = removeFromPath(dir)
	_ = registry.DeleteKey(registry.CURRENT_USER, uninstallKey)

	progress("Removing files", 0.7)
	if removeData {
		if err := os.RemoveAll(dataDir()); err != nil {
			return fmt.Errorf("remove settings: %w", err)
		}
	}
	entries, _ := os.ReadDir(dir)
	self, _ := os.Executable()
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if strings.EqualFold(p, self) {
			continue
		}
		_ = os.RemoveAll(p)
	}
	progress("Removed", 1)
	return nil
}

// RemoveInstallDirOnExit deletes what is left of the install folder --
// this program, when it runs from there -- a moment after it exits.
func RemoveInstallDirOnExit() {
	dir := InstallDir()
	if !fileExists(dir) {
		return
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `/d /c ping -n 3 127.0.0.1 >nul & rmdir /s /q "` + dir + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	_ = cmd.Start()
}

// --- the pieces ---------------------------------------------------------------------

func mb(n int64) string {
	if n <= 0 {
		return "…"
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func hidden(ctx context.Context, exe string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

// placeExe writes a program file. Windows will not overwrite a running .exe
// -- a backup in progress, say -- but it will rename one, and the running
// copy carries on under the new name; r2b deletes r2b.exe.old itself the next
// time it starts.
func placeExe(data []byte, to string) error {
	tmp := to + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, to); err == nil {
		return nil
	}
	old := to + ".old"
	_ = os.Remove(old)
	if fileExists(old) {
		old = fmt.Sprintf("%s.%d.old", to, time.Now().UnixNano())
	}
	if err := os.Rename(to, old); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, to)
}

// closeTray asks a running taskbar app to quit and waits for it to go. It
// reports whether one was running. An app from before the installer existed
// does not listen for this; it restarts itself when its file is replaced.
func closeTray(wait time.Duration) bool {
	mutex, _ := windows.UTF16PtrFromString(`Local\` + appName + `-tray`)
	running := func() bool {
		h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, mutex)
		if err != nil {
			return false
		}
		_ = windows.CloseHandle(h)
		return true
	}
	if !running() {
		return false
	}
	name, _ := windows.UTF16PtrFromString(`Local\` + appName + `-tray-quit`)
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return true
	}
	_ = windows.SetEvent(h)
	_ = windows.CloseHandle(h)
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		if !running() {
			// Give it a moment to let go of its files.
			time.Sleep(300 * time.Millisecond)
			return true
		}
	}
	return true
}

func startMenuLink() string {
	return filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`, appName+".lnk")
}

func setStartAtLogin(on bool, trayExe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(appName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	// The same line the taskbar app writes for itself.
	return k.SetStringValue(appName, syscall.EscapeArg(trayExe)+" tray")
}

func pathEntries() (registry.Key, []string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return 0, nil, err
	}
	v, _, err := k.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		k.Close()
		return 0, nil, err
	}
	var parts []string
	for _, p := range strings.Split(v, ";") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return k, parts, nil
}

func addToPath(dir string) error {
	k, parts, err := pathEntries()
	if err != nil {
		return err
	}
	defer k.Close()
	for _, p := range parts {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			return nil
		}
	}
	parts = append(parts, dir)
	if err := k.SetExpandStringValue("Path", strings.Join(parts, ";")); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}

func removeFromPath(dir string) error {
	k, parts, err := pathEntries()
	if err != nil {
		return err
	}
	defer k.Close()
	var keep []string
	for _, p := range parts {
		if !strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			keep = append(keep, p)
		}
	}
	if len(keep) == len(parts) {
		return nil
	}
	if err := k.SetExpandStringValue("Path", strings.Join(keep, ";")); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}

func broadcastEnvChange() {
	env, _ := windows.UTF16PtrFromString("Environment")
	var res uintptr
	_, _, _ = procSendMessageTimeoutW.Call(0xffff /* HWND_BROADCAST */, 0x001A, /* WM_SETTINGCHANGE */
		0, uintptr(unsafe.Pointer(env)), 0x0002 /* SMTO_ABORTIFHUNG */, 2000, uintptr(unsafe.Pointer(&res)))
}

// copySelf keeps a copy of this installer in the install folder, so Installed
// apps has something to run to remove r2backup again. It returns its path.
func copySelf(dir string) string {
	dst := filepath.Join(dir, setupName)
	self, err := os.Executable()
	if err != nil {
		return dst
	}
	if strings.EqualFold(self, dst) {
		return dst
	}
	if b, err := os.ReadFile(self); err == nil {
		_ = placeExe(b, dst)
	}
	return dst
}

func registerUninstall(dir, version, icon, setupExe string, size int) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	quoted := syscall.EscapeArg(setupExe)
	for name, v := range map[string]string{
		"DisplayName":          "r2backup",
		"DisplayVersion":       version,
		"Publisher":            "r2backup",
		"DisplayIcon":          icon,
		"InstallLocation":      dir,
		"URLInfoAbout":         selfupdate.SiteURL,
		"UninstallString":      quoted + " --uninstall",
		"QuietUninstallString": quoted + " --uninstall --quiet",
		"InstallDate":          time.Now().Format("20060102"),
	} {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	_ = k.SetDWordValue("EstimatedSize", uint32(size/1024))
	return nil
}
