// Package uihost shows the dashboard served by internal/ui.
//
// On Windows a small child process (`stv2 ui-host`) hosts WebView2 in a
// borderless, squircle-clipped popup above the taskbar. The tray process
// drives it over a line protocol on the child's standard input:
//
//	url <server URL>     the loopback dashboard origin (sent once, first)
//	auth <launch token>  log in with a single-use launch token
//	view <name>          switch the page to a view ("pair", "welcome", ...)
//	show <x> <y> <w> <h> show at this physical-pixel rectangle
//	hide                 hide (the page stays loaded)
//	quit                 exit
//
// The child reports on its standard output with "ready", "shown" and
// "hidden" lines. The launch token never appears in argv.
//
// On macOS and Linux, and on Windows when the WebView2 runtime is missing
// (the child exits with code 3), the dashboard opens in the default browser
// through a mode-0600 launch file that auto-submits the launch token to
// /login and is deleted after 30 s.
package uihost

import (
	"bufio"
	"errors"
	"fmt"
	"html"
	"image"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// DashboardSize is the dashboard size in DIP.
var DashboardSize = image.Pt(460, 640)

// Inset is the gap between the dashboard and the taskbar edge, in DIP.
const Inset = 14

// ExitNoWebView2 is the exit code of `stv2 ui-host` when the WebView2
// runtime is missing or cannot be initialised.
const ExitNoWebView2 = 3

// launchFileTTL is how long a launch file stays on disk. The server's launch
// tokens expire after 30 s as well.
var launchFileTTL = 30 * time.Second

// launchTokenFresh is how long after Start the initial launch token is still
// used; afterwards a fresh one is minted, so a token is never presented close
// to its 30 s expiry.
const launchTokenFresh = 25 * time.Second

// Host shows and hides the dashboard.
type Host interface {
	// Show shows the dashboard at r (physical pixels, from Place). In browser
	// mode it opens the dashboard in the default browser and ignores r.
	Show(r image.Rectangle)
	// ShowView is Show that also switches the page to view (see ValidView);
	// an empty or invalid view keeps the page on its current view.
	ShowView(r image.Rectangle, view string)
	// Hide hides the popup. A browser tab cannot be hidden, so it is a no-op
	// in browser mode.
	Hide()
	// Visible reports whether the popup is showing. The tray hides it and
	// waits 45 ms before capturing the glass backdrop. Always false in
	// browser mode.
	Visible() bool
	// Close stops the host process and removes pending launch files.
	Close() error
	// Browser reports whether the dashboard opens in the default browser: always on
	// macOS and Linux, and on Windows after the WebView2 fallback.
	Browser() bool
}

// Config configures StartWith.
type Config struct {
	// ServerURL is the dashboard origin, http://127.0.0.1:<port>.
	ServerURL string
	// LaunchToken is a single-use launch token minted just before the call.
	LaunchToken string
	// NewToken mints a fresh launch token. It is needed for every launch after
	// the first: each browser open, and a restart of a crashed ui-host.
	NewToken func() string
	// OnFallback is called once if the Windows host switches to the browser
	// for the rest of the session (WebView2 missing, or repeated crashes).
	OnFallback func(reason error)

	// Test seams: the command run as the child, and the URL opener.
	exe  string
	args []string
	open func(string) error
	dir  string // launch-file directory; default runtimeDir()
}

// ErrWebView2Missing is the fallback reason when the child exits with
// ExitNoWebView2.
var ErrWebView2Missing = errors.New("uihost: the WebView2 runtime is missing or failed to start")

// Start starts the dashboard host for the server at serverURL, logging in
// with launchToken. It is StartWith without a token source, so launches after
// the first rely on the session cookie the browser or WebView already holds.
func Start(serverURL, launchToken string) (Host, error) {
	return StartWith(Config{ServerURL: serverURL, LaunchToken: launchToken})
}

// StartWith starts the dashboard host. On Windows it spawns the ui-host
// child (prewarmed: it logs in at once and stays hidden); elsewhere it
// prepares the browser launcher and opens nothing until Show.
func StartWith(cfg Config) (Host, error) {
	u, err := checkServerURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	cfg.ServerURL = u
	if cfg.LaunchToken != "" {
		if err := checkToken(cfg.LaunchToken); err != nil {
			return nil, err
		}
	}
	if cfg.open == nil {
		cfg.open = osutil.OpenURL
	}
	return startPlatform(cfg)
}

// checkServerURL accepts only an http origin on a loopback IP literal and
// returns it normalised, without a trailing slash.
func checkServerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("uihost: server URL: %w", err)
	}
	if u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("uihost: server URL %q must be a plain http origin", raw)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("uihost: server URL %q: %w", raw, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("uihost: server URL %q is not on a loopback address", raw)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return "", fmt.Errorf("uihost: server URL %q has an invalid port", raw)
	}
	return "http://" + u.Host, nil
}

