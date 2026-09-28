//go:build windows

package uihost

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/glass"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procAllowSetForegroundWindow      = user32.NewProc("AllowSetForegroundWindow")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procSetWindowRgn                  = user32.NewProc("SetWindowRgn")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procSetTimer                      = user32.NewProc("SetTimer")
	procKillTimer                     = user32.NewProc("KillTimer")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreatePolygonRgn              = gdi32.NewProc("CreatePolygonRgn")
	procCreateSolidBrush              = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procGetModuleHandleW              = kernel32.NewProc("GetModuleHandleW")
)

const (
	csDropShadow   = 0x00020000
	wsPopup        = 0x80000000
	wsExToolWindow = 0x00000080
	wsExTopmost    = 0x00000008

	wmDestroy    = 0x0002
	wmMove       = 0x0003
	wmSize       = 0x0005
	wmActivate   = 0x0006
	wmClose      = 0x0010
	wmTimer      = 0x0113
	wmDpiChanged = 0x02E0
	wmCommand    = 0x8000 + 1 // WM_APP+1: protocol commands are queued

	waInactive = 0
	swHide     = 0
	swShow     = 5

	swpNoActivate = 0x0010
	hwndTopmost   = ^uintptr(0) // (HWND)-1

	idcArrow = 32512
	vkEscape = 0x1B
	winding  = 2

	escTimerID = 1
	// escGrace lets the page play its exit animation and post "hide" itself;
	// if it has not within this time, the host hides natively.
	escGrace = 400
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

// ---------------------------------------------------------------------------
// Tray side: the child process and its protocol.
// ---------------------------------------------------------------------------

// crashLimit ui-host exits (other than ExitNoWebView2) within crashWindow
// switch the session to the browser.
const (
	crashLimit  = 3
	crashWindow = 2 * time.Minute
)

type winHost struct {
	cfg Config

	mu      sync.Mutex
	proc    *childProc
	browser *browserHost
	visible bool
	closed  bool
	crashes []time.Time
}

type childProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	token   string
	tokenAt time.Time
}

func (p *childProc) send(line string) error {
	_, err := io.WriteString(p.stdin, line+"\n")
	return err
}

func startPlatform(cfg Config) (Host, error) {
	if cfg.exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("uihost: %w", err)
		}
		cfg.exe = exe
	}
	if cfg.args == nil {
		cfg.args = []string{"ui-host"}
	}
	h := &winHost{cfg: cfg}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.spawnLocked(cfg.LaunchToken); err != nil {
		return nil, err
	}
	return h, nil
}

// spawnLocked starts a ui-host child, sends it the server URL and, if tok is
// set, the launch token. The child logs in at once and stays hidden.
func (h *winHost) spawnLocked(tok string) error {
	cmd := osutil.Command(context.Background(), h.cfg.exe, h.cfg.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("uihost: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("uihost: %w", err)
	}
	cmd.Stderr = &lineLogger{prefix: "ui-host: "}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("uihost: starting ui-host: %w", err)
	}
	p := &childProc{cmd: cmd, stdin: stdin, done: make(chan struct{}), token: tok, tokenAt: time.Now()}
	h.proc = p
	h.visible = false
	go h.watch(p, stdout)

	err = p.send("url " + h.cfg.ServerURL)
	if err == nil && tok != "" {
		err = p.send("auth " + tok)
	}
	if err != nil {
		applog.Printf("uihost: writing to ui-host: %v", err)
	}
	return nil
}

// watch follows the child's status lines, then reaps it.
func (h *winHost) watch(p *childProc, stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		switch line := strings.TrimSpace(sc.Text()); line {
		case "shown", "hidden":
			h.mu.Lock()
			if h.proc == p {
				h.visible = line == "shown"
			}
			h.mu.Unlock()
		case "ready":
			applog.Printf("uihost: ui-host ready (pid %d)", p.cmd.Process.Pid)
		case "":
		default:
			applog.Printf("uihost: ui-host says %q", line)
		}
	}
	_ = p.cmd.Wait()
	close(p.done)
	h.exited(p, p.cmd.ProcessState.ExitCode())
}

