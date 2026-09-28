package stinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// The test binary doubles as a fake `syncthing` (see TestMain).
const (
	fakeModeEnv = "STV2_STINSTALL_FAKE_SYNCTHING"
	fakeOutEnv  = "STV2_STINSTALL_FAKE_OUT"
)

func TestMain(m *testing.M) {
	mode := os.Getenv(fakeModeEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	os.Exit(fakeSyncthing(mode, os.Args[1:]))
}

// fakeSyncthing imitates the syncthing commands stinstall runs. It records
// its arguments and the Syncthing variables it saw in $STV2_STINSTALL_FAKE_OUT.
func fakeSyncthing(mode string, args []string) int {
	record := func() {
		if out := os.Getenv(fakeOutEnv); out != "" {
			env := map[string]string{}
			for _, k := range []string{"STHOMEDIR", "STCONFDIR", "STDATADIR", "STNOPORTPROBING"} {
				if v, ok := os.LookupEnv(k); ok {
					env[k] = v
				}
			}
			b, _ := json.Marshal(map[string]any{"args": args, "env": env})
			_ = os.WriteFile(out, b, 0o600)
		}
	}
	switch {
	case mode == "ok" && len(args) == 1 && args[0] == "--version":
		fmt.Println(`syncthing v2.1.5 "Hafnium Hornet" (go1.27.1 windows-amd64) builder@example.invalid 2026-09-08 06:57:55 UTC`)
		return 0
	case mode == "ok" && len(args) == 3 && args[0] == "generate" && args[1] == "--home":
		record()
		cfg := `<configuration version="37"><gui enabled="true" tls="false"><address>127.0.0.1:18399</address><apikey>synthetic-generated-key</apikey></gui></configuration>`
		if err := os.WriteFile(filepath.Join(args[2], "config.xml"), []byte(cfg), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case mode == "ok" && len(args) > 0 && args[0] == "serve":
		record()
		return 0
	case mode == "nofile" && len(args) > 0 && args[0] == "generate":
		return 0 // succeeds without writing config.xml
	default:
		fmt.Fprintln(os.Stderr, "fake syncthing: refusing", strings.Join(args, " "))
		return 1
	}
}

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// ---------------------------------------------------------------------------
// Versions

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		v  string
		ok bool
	}{
		{"v1.27.0", true},
		{"v1.27.1", true},
		{"v2.1.5", true},
		{"1.30.0", true},
		{"v2.0.0-beta.3+22-gabcdef", true},
		{"v1.26.9", false},
		{"v1.27.0-rc.1", false},
		{"v0.14.52", false},
		{"", false},
		{"unknown-dev", false},
		{"v1.27", false},
	}
	for _, c := range cases {
		ok, msg := CheckVersion(c.v)
		if ok != c.ok {
			t.Errorf("CheckVersion(%q) = %v (%s), want %v", c.v, ok, msg, c.ok)
		}
		if msg == "" || !strings.Contains(msg, brand.MinSyncthing) {
			t.Errorf("CheckVersion(%q) message %q does not name %s", c.v, msg, brand.MinSyncthing)
		}
	}
	if _, msg := CheckVersion("v1.2.3"); !strings.Contains(msg, "older than") {
		t.Errorf("old-version message = %q", msg)
	}
}