// checkToken accepts printable, non-space ASCII up to 512 bytes, so a token
// can never break the line protocol or the launch page markup.
func checkToken(tok string) error {
	if tok == "" || len(tok) > 512 {
		return errors.New("uihost: launch token is empty or too long")
	}
	for i := 0; i < len(tok); i++ {
		if c := tok[i]; c <= ' ' || c > '~' {
			return errors.New("uihost: launch token contains an invalid character")
		}
	}
	return nil
}

// ValidView reports whether v can name a dashboard view: 1 to 32 characters
// of a-z and '-', starting with a letter. Only such names are sent to the
// page (a URL fragment or a script argument), so they can never inject.
func ValidView(v string) bool {
	if v == "" || len(v) > 32 || v[0] < 'a' || v[0] > 'z' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}
	return true
}

// dashboardPath is the post-login dashboard path for mode, opening view
// when it is valid.
func dashboardPath(mode, view string) string {
	p := "/?mode=" + mode
	if ValidView(view) {
		p += "#" + view
	}
	return p
}

// command is one parsed protocol line.
type command struct {
	op   string // "url", "auth", "view", "show", "hide" or "quit"
	arg  string
	rect image.Rectangle
}

const maxDim = 16384

// parseCommand parses and validates one protocol line.
func parseCommand(line string) (command, error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return command{}, errors.New("empty command")
	}
	c := command{op: f[0]}
	switch c.op {
	case "hide", "quit":
		if len(f) != 1 {
			return command{}, fmt.Errorf("%s takes no arguments", c.op)
		}
	case "url":
		if len(f) != 2 {
			return command{}, errors.New("url takes one argument")
		}
		u, err := checkServerURL(f[1])
		if err != nil {
			return command{}, err
		}
		c.arg = u
	case "auth":
		if len(f) != 2 {
			return command{}, errors.New("auth takes one argument")
		}
		if err := checkToken(f[1]); err != nil {
			return command{}, err
		}
		c.arg = f[1]
	case "view":
		if len(f) != 2 || !ValidView(f[1]) {
			return command{}, errors.New("view takes one view name")
		}
		c.arg = f[1]
	case "show":
		if len(f) != 5 {
			return command{}, errors.New("show takes x y w h")
		}
		var v [4]int
		for i := range v {
			n, err := strconv.Atoi(f[i+1])
			if err != nil {
				return command{}, fmt.Errorf("show: %w", err)
			}
			v[i] = n
		}
		if v[2] <= 0 || v[3] <= 0 || v[2] > maxDim || v[3] > maxDim ||
			v[0] < -1<<16 || v[0] > 1<<16 || v[1] < -1<<16 || v[1] > 1<<16 {
			return command{}, fmt.Errorf("show: rectangle %v out of range", v)
		}
		c.rect = image.Rect(v[0], v[1], v[0]+v[2], v[1]+v[3])
	default:
		return command{}, fmt.Errorf("unknown command %q", c.op)
	}
	return c, nil
}

// formatShow renders the show command for r.
func formatShow(r image.Rectangle) string {
	return fmt.Sprintf("show %d %d %d %d", r.Min.X, r.Min.Y, r.Dx(), r.Dy())
}

// readCommands parses protocol lines from r and passes them to fn until EOF,
// after which it passes a final quit. Invalid lines are logged and skipped.
func readCommands(r io.Reader, fn func(command)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 4096)
	for sc.Scan() {
		c, err := parseCommand(sc.Text())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ui-host: ignoring command: %v\n", err)
			continue
		}
		fn(c)
		if c.op == "quit" {
			return
		}
	}
	fn(command{op: "quit"})
}

