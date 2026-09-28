package stinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// entry is one member of a synthetic archive.
type entry struct {
	name     string
	body     string
	dir      bool
	symlink  string // target, for a symbolic link
	hardlink string // target, for a tar hard link
	typeflag byte   // tar only: overrides the type (for special files)
}

func buildZip(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		body := e.body
		switch {
		case e.dir:
			h.SetMode(fs.ModeDir | 0o755)
		case e.symlink != "":
			h.SetMode(fs.ModeSymlink | 0o777)
			body = e.symlink
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildTarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.body)), Format: tar.FormatPAX}
		switch {
		case e.dir:
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		case e.symlink != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.symlink, 0
		case e.hardlink != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeLink, e.hardlink, 0
		case e.typeflag != 0:
			h.Typeflag, h.Size = e.typeflag, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeArchive stores an archive in its own temp dir and returns its path.
func writeArchive(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// good are the members of a well-formed release archive.
func good(bin string) []entry {
	return []entry{
		{name: "syncthing-x-v9.9.9/", dir: true},
		{name: "syncthing-x-v9.9.9/" + bin, body: "synthetic binary"},
		{name: "syncthing-x-v9.9.9/LICENSE.txt", body: "synthetic licence"},
		{name: "syncthing-x-v9.9.9/README.txt", body: "synthetic readme"},
		{name: "syncthing-x-v9.9.9/AUTHORS.txt", body: "synthetic authors"},
		{name: "syncthing-x-v9.9.9/etc/README.md", body: "not installed"},
		{name: "syncthing-x-v9.9.9/nested/syncthing", body: "not the binary"},
	}
}

func TestExtractInstallsBinaryAndNotices(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			var data []byte
			if format == "zip" {
				data = buildZip(t, append(good("syncthing"), entry{name: "syncthing-x-v9.9.9/link", symlink: "syncthing"}))
			} else {
				data = buildTarGz(t, append(good("syncthing"),
					entry{name: "syncthing-x-v9.9.9/link", symlink: "etc/README.md"},
					entry{name: "syncthing-x-v9.9.9/hard", hardlink: "syncthing-x-v9.9.9/README.txt"}))
			}
			archive := writeArchive(t, "a."+format, data)
			dest := t.TempDir()
			bin, err := extract(archive, dest, "syncthing")
			if err != nil {
				t.Fatal(err)
			}
			if bin != filepath.Join(dest, "syncthing") {
				t.Errorf("binary = %q", bin)
			}
			want := []string{"AUTHORS.txt", "LICENSE.txt", "README.txt", "syncthing"}
			if names := dirNames(t, dest); !reflect.DeepEqual(names, want) {
				t.Errorf("installed %v, want %v", names, want)
			}
			if b, _ := os.ReadFile(bin); string(b) != "synthetic binary" {
				t.Errorf("binary content %q (the nested file must not win)", b)
			}
		})
	}
}

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	bad := []struct {
		name string
		e    entry
	}{
		{"dot-dot", entry{name: "../syncthing", body: "x"}},
		{"nested dot-dot", entry{name: "top/../../syncthing", body: "x"}},
		{"harmless-looking dot-dot", entry{name: "top/a/../syncthing", body: "x"}},
		{"absolute", entry{name: "/tmp/syncthing", body: "x"}},
		{"backslash dot-dot", entry{name: `..\syncthing`, body: "x"}},
		{"backslash absolute", entry{name: `\Windows\syncthing.exe`, body: "x"}},
		{"drive letter", entry{name: `C:\syncthing.exe`, body: "x"}},
		{"drive relative", entry{name: `C:syncthing.exe`, body: "x"}},
		{"alternate data stream", entry{name: "top/LICENSE.txt:evil", body: "x"}},
		{"UNC", entry{name: `\\server\share\syncthing.exe`, body: "x"}},
		{"symlink out", entry{name: "top/link", symlink: "../../outside"}},
		{"symlink absolute", entry{name: "top/link", symlink: "/etc/passwd"}},
		{"symlink drive", entry{name: "top/link", symlink: `C:\Windows`}},
	}
	for _, format := range []string{"zip", "tar.gz"} {
		for _, b := range bad {
			t.Run(format+"/"+b.name, func(t *testing.T) {
				// The unsafe member comes after the good ones, so a
				// streaming extractor would already have written files.
				entries := append(good("syncthing"), b.e)
				var data []byte
				if format == "zip" {
					data = buildZip(t, entries)
				} else {
					data = buildTarGz(t, entries)
				}
				archive := writeArchive(t, "a."+format, data)
				dest := t.TempDir()
				_, err := extract(archive, dest, "syncthing")
				if !errors.Is(err, ErrUnsafeArchive) {
					t.Fatalf("extract error = %v, want ErrUnsafeArchive", err)
				}
				if names := dirNames(t, dest); len(names) != 0 {
					t.Errorf("files written despite the rejection: %v", names)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "syncthing")); err == nil {
					t.Error("a file escaped the destination")
				}
			})
		}
	}
}