func (h *winHost) exited(p *childProc, code int) {
	h.mu.Lock()
	if h.proc != p || h.closed {
		h.mu.Unlock()
		return
	}
	wasVisible := h.visible
	h.proc, h.visible = nil, false
	var b *browserHost
	if code == ExitNoWebView2 {
		applog.Printf("uihost: WebView2 is unavailable; the dashboard opens in the browser for this session")
		// The child exits before reading any command, so its token is unused.
		b = h.fallbackLocked(ErrWebView2Missing, p.token, p.tokenAt)
	} else {
		applog.Printf("uihost: ui-host exited with code %d", code)
		now := time.Now()
		recent := h.crashes[:0]
		for _, t := range h.crashes {
			if now.Sub(t) < crashWindow {
				recent = append(recent, t)
			}
		}
		h.crashes = append(recent, now)
		if len(h.crashes) >= crashLimit {
			b = h.fallbackLocked(fmt.Errorf("uihost: ui-host exited %d times within %v", crashLimit, crashWindow), "", time.Time{})
		}
	}
	h.mu.Unlock()
	if b != nil && wasVisible {
		b.Show(image.Rectangle{}) // the user asked for the dashboard: honour it now
	}
}

// fallbackLocked switches to the browser for the rest of the session and
// returns the browser host.
func (h *winHost) fallbackLocked(reason error, tok string, tokAt time.Time) *browserHost {
	if h.browser != nil {
		return h.browser
	}
	cfg := h.cfg
	cfg.LaunchToken = tok
	b := newBrowserHost(cfg)
	b.tokenAt = tokAt
	h.browser = b
	if f := h.cfg.OnFallback; f != nil {
		go f(reason)
	}
	return b
}

func (h *winHost) Show(r image.Rectangle) { h.ShowView(r, "") }

// ShowView shows the popup at r on view: the child switches the page before
// it shows the window.
func (h *winHost) ShowView(r image.Rectangle, view string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if h.browser == nil && h.proc == nil {
		tok := ""
		if h.cfg.NewToken != nil {
			tok = h.cfg.NewToken()
		}
		if tok == "" {
			h.fallbackLocked(errors.New("uihost: ui-host stopped and no launch token source is set"), "", time.Time{})
		} else if err := h.spawnLocked(tok); err != nil {
			applog.Printf("%v", err)
			h.fallbackLocked(err, tok, time.Now())
		}
	}
	if b := h.browser; b != nil {
		h.mu.Unlock()
		b.ShowView(r, view)
		return
	}
	p := h.proc
	if ValidView(view) {
		if err := p.send("view " + view); err != nil {
			applog.Printf("uihost: view: %v", err)
		}
	}
	// The tray process owns the foreground (the user just clicked its icon);
	// let the child take it so the popup is activated and hides on focus loss.
	procAllowSetForegroundWindow.Call(uintptr(p.cmd.Process.Pid))
	if err := p.send(formatShow(r)); err != nil {
		applog.Printf("uihost: show: %v", err)
	} else {
		h.visible = true
	}
	h.mu.Unlock()
}

func (h *winHost) Hide() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proc == nil || h.browser != nil {
		return
	}
	if err := h.proc.send("hide"); err != nil {
		applog.Printf("uihost: hide: %v", err)
	}
	h.visible = false
}

// unexpectedExits reports how many recent ui-host exits count towards the
// crash limit, and whether a child is running.
func (h *winHost) unexpectedExits() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.crashes), h.proc != nil
}

// Visible reports whether the popup is showing.
func (h *winHost) Visible() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.visible && h.browser == nil
}

func (h *winHost) Browser() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.browser != nil
}

func (h *winHost) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	p, b := h.proc, h.browser
	h.proc = nil
	h.mu.Unlock()

	var errs []error
	if p != nil {
		_ = p.send("quit")
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			if err := p.cmd.Process.Kill(); err != nil {
				errs = append(errs, fmt.Errorf("uihost: killing ui-host: %w", err))
			}
			<-p.done
		}
	}
	if b != nil {
		errs = append(errs, b.Close())
	}
	return errors.Join(errs...)
}

