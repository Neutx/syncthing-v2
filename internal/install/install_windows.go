//go:build windows

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/Neutx/syncthing-v2/internal/autostart"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
)

// UninstallKey is the HKCU-relative Add/Remove Programs entry.
const UninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\` + "SyncThingV2"

// ShortcutName is the Start Menu shortcut file name.
const ShortcutName = brand.DisplayName + ".lnk"

// DefaultRoots returns the current user's real locations.
func DefaultRoots() (Roots, error) {
	lad, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || !filepath.IsAbs(lad) {
		lad = os.Getenv("LOCALAPPDATA")
		if !filepath.IsAbs(lad) {
			return Roots{}, errors.New("install: cannot determine %LOCALAPPDATA%")
		}
	}
	data, err := osutil.AppDataDir()
	if err != nil {
		return Roots{}, err
	}
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, 0)
	if err != nil || !filepath.IsAbs(programs) {
		appdata := os.Getenv("APPDATA")
		if !filepath.IsAbs(appdata) {
			return Roots{}, fmt.Errorf("install: cannot find the Start Menu folder: %v", err)
		}
		programs = filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs")
	}
	ar, err := autostart.DefaultRoots()
	if err != nil {
		return Roots{}, err
	}
	managed, err := stinstall.ManagedDir()
	if err != nil {
		return Roots{}, err
	}
	return Roots{
		InstallDir:          filepath.Join(lad, "Programs", brand.AppDirName()),
		DataDir:             data,
		ManagedSyncthingDir: managed,
		Autostart:           autostart.New(ar),
		UninstallKey:        UninstallKey,
		StartMenuDir:        programs,
		StartupDir:          ar.StartupDir,
		Processes:           listProcesses,
		Terminate:           terminateProcess,
		FirewallRule:        HasFirewallRule,
	}, nil
}

// placeBinary copies src to <InstallDir>\stv2.exe. An existing copy is
// renamed to stv2.exe.old first (a running exe can be renamed but not
// overwritten); CleanupOld deletes it on the next start. When src already is
// the installed copy nothing is copied.
func placeBinary(r Roots, src string) (string, bool, error) {
	dst := InstalledExe(r)
	if samePath(src, dst) {
		return dst, false, nil
	}
	if err := os.MkdirAll(r.InstallDir, 0o755); err != nil {
		return "", false, fmt.Errorf("install: %w", err)
	}
	old := dst + ".old"
	upgraded := false
	if isRegular(dst) {
		if err := removeIfExists(old); err != nil {
			return "", false, fmt.Errorf("install: remove the previous %s: %w", filepath.Base(old), err)
		}
		if err := os.Rename(dst, old); err != nil {
			return "", false, fmt.Errorf("install: move the installed copy aside: %w", err)
		}
		upgraded = true
	}
	if err := copyExecutable(src, dst); err != nil {
		if upgraded {
			_ = os.Rename(old, dst)
		}
		return "", false, fmt.Errorf("install: copy %s: %w", filepath.Base(src), err)
	}
	return dst, upgraded, nil
}

// register writes the Uninstall key and the Start Menu shortcut.
func register(r Roots, exe string) ([]string, error) {
	if r.UninstallKey == "" {
		return nil, errors.New("install: no Uninstall key")
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, r.UninstallKey, registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("install: Uninstall key: %w", err)
	}
	defer key.Close()
	var size uint32
	if fi, err := os.Stat(exe); err == nil {
		size = uint32((fi.Size() + 1023) / 1024)
	}
	quoted := `"` + exe + `"`
	for name, v := range map[string]string{
		"DisplayName":          brand.DisplayName,
		"DisplayVersion":       strings.TrimPrefix(brand.Version, "v"),
		"Publisher":            brand.Publisher,
		"DisplayIcon":          exe + ",0",
		"InstallLocation":      r.InstallDir,
		"UninstallString":      quoted + " uninstall",
		"QuietUninstallString": quoted + " uninstall --yes",
	} {
		if err := key.SetStringValue(name, v); err != nil {
			return nil, fmt.Errorf("install: Uninstall key %s: %w", name, err)
		}
	}
	for name, v := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": size} {
		if err := key.SetDWordValue(name, v); err != nil {
			return nil, fmt.Errorf("install: Uninstall key %s: %w", name, err)
		}
	}
	if r.StartMenuDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(r.StartMenuDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: Start Menu: %w", err)
	}
	err = createShortcut(filepath.Join(r.StartMenuDir, ShortcutName), shortcut{
		Target:      exe,
		WorkDir:     r.InstallDir,
		Description: "Syncthing status and Tailscale pairing",
		Icon:        exe,
	})
	if err != nil {
		return nil, fmt.Errorf("install: Start Menu shortcut: %w", err)
	}
	return nil, nil
}

// unregister deletes the Uninstall key and the Start Menu shortcut.
func unregister(r Roots) error {
	var errs []error
	if r.UninstallKey != "" {
		if err := registry.DeleteKey(registry.CURRENT_USER, r.UninstallKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
			errs = append(errs, fmt.Errorf("uninstall registry key: %w", err))
		}
	}
	if r.StartMenuDir != "" {
		if err := removeIfExists(filepath.Join(r.StartMenuDir, ShortcutName)); err != nil {
			errs = append(errs, fmt.Errorf("start menu shortcut: %w", err))
		}
	}
	return errors.Join(errs...)
}

// removeProgram deletes stv2.exe (and .old) from InstallDir, then the
// directory when nothing else (a kept managed Syncthing) is in it. When the
// running executable is the installed one it cannot delete itself, so a
// detached hidden cmd.exe does it after this process has exited; the same
// cmd.exe also removes leftovers (directories whose files were in use).
func removeProgram(r Roots, o Options, _ bool, leftovers []string) ([]string, error) {
	exe := InstalledExe(r)
	self := within(o.Self, r.InstallDir)
	if !self {
		var errs []error
		for _, f := range []string{exe, exe + ".old"} {
			errs = append(errs, removeIfExists(f))
		}
		errs = append(errs, removeIfEmpty(r.InstallDir))
		if err := errors.Join(errs...); err != nil || len(leftovers) == 0 {
			return nil, err
		}
	}
	// The managed Syncthing was already removed when the user asked for it,
	// so the whole directory may go when no Syncthing is left inside it.
	wholeDir := !exists(r.ManagedSyncthingDir) || !within(r.ManagedSyncthingDir, r.InstallDir)
	script, err := selfDeleteScript(r, self, wholeDir, leftovers)
	if err != nil {
		return nil, err
	}
	if err := r.SelfDelete(script); err != nil {
		return nil, fmt.Errorf("schedule removal of %s: %w", r.InstallDir, err)
	}
	return []string{"The remaining files are removed a few seconds after " + brand.BinaryName + " exits."}, nil
}

// selfDeleteScript is the cmd.exe script run after exit (spec §6.1):
//
//	ping -n 3 127.0.0.1 >nul & rmdir /s /q "<install dir>"
//
// When a kept managed Syncthing lives inside the install dir, only our
// files are deleted and the directory stays. program is false when the
// program files are already gone and only leftovers remain.
func selfDeleteScript(r Roots, program, wholeDir bool, leftovers []string) (string, error) {
	for _, p := range append([]string{r.InstallDir}, leftovers...) {
		if err := checkCmdPath(p); err != nil {
			return "", err
		}
		if filepath.Dir(filepath.Clean(p)) == filepath.Clean(p) {
			return "", fmt.Errorf("refusing to remove the volume root %q", p)
		}
	}
	var b strings.Builder
	b.WriteString("ping -n 3 127.0.0.1 >nul")
	exe := InstalledExe(r)
	switch {
	case program && wholeDir:
		b.WriteString(` & rmdir /s /q "` + r.InstallDir + `"`)
	case program:
		b.WriteString(` & del /f /q "` + exe + `" "` + exe + `.old" 2>nul`)
	}
	for _, p := range leftovers {
		b.WriteString(` & rmdir /s /q "` + p + `"`)
	}
	return b.String(), nil
}

// selfDelete is the production Roots.SelfDelete: `cmd.exe /d /v:off /c <script>`,
// detached, without a window. The command line is passed verbatim because
// cmd.exe does not follow the CommandLineToArgvW quoting rules.
func selfDelete(script string) error {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	comspec := filepath.Join(sys, "cmd.exe")
	cmd := osutil.Detached(comspec)
	cmd.SysProcAttr.CmdLine = selfDeleteCommandLine(comspec, script)
	cmd.Dir = os.TempDir() // never hold the directory being deleted open
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// processAlive reports whether a process with this PID is still running.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means it exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// Inspect reads the installation under r.
func Inspect(r Roots) State {
	var s State
	if exe := InstalledExe(r); isRegular(exe) {
		s.Installed, s.Exe = true, exe
	}
	if r.StartMenuDir != "" {
		if p := filepath.Join(r.StartMenuDir, ShortcutName); isRegular(p) {
			s.Shortcut = p
		}
	}
	if r.UninstallKey == "" {
		return s
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, r.UninstallKey, registry.QUERY_VALUE)
	if err != nil {
		return s
	}
	defer key.Close()
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return s
	}
	s.Values = map[string]string{}
	for _, n := range names {
		if v, _, err := key.GetStringValue(n); err == nil {
			s.Values[n] = v
		} else if d, _, err := key.GetIntegerValue(n); err == nil {
			s.Values[n] = strconv.FormatUint(d, 10)
		}
	}
	s.Version = s.Values["DisplayVersion"]
	return s
}

// WebView2Available reports whether the WebView2 runtime the Windows
// dashboard needs is installed (doctor UI001).
func WebView2Available() bool {
	v, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	return err == nil && v != ""
}
