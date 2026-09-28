//go:build !windows

package stinstall

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "syncthing" }
func (f fakeInfo) Size() int64        { return 1 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestOwnedSafely(t *testing.T) {
	const me = 501
	for _, c := range []struct {
		name string
		fi   fakeInfo
		want bool
	}{
		{"mine", fakeInfo{0o755, me}, true},
		{"root's", fakeInfo{0o555, 0}, true},
		{"another user's", fakeInfo{0o755, 502}, false},
		{"group-writable", fakeInfo{0o775, me}, false},
		{"world-writable root's", fakeInfo{0o757, 0}, false},
		{"directory", fakeInfo{fs.ModeDir | 0o755, me}, false},
	} {
		if got := ownedSafely(c.fi, me); got != c.want {
			t.Errorf("%s: ownedSafely = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTrustedBin(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "syncthing")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if !trustedBin(bin) {
		t.Error("own 0755 binary is not trusted")
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	if trustedBin(bin) {
		t.Error("world-writable binary is trusted")
	}
	if trustedBin(filepath.Join(dir, "missing")) || trustedBin(dir) {
		t.Error("missing file or directory is trusted")
	}
}