// lineLogger forwards the child's standard error to the application log.
type lineLogger struct {
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(l.buf[:i]), "\r"); line != "" {
			applog.Printf("%s%s", l.prefix, line)
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 { // a runaway line without a newline
		applog.Printf("%s%s", l.prefix, l.buf)
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

// runtimeDir is %LOCALAPPDATA%\SyncThingV2\run, protected by the profile ACL.
func runtimeDir() (string, error) {
	d, err := osutil.AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "run"), nil
}

// secureDir creates dir if needed and refuses a symlink or junction in its place.
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("launch directory %s is not a plain directory", dir)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Child side: `stv2 ui-host`.
// ---------------------------------------------------------------------------

// child is the state of the ui-host process. Every field except the command
// queue is owned by the UI thread.
type child struct {
	hwnd uintptr
	wv   *edge.Chromium

	qmu   sync.Mutex
	queue []command

	outMu sync.Mutex

	initialising bool
	ready        bool
	visible      bool
	serverURL    string
	launching    bool   // a launch page is loading or posting its login
	loaded       bool   // the dashboard page (not the launch page) has loaded
	view         string // a view to switch to once the dashboard has loaded
}

// openExternal opens a link the dashboard tried to load in the popup; it is
// replaced in tests.
var openExternal = osutil.OpenURL

var (
	theChild    *child
	wndProcPtr  = windows.NewCallback(wndProc)
	classNameUI = utf16("SyncThingV2.UIHost")
)

// RunChild is the `stv2 ui-host` entry point. It must be called on the main
// goroutine (the WebView2 package initialises COM on the main thread). It
// returns nil after a quit command or when standard input closes. If the
// WebView2 runtime is missing or fails to initialise, the process exits with
// ExitNoWebView2 and the tray switches to the browser.
func RunChild() error {
	runtime.LockOSThread()
	if procSetProcessDpiAwarenessContext.Find() == nil {
		// Fails harmlessly if the manifest already set the awareness.
		procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2)
	}

	if v, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString(""); err != nil || v == "" {
		fmt.Fprintf(os.Stderr, "WebView2 runtime not found (%v)\n", err)
		os.Exit(ExitNoWebView2)
	}

	app, err := osutil.AppDataDir()
	if err != nil {
		return err
	}
	dataDir := filepath.Join(app, "WebView2")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	c := &child{}
	theChild = c
	if err := c.createWindow(); err != nil {
		return err
	}
	go readCommands(os.Stdin, c.enqueue)

	wv := edge.NewChromium()
	wv.DataPath = dataDir
	wv.SetErrorCallback(c.onWebViewError)
	wv.SetGlobalPermission(edge.CoreWebView2PermissionStateDeny)
	wv.AcceleratorKeyCallback = c.onKey
	wv.NavigationCompletedCallback = c.onNavigated

	c.initialising = true
	if !wv.Embed(c.hwnd) {
		fmt.Fprintln(os.Stderr, "WebView2 could not be embedded")
		os.Exit(ExitNoWebView2)
	}
	c.initialising = false
	c.wv = wv
	if err := c.restrictNavigation(); err != nil {
		// Without the filter a link could load foreign content in the
		// frameless popup; the browser fallback is the safe choice.
		fmt.Fprintf(os.Stderr, "WebView2 navigation filter: %v\n", err)
		os.Exit(ExitNoWebView2)
	}

	if s, err := wv.GetSettings(); err == nil {
		_ = s.PutAreDefaultContextMenusEnabled(false)
		_ = s.PutAreDevToolsEnabled(false)
		_ = s.PutIsStatusBarEnabled(false)
		_ = s.PutIsZoomControlEnabled(false)
		_ = s.PutAreBrowserAcceleratorKeysEnabled(false)
		_ = s.PutAreDefaultScriptDialogsEnabled(false)
	}
	wv.SetBackgroundColour(0x11, 0x13, 0x1A, 0xFF)
	wv.Resize()
	_ = wv.Hide()

	c.ready = true
	c.emit("ready")
	c.drain()

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT or error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	wv.ShuttingDown()
	return nil
}

func (c *child) createWindow() error {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	brush, _, _ := procCreateSolidBrush.Call(0x001A1311) // COLORREF of #11131A
	wc := wndClassEx{
		Style:      csDropShadow,
		WndProc:    wndProcPtr,
		Instance:   inst,
		Cursor:     cursor,
		Background: brush,
		ClassName:  classNameUI,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}
	title := utf16(brand.DisplayName)
	hwnd, _, err := procCreateWindowExW.Call(
		wsExToolWindow|wsExTopmost,
		uintptr(unsafe.Pointer(classNameUI)),
		uintptr(unsafe.Pointer(title)),
		wsPopup,
		0, 0, uintptr(DashboardSize.X), uintptr(DashboardSize.Y),
		0, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	c.hwnd = hwnd
	return nil
}

// utf16 converts a constant without NUL bytes to a UTF-16 pointer.
func utf16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

func (c *child) emit(line string) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	fmt.Fprintln(os.Stdout, line)
}