func TestCompareVersionsSemverOrder(t *testing.T) {
	// The precedence example from the SemVer 2.0.0 specification.
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	for i := range order {
		for j := range order {
			got, err := CompareVersions(order[i], order[j])
			if err != nil {
				t.Fatal(err)
			}
			want := cmpInt(i, j)
			if got != want {
				t.Errorf("CompareVersions(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	if c, _ := CompareVersions("v1.2.3+build.1", "1.2.3"); c != 0 {
		t.Errorf("build metadata must be ignored, got %d", c)
	}
	if _, err := CompareVersions("v1.2", "v1.2.0"); err == nil {
		t.Error("CompareVersions accepted a two-part version")
	}
}

func TestParseVersionOutput(t *testing.T) {
	cases := map[string]string{
		`syncthing v2.1.5 "Hafnium Hornet" (go1.27.1 windows-amd64) builder@example.invalid 2026-09-08 06:57:55 UTC`:      "v2.1.5",
		"syncthing v1.27.2 \"Gold Grasshopper\" (go1.21.5 linux-amd64) builder@example.invalid 2023-12-20 00:00:00 UTC\n": "v1.27.2",
		"syncthing v2.0.0-rc.2 \"x\" (go1.24 darwin-arm64)":                                                               "v2.0.0-rc.2",
		"no version here (go1.27.1)": "",
		"":                           "",
	}
	for in, want := range cases {
		if got := ParseVersionOutput(in); got != want {
			t.Errorf("ParseVersionOutput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBinVersionRunsBinary(t *testing.T) {
	t.Setenv(fakeModeEnv, "ok")
	if v := BinVersion(context.Background(), testExe(t)); v != "v2.1.5" {
		t.Errorf("BinVersion = %q, want v2.1.5", v)
	}
	t.Setenv(fakeModeEnv, "fail")
	if v := BinVersion(context.Background(), testExe(t)); v != "" {
		t.Errorf("BinVersion of a failing binary = %q, want empty", v)
	}
}

// ---------------------------------------------------------------------------
// Pins and assets

func TestPinsMatchBootstrapVersion(t *testing.T) {
	if pinnedVersion != brand.BootstrapSyncthing {
		t.Fatalf("pins.go is for %s but brand.BootstrapSyncthing is %s; run scripts/update-syncthing-pin.sh", pinnedVersion, brand.BootstrapSyncthing)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, p := range [][2]string{{"windows", "amd64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		a, err := Asset(p[0], p[1], pinnedVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !hexRe.MatchString(pins[a]) {
			t.Errorf("pin for %s = %q, want 64 lowercase hex digits", a, pins[a])
		}
	}
	if len(pins) != 4 {
		t.Errorf("pins has %d entries, want 4", len(pins))
	}
}

func TestAsset(t *testing.T) {
	cases := map[[2]string]string{
		{"windows", "amd64"}: "syncthing-windows-amd64-v9.9.9.zip",
		{"windows", "arm64"}: "syncthing-windows-amd64-v9.9.9.zip",
		{"darwin", "arm64"}:  "syncthing-macos-universal-v9.9.9.zip",
		{"darwin", "amd64"}:  "syncthing-macos-universal-v9.9.9.zip",
		{"linux", "amd64"}:   "syncthing-linux-amd64-v9.9.9.tar.gz",
		{"linux", "arm64"}:   "syncthing-linux-arm64-v9.9.9.tar.gz",
	}
	for in, want := range cases {
		got, err := Asset(in[0], in[1], "v9.9.9")
		if err != nil || got != want {
			t.Errorf("Asset(%s/%s) = %q, %v; want %q", in[0], in[1], got, err, want)
		}
	}
	for _, in := range [][2]string{{"linux", "386"}, {"freebsd", "amd64"}, {"windows", "386"}} {
		if _, err := Asset(in[0], in[1], "v9.9.9"); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("Asset(%s/%s) error = %v, want ErrUnsupportedPlatform", in[0], in[1], err)
		}
	}
}

// ---------------------------------------------------------------------------
// Download

// releaseServer serves one synthetic release asset under /<version>/<asset>.
func releaseServer(t *testing.T, version, asset string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+version+"/"+asset {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), brand.BinaryName+"/") {
			http.Error(w, "missing user agent", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func testDownloader(srv *httptest.Server, goos, asset, pin string) downloader {
	return downloader{
		client:  srv.Client(),
		baseURL: srv.URL,
		version: "v9.9.9",
		pins:    map[string]string{asset: pin},
		goos:    goos,
		goarch:  "amd64",
	}
}

// dirNames lists the names in dir, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDownloadVerifiesAndInstalls(t *testing.T) {
	for _, tc := range []struct{ goos, asset string }{
		{"windows", "syncthing-windows-amd64-v9.9.9.zip"},
		{"linux", "syncthing-linux-amd64-v9.9.9.tar.gz"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			bin := exeName(tc.goos)
			top := strings.TrimSuffix(strings.TrimSuffix(tc.asset, ".zip"), ".tar.gz")
			entries := []entry{
				{name: top + "/", dir: true},
				{name: top + "/" + bin, body: "synthetic binary"},
				{name: top + "/LICENSE.txt", body: "synthetic licence"},
				{name: top + "/README.txt", body: "synthetic readme"},
				{name: top + "/AUTHORS.txt", body: "synthetic authors"},
				{name: top + "/etc/linux-systemd/user/syncthing.service", body: "[Unit]"},
			}
			var archive []byte
			if strings.HasSuffix(tc.asset, ".zip") {
				archive = buildZip(t, entries)
			} else {
				archive = buildTarGz(t, entries)
			}
			srv := releaseServer(t, "v9.9.9", tc.asset, archive)
			dir := t.TempDir()
			var mu sync.Mutex
			var lastDone, lastTotal int64
			got, err := testDownloader(srv, tc.goos, tc.asset, sum(archive)).download(context.Background(), dir, func(done, total int64) {
				mu.Lock()
				lastDone, lastTotal = done, total
				mu.Unlock()
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != filepath.Join(dir, bin) {
				t.Errorf("Download returned %q, want %q", got, filepath.Join(dir, bin))
			}
			if lastDone != int64(len(archive)) || lastTotal != int64(len(archive)) {
				t.Errorf("progress ended at %d/%d, want %d/%d", lastDone, lastTotal, len(archive), len(archive))
			}
			want := []string{"AUTHORS.txt", "LICENSE.txt", "README.txt", bin}
			if names := dirNames(t, dir); !reflect.DeepEqual(names, want) {
				t.Errorf("installed files = %v, want %v (archive and temp files must be gone)", names, want)
			}
			if b, _ := os.ReadFile(got); string(b) != "synthetic binary" {
				t.Errorf("binary content = %q", b)
			}
			if runtime.GOOS != "windows" {
				if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o755 {
					t.Errorf("binary mode = %v, want 0755", fi.Mode().Perm())
				}
			}
		})
	}
}

func TestDownloadPinMismatchAbortsAndDeletes(t *testing.T) {
	asset := "syncthing-windows-amd64-v9.9.9.zip"
	archive := buildZip(t, []entry{
		{name: "top/syncthing.exe", body: "synthetic binary"},
		{name: "top/LICENSE.txt", body: "synthetic licence"},
	})
	srv := releaseServer(t, "v9.9.9", asset, archive)
	dir := t.TempDir()
	wrong := strings.Repeat("0", 64)
	bin, err := testDownloader(srv, "windows", asset, wrong).download(context.Background(), dir, nil)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Download error = %v, want ErrChecksumMismatch", err)
	}
	if bin != "" {
		t.Errorf("Download returned %q on mismatch", bin)
	}
	if !strings.Contains(err.Error(), sum(archive)) || !strings.Contains(err.Error(), wrong) {
		t.Errorf("error %q should name both hashes", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("files left after a pin mismatch: %v", names)
	}
}

func TestDownloadHTTPErrorsLeaveNothing(t *testing.T) {
	asset := "syncthing-linux-amd64-v9.9.9.tar.gz"
	srv := releaseServer(t, "v9.9.9", "other-asset.tar.gz", []byte("x"))
	dir := t.TempDir()
	if _, err := testDownloader(srv, "linux", asset, strings.Repeat("0", 64)).download(context.Background(), dir, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Download error = %v, want a 404", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("files left after an HTTP error: %v", names)
	}

	d := testDownloader(srv, "linux", asset, strings.Repeat("0", 64))
	d.pins = map[string]string{}
	if _, err := d.download(context.Background(), dir, nil); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("unpinned asset error = %v, want ErrUnsupportedPlatform", err)
	}
	if _, err := testDownloader(srv, "linux", asset, "x").download(context.Background(), "relative/dir", nil); err == nil {
		t.Error("Download accepted a relative directory")
	}
}

func TestDownloadTruncatedBody(t *testing.T) {
	asset := "syncthing-linux-amd64-v9.9.9.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if _, err := testDownloader(srv, "linux", asset, strings.Repeat("0", 64)).download(context.Background(), dir, nil); err == nil {
		t.Fatal("Download accepted a truncated body")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("files left after a truncated download: %v", names)
	}
}

func TestRedirectMustKeepScheme(t *testing.T) {
	c := newHTTPClient()
	https, _ := url.Parse("https://example.invalid/a")
	http1, _ := url.Parse("http://example.invalid/b")
	via := []*http.Request{{URL: https}}
	if err := c.CheckRedirect(&http.Request{URL: http1}, via); err == nil {
		t.Error("an https to http redirect was allowed")
	}
	https2, _ := url.Parse("https://cdn.example.invalid/c")
	if err := c.CheckRedirect(&http.Request{URL: https2}, via); err != nil {
		t.Errorf("an https redirect was refused: %v", err)
	}
}

// TestDownloadReal downloads and verifies the real pinned release for this
// platform. It runs only when selected explicitly, for example
//
//	go test -run TestDownloadReal ./internal/stinstall/
//
// and only when github.com is reachable.
func TestDownloadReal(t *testing.T) {
	if f := flag.Lookup("test.run"); f == nil || !strings.Contains(f.Value.String(), "TestDownloadReal") {
		t.Skip("network test; run with -run TestDownloadReal")
	}
	if _, err := Asset(runtime.GOOS, runtime.GOARCH, pinnedVersion); err != nil {
		t.Skip(err)
	}
	conn, err := net.DialTimeout("tcp", "github.com:443", 5*time.Second)
	if err != nil {
		t.Skipf("github.com unreachable: %v", err)
	}
	conn.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var last int64
	bin, err := Download(ctx, dir, func(done, _ int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("downloaded %d bytes, installed %s", last, bin)
	for _, n := range []string{ExeName(), "LICENSE.txt", "README.txt", "AUTHORS.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("missing %s: %v", n, err)
		}
	}
	out, err := osutil.Output(ctx, 30*time.Second, bin, "--version")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s --version: %s", filepath.Base(bin), strings.TrimSpace(string(out)))
	if got := ParseVersionOutput(string(out)); got != brand.BootstrapSyncthing {
		t.Errorf("version = %q, want %s", got, brand.BootstrapSyncthing)
	}
}

// ---------------------------------------------------------------------------
// Detect

type fakeFS map[string]bool

func (f fakeFS) isFile(p string) bool { return f[filepath.Clean(p)] }

func TestDetectOrder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	lad := filepath.Join(root, "lad")
	running := filepath.Join(root, "proc", "syncthing.exe")
	onPath := filepath.Join(root, "path", "syncthing.exe")
	known := filepath.Join(lad, "Programs", "Syncthing", "syncthing.exe")
	managed := filepath.Join(lad, "Programs", brand.AppDirName(), "syncthing", "syncthing.exe")

	all := fakeFS{running: true, onPath: true, known: true, managed: true}
	mk := func(files fakeFS, run []string, path string) detector {
		return detector{
			goos:   "windows",
			getenv: func(k string) string { return map[string]string{"LOCALAPPDATA": lad, "USERPROFILE": home}[k] },
			home:   func() (string, error) { return home, nil },
			running: func(context.Context) []string {
				return run
			},
			lookPath: func(string) (string, error) {
				if path == "" {
					return "", errors.New("not found")
				}
				return path, nil
			},
			isFile:    files.isFile,
			version:   func(context.Context, string) string { return "v2.1.5" },
			config:    func(context.Context, string) string { return filepath.Join(root, "config.xml") },
			autostart: func() bool { return true },
		}
	}

	cases := []struct {
		name        string
		d           detector
		bin         string
		isRunning   bool
		isManaged   bool
		errNotFound bool
	}{
		{"running first", mk(all, []string{filepath.Join(root, "gone.exe"), running}, onPath), running, true, false, false},
		{"then PATH", mk(all, nil, onPath), onPath, false, false, false},
		{"then known paths", mk(all, nil, ""), known, false, false, false},
		{"then managed", mk(fakeFS{managed: true}, nil, ""), managed, false, true, false},
		{"running managed binary is managed", mk(all, []string{managed}, onPath), managed, true, true, false},
		{"PATH entry that is missing is skipped", mk(fakeFS{known: true}, nil, onPath), known, false, false, false},
		{"nothing", mk(fakeFS{}, nil, ""), "", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := c.d.detect(context.Background())
			if c.errNotFound {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("detect error = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Bin != c.bin || in.Running != c.isRunning || in.Managed != c.isManaged {
				t.Errorf("detect = %+v, want bin %s running %v managed %v", in, c.bin, c.isRunning, c.isManaged)
			}
			if in.Version != "v2.1.5" || in.ConfigPath != filepath.Join(root, "config.xml") || !in.AutostartExists {
				t.Errorf("detect details = %+v", in)
			}
		})
	}
}

func TestKnownAndManagedPaths(t *testing.T) {
	home := filepath.FromSlash("/home/u")
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	h := func() (string, error) { return home, nil }

	lad := filepath.Join(home, "AppData", "Local")
	winKnown := knownPaths("windows", env(nil), h)
	wantWin := []string{
		filepath.Join(lad, "Programs", "Syncthing", "syncthing.exe"),
		filepath.Join(home, "scoop", "shims", "syncthing.exe"),
		filepath.Join(lad, "Microsoft", "WinGet", "Links", "syncthing.exe"),
	}
	if !reflect.DeepEqual(winKnown, wantWin) {
		t.Errorf("windows known paths = %v, want %v", winKnown, wantWin)
	}
	wantLinux := []string{
		"/usr/bin/syncthing",
		filepath.Join(home, ".local", "bin", "syncthing"),
		"/snap/bin/syncthing",
		"/usr/local/bin/syncthing",
		"/home/linuxbrew/.linuxbrew/bin/syncthing",
		filepath.Join(home, ".linuxbrew", "bin", "syncthing"),
		filepath.Join(home, ".nix-profile", "bin", "syncthing"),
	}
	if got := knownPaths("linux", env(nil), h); !reflect.DeepEqual(got, wantLinux) {
		t.Errorf("linux known paths = %v, want %v", got, wantLinux)
	}
	wantDarwin := []string{
		"/opt/homebrew/bin/syncthing",
		"/usr/local/bin/syncthing",
		"/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing",
		"/opt/local/bin/syncthing",
		filepath.Join(home, ".nix-profile", "bin", "syncthing"),
	}
	if got := knownPaths("darwin", env(nil), h); !reflect.DeepEqual(got, wantDarwin) {
		t.Errorf("darwin known paths = %v, want %v", got, wantDarwin)
	}

	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"windows", nil, filepath.Join(lad, "Programs", brand.AppDirName(), "syncthing")},
		{"darwin", nil, filepath.Join(home, "Library", "Application Support", brand.AppDirName(), "syncthing")},
		{"linux", nil, filepath.Join(home, ".local", "share", brand.AppDirName(), "syncthing")},
		{"linux", map[string]string{"XDG_DATA_HOME": "relative"}, filepath.Join(home, ".local", "share", brand.AppDirName(), "syncthing")},
	} {
		got, err := managedDir(c.goos, env(c.env), h)
		if err != nil || got != c.want {
			t.Errorf("managedDir(%s, %v) = %q, %v; want %q", c.goos, c.env, got, err, c.want)
		}
	}

	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"windows", nil, filepath.Join(lad, "Syncthing")},
		{"darwin", nil, filepath.Join(home, "Library", "Application Support", "Syncthing")},
		{"linux", nil, filepath.Join(home, ".local", "state", "syncthing")},
	} {
		got, err := defaultHome(c.goos, env(c.env), h)
		if err != nil || got != c.want {
			t.Errorf("defaultHome(%s) = %q, %v; want %q", c.goos, got, err, c.want)
		}
	}
	abs := t.TempDir()
	if got, _ := defaultHome("linux", env(map[string]string{"XDG_STATE_HOME": abs}), h); got != filepath.Join(abs, "syncthing") {
		t.Errorf("defaultHome with XDG_STATE_HOME = %q", got)
	}
}

func TestParsePS(t *testing.T) {
	out := []byte("  1     0 /sbin/launchd\n" +
		" 501   501 /Applications/Syncthing.app/Contents/Resources/syncthing/syncthing\n" +
		" 502   501 syncthing\n" +
		" 503   501 /usr/local/bin/syncthing-inotify\n" +
		" 504   501 /opt/homebrew/bin/syncthing\n" +
		" 505   502 /Users/Shared/bin/syncthing\n" + // another user's process
		" 506     0 /usr/local/bin/syncthing\n" + // root's process
		" 507   501 /Applications/Sync Tools.app/Contents/MacOS/syncthing\n" +
		"bad line\n 508 /opt/homebrew/bin/syncthing\n")
	want := []string{
		"/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing",
		"/opt/homebrew/bin/syncthing",
		"/Applications/Sync Tools.app/Contents/MacOS/syncthing",
	}
	if got := parsePS(out, 501); !reflect.DeepEqual(got, want) {
		t.Errorf("parsePS = %v, want %v", got, want)
	}
	if got := parsePS(out, 502); !reflect.DeepEqual(got, []string{"/Users/Shared/bin/syncthing"}) {
		t.Errorf("parsePS for uid 502 = %v", got)
	}
}

func TestSnapLauncher(t *testing.T) {
	for _, c := range []struct {
		exe, launcher string
		isSnap        bool
	}{
		{"/snap/syncthing/123/syncthing", "/snap/bin/syncthing", true},
		{"/snap/syncthing/current/bin/syncthing", "/snap/bin/syncthing", true},
		{"/snap/bin/syncthing", "/snap/bin/syncthing", true},
		{"/snap/", "", false},
		{"/snap/../usr/bin/syncthing", "", false},
		{"/usr/bin/syncthing", "", false},
		{"/snapshots/syncthing", "", false},
	} {
		launcher, isSnap := snapLauncher(c.exe)
		if launcher != c.launcher || isSnap != c.isSnap {
			t.Errorf("snapLauncher(%q) = %q, %v; want %q, %v", c.exe, launcher, isSnap, c.launcher, c.isSnap)
		}
	}
}

func TestProcSyncthing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses symbolic links like /proc/<pid>/exe")
	}
	proc := t.TempDir()
	mk := func(pid, comm, exe string) {
		d := filepath.Join(proc, pid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(d, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	mk("10", "bash", "/usr/bin/bash")
	mk("20", "syncthing", "/home/u/.local/share/syncthing-v2/syncthing/syncthing (deleted)")
	mk("self", "syncthing", "/usr/bin/syncthing")
	want := []string{"/home/u/.local/share/syncthing-v2/syncthing/syncthing"}
	none := func(string) bool { return false }
	if got := procSyncthing(context.Background(), proc, none); !reflect.DeepEqual(got, want) {
		t.Errorf("procSyncthing = %v, want %v", got, want)
	}

	// A snap Syncthing is reported as its launcher, never by its path inside
	// the snap; without a launcher it is skipped.
	mk("30", "syncthing", "/snap/syncthing/123/syncthing")
	if got := procSyncthing(context.Background(), proc, none); !reflect.DeepEqual(got, want) {
		t.Errorf("procSyncthing without launcher = %v, want %v", got, want)
	}
	launcher := fakeFS{"/snap/bin/syncthing": true}
	wantSnap := append(want, "/snap/bin/syncthing")
	if got := procSyncthing(context.Background(), proc, launcher.isFile); !reflect.DeepEqual(got, wantSnap) {
		t.Errorf("procSyncthing with launcher = %v, want %v", got, wantSnap)
	}

	// Detect then adopts the launcher as the running Syncthing.
	d := detector{
		goos:      "linux",
		getenv:    func(string) string { return "" },
		home:      func() (string, error) { return "/home/u", nil },
		running:   func(ctx context.Context) []string { return procSyncthing(ctx, proc, launcher.isFile) },
		lookPath:  func(string) (string, error) { return "", errors.New("not found") },
		isFile:    launcher.isFile,
		version:   func(context.Context, string) string { return "v2.1.5" },
		config:    func(context.Context, string) string { return "" },
		autostart: func() bool { return false },
	}
	in, err := d.detect(context.Background())
	if err != nil || in.Bin != "/snap/bin/syncthing" || !in.Running {
		t.Errorf("detect = %+v, %v; want running /snap/bin/syncthing", in, err)
	}
}

// A running Syncthing seen through /proc/<pid>/exe is the resolved store path
// (Homebrew Cellar, Nix store), which the next upgrade or garbage collection
// deletes. Detect reports the stable symlink that points at it instead, so
// login entries keep working.
func TestDetectPrefersStableLink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cellar := filepath.Join(root, "linuxbrew", "Cellar", "syncthing", "2.1.5", "bin", "syncthing")
	brewLink := filepath.Join(root, "linuxbrew", "bin", "syncthing")
	store := filepath.Join(root, "nix", "store", "abc123-syncthing-2.1.5", "bin", "syncthing")
	nixLink := filepath.Join(home, ".nix-profile", "bin", "syncthing")
	plain := filepath.Join(root, "opt", "syncthing")
	files := fakeFS{cellar: true, brewLink: true, store: true, nixLink: true, plain: true}
	links := map[string]string{brewLink: cellar, nixLink: store}

	mk := func(run, onPath string) detector {
		return detector{
			goos:    "linux",
			getenv:  func(string) string { return "" },
			home:    func() (string, error) { return home, nil },
			running: func(context.Context) []string { return []string{run} },
			lookPath: func(string) (string, error) {
				if onPath == "" {
					return "", errors.New("not found")
				}
				return onPath, nil
			},
			isFile:    files.isFile,
			version:   func(context.Context, string) string { return "v2.1.5" },
			config:    func(context.Context, string) string { return "" },
			autostart: func() bool { return false },
			resolve: func(p string) (string, error) {
				if r, ok := links[p]; ok {
					return r, nil
				}
				return p, nil
			},
		}
	}
	for _, c := range []struct {
		name, run, onPath, want string
	}{
		{"Linuxbrew Cellar -> PATH symlink", cellar, brewLink, brewLink},
		{"Nix store -> ~/.nix-profile", store, "", nixLink},
		{"no link points at it", cellar, plain, cellar},
		{"not versioned", plain, brewLink, plain},
	} {
		t.Run(c.name, func(t *testing.T) {
			in, err := mk(c.run, c.onPath).detect(context.Background())
			if err != nil || in.Bin != c.want || !in.Running {
				t.Errorf("detect = %+v, %v; want running %s", in, err, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Generate and Start

type fakeRecord struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func readRecord(t *testing.T, p string) fakeRecord {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r fakeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGenerate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "record.json")
	home := filepath.Join(t.TempDir(), "st-home")
	t.Setenv(fakeModeEnv, "ok")
	t.Setenv(fakeOutEnv, out)
	t.Setenv("STNOPORTPROBING", "1")
	t.Setenv("STHOMEDIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("STCONFDIR", filepath.Join(t.TempDir(), "elsewhere2"))

	if err := Generate(context.Background(), testExe(t), home); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, out)
	if want := []string{"generate", "--home", home}; !reflect.DeepEqual(rec.Args, want) {
		t.Errorf("generate args = %v, want %v (no --no-port-probing)", rec.Args, want)
	}
	if len(rec.Env) != 0 {
		t.Errorf("generate saw Syncthing variables %v; they must be removed", rec.Env)
	}
	ep, _, err := stclient.ReadConfig(filepath.Join(home, "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL.Host != "127.0.0.1:18399" || ep.APIKey != "synthetic-generated-key" {
		t.Errorf("generated endpoint = %s %q", ep.BaseURL, ep.APIKey)
	}
}

func TestGenerateErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv(fakeModeEnv, "fail")
	err := Generate(context.Background(), testExe(t), home)
	if err == nil || !strings.Contains(err.Error(), "fake syncthing: refusing") {
		t.Errorf("Generate with a failing binary = %v, want its stderr in the error", err)
	}
	t.Setenv(fakeModeEnv, "nofile")
	if err := Generate(context.Background(), testExe(t), home); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Errorf("Generate without config.xml = %v", err)
	}
	if err := Generate(context.Background(), testExe(t), "relative"); err == nil {
		t.Error("Generate accepted a relative home")
	}
	if err := Generate(context.Background(), "", home); err == nil {
		t.Error("Generate accepted an empty binary")
	}
}

func TestFilterEnv(t *testing.T) {
	got := filterEnv([]string{"PATH=/bin", "sthomedir=x", "STNOPORTPROBING=1", "STGUIADDRESS=y", "STDATADIR=z"}, generateEnvDrop)
	want := []string{"PATH=/bin", "STGUIADDRESS=y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterEnv = %v, want %v", got, want)
	}
}

func TestStartDetached(t *testing.T) {
	// CI's Windows runners are elevated; Start refuses that without the override.
	t.Setenv(osutil.AllowPrivilegedEnv, "1")
	out := filepath.Join(t.TempDir(), "record.json")
	t.Setenv(fakeModeEnv, "ok")
	t.Setenv(fakeOutEnv, out)
	home := t.TempDir()
	if err := Start(testExe(t), "--home", home); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached process never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec := readRecord(t, out)
	if want := append(ServeArgs(), "--home", home); !reflect.DeepEqual(rec.Args, want) {
		t.Errorf("start args = %v, want %v", rec.Args, want)
	}
	if runtime.GOOS == "windows" && !reflect.DeepEqual(ServeArgs(), []string{"serve", "--no-console", "--no-browser"}) {
		t.Errorf("windows serve args = %v", ServeArgs())
	}
}

func TestStartRejectsMissingBinary(t *testing.T) {
	if err := Start(filepath.Join(t.TempDir(), ExeName())); err == nil {
		t.Error("Start accepted a missing binary")
	}
	if err := Start("syncthing"); err == nil {
		t.Error("Start accepted a relative path")
	}
}

// ---------------------------------------------------------------------------
// Profiles

// optionsServer imitates /rest/config/options. ignorePatch makes it accept
// PATCH without storing anything.
func optionsServer(t *testing.T, ignorePatch bool) (*stclient.Client, *map[string]any, *[]map[string]any) {
	t.Helper()
	opts := map[string]any{
		"globalAnnounceEnabled": true, "relaysEnabled": true, "natEnabled": true,
		"localAnnounceEnabled": true, "listenAddresses": []any{"default"}, "urAccepted": float64(-1),
	}
	var patches []map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "synthetic-key" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/rest/config/options" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(opts)
		case http.MethodPatch:
			var p map[string]any
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			patches = append(patches, p)
			if !ignorePatch {
				for k, v := range p {
					opts[k] = v
				}
			}
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return stclient.New(stclient.Endpoint{BaseURL: u, APIKey: "synthetic-key"}), &opts, &patches
}

func TestApplyProfile(t *testing.T) {
	c, opts, patches := optionsServer(t, false)
	ctx := context.Background()

	if p, ok, err := CurrentProfile(ctx, c); err != nil || !ok || p != Hybrid {
		t.Fatalf("CurrentProfile on upstream defaults = %v %v %v, want hybrid", p, ok, err)
	}
	if err := ApplyProfile(ctx, c, Tailnet); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"globalAnnounceEnabled": false, "relaysEnabled": false, "natEnabled": false,
		"localAnnounceEnabled": true, "listenAddresses": []any{"default"},
	}
	if len(*patches) != 1 || !reflect.DeepEqual((*patches)[0], want) {
		t.Errorf("tailnet PATCH = %v, want %v", *patches, want)
	}
	if (*opts)["urAccepted"] != float64(-1) {
		t.Error("ApplyProfile changed an unrelated option")
	}
	if p, ok, _ := CurrentProfile(ctx, c); !ok || p != Tailnet {
		t.Errorf("CurrentProfile after tailnet = %v %v", p, ok)
	}

	if err := ApplyProfile(ctx, c, Hybrid); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"globalAnnounceEnabled", "relaysEnabled", "natEnabled", "localAnnounceEnabled"} {
		if (*patches)[1][k] != true {
			t.Errorf("hybrid PATCH %s = %v, want true", k, (*patches)[1][k])
		}
	}

	(*opts)["relaysEnabled"] = true // a custom mix matches neither profile
	(*opts)["globalAnnounceEnabled"] = false
	if _, ok, err := CurrentProfile(ctx, c); err != nil || ok {
		t.Errorf("CurrentProfile on a custom mix = ok %v err %v, want not ok", ok, err)
	}
	if err := ApplyProfile(ctx, c, Profile(7)); err == nil {
		t.Error("ApplyProfile accepted an unknown profile")
	}
}

func TestApplyProfileNotKept(t *testing.T) {
	c, _, _ := optionsServer(t, true)
	if err := ApplyProfile(context.Background(), c, Tailnet); err == nil || !strings.Contains(err.Error(), "did not keep") {
		t.Errorf("ApplyProfile against a server that drops the PATCH = %v", err)
	}
}

func TestParseProfile(t *testing.T) {
	for in, want := range map[string]Profile{"tailnet": Tailnet, " Hybrid ": Hybrid, "TAILNET": Tailnet} {
		if got, err := ParseProfile(in); err != nil || got != want {
			t.Errorf("ParseProfile(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseProfile("lan"); err == nil {
		t.Error("ParseProfile accepted lan")
	}
	if Tailnet.String() != "tailnet" || Hybrid.String() != "hybrid" {
		t.Error("Profile.String")
	}
}
