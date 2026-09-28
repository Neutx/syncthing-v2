//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// Default HKCU-relative keys.
const (
	RunKey            = `Software\Microsoft\Windows\CurrentVersion\Run`
	ApprovedKey       = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	ApprovedFolderKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\StartupFolder`
)

// maxLnk bounds the size of a Startup-folder shortcut that is parsed.
const maxLnk = 1 << 20

// DefaultRoots returns the current user's Run key and Startup folder.
func DefaultRoots() (Roots, error) {
	startup, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0)
	if err != nil || !filepath.IsAbs(startup) {
		appdata := os.Getenv("APPDATA")
		if !filepath.IsAbs(appdata) {
			return Roots{}, fmt.Errorf("autostart: cannot find the Startup folder: %v", err)
		}
		startup = filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	}
	return Roots{
		RunKey:            RunKey,
		ApprovedKey:       ApprovedKey,
		ApprovedFolderKey: ApprovedFolderKey,
		StartupDir:        startup,
	}, nil
}

// New returns a Manager for r. On Windows it uses HKCU Run values.
func New(r Roots) *Manager { return &Manager{impl: &runKey{r: r}} }

type runKey struct{ r Roots }

// valueName returns our Run value name for t.
func valueName(t Target) string {
	if t == Syncthing {
		return brand.AppDirName() + "-Syncthing"
	}
	return brand.AppDirName()
}

// commandLine builds a Run value: the quoted program followed by the
// arguments, each escaped with the CommandLineToArgvW rules.
func commandLine(bin string, args []string) string {
	var b strings.Builder
	b.WriteString(`"` + bin + `"`)
	for _, a := range args {
		b.WriteString(" " + syscall.EscapeArg(a))
	}
	return b.String()
}

func (k *runKey) enabled(t Target) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, k.r.RunKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	v, _, err := key.GetStringValue(valueName(t))
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(v) != "" && !k.disabled(k.r.ApprovedKey, valueName(t)), nil
}

// disabled reports whether Task Manager's Startup tab disabled the entry
// name: its StartupApproved value has an odd first byte.
func (k *runKey) disabled(approvedKey, name string) bool {
	if approvedKey == "" {
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, approvedKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	v, _, err := key.GetBinaryValue(name)
	return err == nil && len(v) > 0 && v[0]&1 == 1
}

func (k *runKey) set(t Target, on bool, bin string, args []string) error {
	name := valueName(t)
	if !on {
		key, err := registry.OpenKey(registry.CURRENT_USER, k.r.RunKey, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return k.clearApproved(name)
		}
		if err != nil {
			return err
		}
		defer key.Close()
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return k.clearApproved(name)
	}
	if args == nil {
		args = []string{"--background"}
		if t == Syncthing {
			args = []string{"serve", "--no-console", "--no-browser"}
		}
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, k.r.RunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetStringValue(name, commandLine(bin, args)); err != nil {
		return err
	}
	// A "disabled" mark left by Task Manager would keep the new entry off.
	return k.clearApproved(name)
}

func (k *runKey) clearApproved(name string) error {
	if k.r.ApprovedKey == "" {
		return nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, k.r.ApprovedKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// existing finds Syncthing autostart entries SyncThing V2 does not own:
// Startup-folder shortcuts whose target is syncthing.exe, then other Run
// values that start Syncthing. Entries disabled in Task Manager are skipped.
func (k *runKey) existing(Target) (string, bool) {
	if desc, ok := k.startupShortcut(); ok {
		return desc, true
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, k.r.RunKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer key.Close()
	names, err := key.ReadValueNames(0)
	if err != nil {
		return "", false
	}
	for _, n := range names {
		if strings.EqualFold(n, valueName(Tray)) || strings.EqualFold(n, valueName(Syncthing)) {
			continue
		}
		v, _, err := key.GetStringValue(n)
		if err != nil || !ReferencesSyncthing(v) || k.disabled(k.r.ApprovedKey, n) {
			continue
		}
		return "Run entry " + n, true
	}
	return "", false
}

func (k *runKey) startupShortcut() (string, bool) {
	if k.r.StartupDir == "" {
		return "", false
	}
	entries, err := os.ReadDir(k.r.StartupDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.EqualFold(filepath.Ext(n), ".lnk") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(k.r.StartupDir, n)
		fi, err := e.Info()
		if err != nil || fi.Size() > maxLnk {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		link, err := ParseShellLink(data)
		if err != nil || !strings.EqualFold(windowsBase(link.Target), "syncthing.exe") {
			continue
		}
		if k.disabled(k.r.ApprovedFolderKey, n) {
			continue
		}
		return "Startup folder shortcut " + n, true
	}
	return "", false
}

// windowsBase returns the last element of a Windows path.
func windowsBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// startService: Run values are not services.
func (k *runKey) startService(Target) (bool, error) { return false, nil }
