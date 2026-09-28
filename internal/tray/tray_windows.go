//go:build windows

package tray

import (
	"errors"
	"image"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/icon"
	"github.com/Neutx/syncthing-v2/internal/model"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procCreateIconIndirect     = user32.NewProc("CreateIconIndirect")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procGetSystemMetricsForDpi = user32.NewProc("GetSystemMetricsForDpi") // Windows 10 1607+
	procGetDpiForWindow        = user32.NewProc("GetDpiForWindow")        // Windows 10 1607+
	procSetTimer               = user32.NewProc("SetTimer")
	procKillTimer              = user32.NewProc("KillTimer")
	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap           = gdi32.NewProc("CreateBitmap")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmSettingChange = 0x001A
	wmContextMenu   = 0x007B
	wmDisplayChange = 0x007E
	wmTimer         = 0x0113
	wmDPIChanged    = 0x02E0
	wmUser          = 0x0400
	wmApp           = 0x8000

	wmTrayUpdate   = wmApp + 1 // apply pending icon, tooltip and balloons
	wmTrayCallback = wmApp + 2 // Shell_NotifyIcon callback message

	ninSelect           = wmUser + 0
	ninKeySelect        = wmUser + 1
	ninBalloonHide      = wmUser + 3
	ninBalloonTimeout   = wmUser + 4
	ninBalloonUserClick = wmUser + 5

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	nifShowTip = 0x80
	niifInfo   = 0x01

	notifyIconVersion4 = 4
	balloonTimeoutMS   = 6000

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmRightAlign  = 0x0008
	tpmBottomAlign = 0x0020
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	smMenuDropAlignment = 40
	smCxSmIcon          = 49

	wsExToolWindow = 0x00000080
	biBitfields    = 3
	dibRGBColors   = 0

	trayUID      = 1
	retryTimerID = 1
	retryEveryMS = 2000

	// clickDebounce drops the duplicate NIN_KEYSELECT that Enter produces
	// and accidental double clicks.
	clickDebounce = 300 * time.Millisecond
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeoutVersion  uint32 // union { uTimeout; uVersion }
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GUIDItem         windows.GUID
	HBalloonIcon     uintptr
}

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

type point struct{ X, Y int32 }

type winMsg struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type bitmapV5Header struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
	RedMask       uint32
	GreenMask     uint32
	BlueMask      uint32
	AlphaMask     uint32
	CSType        uint32
	Endpoints     [36]byte
	GammaRed      uint32
	GammaGreen    uint32
	GammaBlue     uint32
	Intent        uint32
	ProfileData   uint32
	ProfileSize   uint32
	Reserved      uint32
}

type balloon struct {
	title, body string
	onClick     func()
}

type iconKey struct {
	state model.State
	pct   int
	size  int
}

// winTray is the native Windows tray. Fields under mu are written by any
// goroutine; the rest belong to the tray thread.
type winTray struct {
	mu       sync.Mutex
	hwnd     uintptr
	started  bool
	quitting bool
	posted   bool // a wmTrayUpdate is already queued
	iconSet  bool // SetIcon was called; until then the app icon is shown
	state    model.State
	pct      int
	tip      string
	menu     []MenuItem
	queue    []balloon
	added    atomic.Bool // the icon is in the notification area

	// Tray thread only.
	onClick        func()
	onMenu         func(string)
	hicon          uintptr
	shown          iconKey
	shownTip       string
	balloonClick   func()
	lastClick      time.Time
	taskbarCreated uint32
	retrying       bool
}

var (
	activeTray atomic.Pointer[winTray]
	classOnce  sync.Once
	classErr   error
	className  = windows.StringToUTF16Ptr("SyncThingV2.TrayWindow")
	wndProcPtr = windows.NewCallback(wndProc)
)

func newTray() Tray {
	return &winTray{tip: startingTooltip}
}

func registerClass() error {
	classOnce.Do(func() {
		hinst, _, _ := procGetModuleHandleW.Call(0)
		wc := wndClassEx{WndProc: wndProcPtr, Instance: hinst, ClassName: className}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = err
		}
	})
	return classErr
}