// enqueue runs on the stdin reader goroutine and hands the command to the UI thread.
func (c *child) enqueue(cmd command) {
	c.qmu.Lock()
	c.queue = append(c.queue, cmd)
	c.qmu.Unlock()
	procPostMessageW.Call(c.hwnd, wmCommand, 0, 0)
}

func (c *child) drain() {
	if !c.ready {
		return
	}
	for {
		c.qmu.Lock()
		if len(c.queue) == 0 {
			c.qmu.Unlock()
			return
		}
		cmd := c.queue[0]
		c.queue = c.queue[1:]
		c.qmu.Unlock()
		c.run(cmd)
	}
}

func (c *child) run(cmd command) {
	switch cmd.op {
	case "url":
		c.serverURL = cmd.arg
	case "auth":
		if c.serverURL == "" {
			fmt.Fprintln(os.Stderr, "auth before url; ignored")
			return
		}
		c.launching, c.loaded = true, false
		c.wv.NavigateToString(launchPage(c.serverURL, cmd.arg, "glass", c.view))
		c.view = "" // the login lands on it through the fragment
	case "view":
		c.view = cmd.arg
		c.applyView()
	case "show":
		c.show(cmd.rect)
	case "hide":
		procKillTimer.Call(c.hwnd, escTimerID)
		c.hide()
	case "quit":
		procDestroyWindow.Call(c.hwnd)
	}
}

func (c *child) show(r image.Rectangle) {
	w, h := r.Dx(), r.Dy()
	procSetWindowPos.Call(c.hwnd, hwndTopmost, uintptr(int32(r.Min.X)), uintptr(int32(r.Min.Y)),
		uintptr(w), uintptr(h), swpNoActivate)

	scale := 1.0
	if procGetDpiForWindow.Find() == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(c.hwnd); dpi != 0 {
			scale = float64(dpi) / 96
		}
	}
	c.setRegion(w, h, glass.Radius*scale)
	if ctl := c.wv.GetController(); ctl != nil {
		if ctl3 := ctl.GetICoreWebView2Controller3(); ctl3 != nil {
			putRasterizationScale(ctl3, scale)
		}
	}
	c.wv.Resize()
	_ = c.wv.NotifyParentWindowPositionChanged()
	_ = c.wv.Show()

	procShowWindow.Call(c.hwnd, swShow)
	procSetForegroundWindow.Call(c.hwnd)
	c.wv.Focus()
	c.wv.Eval(`window.dispatchEvent(new Event("stv2:show"))`)
	c.visible = true
	c.emit("shown")
}

// putRasterizationScale sets the page scale for raw-pixel bounds. The method
// takes a double by value; go-webview2's wrapper passes a pointer instead,
// which sets a garbage scale, so the vtable slot is called directly. Go's
// Windows syscall path also loads the first four arguments into XMM0-3,
// which is where the x64 ABI expects a double.
func putRasterizationScale(ctl3 *edge.ICoreWebView2Controller3, scale float64) {
	hr, _, _ := ctl3.Vtbl.PutRasterizationScale.Call(uintptr(unsafe.Pointer(ctl3)), uintptr(math.Float64bits(scale)))
	if hr != 0 {
		fmt.Fprintf(os.Stderr, "PutRasterizationScale(%v): HRESULT 0x%08X\n", scale, uint32(hr))
		return
	}
	if got, err := ctl3.GetRasterizationScale(); err == nil && math.Abs(got-scale) > 1e-6 {
		fmt.Fprintf(os.Stderr, "PutRasterizationScale(%v) left the scale at %v\n", scale, got)
	}
}

func (c *child) hide() {
	if !c.visible {
		return
	}
	c.visible = false
	procShowWindow.Call(c.hwnd, swHide)
	_ = c.wv.Hide()
	c.emit("hidden")
}

// setRegion clips the window to the squircle traced by glass.SquirclePath.
func (c *child) setRegion(w, h int, radius float64) {
	path := glass.SquirclePath(0, 0, float64(w), float64(h), radius)
	pts := make([]point, len(path))
	for i, p := range path {
		pts[i] = point{int32(math.Round(p.X)), int32(math.Round(p.Y))}
	}
	rgn, _, _ := procCreatePolygonRgn.Call(uintptr(unsafe.Pointer(&pts[0])), uintptr(len(pts)), winding)
	if rgn == 0 {
		return
	}
	if ok, _, _ := procSetWindowRgn.Call(c.hwnd, rgn, 1); ok == 0 {
		procDeleteObject.Call(rgn) // the system owns the region only on success
	}
}

