//go:build windows

package glass

import (
	"fmt"
	"image"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDC                        = user32.NewProc("GetDC")
	procReleaseDC                    = user32.NewProc("ReleaseDC")
	procSetThreadDpiAwarenessContext = user32.NewProc("SetThreadDpiAwarenessContext")
	procCreateCompatibleDC           = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC                     = gdi32.NewProc("DeleteDC")
	procCreateDIBSection             = gdi32.NewProc("CreateDIBSection")
	procSelectObject                 = gdi32.NewProc("SelectObject")
	procDeleteObject                 = gdi32.NewProc("DeleteObject")
	procBitBlt                       = gdi32.NewProc("BitBlt")
	procGdiFlush                     = gdi32.NewProc("GdiFlush")
)

const (
	srcCopy      = 0x00CC0020
	captureBlt   = 0x40000000
	dibRGBColors = 0
	biRGB        = 0
)

// dpiAwarenessPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2.
const dpiAwarenessPerMonitorV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4

type bitmapInfoHeader struct {
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
}

// Capture copies the screen pixels inside r (virtual-screen coordinates in
// physical pixels) with BitBlt. The calling thread is made per-monitor DPI
// aware for the duration, so the rectangle is never rescaled by DPI
// virtualisation. The dashboard popup must be hidden first, or it photographs
// itself.
func Capture(r image.Rectangle) (*image.RGBA, error) {
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("glass: capture: empty rectangle %v", r)
	}
	if w > 16384 || h > 16384 {
		return nil, fmt.Errorf("glass: capture: rectangle %v is too large", r)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if procSetThreadDpiAwarenessContext.Find() == nil {
		if old, _, _ := procSetThreadDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); old != 0 {
			defer procSetThreadDpiAwarenessContext.Call(old)
		}
	}

	screen, _, err := procGetDC.Call(0)
	if screen == 0 {
		return nil, fmt.Errorf("glass: capture: GetDC: %w", err)
	}
	defer procReleaseDC.Call(0, screen)

	mem, _, err := procCreateCompatibleDC.Call(screen)
	if mem == 0 {
		return nil, fmt.Errorf("glass: capture: CreateCompatibleDC: %w", err)
	}
	defer procDeleteDC.Call(mem)

	bi := bitmapInfoHeader{
		Width:       int32(w),
		Height:      -int32(h), // top-down rows
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	bmp, _, err := procCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return nil, fmt.Errorf("glass: capture: CreateDIBSection: %w", err)
	}
	defer procDeleteObject.Call(bmp)

	old, _, _ := procSelectObject.Call(mem, bmp)
	defer procSelectObject.Call(mem, old)

	if ok, _, err := procBitBlt.Call(mem, 0, 0, uintptr(w), uintptr(h), screen,
		uintptr(int32(r.Min.X)), uintptr(int32(r.Min.Y)), srcCopy|captureBlt); ok == 0 {
		return nil, fmt.Errorf("glass: capture: BitBlt: %w", err)
	}
	procGdiFlush.Call()

	// The DIB is BGRX; convert to opaque RGBA.
	src := unsafe.Slice((*uint8)(bits), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	dst := img.Pix
	for i := 0; i < len(src); i += 4 {
		dst[i] = src[i+2]
		dst[i+1] = src[i+1]
		dst[i+2] = src[i]
		dst[i+3] = 255
	}
	return img, nil
}
