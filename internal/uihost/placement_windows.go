//go:build windows

package uihost

import (
	"image"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shcore = windows.NewLazySystemDLL("shcore.dll")

	procGetCursorPos                 = user32.NewProc("GetCursorPos")
	procMonitorFromRect              = user32.NewProc("MonitorFromRect")
	procGetMonitorInfoW              = user32.NewProc("GetMonitorInfoW")
	procSetThreadDpiAwarenessContext = user32.NewProc("SetThreadDpiAwarenessContext")
	procGetDpiForMonitor             = shcore.NewProc("GetDpiForMonitor")
)

const (
	monitorDefaultToNearest = 2
	mdtEffectiveDPI         = 0
	// dpiAwarenessPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, (HANDLE)-4.
	dpiAwarenessPerMonitorV2 = ^uintptr(3)
)

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) image() image.Rectangle {
	return image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
}

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

// Place returns the dashboard rectangle in physical pixels and the monitor
// scale: on the monitor under the cursor, sizeDIP × the monitor's DPI scale,
// inset 14 DIP from the corner at the taskbar edge.
func Place(sizeDIP image.Point) (image.Rectangle, float64) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer withPerMonitorDPI()()

	var pt point
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ok == 0 {
		pt = point{} // no cursor (e.g. a locked session): use the primary monitor
	}
	probe := rect{pt.X, pt.Y, pt.X + 1, pt.Y + 1}
	mon, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&probe)), monitorDefaultToNearest)
	mi := monitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if mon == 0 {
		return placeRect(image.Rect(0, 0, 1920, 1080), image.Rectangle{}, 96, sizeDIP)
	}
	if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok == 0 {
		return placeRect(image.Rect(0, 0, 1920, 1080), image.Rectangle{}, 96, sizeDIP)
	}
	var dpiX, dpiY uint32 = 96, 96
	if procGetDpiForMonitor.Find() == nil {
		if hr, _, _ := procGetDpiForMonitor.Call(mon, mdtEffectiveDPI,
			uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY))); hr != 0 {
			dpiX = 96
		}
	}
	return placeRect(mi.Monitor.image(), mi.Work.image(), dpiX, sizeDIP)
}

// withPerMonitorDPI makes the calling (locked) thread per-monitor DPI aware,
// so coordinates are physical pixels whatever the process manifest says, and
// returns a function restoring the previous context.
func withPerMonitorDPI() func() {
	if procSetThreadDpiAwarenessContext.Find() != nil {
		return func() {}
	}
	old, _, _ := procSetThreadDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2)
	if old == 0 {
		return func() {}
	}
	return func() { procSetThreadDpiAwarenessContext.Call(old) }
}