func (t *winTray) Run(onReady func(), onClick func(), onMenu func(id string)) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t.mu.Lock()
	if t.started || t.quitting {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()
	if !activeTray.CompareAndSwap(nil, t) {
		applog.Printf("tray: another tray is already running in this process")
		return
	}
	defer activeTray.Store(nil)

	t.onClick, t.onMenu = onClick, onMenu
	if err := registerClass(); err != nil {
		applog.Printf("tray: RegisterClassExW: %v", err)
		return
	}
	msgName := windows.StringToUTF16Ptr("TaskbarCreated")
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(msgName)))
	t.taskbarCreated = uint32(r)

	hinst, _, _ := procGetModuleHandleW.Call(0)
	title := windows.StringToUTF16Ptr(brand.DisplayName)
	hwnd, _, err := procCreateWindowExW.Call(wsExToolWindow, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, hinst, 0)
	if hwnd == 0 {
		applog.Printf("tray: CreateWindowExW: %v", err)
		return
	}

	t.mu.Lock()
	t.hwnd = hwnd
	quitting := t.quitting
	t.mu.Unlock()
	if quitting {
		procDestroyWindow.Call(hwnd)
	} else {
		t.applyPending(hwnd)
		if !t.addIcon(hwnd) {
			t.startRetry(hwnd)
		}
		if onReady != nil {
			go onReady()
		}
	}

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT or error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	t.mu.Lock()
	t.hwnd = 0
	t.mu.Unlock()
}

func (t *winTray) SetIcon(s model.State, pct int) {
	t.mu.Lock()
	t.state, t.pct, t.iconSet = s, pct, true
	t.postLocked()
	t.mu.Unlock()
}

func (t *winTray) SetTooltip(text string) {
	t.mu.Lock()
	t.tip = text
	t.postLocked()
	t.mu.Unlock()
}

func (t *winTray) SetMenu(items []MenuItem) {
	t.mu.Lock()
	t.menu = items // read when the menu opens
	t.mu.Unlock()
}

func (t *winTray) Balloon(title, body string) bool { return t.BalloonWithClick(title, body, nil) }

// BalloonWithClick shows a balloon whose click runs onClick instead of the
// tray's onClick (when onClick is not nil). It reports whether the balloon
// was queued for an icon that is in the notification area.
func (t *winTray) BalloonWithClick(title, body string, onClick func()) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hwnd == 0 || t.quitting || !t.added.Load() {
		return false
	}
	t.queue = append(t.queue, balloon{title, body, onClick})
	return t.postLocked()
}

func (t *winTray) Quit() {
	t.mu.Lock()
	t.quitting = true
	hwnd := t.hwnd
	t.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
	}
}

// postLocked queues one wmTrayUpdate; the caller holds mu.
func (t *winTray) postLocked() bool {
	if t.hwnd == 0 {
		return false // applied when Run creates the window
	}
	if t.posted {
		return true
	}
	r, _, _ := procPostMessageW.Call(t.hwnd, wmTrayUpdate, 0, 0)
	t.posted = r != 0
	return t.posted
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := activeTray.Load()
	if t == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	}
	switch uint32(msg) {
	case wmTrayUpdate:
		t.applyPending(hwnd)
		return 0
	case wmTrayCallback:
		t.onCallback(hwnd, uint32(lParam&0xFFFF), wParam)
		return 0
	case wmTimer:
		if wParam == retryTimerID && t.addIcon(hwnd) {
			procKillTimer.Call(hwnd, retryTimerID)
			t.retrying = false
		}
		return 0
	case wmDPIChanged, wmDisplayChange, wmSettingChange:
		t.renderIcon(hwnd) // the small-icon size may have changed
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if t.retrying {
			procKillTimer.Call(hwnd, retryTimerID)
		}
		if t.added.Load() {
			nid := t.nid(hwnd)
			shellNotify(nimDelete, &nid)
			t.added.Store(false)
		}
		if t.hicon != 0 {
			procDestroyIcon.Call(t.hicon)
			t.hicon = 0
		}
		procPostQuitMessage.Call(0)
		return 0
	default:
		if t.taskbarCreated != 0 && uint32(msg) == t.taskbarCreated {
			// Explorer restarted: the icon is gone and must be added again.
			t.added.Store(false)
			t.shown = iconKey{}
			if !t.addIcon(hwnd) {
				t.startRetry(hwnd)
			}
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func (t *winTray) onCallback(hwnd uintptr, event uint32, anchor uintptr) {
	switch event {
	case ninSelect, ninKeySelect:
		now := time.Now()
		if now.Sub(t.lastClick) < clickDebounce {
			return
		}
		t.lastClick = now
		if t.onClick != nil {
			t.onClick()
		}
	case wmContextMenu:
		x, y := int32(int16(anchor&0xFFFF)), int32(int16((anchor>>16)&0xFFFF))
		t.showMenu(hwnd, x, y)
	case ninBalloonUserClick:
		f := t.balloonClick
		t.balloonClick = nil
		if f == nil {
			f = t.onClick
		}
		if f != nil {
			f()
		}
		// NIN_BALLOONTIMEOUT and NIN_BALLOONHIDE deliberately keep
		// balloonClick: the shell posts them asynchronously, so a late one for
		// the previous balloon would drop the handler of the balloon that
		// replaced it, and a timed-out balloon stays clickable in the Action
		// Center. balloonClick is replaced only when applyPending shows the
		// next balloon.
	}
}

func (t *winTray) nid(hwnd uintptr) notifyIconData {
	var n notifyIconData
	n.CbSize = uint32(unsafe.Sizeof(n))
	n.HWnd = hwnd
	n.UID = trayUID
	return n
}

func shellNotify(op uintptr, n *notifyIconData) bool {
	r, _, _ := procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(n)))
	return r != 0
}

