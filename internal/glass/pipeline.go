package glass

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"
)

// ErrUnsupported is returned by Capture where screen capture is not
// implemented (every platform except Windows).
var ErrUnsupported = fmt.Errorf("glass: screen capture: %w", errors.ErrUnsupported)

// Material constants of the prototype's backdrop (Dashboard.CaptureBackdrop).
var (
	// TintColor is the base wash, #11131A.
	TintColor = color.RGBA{R: 0x11, G: 0x13, B: 0x1A, A: 0xFF}
)

const (
	// Over a bright backdrop (mean luminance > 0.5) the glass tints and
	// darkens more so white text stays legible.
	tintAlphaBright, tintAlphaDark = 0.20, 0.12
	darkenBright, darkenDark       = 0.62, 0.78
	// The key light comes from the upper left.
	lightX, lightY = -0.62, -0.78
	specStrength   = 0.90
	rimStrength    = 0.40
)

// Render turns a capture of the desktop behind the dashboard into the glass
// backdrop: it measures the mean luminance, blurs and saturates the capture,
// and refracts it through the cached squircle lens. The result has the same
// size as src, with the squircle coverage in its alpha channel.
//
// Unlike the prototype, which uses fixed pixel values at every monitor
// scale, Render multiplies the blur radius, corner radius, bevel and maximum
// shift by scale (for example a blur radius of 6 and a 37.5 px shift at
// 125%), and BlurAndSaturate resamples with a box filter and bilinear
// filtering in place of GDI+'s high-quality modes; see the package comment.
func Render(src *image.RGBA, scale float64) *image.RGBA {
	if src == nil || src.Rect.Empty() {
		return image.NewRGBA(image.Rectangle{})
	}
	if !(scale > 0) || math.IsInf(scale, 0) {
		scale = 1
	}
	src = originZero(src)
	w, h := src.Rect.Dx(), src.Rect.Dy()

	lum := MeanLuminance(src)
	blurred := BlurAndSaturate(src, int(math.Round(BlurRadius*scale)), Saturation)

	// The corner radius is clamped the same way SquirclePath clamps it, so the
	// alpha edge agrees with a window region built from the path.
	radius := math.Min(Radius*scale, float64(min(w, h))*0.5)
	d := displacementFor(w, h, float32(radius), float32(Bevel*scale), Strength, float32(MaxShift*scale))

	// Legibility comes from the content scrim drawn under the text, not from
	// crushing the backdrop: a heavy multiply kills the contrast that makes
	// refraction and material read.
	p := RefractParams{
		LensAmount: 1,
		Tint:       TintColor,
		TintAlpha:  tintAlphaDark,
		LightX:     lightX,
		LightY:     lightY,
		Spec:       specStrength,
		Rim:        rimStrength,
		Darken:     darkenDark,
	}
	if lum > 0.5 {
		p.TintAlpha, p.Darken = tintAlphaBright, darkenBright
	}
	return Refract(blurred, d, p, src)
}

// MeanLuminance returns the mean Rec. 709 luminance (0..1) of img, sampled
// on a 3-pixel grid. It drives the adaptive tint. An empty image gives 0.5.
func MeanLuminance(img *image.RGBA) float32 {
	b := img.Rect
	var sum float64
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		for x := b.Min.X; x < b.Max.X; x += 3 {
			o := img.PixOffset(x, y)
			q := img.Pix[o : o+3 : o+3]
			sum += 0.2126*float64(q[0]) + 0.7152*float64(q[1]) + 0.0722*float64(q[2])
			n++
		}
	}
	if n == 0 {
		return 0.5
	}
	return float32(sum / float64(n) / 255)
}

// PNG encodes a rendered backdrop. It favours speed over size, because the
// backdrop is re-encoded on every show and only travels over loopback.
func PNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// originZero returns img itself if its origin is (0, 0), or a copy moved there.
func originZero(img *image.RGBA) *image.RGBA {
	if img.Rect.Min == (image.Point{}) {
		return img
	}
	c := image.NewRGBA(image.Rect(0, 0, img.Rect.Dx(), img.Rect.Dy()))
	draw.Draw(c, c.Rect, img, img.Rect.Min, draw.Src)
	return c
}

// The displacement map depends only on geometry, which changes only when the
// dashboard moves to a monitor with a different scale, so a few entries are
// kept.
type dispKey struct {
	w, h                           int
	radius, bevel, strength, shift float32
}

var (
	dispMu    sync.Mutex
	dispCache = map[dispKey]*Displacement{}
)

const dispCacheMax = 4

func displacementFor(w, h int, radius, bevel, strength, shift float32) *Displacement {
	k := dispKey{w, h, radius, bevel, strength, shift}
	dispMu.Lock()
	defer dispMu.Unlock()
	if d, ok := dispCache[k]; ok {
		return d
	}
	if len(dispCache) >= dispCacheMax {
		clear(dispCache)
	}
	d := BuildDisplacement(w, h, radius, bevel, strength, shift)
	dispCache[k] = d
	return d
}
