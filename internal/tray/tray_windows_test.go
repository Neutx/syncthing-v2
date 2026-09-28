//go:build windows

package tray

import (
	"image"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Neutx/syncthing-v2/internal/icon"
	"github.com/Neutx/syncthing-v2/internal/model"
)

var (
	procGetMenuItemCount = user32.NewProc("GetMenuItemCount")
	procGetMenuStringW   = user32.NewProc("GetMenuStringW")
	procGetMenuState     = user32.NewProc("GetMenuState")
	procGetSubMenu       = user32.NewProc("GetSubMenu")
	procGetMenuItemID    = user32.NewProc("GetMenuItemID")
	procGetIconInfo      = user32.NewProc("GetIconInfo")
	procGetObjectW       = gdi32.NewProc("GetObjectW")
)

const mfByPosition = 0x400

func menuString(h uintptr, pos int) string {
	buf := make([]uint16, 256)
	procGetMenuStringW.Call(h, uintptr(pos), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), mfByPosition)
	return windows.UTF16ToString(buf)
}

func TestBuildHMenu(t *testing.T) {
	s := model.Snapshot{
		State: model.StateSyncing, Label: "Syncing", GUIURL: "http://127.0.0.1:8384/",
		Folders: []model.Folder{{ID: "f1", Label: "Photos & Video"}, {ID: "f2", Label: "Notes"}},
		Startup: model.Startup{Tray: true},
	}
	items := buildMenu(s, "with Windows")
	h, ids, err := buildHMenu(items)
	if err != nil {
		t.Fatal(err)
	}
	defer procDestroyMenu.Call(h)

	var visible []MenuItem
	for _, it := range items {
		if it.Visible {
			visible = append(visible, it)
		}
	}
	if n, _, _ := procGetMenuItemCount.Call(h); int(n) != len(visible) {
		t.Fatalf("menu has %d items, want %d", n, len(visible))
	}
	seen := map[string]bool{}
	for pos, it := range visible {
		state, _, _ := procGetMenuState.Call(h, uintptr(pos), mfByPosition)
		if it.ID == Separator {
			if state&mfSeparator == 0 {
				t.Errorf("pos %d: want a separator", pos)
			}
			continue
		}
		if got := menuString(h, pos); got != stringsAmp(it.Text) {
			t.Errorf("pos %d text %q, want %q", pos, got, stringsAmp(it.Text))
		}
		if (state&mfGrayed != 0) == it.Enabled {
			t.Errorf("pos %d (%s) grayed=%v, enabled=%v", pos, it.ID, state&mfGrayed != 0, it.Enabled)
		}
		if (state&mfChecked != 0) != it.Checked {
			t.Errorf("pos %d (%s) checked mismatch", pos, it.ID)
		}
		if len(it.Children) > 0 {
			sub, _, _ := procGetSubMenu.Call(h, uintptr(pos))
			if sub == 0 {
				t.Fatalf("pos %d: no submenu", pos)
			}
			for i, c := range it.Children {
				if got := menuString(sub, i); got != stringsAmp(c.Text) {
					t.Errorf("submenu %d text %q, want %q", i, got, stringsAmp(c.Text))
				}
				cmd, _, _ := procGetMenuItemID.Call(sub, uintptr(i))
				if ids[int(cmd)-1] != c.ID {
					t.Errorf("submenu %d command maps to %q, want %q", i, ids[int(cmd)-1], c.ID)
				}
				seen[c.ID] = true
			}
			continue
		}
		cmd, _, _ := procGetMenuItemID.Call(h, uintptr(pos))
		if int(cmd) < 1 || int(cmd) > len(ids) || ids[int(cmd)-1] != it.ID {
			t.Errorf("pos %d command %d does not map to %q", pos, cmd, it.ID)
		}
		seen[it.ID] = true
	}
	if len(seen) != len(ids) {
		t.Errorf("%d commands registered, %d reachable", len(ids), len(seen))
	}
}

func stringsAmp(s string) string { return strings.ReplaceAll(s, "&", "&&") }

func TestCreateIcon(t *testing.T) {
	for _, n := range []int{16, 20, 24, 32} {
		h, err := createIcon(icon.Ring(model.StateSyncing, 30, n))
		if err != nil || h == 0 {
			t.Fatalf("size %d: %v", n, err)
		}
		var ii iconInfo
		if r, _, err := procGetIconInfo.Call(h, uintptr(unsafe.Pointer(&ii))); r == 0 {
			t.Fatalf("GetIconInfo: %v", err)
		}
		var bm struct {
			Type, Width, Height, WidthBytes int32
			Planes, BitsPixel               uint16
			Bits                            uintptr
		}
		procGetObjectW.Call(ii.HbmColor, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm)))
		procDeleteObject.Call(ii.HbmColor)
		procDeleteObject.Call(ii.HbmMask)
		if ii.FIcon != 1 || int(bm.Width) != n || int(bm.Height) != n || bm.BitsPixel != 32 {
			t.Errorf("size %d: icon %v bitmap %dx%d@%d", n, ii.FIcon, bm.Width, bm.Height, bm.BitsPixel)
		}
		if r, _, _ := procDestroyIcon.Call(h); r == 0 {
			t.Errorf("DestroyIcon failed for size %d", n)
		}
	}
	if _, err := createIcon(image.NewRGBA(image.Rect(0, 0, 0, 0))); err == nil {
		t.Error("createIcon accepted an empty image")
	}
}