func TestExtractRejectsTarSpecials(t *testing.T) {
	for name, e := range map[string]entry{
		"hard link out": {name: "top/hard", hardlink: "../etc/passwd"},
		"hard link abs": {name: "top/hard", hardlink: "/etc/passwd"},
		"char device":   {name: "top/dev", typeflag: tar.TypeChar},
		"fifo":          {name: "top/fifo", typeflag: tar.TypeFifo},
	} {
		t.Run(name, func(t *testing.T) {
			archive := writeArchive(t, "a.tar.gz", buildTarGz(t, append(good("syncthing"), e)))
			dest := t.TempDir()
			if _, err := extract(archive, dest, "syncthing"); !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("extract error = %v, want ErrUnsafeArchive", err)
			}
			if names := dirNames(t, dest); len(names) != 0 {
				t.Errorf("files written despite the rejection: %v", names)
			}
		})
	}
}

func TestExtractNeedsBinaryAndLicence(t *testing.T) {
	noBin := buildZip(t, []entry{{name: "top/LICENSE.txt", body: "l"}})
	noLicence := buildZip(t, []entry{{name: "top/syncthing.exe", body: "b"}})
	dup := buildZip(t, []entry{{name: "top/syncthing.exe", body: "b"}, {name: "other/syncthing.exe", body: "c"}, {name: "top/LICENSE.txt", body: "l"}})
	for name, data := range map[string][]byte{"no binary": noBin, "no licence": noLicence, "two binaries": dup} {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			if _, err := extract(writeArchive(t, "a.zip", data), dest, "syncthing.exe"); err == nil {
				t.Fatal("extract succeeded")
			}
			if names := dirNames(t, dest); len(names) != 0 {
				t.Errorf("files left behind: %v", names)
			}
		})
	}
	if _, err := extract(writeArchive(t, "a.rar", []byte("x")), t.TempDir(), "syncthing"); err == nil {
		t.Error("extract accepted an unknown archive type")
	}
	if _, err := extract(writeArchive(t, "a.zip", []byte("not a zip")), t.TempDir(), "syncthing"); err == nil {
		t.Error("extract accepted a corrupt zip")
	}
}

// blockWith puts a non-empty directory at dest/name, so renaming a file onto
// that name fails on every OS (as a locked syncthing.exe does on Windows).
func blockWith(t *testing.T, dest, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dest, name, "busy"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBinaryFailureLeavesNoticesUntouched(t *testing.T) {
	data := buildZip(t, good("syncthing"))
	// Repeated because the old implementation renamed in map order, which
	// touched a notice before the binary only some of the time.
	for i := 0; i < 8; i++ {
		dest := t.TempDir()
		blockWith(t, dest, "syncthing")
		if err := os.WriteFile(filepath.Join(dest, "LICENSE.txt"), []byte("old licence"), 0o644); err != nil {
			t.Fatal(err)
		}
		bin, err := extract(writeArchive(t, "a.zip", data), dest, "syncthing")
		if err == nil {
			t.Fatal("extract succeeded although the binary could not be replaced")
		}
		if bin != "" {
			t.Errorf("binary path %q returned for a failed install", bin)
		}
		if b, _ := os.ReadFile(filepath.Join(dest, "LICENSE.txt")); string(b) != "old licence" {
			t.Fatalf("LICENSE.txt replaced (%q) although the binary was not", b)
		}
		if names := dirNames(t, dest); !reflect.DeepEqual(names, []string{"LICENSE.txt", "syncthing"}) {
			t.Fatalf("dest holds %v, want only the previous files", names)
		}
	}
}

func TestExtractNoticeFailureKeepsBinary(t *testing.T) {
	dest := t.TempDir()
	blockWith(t, dest, "README.txt")
	bin, err := extract(writeArchive(t, "a.zip", buildZip(t, good("syncthing"))), dest, "syncthing")
	if err == nil {
		t.Fatal("extract hid the failed notice")
	}
	if !strings.Contains(err.Error(), "was installed") || !strings.Contains(err.Error(), "README.txt") {
		t.Errorf("error %q does not say the binary is installed and which notice failed", err)
	}
	if bin != filepath.Join(dest, "syncthing") {
		t.Errorf("binary path = %q", bin)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "syncthing")); string(b) != "synthetic binary" {
		t.Errorf("binary content %q", b)
	}
	for _, n := range []string{"LICENSE.txt", "AUTHORS.txt"} {
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			t.Errorf("%s not installed after the README failure: %v", n, err)
		}
	}
	if names := dirNames(t, dest); !reflect.DeepEqual(names, []string{"AUTHORS.txt", "LICENSE.txt", "README.txt", "syncthing"}) {
		t.Errorf("dest holds %v (staged temporary files must be removed)", names)
	}
}

func TestEntryPath(t *testing.T) {
	ok := map[string]string{
		"a/b.txt":   "a/b.txt",
		"a/./b.txt": "a/b.txt",
		"./a/":      "a",
		`a\b.txt`:   "a/b.txt",
		"a//b":      "a/b",
	}
	for in, want := range ok {
		if got, err := entryPath(in); err != nil || got != want {
			t.Errorf("entryPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "..", "../a", "a/../../b", "/a", `\a`, "C:/a", "a\x00b", `a\..\..\b`} {
		if _, err := entryPath(in); !errors.Is(err, ErrUnsafeArchive) {
			t.Errorf("entryPath(%q) error = %v, want ErrUnsafeArchive", in, err)
		}
	}
}
