package uihost

import (
	"errors"
	"flag"
	"fmt"
	"html"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/glass"
	"github.com/Neutx/syncthing-v2/internal/ui"
)

var (
	manual     = flag.Bool("manual", false, "run tests that open real desktop windows")
	manualWait = flag.Duration("manual-wait", 6*time.Second, "how long TestManualHost leaves the popup up for inspection")
)

// The test binary doubles as the ui-host child:
//
//	STV2_UIHOST_TEST_CHILD=fake  a scripted child that records the protocol
//	STV2_UIHOST_TEST_CHILD=real  the real RunChild (WebView2), for -manual
func TestMain(m *testing.M) {
	switch os.Getenv("STV2_UIHOST_TEST_CHILD") {
	case "fake":
		os.Exit(fakeChild())
	case "real":
		if err := RunChild(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeChild speaks the child side of the protocol through the real parser.
// STV2_UIHOST_TEST_LOG names a transcript file; STV2_UIHOST_TEST_EXIT picks a
// failure mode: "3" exits at once as if WebView2 were missing, "crash" exits
// with code 1 after the auth line.
func fakeChild() int {
	switch os.Getenv("STV2_UIHOST_TEST_EXIT") {
	case "3":
		return ExitNoWebView2
	}
	crash := os.Getenv("STV2_UIHOST_TEST_EXIT") == "crash"
	logf, err := os.OpenFile(os.Getenv("STV2_UIHOST_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 2
	}
	defer logf.Close()
	fmt.Println("ready")
	readCommands(os.Stdin, func(c command) {
		switch c.op {
		case "show":
			fmt.Fprintln(logf, formatShow(c.rect))
			fmt.Println("shown")
		case "hide":
			fmt.Fprintln(logf, "hide")
			fmt.Println("hidden")
		case "quit":
			fmt.Fprintln(logf, "quit")
		default:
			fmt.Fprintln(logf, c.op+" "+c.arg)
			if c.op == "auth" && crash {
				logf.Close()
				os.Exit(1)
			}
		}
	})
	return 0
}

func TestCheckServerURL(t *testing.T) {
	good := map[string]string{
		"http://127.0.0.1:8384":  "http://127.0.0.1:8384",
		"http://127.0.0.1:8384/": "http://127.0.0.1:8384",
		"http://[::1]:9000":      "http://[::1]:9000",
	}
	for in, want := range good {
		if got, err := checkServerURL(in); err != nil || got != want {
			t.Errorf("checkServerURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "https://127.0.0.1:1", "http://localhost:8384", "http://192.0.2.1:8384", "http://127.0.0.1",
		"http://127.0.0.1:0", "http://127.0.0.1:99999", "http://u:p@127.0.0.1:1", "http://127.0.0.1:1/x",
		"http://127.0.0.1:1/?a=b", "file:///etc/passwd", "javascript:alert(1)",
	} {
		if _, err := checkServerURL(bad); err == nil {
			t.Errorf("checkServerURL(%q) accepted", bad)
		}
	}
}

func TestParseCommand(t *testing.T) {
	ok := map[string]command{
		"hide":                       {op: "hide"},
		"quit":                       {op: "quit"},
		"auth abc-DEF_123":           {op: "auth", arg: "abc-DEF_123"},
		"url http://127.0.0.1:41000": {op: "url", arg: "http://127.0.0.1:41000"},
		"show 10 20 460 640":         {op: "show", rect: image.Rect(10, 20, 470, 660)},
		"show -1920 -40 575 800":     {op: "show", rect: image.Rect(-1920, -40, -1345, 760)},
		"  show  1 2 3 4  ":          {op: "show", rect: image.Rect(1, 2, 4, 6)},
	}
	for line, want := range ok {
		got, err := parseCommand(line)
		if err != nil || got != want {
			t.Errorf("parseCommand(%q) = %+v, %v; want %+v", line, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "   ", "open http://example.com", "hide now", "quit 1", "auth", "auth a b",
		"auth tok\x7f", "url http://example.com:80", "url", "show 1 2 3", "show 1 2 0 4",
		"show 1 2 3 -4", "show a b c d", "show 1 2 99999 4", "show 99999999 0 10 10",
	} {
		if c, err := parseCommand(bad); err == nil {
			t.Errorf("parseCommand(%q) = %+v, want an error", bad, c)
		}
	}
	// formatShow and parseCommand round-trip.
	r := image.Rect(-5, 7, 455, 647)
	if c, err := parseCommand(formatShow(r)); err != nil || c.rect != r {
		t.Errorf("round trip of %v gave %v, %v", r, c.rect, err)
	}
}

func TestReadCommands(t *testing.T) {
	in := strings.NewReader("url http://127.0.0.1:5000\nbogus\nauth t0k\nshow 1 2 3 4\n")
	var got []string
	readCommands(in, func(c command) { got = append(got, c.op) })
	want := []string{"url", "auth", "show", "quit"} // invalid line skipped; EOF means quit
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commands = %v, want %v", got, want)
	}
	got = nil
	readCommands(strings.NewReader("quit\nhide\n"), func(c command) { got = append(got, c.op) })
	if len(got) != 1 || got[0] != "quit" {
		t.Errorf("reading must stop at quit, got %v", got)
	}
}

func TestPlaceRect(t *testing.T) {
	fhd := image.Rect(0, 0, 1920, 1080)
	cases := []struct {
		name      string
		mon, work image.Rectangle
		dpi       uint32
		want      image.Rectangle
		scale     float64
	}{
		{"bottom taskbar 100%", fhd, image.Rect(0, 0, 1920, 1040), 96,
			image.Rect(1920-14-460, 1040-14-640, 1920-14, 1040-14), 1},
		{"bottom taskbar 125%", fhd, image.Rect(0, 0, 1920, 1030), 120,
			image.Rect(1920-18-575, 1030-18-800, 1920-18, 1030-18), 1.25},
		{"top taskbar", fhd, image.Rect(0, 40, 1920, 1080), 96,
			image.Rect(1920-14-460, 40+14, 1920-14, 40+14+640), 1},
		{"left taskbar", fhd, image.Rect(60, 0, 1920, 1080), 96,
			image.Rect(60+14, 1080-14-640, 60+14+460, 1080-14), 1},
		{"right taskbar", fhd, image.Rect(0, 0, 1860, 1080), 96,
			image.Rect(1860-14-460, 1080-14-640, 1860-14, 1080-14), 1},
		{"auto-hide taskbar", fhd, fhd, 96,
			image.Rect(1920-14-460, 1080-14-640, 1920-14, 1080-14), 1},
		{"secondary monitor left of primary at 150%", image.Rect(-2560, -200, 0, 1240), image.Rect(-2560, -200, 0, 1180), 144,
			image.Rect(-21-690, 1180-21-960, -21, 1180-21), 1.5},
		{"small screen clamps the size", image.Rect(0, 0, 1024, 600), image.Rect(0, 0, 1024, 560), 96,
			image.Rect(1024-14-460, 14, 1024-14, 560-14), 1},
		{"unknown DPI means 96", fhd, image.Rect(0, 0, 1920, 1040), 0,
			image.Rect(1920-14-460, 1040-14-640, 1920-14, 1040-14), 1},
	}
	for _, c := range cases {
		got, scale := placeRect(c.mon, c.work, c.dpi, DashboardSize)
		if got != c.want || scale != c.scale {
			t.Errorf("%s: placeRect = %v @%v, want %v @%v", c.name, got, scale, c.want, c.scale)
		}
		if !got.In(c.work) {
			t.Errorf("%s: %v is not inside the work area %v", c.name, got, c.work)
		}
	}
}

// The page scales its entrance from the corner the dashboard was placed in.
func TestTrayCorner(t *testing.T) {
	fhd := image.Rect(0, 0, 1920, 1080)
	for _, c := range []struct {
		name string
		work image.Rectangle
		want string
	}{
		{"bottom taskbar", image.Rect(0, 0, 1920, 1040), "br"},
		{"top taskbar", image.Rect(0, 40, 1920, 1080), "tr"},
		{"left taskbar", image.Rect(60, 0, 1920, 1080), "bl"},
		{"right taskbar", image.Rect(0, 0, 1860, 1080), "br"},
		{"auto-hide taskbar", fhd, "br"},
		{"no work area", image.Rectangle{}, "br"},
	} {
		if got := trayCorner(fhd, c.work); got != c.want {
			t.Errorf("%s: trayCorner = %q, want %q", c.name, got, c.want)
		}
		// The corner matches where placeRect put the dashboard.
		r, _ := placeRect(fhd, c.work, 96, DashboardSize)
		work := c.work
		if work.Empty() {
			work = fhd
		}
		top, left := r.Min.Y-work.Min.Y < work.Max.Y-r.Max.Y, r.Min.X-work.Min.X < work.Max.X-r.Max.X
		want := map[[2]bool]string{{false, false}: "br", {true, false}: "tr", {false, true}: "bl", {true, true}: "tl"}[[2]bool{top, left}]
		if want != c.want {
			t.Errorf("%s: placeRect put the dashboard at %v (%s), not in corner %s", c.name, r, want, c.want)
		}
	}
}

func TestPlace(t *testing.T) {
	r, scale := Place(DashboardSize)
	if scale < 1 || r.Dx() <= 0 || r.Dy() <= 0 {
		t.Fatalf("Place = %v @%v", r, scale)
	}
	if runtime.GOOS != "windows" && (r != image.Rectangle{Max: DashboardSize} || scale != 1) {
		t.Errorf("Place off Windows = %v @%v, want the plain DIP size", r, scale)
	}
}

func TestLaunchPage(t *testing.T) {
	p := launchPage("http://127.0.0.1:41000", "tok_123", "browser", "")
	for _, want := range []string{
		`<form id="login" method="post" action="http://127.0.0.1:41000/login">`,
		`<input type="hidden" name="token" value="tok_123">`,
		`<input type="hidden" name="next" value="/?mode=browser">`,
		`document.getElementById("login").submit()`,
		`<meta name="referrer" content="no-referrer">`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("launch page lacks %s", want)
		}
	}
	if strings.Contains(p, `name="mode"`) {
		t.Error("the launch page must post only the fields /login reads (token, next)")
	}
	if q := launchPage(`http://x/"><script>`, `a"b`, "glass", `x"><b>`); strings.Contains(q, `"><script>`) || strings.Contains(q, `a"b`) || strings.Contains(q, `<b>`) {
		t.Error("launch page values must be HTML-escaped")
	}
	// The login lands on the page's mode, and on a valid view only.
	for view, next := range map[string]string{
		"":          "/?mode=browser",
		"pair":      "/?mode=browser#pair",
		"welcome":   "/?mode=browser#welcome",
		"Pair":      "/?mode=browser",
		"a b":       "/?mode=browser",
		`x"><b>`:    "/?mode=browser",
		"-pair":     "/?mode=browser",
		"pair#evil": "/?mode=browser",
	} {
		want := `<input type="hidden" name="next" value="` + next + `">`
		if p := launchPage("http://127.0.0.1:41000", "tok_123", "browser", view); !strings.Contains(p, want) {
			t.Errorf("launch page for view %q lacks %s", view, want)
		}
	}
}

// launchForm extracts the form action and hidden fields of a launch page.
func launchForm(t *testing.T, page string) (string, url.Values) {
	t.Helper()
	action := regexp.MustCompile(`<form id="login" method="post" action="([^"]*)">`).FindStringSubmatch(page)
	if action == nil {
		t.Fatalf("no login form in %s", page)
	}
	form := url.Values{}
	for _, m := range regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`).FindAllStringSubmatch(page, -1) {
		form.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return html.UnescapeString(action[1]), form
}

// TestLaunchPageLogsInToRealServer posts the launch page's form, as a browser
// or WebView2 would from a file or string page (Origin: null), to a real
// ui.Server: the login must succeed and land straight on the requested mode
// and view, with no second navigation by the host.
func TestLaunchPageLogsInToRealServer(t *testing.T) {
	s, err := ui.NewServer(ui.Demo())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, c := range []struct{ mode, view, next string }{
		{"glass", "", "/?mode=glass"},
		{"browser", "", "/?mode=browser"},
		{"glass", "pair", "/?mode=glass#pair"},
		{"browser", "welcome", "/?mode=browser#welcome"},
	} {
		if got := ui.SafeNext(dashboardPath(c.mode, c.view)); got != c.next {
			t.Errorf("SafeNext(%q) = %q: the host's target is not in canonical form", dashboardPath(c.mode, c.view), got)
		}
		action, form := launchForm(t, launchPage(s.URL(), s.NewLaunchToken(), c.mode, c.view))
		req, err := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "null")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s/%s: /login answered %d: %s", c.mode, c.view, resp.StatusCode, body)
		}
		var session bool
		for _, ck := range resp.Cookies() {
			session = session || (ck.Name == ui.CookieName && ck.Value != "")
		}
		if !session {
			t.Errorf("%s/%s: /login set no session cookie", c.mode, c.view)
		}
		if want := `content="0;url=` + html.EscapeString(c.next) + `"`; !strings.Contains(string(body), want) {
			t.Errorf("%s/%s: /login does not refresh to %s: %s", c.mode, c.view, c.next, body)
		}
	}
}

func TestNavigationPolicy(t *testing.T) {
	const srv = "http://127.0.0.1:41000"
	cases := []struct {
		uri       string
		launching bool
		want      navAction
	}{
		{srv + "/?mode=glass", false, navAllow},
		{srv + "/login", true, navAllow},
		{srv + "/api/state#x", false, navAllow},
		{"HTTP://127.0.0.1:41000/", false, navAllow},
		{"data:text/html;charset=utf-8;base64,PGh0bWw+", true, navAllow},
		{"about:blank", true, navAllow},
		// The launch page only while the host is logging in.
		{"data:text/html,<script>alert(1)</script>", false, navBlock},
		{"about:blank", false, navBlock},
		{"about:srcdoc", true, navBlock},
		// Everything off the dashboard origin leaves the popup.
		{"https://github.com/Neutx/syncthing-v2/releases", false, navExternal},
		{"http://127.0.0.1:8384/", false, navExternal},
		{"https://127.0.0.1:41000/", false, navExternal},
		{"http://127.0.0.1:41000.example.com/", false, navBlock}, // not a valid URL
		{"http://localhost:41000/", false, navExternal},
		// Credentials, other schemes and junk are refused outright.
		{"http://127.0.0.1:41000@evil.example/", false, navBlock},
		{"http://user@127.0.0.1:41000/", false, navBlock},
		{"file:///C:/Windows/win.ini", false, navBlock},
		{"javascript:alert(1)", true, navBlock},
		{"ms-settings:display", false, navBlock},
		{"mailto:someone@example.com", false, navBlock},
		{"http:///nohost", false, navBlock},
		{"http://%zz/", false, navBlock},
		{"", true, navBlock},
	}
	for _, c := range cases {
		if got := navigationPolicy(srv, c.uri, c.launching); got != c.want {
			t.Errorf("navigationPolicy(%q, launching=%v) = %d, want %d", c.uri, c.launching, got, c.want)
		}
	}
	// Before the url command nothing is the dashboard.
	if got := navigationPolicy("", srv+"/", false); got != navExternal {
		t.Errorf("without a server URL the dashboard origin = %d, want external", got)
	}
}

func TestSnapLaunchDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	snaps := map[string]bool{"firefox_firefox.desktop": true, "chromium_chromium.desktop": true, "firefox+work_firefox.desktop": true}
	dirs := map[string]bool{
		filepath.Join(home, "snap", "firefox", "common"):      true,
		filepath.Join(home, "snap", "firefox_work", "common"): true,
	}
	isSnap := func(id string) bool { return snaps[id] }
	isDir := func(d string) bool { return dirs[d] }
	for id, want := range map[string]string{
		"firefox_firefox.desktop":      filepath.Join(home, "snap", "firefox", "common", "stv2"),
		"firefox+work_firefox.desktop": filepath.Join(home, "snap", "firefox_work", "common", "stv2"),
		"chromium_chromium.desktop":    "", // installed, but has never run: no common directory yet
		"firefox.desktop":              "", // a deb or tarball Firefox
		"org.mozilla.firefox.desktop":  "", // Flatpak forwards files through the document portal
		"evil_x.desktop":               "", // not installed by snapd
		"../x_y.desktop":               "",
		"firefox_firefox.desktop\n":    "",
		"":                             "",
	} {
		if got := snapLaunchDir(id, home, isSnap, isDir); got != want {
			t.Errorf("snapLaunchDir(%q) = %q, want %q", id, got, want)
		}
	}
	if got := snapLaunchDir("firefox_firefox.desktop", "relative/home", isSnap, func(string) bool { return true }); got != "" {
		t.Errorf("a relative home gave %q", got)
	}
}

func TestValidView(t *testing.T) {
	for _, v := range []string{"pair", "welcome", "settings", "status", "a", "a-b", strings.Repeat("a", 32)} {
		if !ValidView(v) {
			t.Errorf("ValidView(%q) = false", v)
		}
	}
	for _, v := range []string{"", "Pair", "-x", "1a", "a b", "a\"b", "a_b", "a/b", "pair#x", strings.Repeat("a", 33), "païr"} {
		if ValidView(v) {
			t.Errorf("ValidView(%q) = true", v)
		}
	}
	if c, err := parseCommand("view pair"); err != nil || c != (command{op: "view", arg: "pair"}) {
		t.Errorf("parseCommand(view pair) = %+v, %v", c, err)
	}
	for _, bad := range []string{"view", "view a b", `view ");alert(1);("`, "view Pair", "view pair\x00"} {
		if c, err := parseCommand(bad); err == nil {
			t.Errorf("parseCommand(%q) = %+v, want an error", bad, c)
		}
	}
}

func TestBrowserHostShowView(t *testing.T) {
	// With a token, the launch file posts next=/?mode=browser#<view>.
	fo := &fakeOpener{}
	b := newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", LaunchToken: "tok", open: fo.open, dir: t.TempDir()})
	b.ShowView(image.Rectangle{}, "pair")
	_, pages := fo.snapshot()
	if len(pages) != 1 || !strings.Contains(pages[0], `name="next" value="/?mode=browser#pair"`) {
		t.Errorf("launch page does not open the Pair view: %v", pages)
	}
	b.Close()

	// Without a token the dashboard URL carries the view as its fragment; an
	// invalid view is dropped.
	fo = &fakeOpener{}
	b = newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", open: fo.open, dir: t.TempDir()})
	b.ShowView(image.Rectangle{}, "welcome")
	b.ShowView(image.Rectangle{}, "x\"y")
	urls, _ := fo.snapshot()
	if len(urls) != 2 || urls[0] != "http://127.0.0.1:41000/?mode=browser#welcome" || urls[1] != "http://127.0.0.1:41000/?mode=browser" {
		t.Errorf("opened %v", urls)
	}
	b.Close()
}

// fakeOpener records the URLs a host opens and the launch-file contents at
// that moment.
type fakeOpener struct {
	mu    sync.Mutex
	urls  []string
	pages []string
}

func (f *fakeOpener) open(u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, u)
	page := ""
	if pu, err := url.Parse(u); err == nil && pu.Scheme == "file" {
		p := pu.Path
		if runtime.GOOS == "windows" {
			p = strings.TrimPrefix(p, "/")
		}
		if b, err := os.ReadFile(filepath.FromSlash(p)); err == nil {
			page = string(b)
		}
	}
	f.pages = append(f.pages, page)
	return nil
}

func (f *fakeOpener) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...), append([]string(nil), f.pages...)
}

func launchFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "open-*.html"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBrowserHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	fo := &fakeOpener{}
	var minted atomic.Int32
	b := newBrowserHost(Config{
		ServerURL:   "http://127.0.0.1:41000",
		LaunchToken: "initial-token",
		NewToken:    func() string { return fmt.Sprintf("minted-%d", minted.Add(1)) },
		open:        fo.open,
		dir:         dir,
	})
	if !b.Browser() || b.Visible() {
		t.Fatal("a browser host is always in browser mode and never visible")
	}

	// A stale launch file from an earlier run is swept.
	if err := secureDir(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "open-stale.html")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	b.Show(image.Rect(1, 2, 3, 4))
	b.Show(image.Rectangle{})
	b.Hide() // no-op

	urls, pages := fo.snapshot()
	if len(urls) != 2 {
		t.Fatalf("opened %d URLs, want 2: %v", len(urls), urls)
	}
	for i, tok := range []string{"initial-token", "minted-1"} {
		if !strings.HasPrefix(urls[i], "file://") {
			t.Errorf("launch %d opened %q, want a file URL", i, urls[i])
		}
		if !strings.Contains(pages[i], `name="token" value="`+tok+`"`) || !strings.Contains(pages[i], `name="next" value="/?mode=browser"`) {
			t.Errorf("launch %d page does not carry token %q in browser mode", i, tok)
		}
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale launch file was not removed")
	}
	files := launchFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("%d launch files on disk, want 2", len(files))
	}
	if runtime.GOOS != "windows" {
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s has mode %v, want 0600", f, fi.Mode().Perm())
			}
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("launch directory mode = %v, %v; want 0700", fi.Mode().Perm(), err)
		}
	}

	// Close removes the pending files and stops further launches.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(launchFiles(t, dir)); n != 0 {
		t.Errorf("%d launch files left after Close", n)
	}
	if err := b.launch(""); err == nil {
		t.Error("launch after Close must fail")
	}
}

func TestBrowserHostDeletesFilesAfterTTL(t *testing.T) {
	saved := launchFileTTL
	launchFileTTL = 150 * time.Millisecond
	defer func() { launchFileTTL = saved }()

	dir := t.TempDir()
	fo := &fakeOpener{}
	b := newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", LaunchToken: "tok", open: fo.open, dir: dir})
	defer b.Close()
	if err := b.launch(""); err != nil {
		t.Fatal(err)
	}
	if n := len(launchFiles(t, dir)); n != 1 {
		t.Fatalf("%d launch files, want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(launchFiles(t, dir)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("launch file still present after its TTL")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBrowserHostTokenSources(t *testing.T) {
	// An initial token older than launchTokenFresh is not presented.
	fo := &fakeOpener{}
	b := newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", LaunchToken: "old", NewToken: func() string { return "fresh" },
		open: fo.open, dir: t.TempDir()})
	b.tokenAt = time.Now().Add(-launchTokenFresh - time.Second)
	if err := b.launch(""); err != nil {
		t.Fatal(err)
	}
	_, pages := fo.snapshot()
	if !strings.Contains(pages[0], `value="fresh"`) || strings.Contains(pages[0], `value="old"`) {
		t.Error("an aged initial token must be replaced by a fresh one")
	}
	b.Close()

	// Without any token the dashboard URL opens directly (the browser may
	// still hold this session's cookie), and nothing is written.
	fo = &fakeOpener{}
	dir := t.TempDir()
	b = newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", open: fo.open, dir: dir})
	if err := b.launch(""); err != nil {
		t.Fatal(err)
	}
	urls, _ := fo.snapshot()
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:41000/?mode=browser" {
		t.Errorf("opened %v", urls)
	}
	if n := len(launchFiles(t, dir)); n != 0 {
		t.Errorf("%d launch files written without a token", n)
	}
	b.Close()

	// A minted token that could break the page is refused.
	b = newBrowserHost(Config{ServerURL: "http://127.0.0.1:41000", NewToken: func() string { return `x" onload="` },
		open: (&fakeOpener{}).open, dir: t.TempDir()})
	if err := b.launch(""); err == nil {
		t.Error("an invalid token must be refused")
	}
	b.Close()
}

func TestStartValidation(t *testing.T) {
	if _, err := Start("http://192.0.2.10:8080", "tok"); err == nil {
		t.Error("a non-loopback server must be refused")
	}
	if _, err := Start("http://127.0.0.1:8080", "bad token"); err == nil {
		t.Error("a token with a space must be refused")
	}
}

func TestStartBrowserPlatforms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows starts a ui-host child")
	}
	fo := &fakeOpener{}
	h, err := StartWith(Config{ServerURL: "http://127.0.0.1:41000", LaunchToken: "tok", open: fo.open, dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.Browser() || h.Visible() {
		t.Error("macOS and Linux always use the browser")
	}
	if urls, _ := fo.snapshot(); len(urls) != 0 {
		t.Error("Start must not open anything before Show")
	}
	h.Show(image.Rect(0, 0, 460, 640))
	if urls, _ := fo.snapshot(); len(urls) != 1 {
		t.Errorf("Show opened %d URLs, want 1", len(urls))
	}
	if err := RunChild(); err == nil {
		t.Error("RunChild must fail off Windows")
	}
}

// fakeChildConfig returns a Config that runs the test binary as a scripted child.
func fakeChildConfig(t *testing.T, mode string) (Config, string, *fakeOpener) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the ui-host child exists only on Windows")
	}
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	t.Setenv("STV2_UIHOST_TEST_CHILD", "fake")
	t.Setenv("STV2_UIHOST_TEST_LOG", transcript)
	t.Setenv("STV2_UIHOST_TEST_EXIT", mode)
	fo := &fakeOpener{}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		ServerURL:   "http://127.0.0.1:41000",
		LaunchToken: "tok-1",
		exe:         exe,
		args:        []string{"-test.run=^$"},
		open:        fo.open,
		dir:         t.TempDir(),
	}, transcript, fo
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readTranscript(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestWindowsHostProtocol(t *testing.T) {
	cfg, transcript, fo := fakeChildConfig(t, "")
	h, err := StartWith(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h.Browser() {
		t.Fatal("the WebView2 host must not start in browser mode")
	}
	r := image.Rect(1446, 386, 1906, 1026)
	h.Show(r)
	waitFor(t, "shown", h.Visible)
	h.Hide()
	waitFor(t, "hidden", func() bool { return !h.Visible() })
	// A view is sent before the show; an invalid one is never sent.
	h.ShowView(r, "pair")
	waitFor(t, "shown on pair", h.Visible)
	h.Hide()
	waitFor(t, "hidden", func() bool { return !h.Visible() })
	h.ShowView(r, `x");alert(1);("`)
	waitFor(t, "shown", h.Visible)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"url http://127.0.0.1:41000", "auth tok-1", "show 1446 386 460 640", "hide",
		"view pair", "show 1446 386 460 640", "hide", "show 1446 386 460 640", "quit"}
	if got := readTranscript(t, transcript); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if urls, _ := fo.snapshot(); len(urls) != 0 {
		t.Errorf("the browser was opened: %v", urls)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestWindowsHostFallsBackWithoutWebView2(t *testing.T) {
	cfg, _, fo := fakeChildConfig(t, "3")
	var reason atomic.Value
	cfg.OnFallback = func(err error) { reason.Store(err) }
	h, err := StartWith(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	waitFor(t, "fallback", h.Browser)
	waitFor(t, "OnFallback", func() bool { return reason.Load() != nil })
	if err, _ := reason.Load().(error); !errors.Is(err, ErrWebView2Missing) {
		t.Errorf("fallback reason = %v, want ErrWebView2Missing", err)
	}
	h.Show(image.Rect(0, 0, 460, 640))
	urls, pages := fo.snapshot()
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "file://") {
		t.Fatalf("browser fallback opened %v", urls)
	}
	// The child never read its token, so the fallback uses it while fresh.
	if !strings.Contains(pages[0], `value="tok-1"`) {
		t.Error("the unused launch token should open the browser")
	}
	if h.Visible() {
		t.Error("browser mode is never visible")
	}
}

func TestWindowsHostRestartsAndGivesUp(t *testing.T) {
	cfg, transcript, fo := fakeChildConfig(t, "crash")
	var minted atomic.Int32
	cfg.NewToken = func() string { return fmt.Sprintf("tok-%d", minted.Add(1)+1) }
	var fellBack atomic.Bool
	cfg.OnFallback = func(error) { fellBack.Store(true) }
	h, err := StartWith(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	exits := h.(interface{ unexpectedExits() (int, bool) })
	exitedOnce := func(n int) func() bool {
		return func() bool {
			got, running := exits.unexpectedExits()
			return got == n && !running
		}
	}
	// Each child crashes after reading its token; each Show restarts it with
	// a freshly minted token, until the third crash switches to the browser.
	waitFor(t, "first crash", exitedOnce(1))
	h.Show(image.Rect(0, 0, 460, 640))
	waitFor(t, "second crash", exitedOnce(2))
	h.Show(image.Rect(0, 0, 460, 640))
	waitFor(t, "fallback", h.Browser)
	waitFor(t, "OnFallback", fellBack.Load)

	auths := 0
	for _, l := range readTranscript(t, transcript) {
		if strings.HasPrefix(l, "auth ") {
			auths++
			if want := fmt.Sprintf("auth tok-%d", auths); l != want {
				t.Errorf("transcript line %q, want %q", l, want)
			}
		}
	}
	if auths != 3 {
		t.Errorf("%d children authenticated, want 3", auths)
	}
	h.Show(image.Rect(0, 0, 460, 640))
	if urls, _ := fo.snapshot(); len(urls) == 0 {
		t.Error("after giving up, Show must open the browser")
	}
}

// TestManualHost runs the real WebView2 host against a local test server.
// It puts a popup on the desktop, so it runs only with -manual. It checks
// automatically that the popup loads the page after the token login, shows
// at the placed rectangle with the squircle clip, fires the stv2:show event,
// and hides on command; the drop shadow, hide-on-focus-loss (click
// elsewhere while it is up) and sizing at 100% and 125% scaling are checked
// by eye (screenshots are saved and their paths logged).
func TestManualHost(t *testing.T) {
	if !*manual {
		t.Skip("run with -manual")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the popup host exists only on Windows")
	}
	const pageColor = "#3AC672"
	var (
		mu        sync.Mutex
		loggedIn  bool
		dashboard []string
		shows     int
		stillHere int
	)
	external := filepath.Join(t.TempDir(), "external.txt") // see host_windows_test.go
	t.Setenv("STV2_UIHOST_TEST_LOG", external)
	session := "s-" + fmt.Sprint(time.Now().UnixNano())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodPost:
			if r.FormValue("token") != "manual-token" {
				http.Error(w, "bad token", http.StatusForbidden)
				return
			}
			loggedIn = true
			// Exactly what ui.Server's /login does: a Strict session cookie
			// and a same-origin meta refresh to SafeNext(next).
			http.SetCookie(w, &http.Cookie{Name: "stv2s", Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			next := html.EscapeString(ui.SafeNext(r.FormValue("next")))
			fmt.Fprintf(w, `<!doctype html><html><head><meta http-equiv="refresh" content="0;url=%s"></head><body></body></html>`, next)
		case r.URL.Path == "/shown" && r.Method == http.MethodPost:
			shows++
		case r.URL.Path == "/still-here" && r.Method == http.MethodPost:
			stillHere++
		case r.URL.Path == "/":
			if c, err := r.Cookie("stv2s"); err != nil || c.Value != session {
				http.Error(w, "no session", http.StatusForbidden)
				return
			}
			dashboard = append(dashboard, r.URL.Query().Get("mode"))
			// On show the page tries to leave: a new window and a top-level
			// navigation off the dashboard origin. Both must go to the
			// default browser while the page stays.
			fmt.Fprintf(w, `<!doctype html><html><body style="margin:0;background:%s;color:#fff;font:20px sans-serif">
<p style="padding:40px">SyncThing V2 host test<br>Press Esc or click elsewhere to hide.</p>
<script>window.addEventListener("stv2:show",()=>{
fetch("/shown",{method:"POST"});
window.open("https://example.invalid/new-window");
setTimeout(()=>{location.href="https://example.invalid/navigate"},100);
setTimeout(()=>fetch("/still-here",{method:"POST"}),900);
});</script></body></html>`, pageColor)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("STV2_UIHOST_TEST_CHILD", "real")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var fellBack atomic.Bool
	h, err := StartWith(Config{
		ServerURL:   srv.URL,
		LaunchToken: "manual-token",
		NewToken:    func() string { return "manual-token" },
		OnFallback:  func(err error) { t.Logf("fallback: %v", err); fellBack.Store(true) },
		exe:         exe,
		args:        []string{"-test.run=^$"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	waitFor(t, "the dashboard page to load with the session cookie", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return loggedIn && len(dashboard) > 0 && dashboard[len(dashboard)-1] == "glass"
	})
	if fellBack.Load() {
		t.Fatal("the host fell back to the browser")
	}

	rect, scale := Place(DashboardSize)
	t.Logf("placement %v at scale %.2f (DIP size %v)", rect, scale, DashboardSize)
	if want := image.Pt(int(float64(DashboardSize.X)*scale+0.5), int(float64(DashboardSize.Y)*scale+0.5)); rect.Size() != want {
		t.Errorf("size %v, want %v", rect.Size(), want)
	}
	h.Show(rect)
	waitFor(t, "shown", h.Visible)
	waitFor(t, "the stv2:show event", func() bool { mu.Lock(); defer mu.Unlock(); return shows > 0 })
	time.Sleep(800 * time.Millisecond) // let the compositor present the page

	shot, err := glass.Capture(rect.Inset(-24))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.TempDir(), "stv2-uihost-manual.png")
	if f, err := os.Create(path); err == nil {
		_ = png.Encode(f, shot)
		f.Close()
		t.Logf("screenshot (popup plus a 24 px margin for the shadow): %s", path)
	}
	at := func(x, y int) (uint8, uint8, uint8) {
		o := shot.PixOffset(x+24, y+24)
		return shot.Pix[o], shot.Pix[o+1], shot.Pix[o+2]
	}
	near := func(r, g, b uint8) bool {
		d := func(a, b uint8) int { return max(int(a)-int(b), int(b)-int(a)) }
		return d(r, 0x3A) < 12 && d(g, 0xC6) < 12 && d(b, 0x72) < 12
	}
	if r, g, b := at(rect.Dx()/2, rect.Dy()-20); !near(r, g, b) {
		t.Errorf("popup body is #%02X%02X%02X, want the page colour %s", r, g, b, pageColor)
	}
	if r, g, b := at(1, 1); near(r, g, b) {
		t.Error("the top-left corner is not clipped by the squircle region")
	}

	// The page's attempts to leave went to the default browser (recorded by
	// the child's test opener), the popup kept the dashboard, and the login
	// loaded it once, with no second navigation by the host.
	waitFor(t, "the page to report after its navigation attempts", func() bool { mu.Lock(); defer mu.Unlock(); return stillHere > 0 })
	opened, _ := os.ReadFile(external)
	for _, u := range []string{"https://example.invalid/new-window", "https://example.invalid/navigate"} {
		if !strings.Contains(string(opened), u+"\n") {
			t.Errorf("%s was not handed to the default browser; opened: %q", u, opened)
		}
	}
	mu.Lock()
	if len(dashboard) != 1 {
		t.Errorf("the dashboard loaded %d times (%v), want once", len(dashboard), dashboard)
	}
	mu.Unlock()

	t.Logf("the popup stays up for %v: check the drop shadow, then click elsewhere to see it hide", *manualWait)
	deadline := time.Now().Add(*manualWait)
	for time.Now().Before(deadline) && h.Visible() {
		time.Sleep(50 * time.Millisecond)
	}
	if !h.Visible() {
		t.Log("the popup hid on its own (focus loss or Esc)")
	} else {
		h.Hide()
		waitFor(t, "hidden", func() bool { return !h.Visible() })
	}
}
