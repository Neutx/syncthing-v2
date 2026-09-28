package osutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// The test binary doubles as the helper process for the exec tests.
const helperEnv = "STV2_OSUTIL_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "echo":
		fmt.Print(strings.Join(os.Args[1:], "|"))
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "synthetic failure")
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "touch":
		// Outlive the parent's Wait briefly, then write the marker file.
		time.Sleep(200 * time.Millisecond)
		if err := os.WriteFile(os.Args[1], []byte("detached"), 0o600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func helper(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv(helperEnv, mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	if err := WriteFileAtomic(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2}` {
		t.Fatalf("content = %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left behind: %v", names)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "x.json")
	if err := WriteFileAtomic(p, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestOpenURLRejectsUnsafeInput(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"calc.exe",
		`C:\Windows\System32\calc.exe`,
		"ms-settings:privacy",
		"-flag",
		"http://",
		"https:///path-only",
		"file:",
		"",
	} {
		if err := OpenURL(raw); err == nil {
			t.Errorf("OpenURL(%q) = nil, want an error", raw)
		}
	}
}

func TestOpenURLRejectsFileURLsThatAreNotLaunchPages(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exe := write("calc.exe")
	desktop := write("x.desktop")
	script := write("run.sh")
	page := write("open-1.html")
	htmlDir := filepath.Join(dir, "folder.html")
	if err := os.Mkdir(htmlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "Calculator.app")
	if err := os.Mkdir(app, 0o700); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"file:///C:/Windows/System32/calc.exe",
		"file:///tmp/x.desktop",
		"file:///Applications/Calculator.app",
		FileURL(exe),
		FileURL(desktop),
		FileURL(script),
		FileURL(app),
		FileURL(htmlDir),                            // a directory, whatever its name
		FileURL(filepath.Join(dir, "missing.html")), // must exist
		FileURL(page) + "?q=1",                      // no query
		FileURL(page) + "#frag",                     // no fragment
		"file://server/share/open-1.html",           // no network share
		"file:open-1.html",                          // opaque
		"file://relative/open-1.html",               // "relative" is a host here
		"file:",
	}
	if runtime.GOOS == "windows" {
		bad = append(bad,
			"file:///C:open-1.html", // drive-relative
			"file:////server/share/open-1.html",
			"file:///"+filepath.ToSlash(exe)+":stream.html", // alternate data stream
		)
	}
	if link := filepath.Join(dir, "link.html"); os.Symlink(exe, link) == nil {
		bad = append(bad, FileURL(link)) // a symbolic link, even one named .html
	}
	for _, raw := range bad {
		// checkOpenURL, not OpenURL: a regression must fail the test, not
		// launch the file.
		if got, err := checkOpenURL(raw); err == nil {
			t.Errorf("checkOpenURL(%q) = %q, nil; want an error", raw, got)
		}
	}
}

func TestCheckOpenURLAcceptsWebURLsAndLaunchPages(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "open 1.html")
	upper := filepath.Join(dir, "OPEN-2.HTM")
	for _, p := range []string{page, upper} {
		if err := os.WriteFile(p, []byte("<!doctype html>"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for raw, want := range map[string]string{
		"https://syncthing.net/":            "https://syncthing.net/",
		"http://127.0.0.1:18384/#dashboard": "http://127.0.0.1:18384/#dashboard",
		FileURL(page):                       FileURL(page),
		FileURL(upper):                      FileURL(upper),
	} {
		got, err := checkOpenURL(raw)
		if err != nil {
			t.Errorf("checkOpenURL(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("checkOpenURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestOpenFolderRejectsInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"relative/dir",
		filepath.Join(dir, "does-not-exist"),
		file,
	} {
		if err := OpenFolder(p); err == nil {
			t.Errorf("OpenFolder(%q) = nil, want an error", p)
		}
	}
}

func TestFileURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		if got, want := FileURL(`C:\Temp Dir\open-1.html`), "file:///C:/Temp%20Dir/open-1.html"; got != want {
			t.Fatalf("FileURL = %q, want %q", got, want)
		}
		return
	}
	if got, want := FileURL("/run/user/1000/stv2/open 1.html"), "file:///run/user/1000/stv2/open%201.html"; got != want {
		t.Fatalf("FileURL = %q, want %q", got, want)
	}
}

func TestDirs(t *testing.T) {
	d, err := AppDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(d) || filepath.Base(d) != brand.AppDirName() {
		t.Fatalf("AppDataDir = %q", d)
	}
	l, err := LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(l) || !strings.Contains(l, brand.AppDirName()) {
		t.Fatalf("LogDir = %q", l)
	}
	h, err := HomeDir()
	if err != nil || !filepath.IsAbs(h) {
		t.Fatalf("HomeDir = %q, %v", h, err)
	}
}

func TestEnsureDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(d)
	if err != nil || !fi.IsDir() {
		t.Fatalf("EnsureDir did not create %q: %v", d, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestCommandIsHiddenOnWindows(t *testing.T) {
	cmd := Command(context.Background(), "x")
	if runtime.GOOS == "windows" && cmd.SysProcAttr == nil {
		t.Fatal("Command must set SysProcAttr on Windows")
	}
	if Detached("x").SysProcAttr == nil {
		t.Fatal("Detached must set SysProcAttr")
	}
}

func TestOutputPassesArgumentsVerbatim(t *testing.T) {
	exe := helper(t, "echo")
	args := []string{`a b`, `"quoted"`, `semi;colon & amp`, `$HOME`}
	out, err := Output(context.Background(), 20*time.Second, exe, args...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), strings.Join(args, "|"); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestOutputReportsStderr(t *testing.T) {
	exe := helper(t, "fail")
	_, err := Output(context.Background(), 20*time.Second, exe)
	if err == nil || !strings.Contains(err.Error(), "synthetic failure") {
		t.Fatalf("err = %v, want it to include stderr", err)
	}
}

func TestOutputTimeout(t *testing.T) {
	exe := helper(t, "sleep")
	start := time.Now()
	_, err := Output(context.Background(), 300*time.Millisecond, exe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}

func TestStartDetached(t *testing.T) {
	exe := helper(t, "touch")
	marker := filepath.Join(t.TempDir(), "marker.txt")
	pid, err := StartDetached(exe, marker)
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Fatalf("pid = %d", pid)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && string(b) == "detached" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached helper never wrote its marker file")
}

func TestStartDetachedMissingBinary(t *testing.T) {
	if _, err := StartDetached(filepath.Join(t.TempDir(), "no-such-binary")); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}
