//go:build linux

package uihost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeDirSnapBrowser(t *testing.T) {
	home := t.TempDir()
	desktops := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "run"))
	savedDir, savedHandler := snapDesktopDir, htmlHandler
	defer func() { snapDesktopDir, htmlHandler = savedDir, savedHandler }()
	snapDesktopDir = desktops
	handler := "firefox_firefox.desktop"
	htmlHandler = func() string { return handler }

	// Not a snap (no desktop file from snapd): the usual runtime directory.
	if d, err := runtimeDir(); err != nil || d != filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "stv2") {
		t.Fatalf("runtimeDir = %q, %v; want the XDG runtime directory", d, err)
	}

	// A snap that has never run (no ~/snap/firefox/common) changes nothing.
	if err := os.WriteFile(filepath.Join(desktops, handler), []byte("[Desktop Entry]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _ := runtimeDir(); d != filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "stv2") {
		t.Fatalf("runtimeDir = %q before the snap has run", d)
	}

	// The snap's own common directory once it exists, and secureDir makes a
	// private launch directory there.
	common := filepath.Join(home, "snap", "firefox", "common")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := runtimeDir()
	if err != nil || d != filepath.Join(common, "stv2") {
		t.Fatalf("runtimeDir = %q, %v; want %q", d, err, filepath.Join(common, "stv2"))
	}
	if err := secureDir(d); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("snap launch directory mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}

	// No handler information: the usual directory.
	handler = ""
	if d, _ := runtimeDir(); d != filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "stv2") {
		t.Errorf("runtimeDir = %q without a known handler", d)
	}
}