func TestCopyUTF16(t *testing.T) {
	buf := make([]uint16, 8)
	copyUTF16(buf, "abc\x00def")
	if got := windows.UTF16ToString(buf); got != "abcdef" {
		t.Errorf("NUL not removed: %q", got)
	}
	copyUTF16(buf, "0123456789")
	if got := windows.UTF16ToString(buf); got != "0123..." || buf[7] != 0 {
		t.Errorf("long text %q", got)
	}
}

// TestTrayLoop drives the real message loop with synthetic shell callbacks.
// It adds a short-lived icon to the notification area when Explorer runs,
// but shows no balloon; TestManualTray covers real balloons and clicks.
func TestTrayLoop(t *testing.T) {
	tr := newTray().(*winTray)
	ready := make(chan struct{})
	clicks := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Run(func() { close(ready) }, func() { clicks <- "click" }, func(id string) { clicks <- "menu:" + id })
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("onReady not called")
	}
	tr.mu.Lock()
	hwnd := tr.hwnd
	tr.mu.Unlock()
	if hwnd == 0 {
		t.Fatal("no window")
	}

	// Concurrent updates from many goroutines (run with -race).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				tr.SetIcon(model.State(j%8), j*2)
				tr.SetTooltip(Tooltip(model.Snapshot{Label: "In sync", PeerLine: "peer", FolderLine: "Up to date"}))
				tr.SetMenu(BuildMenu(model.Snapshot{State: model.State(i % 8)}))
			}
		}(i)
	}
	wg.Wait()

	expect := func(want string) {
		t.Helper()
		select {
		case got := <-clicks:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	post := func(event uintptr) {
		if r, _, err := procPostMessageW.Call(hwnd, wmTrayCallback, 0, event|trayUID<<16); r == 0 {
			t.Fatal(err)
		}
	}

	post(ninSelect)
	expect("click")
	time.Sleep(clickDebounce + 50*time.Millisecond)
	post(ninBalloonUserClick)
	expect("click")

	tr.Quit()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Quit")
	}
	if tr.Balloon("late", "late") {
		t.Error("Balloon after Quit must return false")
	}
}

func TestQuitBeforeRun(t *testing.T) {
	tr := newTray()
	tr.Quit()
	done := make(chan struct{})
	go func() { tr.Run(nil, nil, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run after Quit did not return")
	}
}

// TestBalloonClickSurvivesHide checks that a late NIN_BALLOONTIMEOUT or
// NIN_BALLOONHIDE (posted asynchronously for the previous balloon, or for a
// balloon that moved to the Action Center) does not drop the click handler
// of the balloon that is shown.
func TestBalloonClickSurvivesHide(t *testing.T) {
	var got []string
	tr := &winTray{onClick: func() { got = append(got, "tray") }}
	tr.balloonClick = func() { got = append(got, "balloon") }

	tr.onCallback(0, ninBalloonTimeout, 0)
	tr.onCallback(0, ninBalloonHide, 0)
	tr.onCallback(0, ninBalloonUserClick, 0)
	tr.onCallback(0, ninBalloonUserClick, 0) // consumed: falls back to the tray click
	if want := []string{"balloon", "tray"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("clicks %v, want %v", got, want)
	}
}

// TestInitialIconIsAppIcon checks that the tray shows the neutral app icon,
// not the zero State (Down), until the first SetIcon.
func TestInitialIconIsAppIcon(t *testing.T) {
	tr := newTray().(*winTray)
	defer func() {
		if tr.hicon != 0 {
			procDestroyIcon.Call(tr.hicon)
		}
	}()
	tr.renderIcon(0)
	if tr.hicon == 0 || tr.shown.state != icon.PendingState {
		t.Fatalf("before SetIcon: shown %+v, want the pending state %v", tr.shown, icon.PendingState)
	}
	if tr.tip != startingTooltip {
		t.Errorf("initial tooltip %q, want %q", tr.tip, startingTooltip)
	}
	tr.SetIcon(model.StateDown, 0) // no window yet: stored, applied on render
	tr.renderIcon(0)
	if tr.shown.state != model.StateDown {
		t.Errorf("after SetIcon(Down): shown %+v", tr.shown)
	}
}