func (c *child) onWebViewError(err error) {
	fmt.Fprintf(os.Stderr, "WebView2: %v\n", err)
	if c.initialising {
		os.Exit(ExitNoWebView2)
	}
	// After initialisation the WebView2 package exits with code 1 once this
	// returns; the tray restarts the host on the next show.
}

// onKey sees accelerator keys (Esc always counts as one). Esc is left to the
// page, which plays its exit animation and asks the tray to hide the popup;
// a timer hides it natively if the page does not.
func (c *child) onKey(vk uint) bool {
	if vk == vkEscape && c.visible {
		procSetTimer.Call(c.hwnd, escTimerID, escGrace, 0)
	}
	return false
}

// onNavigated notes when the dashboard has loaded, so a view asked for during
// the login is applied. The login itself needs no help: /login answers with a
// same-origin refresh to the launch page's next (/?mode=glass), which carries
// the new SameSite=Strict session cookie.
func (c *child) onNavigated(sender *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
	if sender == nil {
		return
	}
	src, err := sender.GetSource()
	if err != nil {
		return
	}
	if strings.HasPrefix(src, c.serverURL+"/?mode=glass") {
		c.loaded = true
		c.applyView()
	}
}

// ---------------------------------------------------------------------------
// Navigation filter. go-webview2's edge package does not expose
// NavigationStarting or NewWindowRequested, and the generated wrappers in its
// webview2 package pass the wrong arguments (GetUri hands over a nil
// out-pointer, PutCancel and PutHandled a pointer where the ABI wants a BOOL
// by value), so the two handlers are minimal COM objects registered through
// the ICoreWebView2 vtable of WebView2.h. They run on the UI thread.
// ---------------------------------------------------------------------------

// ICoreWebView2 vtable slots (IUnknown takes 0-2).
const (
	slotRelease               = 2
	slotAddNavigationStarting = 7
	slotAddNewWindowRequested = 44
	slotArgsGetURI            = 3 // both event-args interfaces
	slotNavStartingPutCancel  = 8
	slotNewWindowPutHandled   = 6
	hrOK                      = 0
	hrNoInterface             = 0x80004002
	hrPointer                 = 0x80004003
)

var (
	iidIUnknown                  = windows.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidNavigationStartingHandler = windows.GUID{Data1: 0x9adbe429, Data2: 0xf36d, Data3: 0x432b, Data4: [8]byte{0x9d, 0xdc, 0xf8, 0x88, 0x1f, 0xbd, 0x76, 0xe3}}
	iidNewWindowRequestedHandler = windows.GUID{Data1: 0xd4c185fe, Data2: 0xc81c, Data3: 0x4989, Data4: [8]byte{0x97, 0xaf, 0x2d, 0x3f, 0xa7, 0xab, 0x56, 0x51}}
)

// comHandler is a WebView2 event handler object: IUnknown plus Invoke. The
// two instances are package globals that live as long as the process, so
// AddRef and Release do not count.
type comHandler struct {
	vtbl *comHandlerVtbl
	iid  *windows.GUID
}

type comHandlerVtbl struct {
	QueryInterface, AddRef, Release, Invoke uintptr
}

var (
	comQI      = windows.NewCallback(comHandlerQueryInterface)
	comAddRef  = windows.NewCallback(comHandlerAddRef)
	comRelease = windows.NewCallback(comHandlerAddRef)

	navStartingVtbl = comHandlerVtbl{comQI, comAddRef, comRelease, windows.NewCallback(navStartingInvoke)}
	newWindowVtbl   = comHandlerVtbl{comQI, comAddRef, comRelease, windows.NewCallback(newWindowInvoke)}

	navStartingHandler = &comHandler{vtbl: &navStartingVtbl, iid: &iidNavigationStartingHandler}
	newWindowHandler   = &comHandler{vtbl: &newWindowVtbl, iid: &iidNewWindowRequestedHandler}
)

func comHandlerQueryInterface(this *comHandler, riid *windows.GUID, ppv *unsafe.Pointer) uintptr {
	if ppv == nil {
		return hrPointer
	}
	if riid != nil && (*riid == iidIUnknown || *riid == *this.iid) {
		*ppv = unsafe.Pointer(this)
		return hrOK
	}
	*ppv = nil
	return hrNoInterface
}

