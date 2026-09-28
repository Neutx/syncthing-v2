//go:build windows

package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// netshTimeout bounds one netsh run.
const netshTimeout = 30 * time.Second

func productionFirewall() firewall {
	return firewall{elevated: isElevated, netsh: runNetsh, relaunch: relaunchElevated}
}

// AllowFirewall adds the inbound rule "SyncThing V2 - Syncthing" for the
// Syncthing executable bin on port 22000, TCP and UDP, from the tailnet
// (100.64.0.0/10) and the local subnet (§5.5). Without administrator rights
// it re-launches this executable with the "runas" verb (a UAC prompt) and
// ElevatedFirewallArgs, and waits for it. A declined prompt returns
// ErrElevationDeclined.
func AllowFirewall(ctx context.Context, bin string) error {
	return productionFirewall().allow(ctx, bin)
}

// AllowFirewallElevated is AllowFirewall for the elevated child started with
// ElevatedFirewallArgs: it adds the rule when it has administrator rights and
// otherwise returns ErrNotElevated, never re-launching again.
func AllowFirewallElevated(ctx context.Context, bin string) error {
	f := productionFirewall()
	f.child = true
	return f.allow(ctx, bin)
}

// HasFirewallRule reports whether the rule "SyncThing V2 - Syncthing"
// exists. It looks up our own rule name only and needs no elevation.
func HasFirewallRule(ctx context.Context) (bool, error) {
	return productionFirewall().hasRule(ctx)
}

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// runNetsh runs %SystemRoot%\System32\netsh.exe with args passed verbatim:
// netsh parses name="..." itself, which the usual argument quoting would
// break.
func runNetsh(ctx context.Context, args string) ([]byte, int, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, 0, err
	}
	netsh := filepath.Join(sys, "netsh.exe")
	ctx, cancel := context.WithTimeout(ctx, netshTimeout)
	defer cancel()
	cmd := osutil.Command(ctx, netsh)
	cmd.SysProcAttr.CmdLine = `"` + netsh + `" ` + args
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	if err != nil {
		return out, 0, fmt.Errorf("netsh: %w", err)
	}
	return out, 0, nil
}

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// relaunchElevated starts this executable elevated with args and waits for
// it to finish.
func relaunchElevated(ctx context.Context, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return err
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        windows.SW_HIDE,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	ok, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return ErrElevationDeclined
		}
		return fmt.Errorf("elevate: %w", callErr)
	}
	if info.hProcess == 0 {
		return errors.New("elevate: no process handle")
	}
	defer windows.CloseHandle(info.hProcess)
	for {
		ev, err := windows.WaitForSingleObject(info.hProcess, 250)
		if err != nil {
			return err
		}
		if ev == windows.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the elevated firewall setup failed (exit code %d)", code)
	}
	return nil
}