func (t *winTray) startRetry(hwnd uintptr) {
	if !t.retrying {
		procSetTimer.Call(hwnd, retryTimerID, retryEveryMS, 0)
		t.retrying = true
	}
}

// addIcon adds the icon to the notification area (or refreshes one that
// survived an Explorer restart) and switches it to NOTIFYICON_VERSION_4.
func (t *winTray) addIcon(hwnd uintptr) bool {
	if t.added.Load() {
		return true
	}
	t.renderIcon(hwnd)
	n := t.nid(hwnd)
	n.UFlags = nifMessage | nifIcon | nifTip | nifShowTip
	n.UCallbackMessage = wmTrayCallback
	n.HIcon = t.hicon
	copyUTF16(n.SzTip[:], t.shownTip)
	if !shellNotify(nimAdd, &n) && !shellNotify(nimModify, &n) {
		return false
	}
	n.UTimeoutVersion = notifyIconVersion4
	shellNotify(nimSetVersion, &n)
	t.added.Store(true)
	return true
}

// applyPending takes the queued icon, tooltip and balloons and applies them.
func (t *winTray) applyPending(hwnd uintptr) {
	t.mu.Lock()
	t.posted = false
	tip := t.tip
	queue := t.queue
	t.queue = nil
	t.mu.Unlock()

	t.renderIcon(hwnd)
	if tip != t.shownTip {
		t.shownTip = tip
		if t.added.Load() {
			n := t.nid(hwnd)
			n.UFlags = nifTip | nifShowTip
			copyUTF16(n.SzTip[:], tip)
			shellNotify(nimModify, &n)
		}
	}
	for _, b := range queue {
		if !t.added.Load() {
			break
		}
		n := t.nid(hwnd)
		n.UFlags = nifInfo
		n.UTimeoutVersion = balloonTimeoutMS // ignored since Vista; kept for older shells
		n.DwInfoFlags = niifInfo
		copyUTF16(n.SzInfoTitle[:], b.title)
		copyUTF16(n.SzInfo[:], b.body)
		if shellNotify(nimModify, &n) {
			t.balloonClick = b.onClick
		}
	}
}

// renderIcon draws the current state at the current small-icon size, if it
// changed, and swaps it in, destroying the previous HICON.
func (t *winTray) renderIcon(hwnd uintptr) {
	size := smallIconSize(hwnd)
	t.mu.Lock()
	key := iconKey{state: t.state, pct: t.pct, size: size}
	if !t.iconSet {
		// Before the first status poll the state is unknown: show the neutral
		// app icon rather than the zero State (Down).
		key.state = icon.AppState
	}
	t.mu.Unlock()
	if key.state != model.StateSyncing {
		key.pct = 0
	} else {
		key.pct = max(0, min(key.pct, 100))
	}
	if key == t.shown && t.hicon != 0 {
		return
	}
	h, err := createIcon(icon.Ring(key.state, key.pct, key.size))
	if err != nil {
		applog.Printf("tray: create icon: %v", err)
		return
	}
	if t.added.Load() {
		n := t.nid(hwnd)
		n.UFlags = nifIcon
		n.HIcon = h
		shellNotify(nimModify, &n)
	}
	if t.hicon != 0 {
		procDestroyIcon.Call(t.hicon)
	}
	t.hicon, t.shown = h, key
}

// smallIconSize is SM_CXSMICON at the window's DPI (16 px at 100%).
func smallIconSize(hwnd uintptr) int {
	if hwnd != 0 && procGetDpiForWindow.Find() == nil && procGetSystemMetricsForDpi.Find() == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
			if n, _, _ := procGetSystemMetricsForDpi.Call(smCxSmIcon, dpi); int32(n) > 0 {
				return int(int32(n))
			}
		}
	}
	if n, _, _ := procGetSystemMetrics.Call(smCxSmIcon); int32(n) > 0 {
		return int(int32(n))
	}
	return 16
}

