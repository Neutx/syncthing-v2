// Package icon rasterises the tray and app icon: a coloured status ring with
// a per-state glyph. It is a port of the prototype's runtime-drawn icon
// (feature F19), drawn in a 32-unit design space and scaled to any size.
//
// The glyphs are:
//
//   - InSync: full ring and a tick
//   - Syncing: a faint track ring and a progress arc from 12 o'clock
//     (clockwise, at least 2%)
//   - NoPeer and Error: full ring and an X
//   - Unauthorized: full ring and an X in the Down colour
//   - Scanning, Paused and Down: full ring and a centre dot
package icon

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/vector"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// Design-space geometry (32×32), taken from the prototype's GDI+ drawing:
// the ring is the ellipse (3,3,26,26) stroked with a 5 px pen, glyph lines use
// a 4 px pen with round caps, and the dot is the 6 px ellipse (13,13,6,6).
const (
	design     = 32.0
	ringCX     = 16.0
	ringCY     = 16.0
	ringRadius = 13.0
	ringWidth  = 5.0
	glyphWidth = 4.0
	dotRadius  = 3.0
	trackAlpha = 70
	minPct     = 2
)

// Colours per state, as in the prototype (spec §8.2).
var (
	colInSync   = color.NRGBA{0x2E, 0xBE, 0x64, 0xFF}
	colSyncing  = color.NRGBA{0x38, 0x96, 0xF0, 0xFF}
	colScanning = color.NRGBA{0xF0, 0xB4, 0x28, 0xFF}
	colAlert    = color.NRGBA{0xE6, 0x50, 0x46, 0xFF} // NoPeer and Error
	colPaused   = color.NRGBA{0x96, 0x96, 0x9B, 0xFF}
	colDown     = color.NRGBA{0x6E, 0x6E, 0x73, 0xFF} // Down and Unauthorized
)

// AppState is the look of the application icon (assets/icons) and of the
// tray icon before the first status poll: the in-sync ring with its tick.
const AppState = model.StateInSync

// StateColor returns the ring colour used for state s.
func StateColor(s model.State) color.NRGBA {
	switch s {
	case model.StateInSync:
		return colInSync
	case model.StateSyncing:
		return colSyncing
	case model.StateScanning:
		return colScanning
	case model.StateNoPeer, model.StateError:
		return colAlert
	case model.StatePaused:
		return colPaused
	default:
		return colDown
	}
}

// Ring draws the status icon for state s at size×size pixels. pct is the sync
// percentage and only matters for StateSyncing; it is clamped to 0..100 and
// drawn as at least 2% so progress is always visible. Sizes below 1 are
// treated as 1.
func Ring(s model.State, pct int, size int) *image.RGBA {
	if size < 1 {
		size = 1
	}
	p := &painter{
		img: image.NewRGBA(image.Rect(0, 0, size, size)),
		k:   float64(size) / design,
		z:   vector.NewRasterizer(size, size),
	}
	c := StateColor(s)
	track := color.NRGBA{c.R, c.G, c.B, trackAlpha}

	// The faint track ring is always drawn first.
	p.fill(track, func() { p.annulus(ringCX, ringCY, ringRadius-ringWidth/2, ringRadius+ringWidth/2) })

	if s == model.StateSyncing {
		pct = max(minPct, min(pct, 100))
		sweep := 360 * float64(pct) / 100
		p.fill(c, func() { p.arcStroke(ringCX, ringCY, ringRadius, ringWidth, -90, sweep) })
		return p.img
	}

	p.fill(c, func() { p.annulus(ringCX, ringCY, ringRadius-ringWidth/2, ringRadius+ringWidth/2) })
	switch s {
	case model.StateInSync:
		p.fill(c, func() {
			p.capsule(11, 16, 15, 20, glyphWidth)
			p.capsule(15, 20, 22, 12, glyphWidth)
		})
	case model.StateNoPeer, model.StateError, model.StateUnauthorized:
		p.fill(c, func() {
			p.capsule(12, 12, 20, 20, glyphWidth)
			p.capsule(20, 12, 12, 20, glyphWidth)
		})
	default:
		p.fill(c, func() { p.circle(ringCX, ringCY, dotRadius, 360) })
	}
	return p.img
}

