// Package glass is a pure-Go port of the prototype's "Liquid Glass" optics
// (Glass.cs): a squircle signed-distance field, a cached bevel displacement
// map, a half-resolution box blur with saturation, and a refraction pass with
// chromatic dispersion, Fresnel rim, Blinn-Phong specular and bevel shading.
//
// The constants are the prototype's code constants, which differ from its old
// design notes; where the two disagree, the code wins.
//
// Intentional deviations from Glass.cs:
//
//   - Resampling. The prototype halves and restores the backdrop with GDI+
//     (InterpolationMode.HighQualityBilinear down, HighQualityBicubic up).
//     GDI+ is Windows-only and its filters are not specified exactly, so the
//     port uses a 2×2 box average down and bilinear filtering up. Both are
//     low-pass at this ratio; the difference is sub-pixel softness that the
//     two blur passes erase. The golden tests pin the port's own output.
//   - DPI scaling. The prototype passes the fixed pixel constants BlurRadius
//     (5) and MaxShift (30) at every monitor scale, so its glass looks
//     sharper and flatter on high-DPI screens. Render scales the blur radius,
//     corner radius, bevel and maximum shift by the monitor scale so the
//     material looks the same at every scale: at 125% the blur radius is
//     round(5×1.25) = 6 (3 at half resolution) and the shift is 37.5 px. At
//     scale 1 the values are exactly the prototype's (radius 5, which becomes
//     2 at half resolution; shift 30).
package glass

import "math"

// Code constants of the prototype (Glass.cs and Dashboard.cs).
const (
	// Radius is the window corner radius in DIP.
	Radius = 40.0
	// SquircleN is the superellipse exponent: n = 4 gives continuous curvature.
	SquircleN = 4.0
	// Bevel is the depth of the lens bevel in DIP.
	Bevel = 30.0
	// Strength scales the refraction magnitude.
	Strength = 1.0
	// IOR is the index of refraction of the virtual glass slab.
	IOR = 1.5
	// Dispersion is the percentage of the shift split across R and B at the rim.
	Dispersion = 22.0
	// BlurRadius is the full-resolution box-blur radius; the blur runs at half
	// resolution, so the effective radius is BlurRadius/2.
	BlurRadius = 5
	// Saturation is the saturation boost applied to the blurred backdrop.
	Saturation = 2.0
	// MaxShift is the peak inward pull at the rim, in DIP.
	MaxShift = 30.0
)

// PointF is a point with floating-point coordinates.
type PointF struct{ X, Y float64 }

// SquircleSDF is the signed distance from (x, y) to the edge of a w×h
// rounded box whose corners are n = 4 superellipses of the given radius:
// negative inside, zero on the edge, positive outside, in pixels
// (approximately, in the corners).
func SquircleSDF(x, y, w, h, radius float32) float32 {
	// The interior branch must subtract the radius: without it the function
	// returns 0 both at the true edge and radius px inset, which creates a
	// phantom second rim and a discontinuous bevel.
	a, b := w*0.5, h*0.5
	px, py := abs32(x-a), abs32(y-b)
	qx, qy := px-(a-radius), py-(b-radius)
	if qx <= 0 && qy <= 0 {
		return max(qx, qy) - radius // flat interior
	}
	qx, qy = max(qx, 0), max(qy, 0)
	d := math.Pow(math.Pow(float64(qx), SquircleN)+math.Pow(float64(qy), SquircleN), 1.0/SquircleN)
	return float32(d) - radius
}

// SquirclePath traces the actual superellipse outline of the rectangle
// (x, y, w, h) with the given corner radius, clockwise in screen coordinates,
// starting at the top edge of the top-right corner. The polygon is closed
// implicitly (the first point is not repeated). Tracing the true n = 4 curve
// keeps a clip or window region in agreement with the SDF the lighting uses.
func SquirclePath(x, y, w, h, radius float64) []PointF {
	rad := math.Min(radius, math.Min(w, h)*0.5)
	if rad <= 0.5 {
		return []PointF{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}}
	}
	// |x|^n + |y|^n = rad^n is parameterised by x = rad·cos(a)^(2/n),
	// y = rad·sin(a)^(2/n). (The prototype used the exponent 1/n, which bulges
	// about 3 px off the curve at 45° for radius 40, so its clip disagreed
	// with the SDF it was meant to match.)
	const inv = 2.0 / SquircleN
	steps := max(6, int(float32(rad)*0.7))

	// One corner arc, from the straight edge to the diagonal, in local coordinates.
	arc := make([]PointF, steps+1)
	for i := 0; i <= steps; i++ {
		a := float64(i) / float64(steps) * math.Pi / 2
		cs, sn := math.Cos(a), math.Sin(a)
		arc[i] = PointF{
			X: rad * math.Pow(math.Abs(cs), inv) * sign(cs),
			Y: rad * math.Pow(math.Abs(sn), inv) * sign(sn),
		}
	}

	l, t, r, b := x, y, x+w, y+h
	pts := make([]PointF, 0, 4*(steps+1))
	for i := steps; i >= 0; i-- { // top-right
		pts = append(pts, PointF{r - rad + arc[i].X, t + rad - arc[i].Y})
	}
	for i := 0; i <= steps; i++ { // bottom-right
		pts = append(pts, PointF{r - rad + arc[i].X, b - rad + arc[i].Y})
	}
	for i := steps; i >= 0; i-- { // bottom-left
		pts = append(pts, PointF{l + rad - arc[i].X, b - rad + arc[i].Y})
	}
	for i := 0; i <= steps; i++ { // top-left
		pts = append(pts, PointF{l + rad - arc[i].X, t + rad - arc[i].Y})
	}
	return pts
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v float64) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