// createIcon converts img to an HICON with a 32-bit alpha colour bitmap.
// The caller owns the handle and frees it with DestroyIcon.
func createIcon(img *image.RGBA) (uintptr, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w < 1 || h < 1 {
		return 0, errors.New("empty icon image")
	}
	bi := bitmapV5Header{
		Width: int32(w), Height: -int32(h), // top-down
		Planes: 1, BitCount: 32, Compression: biBitfields,
		RedMask: 0x00FF0000, GreenMask: 0x0000FF00, BlueMask: 0x000000FF, AlphaMask: 0xFF000000,
	}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	color, _, err := procCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if color == 0 || bits == nil {
		return 0, err
	}
	defer procDeleteObject.Call(color)
	px := icon.BGRA(img)
	copy(unsafe.Slice((*byte)(bits), len(px)), px)

	maskBits := make([]byte, ((w+15)/16)*2*h) // all zero: the alpha channel decides
	mask, _, err := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if mask == 0 {
		return 0, err
	}
	defer procDeleteObject.Call(mask)

	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: color}
	hicon, _, err := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if hicon == 0 {
		return 0, err
	}
	return hicon, nil
}

func (t *winTray) showMenu(hwnd uintptr, x, y int32) {
	t.mu.Lock()
	items := t.menu
	t.mu.Unlock()
	if len(items) == 0 {
		return
	}
	hmenu, ids, err := buildHMenu(items)
	if err != nil {
		applog.Printf("tray: build menu: %v", err)
		return
	}
	defer procDestroyMenu.Call(hmenu) // also destroys the submenus

	if x == 0 && y == 0 {
		var p point
		procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
		x, y = p.X, p.Y
	}
	flags := uintptr(tpmReturnCmd | tpmNoNotify | tpmRightButton | tpmBottomAlign | tpmLeftAlign)
	if r, _, _ := procGetSystemMetrics.Call(smMenuDropAlignment); r != 0 {
		flags |= tpmRightAlign
	}
	// The window must be in the foreground or the menu does not close when
	// the user clicks elsewhere (KB135788).
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hmenu, flags, uintptr(x), uintptr(y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	if cmd > 0 && int(cmd) <= len(ids) && t.onMenu != nil {
		t.onMenu(ids[cmd-1])
	}
}

// buildHMenu creates a popup menu for the visible items. Command n (1-based)
// maps to ids[n-1]. The caller destroys the menu.
func buildHMenu(items []MenuItem) (uintptr, []string, error) {
	var ids []string
	var build func(items []MenuItem) (uintptr, error)
	build = func(items []MenuItem) (uintptr, error) {
		h, _, err := procCreatePopupMenu.Call()
		if h == 0 {
			return 0, err
		}
		for _, it := range items {
			if !it.Visible {
				continue
			}
			if it.ID == Separator {
				procAppendMenuW.Call(h, mfSeparator, 0, 0)
				continue
			}
			flags := uintptr(mfString)
			if !it.Enabled {
				flags |= mfGrayed
			}
			if it.Checked {
				flags |= mfChecked
			}
			text, err := windows.UTF16PtrFromString(strings.ReplaceAll(strings.ReplaceAll(it.Text, "\x00", ""), "&", "&&"))
			if err != nil {
				procDestroyMenu.Call(h)
				return 0, err
			}
			var id uintptr
			if len(it.Children) > 0 {
				sub, err := build(it.Children)
				if err != nil {
					procDestroyMenu.Call(h)
					return 0, err
				}
				flags |= mfPopup
				id = sub
			} else {
				ids = append(ids, it.ID)
				id = uintptr(len(ids))
			}
			if r, _, err := procAppendMenuW.Call(h, flags, id, uintptr(unsafe.Pointer(text))); r == 0 {
				if flags&mfPopup != 0 {
					procDestroyMenu.Call(id)
				}
				procDestroyMenu.Call(h)
				return 0, err
			}
		}
		return h, nil
	}
	h, err := build(items)
	return h, ids, err
}

// copyUTF16 copies s into a fixed WCHAR buffer, cut with "..." to fit and
// always NUL-terminated.
func copyUTF16(dst []uint16, s string) {
	s = strings.ReplaceAll(s, "\x00", "")
	u := utf16.Encode([]rune(truncateUTF16(s, len(dst)-1)))
	n := copy(dst[:len(dst)-1], u)
	dst[n] = 0
}