// painter builds paths in design units and composites them onto img.
//
// Shapes of one kind are always emitted with the same orientation, so
// overlapping shapes in one fill form a union (the rasteriser clamps coverage
// at 1), and a reversed inner circle cuts a hole.
type painter struct {
	img *image.RGBA
	k   float64
	z   *vector.Rasterizer
}

func (p *painter) fill(c color.NRGBA, build func()) {
	b := p.img.Bounds()
	p.z.Reset(b.Dx(), b.Dy())
	build()
	p.z.Draw(p.img, b, image.NewUniform(c), image.Point{})
}

func (p *painter) pt(x, y float64) (float32, float32) {
	return float32(x * p.k), float32(y * p.k)
}

func (p *painter) moveTo(x, y float64) { p.z.MoveTo(p.pt(x, y)) }
func (p *painter) lineTo(x, y float64) { p.z.LineTo(p.pt(x, y)) }

// arcTo appends an arc of radius r around (cx, cy) that starts at angle a0
// (degrees; the pen must already be there) and turns by sweep degrees.
// Positive sweeps run clockwise on screen (y grows downwards).
func (p *painter) arcTo(cx, cy, r, a0, sweep float64) {
	n := int(math.Ceil(math.Abs(sweep) / 90))
	if n == 0 {
		return
	}
	step := sweep / float64(n) * math.Pi / 180
	a := a0 * math.Pi / 180
	h := 4.0 / 3.0 * math.Tan(step/4)
	for i := 0; i < n; i++ {
		b := a + step
		ca, sa := math.Cos(a), math.Sin(a)
		cb, sb := math.Cos(b), math.Sin(b)
		x1, y1 := cx+r*(ca-h*sa), cy+r*(sa+h*ca)
		x2, y2 := cx+r*(cb+h*sb), cy+r*(sb-h*cb)
		x3, y3 := cx+r*cb, cy+r*sb
		p.z.CubeTo(float32(x1*p.k), float32(y1*p.k), float32(x2*p.k), float32(y2*p.k), float32(x3*p.k), float32(y3*p.k))
		a = b
	}
}

func polar(cx, cy, r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return cx + r*math.Cos(rad), cy + r*math.Sin(rad)
}

// circle adds a closed circle; sweep is +360 or -360 and sets its orientation.
func (p *painter) circle(cx, cy, r, sweep float64) {
	p.moveTo(polar(cx, cy, r, 0))
	p.arcTo(cx, cy, r, 0, sweep)
	p.z.ClosePath()
}

// annulus adds the ring between radii ri and ro.
func (p *painter) annulus(cx, cy, ri, ro float64) {
	p.circle(cx, cy, ro, 360)
	p.circle(cx, cy, ri, -360)
}

// arcStroke adds a stroked arc of centre radius r and width w with round
// caps, starting at angle a0 and turning clockwise by sweep degrees.
func (p *painter) arcStroke(cx, cy, r, w, a0, sweep float64) {
	if sweep >= 360 {
		p.annulus(cx, cy, r-w/2, r+w/2)
		return
	}
	ro, ri, h := r+w/2, r-w/2, w/2
	a1 := a0 + sweep
	p.moveTo(polar(cx, cy, ro, a0))
	p.arcTo(cx, cy, ro, a0, sweep)
	ex, ey := polar(cx, cy, r, a1)
	p.arcTo(ex, ey, h, a1, 180) // end cap, bulging along the direction of travel
	p.arcTo(cx, cy, ri, a1, -sweep)
	sx, sy := polar(cx, cy, r, a0)
	p.arcTo(sx, sy, h, a0+180, 180) // start cap, bulging backwards
	p.z.ClosePath()
}

// capsule adds the line (x1,y1)-(x2,y2) stroked with width w and round caps.
func (p *painter) capsule(x1, y1, x2, y2, w float64) {
	h := w / 2
	dir := math.Atan2(y2-y1, x2-x1) * 180 / math.Pi
	nx, ny := polar(0, 0, h, dir+90)
	p.moveTo(x1+nx, y1+ny)
	p.lineTo(x2+nx, y2+ny)
	p.arcTo(x2, y2, h, dir+90, -180)
	p.lineTo(x1-nx, y1-ny)
	p.arcTo(x1, y1, h, dir-90, -180)
	p.z.ClosePath()
}