func comHandlerAddRef(*comHandler) uintptr { return 1 }

// comCall calls vtable slot of the COM object obj with args. uintptrescapes
// keeps pointers converted in the arguments alive and unmoved during the call,
// as for syscall.SyscallN itself.
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, slot*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

// comURI reads an event's Uri property (an LPWSTR the caller frees).
func comURI(args unsafe.Pointer) (string, bool) {
	var p *uint16
	if hr := comCall(args, slotArgsGetURI, uintptr(unsafe.Pointer(&p))); hr != hrOK || p == nil {
		return "", false
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), true
}

// restrictNavigation registers the navigation filter on the WebView.
func (c *child) restrictNavigation() error {
	ctl := c.wv.GetController()
	if ctl == nil {
		return errors.New("no WebView2 controller")
	}
	wv, err := ctl.GetCoreWebView2()
	if err != nil {
		return err
	}
	obj := unsafe.Pointer(wv)
	defer comCall(obj, slotRelease)
	var token int64
	if hr := comCall(obj, slotAddNavigationStarting, uintptr(unsafe.Pointer(navStartingHandler)), uintptr(unsafe.Pointer(&token))); hr != hrOK {
		return fmt.Errorf("add_NavigationStarting: HRESULT 0x%08X", uint32(hr))
	}
	if hr := comCall(obj, slotAddNewWindowRequested, uintptr(unsafe.Pointer(newWindowHandler)), uintptr(unsafe.Pointer(&token))); hr != hrOK {
		return fmt.Errorf("add_NewWindowRequested: HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

// navStartingInvoke keeps top-level navigations on the dashboard origin.
func navStartingInvoke(_ *comHandler, _, args unsafe.Pointer) uintptr {
	c := theChild
	if c == nil || args == nil {
		return hrOK
	}
	uri, ok := comURI(args)
	action := navBlock
	if ok {
		action = navigationPolicy(c.serverURL, uri, c.launching)
	}
	if action == navAllow {
		if strings.HasPrefix(uri, "http") {
			c.launching = false // the launch page is posting its login
		}
		return hrOK
	}
	comCall(args, slotNavStartingPutCancel, 1)
	c.leave(uri, action)
	return hrOK
}

// newWindowInvoke refuses every new WebView2 window; links meant for a new
// window open in the default browser.
func newWindowInvoke(_ *comHandler, _, args unsafe.Pointer) uintptr {
	c := theChild
	if c == nil || args == nil {
		return hrOK
	}
	comCall(args, slotNewWindowPutHandled, 1) // handled, no NewWindow: nothing opens
	uri, ok := comURI(args)
	action := navBlock
	if ok {
		action = navigationPolicy(c.serverURL, uri, false)
	}
	if action == navAllow {
		action = navBlock // the dashboard is a single page: no second window of it
	}
	c.leave(uri, action)
	return hrOK
}

// leave hands an external link to the default browser and logs the rest.
func (c *child) leave(uri string, action navAction) {
	if action != navExternal {
		fmt.Fprintf(os.Stderr, "blocked navigation to %.200q\n", uri)
		return
	}
	go func() {
		if err := openExternal(uri); err != nil {
			fmt.Fprintf(os.Stderr, "opening %.200q: %v\n", uri, err)
		}
	}()
}

// applyView switches the loaded dashboard to the pending view. The name was
// checked by ValidView, so it is safe inside the script string.
func (c *child) applyView() {
	if !c.loaded || c.view == "" {
		return
	}
	c.wv.Eval(`window.stv2 && window.stv2.view("` + c.view + `")`)
	c.view = ""
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	c := theChild
	if c == nil || (c.hwnd != 0 && hwnd != c.hwnd) {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	}
	switch msg {
	case wmCommand:
		c.drain()
		return 0
	case wmActivate:
		if wParam&0xFFFF == waInactive && c.ready {
			c.hide()
		}
	case wmTimer:
		if wParam == escTimerID {
			procKillTimer.Call(hwnd, escTimerID)
			if c.ready {
				c.hide()
			}
			return 0
		}
	case wmSize:
		if c.ready {
			c.wv.Resize()
		}
	case wmMove:
		if c.ready {
			_ = c.wv.NotifyParentWindowPositionChanged()
		}
	case wmDpiChanged:
		return 0 // the tray sends an explicit size for each show
	case wmClose:
		if c.ready {
			c.hide() // Alt+F4 hides; only "quit" ends the host
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}