// launchPage is the HTML that logs in with a launch token: it auto-submits a
// POST of token and next to /login (with a button as a no-script fallback).
// That is the form contract of ui.Server's /login handler, which answers with
// a same-origin refresh to next (reduced by ui.SafeNext), so the login lands
// straight on mode ("glass" or "browser") and, when view is valid, on that
// view. It mirrors ui.Server.LaunchPage because the ui-host child runs in its
// own process with no ui.Server to ask; a test posts this page's form to a
// real ui.Server to keep the two in step.
func launchPage(serverURL, token, mode, view string) string {
	e := html.EscapeString
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">
<title>` + e(brand.DisplayName) + `</title></head>
<body style="background:#11131A;color:#F6F8FC;font:14px 'Segoe UI',-apple-system,sans-serif">
<form id="login" method="post" action="` + e(serverURL+"/login") + `">
<input type="hidden" name="token" value="` + e(token) + `">
<input type="hidden" name="next" value="` + e(dashboardPath(mode, view)) + `">
<noscript><button type="submit">Open ` + e(brand.DisplayName) + `</button></noscript>
</form>
<script>document.getElementById("login").submit();</script>
</body></html>
`
}

// navAction is what the WebView2 popup does with a navigation.
type navAction int

const (
	navAllow    navAction = iota // load it in the popup
	navExternal                  // cancel it and open it in the default browser
	navBlock                     // cancel it
)

// navigationPolicy decides where the popup may navigate. Only the dashboard
// origin loads in it; the launch page (a data: URI or about:blank from
// NavigateToString) is allowed only while a login the host started is in
// flight. Other http and https targets open in the default browser, and
// everything else (file:, javascript:, custom schemes) is refused, so the
// frameless, always-on-top popup never shows foreign content.
func navigationPolicy(serverURL, uri string, launching bool) navAction {
	u, err := url.Parse(uri)
	if err != nil {
		return navBlock
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" || u.User != nil {
			return navBlock
		}
		if serverURL != "" && strings.EqualFold(u.Scheme, "http") &&
			strings.EqualFold(u.Host, strings.TrimPrefix(serverURL, "http://")) {
			return navAllow
		}
		return navExternal
	case "data":
		if launching {
			return navAllow
		}
	case "about":
		if launching && strings.EqualFold(uri, "about:blank") {
			return navAllow
		}
	}
	return navBlock
}

// placeRect computes the dashboard rectangle on a monitor: the size is
// sizeDIP × dpi/96, and it sits inset 14 DIP from the corner at the taskbar
// edge, the side where the work area differs from the monitor bounds
// (bottom-right when the taskbar auto-hides).
func placeRect(monitor, work image.Rectangle, dpi uint32, sizeDIP image.Point) (image.Rectangle, float64) {
	if dpi == 0 {
		dpi = 96
	}
	scale := float64(dpi) / 96
	if work.Empty() {
		work = monitor
	}
	inset := int(math.Round(Inset * scale))
	w := int(math.Round(float64(sizeDIP.X) * scale))
	h := int(math.Round(float64(sizeDIP.Y) * scale))
	w = max(1, min(w, work.Dx()-2*inset))
	h = max(1, min(h, work.Dy()-2*inset))

	x := work.Max.X - inset - w // right-aligned unless the taskbar is on the left
	y := work.Max.Y - inset - h // bottom-aligned unless the taskbar is at the top
	switch trayCorner(monitor, work) {
	case "tr":
		y = work.Min.Y + inset
	case "bl":
		x = work.Min.X + inset
	}
	return image.Rect(x, y, x+w, y+h), scale
}

// trayCorner names the work-area corner placeRect puts the dashboard in,
// the one nearest the tray: "br" (bottom or right taskbar, or none visible),
// "tr" (top taskbar) or "bl" (left taskbar). The page scales its entrance
// from that corner (spec §8.1 Motion).
func trayCorner(monitor, work image.Rectangle) string {
	if work.Empty() {
		work = monitor
	}
	switch {
	case work.Max.Y < monitor.Max.Y: // bottom taskbar
	case work.Min.Y > monitor.Min.Y: // top taskbar
		return "tr"
	case work.Min.X > monitor.Min.X: // left taskbar
		return "bl"
	}
	return "br"
}

// browserHost opens the dashboard in the default browser through launch files.
type browserHost struct {
	serverURL string
	newToken  func() string
	open      func(string) error
	dir       string

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	files   map[string]*time.Timer
	closed  bool
}

func newBrowserHost(cfg Config) *browserHost {
	return &browserHost{
		serverURL: cfg.ServerURL,
		newToken:  cfg.NewToken,
		open:      cfg.open,
		dir:       cfg.dir,
		token:     cfg.LaunchToken,
		tokenAt:   time.Now(),
		files:     map[string]*time.Timer{},
	}
}

func (b *browserHost) Show(r image.Rectangle) { b.ShowView(r, "") }

// ShowView opens the dashboard in the browser on view.
func (b *browserHost) ShowView(_ image.Rectangle, view string) {
	if err := b.launch(view); err != nil {
		applog.Printf("uihost: opening the dashboard in the browser: %v", err)
	}
}

func (b *browserHost) Hide() {}

func (b *browserHost) Visible() bool { return false }

func (b *browserHost) Browser() bool { return true }

func (b *browserHost) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	var errs []error
	for path, t := range b.files {
		t.Stop()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	clear(b.files)
	return errors.Join(errs...)
}

// takeToken returns the initial launch token while it is fresh, otherwise a
// newly minted one, or "" if neither is available.
func (b *browserHost) takeToken() string {
	if b.token != "" {
		tok := b.token
		b.token = ""
		if time.Since(b.tokenAt) < launchTokenFresh {
			return tok
		}
	}
	if b.newToken != nil {
		return b.newToken()
	}
	return ""
}

// launch writes a launch file with a token and opens it. Without a token it
// opens the dashboard URL directly, which works while the browser still holds
// this session's cookie. Either way the dashboard lands on view when valid.
func (b *browserHost) launch(view string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("host is closed")
	}
	tok := b.takeToken()
	if tok == "" {
		return b.open(b.serverURL + dashboardPath("browser", view))
	}
	if err := checkToken(tok); err != nil {
		return err
	}
	dir := b.dir
	if dir == "" {
		d, err := runtimeDir()
		if err != nil {
			return err
		}
		dir = d
	}
	if err := secureDir(dir); err != nil {
		return err
	}
	removeStale(dir)

	f, err := os.CreateTemp(dir, "open-*.html") // mode 0600, O_EXCL
	if err != nil {
		return err
	}
	path := f.Name()
	_, werr := f.WriteString(launchPage(b.serverURL, tok, "browser", view))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(path)
		return err
	}
	b.files[path] = time.AfterFunc(launchFileTTL, func() {
		b.mu.Lock()
		delete(b.files, path)
		b.mu.Unlock()
		_ = os.Remove(path)
	})
	if err := b.open(osutil.FileURL(path)); err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	return nil
}

// snapDesktopID matches the desktop file snapd installs for a snap app,
// "<snap>[+<instance key>]_<app>.desktop" (firefox_firefox.desktop for the
// Ubuntu Firefox snap).
var snapDesktopID = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,39})(?:\+([a-z0-9]{1,10}))?_[A-Za-z0-9][A-Za-z0-9-]*\.desktop$`)

