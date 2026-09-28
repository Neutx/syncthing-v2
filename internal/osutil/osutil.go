// Package osutil wraps the few OS services SyncThing V2 needs: per-user
// directories, opening URLs and folders in the desktop shell, atomic file
// writes, and running helper processes (hidden on Windows, detached when
// they must outlive the tray). Processes are always started from an argument
// list, never through a shell.
package osutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AppDataDir returns the per-user application data directory:
//
//	Windows: %LOCALAPPDATA%\SyncThingV2
//	macOS:   ~/Library/Application Support/SyncThingV2
//	Linux:   $XDG_DATA_HOME/syncthing-v2 (default ~/.local/share/syncthing-v2)
//
// The directory is not created.
func AppDataDir() (string, error) { return appDataDir() }

// LogDir returns the directory for SyncThing V2's own log files:
//
//	Windows: %LOCALAPPDATA%\SyncThingV2\logs
//	macOS:   ~/Library/Logs/SyncThingV2
//	Linux:   $XDG_DATA_HOME/syncthing-v2/logs
//
// The directory is not created.
func LogDir() (string, error) { return logDir() }

// HomeDir returns the current user's home directory.
func HomeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(h) {
		return "", fmt.Errorf("home directory %q is not absolute", h)
	}
	return h, nil
}

// EnsureDir creates dir (and parents) with mode 0700 if it does not exist.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory followed by a rename, so readers see either the old or the new
// content, never a partial file. The file ends up with mode perm.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = setPerm(tmp, perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// On Windows a scanner or indexer can hold the target open for a moment,
	// which makes MoveFileEx fail with a sharing violation; retry briefly.
	for attempt := 0; ; attempt++ {
		if err = os.Rename(tmpName, path); err == nil || attempt == 9 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
}

// OpenURL opens a URL with the desktop's default handler. It accepts only
// http and https URLs with a host, and file URLs that name an existing,
// regular, local .html or .htm file (the dashboard launch file), so the shell
// opens it in the browser. Every other scheme, and any file URL that names a
// program, script, shortcut, .desktop file, bundle, directory, symbolic link
// or network share, is rejected, so a crafted string is never run as a program.
func OpenURL(raw string) error {
	target, err := checkOpenURL(raw)
	if err != nil {
		return err
	}
	return openURL(target)
}

// checkOpenURL validates raw for OpenURL and returns the string to hand to
// the desktop shell.
func checkOpenURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("open url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return "", fmt.Errorf("open url: %q has no host", raw)
		}
		return u.String(), nil
	case "file":
		p, err := launchFilePath(u)
		if err != nil {
			return "", fmt.Errorf("open url: %q: %w", raw, err)
		}
		return FileURL(p), nil
	default:
		return "", fmt.Errorf("open url: scheme %q is not allowed", u.Scheme)
	}
}

// launchFilePath returns the local path of a file URL when it names an
// existing regular .html or .htm file, and an error otherwise.
func launchFilePath(u *url.URL) (string, error) {
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("only a plain file:///path URL is allowed")
	}
	if u.Host != "" {
		return "", errors.New("file URLs on another host are not allowed")
	}
	p := u.Path
	if strings.ContainsRune(p, 0) {
		return "", errors.New("the path contains a NUL byte")
	}
	if runtime.GOOS == "windows" {
		// file:///C:/dir/x.html has the path /C:/dir/x.html.
		p = filepath.FromSlash(strings.TrimPrefix(p, "/"))
		// Only a drive-letter path: no UNC share (which would reach the
		// network), no device path, no alternate data stream after the drive.
		if len(p) < 3 || !isASCIILetter(p[0]) || p[1] != ':' || p[2] != '\\' || strings.Contains(p[2:], ":") {
			return "", errors.New("only a local drive path is allowed")
		}
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("the path is not absolute")
	}
	p = filepath.Clean(p)
	if ext := strings.ToLower(filepath.Ext(p)); ext != ".html" && ext != ".htm" {
		return "", errors.New("only .html and .htm files may be opened")
	}
	fi, err := os.Lstat(p) // Lstat: a symbolic link is not a regular file
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	return p, nil
}

func isASCIILetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// FileURL returns the file:// URL for an absolute local path.
func FileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive paths: file:///C:/...
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// OpenFolder shows an existing directory in the desktop file manager. On
// macOS a bundle directory (such as X.app) is revealed in its parent folder
// rather than opened, so OpenFolder never launches a program.
func OpenFolder(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("open folder: %q is not an absolute path", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("open folder: %q is not a directory", path)
	}
	return openFolder(filepath.Clean(path))
}

// Command returns a command that runs without a console window on Windows.
// Standard streams default to the null device, as with exec.Command.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = hiddenAttr()
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// Output runs a hidden command with a timeout and returns its standard
// output. On failure the error includes the trimmed standard error.
func Output(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := Command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			const max = 512
			if len(msg) > max {
				msg = msg[:max] + "..."
			}
			return stdout.Bytes(), fmt.Errorf("%s: %w: %s", filepath.Base(name), err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return stdout.Bytes(), nil
}

// Detached returns a command configured to outlive this process: on Windows
// it has no console and runs in a new process group
// (CREATE_NO_WINDOW | DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP); on Unix it
// starts a new session (Setsid). Callers may adjust Dir or Env before Start.
func Detached(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = detachedAttr()
	return cmd
}

// StartDetached starts name with args as a detached process and returns its
// PID. The child is reaped in the background, so it never becomes a zombie
// while this process keeps running.
func StartDetached(name string, args ...string) (int, error) {
	return startDetached(Detached(name, args...))
}

func startDetached(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