// snapLaunchDir returns the launch-file directory for a default HTML handler
// that is a strictly confined snap, or "" when it is not one. Such a browser
// cannot read hidden directories in the home folder or the host's
// XDG_RUNTIME_DIR, so xdg-open of a launch file in the usual place fails
// with "file not found". It can read its own ~/snap/<instance>/common
// ($SNAP_USER_COMMON), which snapd creates the first time the browser runs,
// so the launch files go in a 0700 subdirectory there and the token still
// never appears in a command line. isSnap confirms desktopID is installed by
// snapd; isDir reports whether a directory exists.
func snapLaunchDir(desktopID, home string, isSnap, isDir func(string) bool) string {
	m := snapDesktopID.FindStringSubmatch(desktopID)
	if m == nil || !filepath.IsAbs(home) || !isSnap(desktopID) {
		return ""
	}
	instance := m[1]
	if m[2] != "" {
		instance += "_" + m[2]
	}
	common := filepath.Join(home, "snap", instance, "common")
	if !isDir(common) {
		return ""
	}
	return filepath.Join(common, brand.BinaryName)
}

// removeStale deletes launch files left behind by an earlier run that exited
// before its deletion timers fired.
func removeStale(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "open-*.html"))
	for _, m := range matches {
		if fi, err := os.Lstat(m); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > launchFileTTL {
			_ = os.Remove(m)
		}
	}
}
