package glass

import (
	"image"
	"image/color"
	"math"
)

// Displacement is the static lens description for one (size, radius, bevel,
// strength, shift) combination. It depends only on geometry, so it is built
// once and cached (see Render).
type Displacement struct {
	W, H int
	// Dx and Dy are the refraction offsets in 1/16 px.
	Dx, Dy []int16
	// Edge is 0 at the flat centre and 255 at the extreme edge.
	Edge []uint8
	// Inside is the antialiased coverage: 255 inside the squircle, 0 outside.
	Inside []uint8
	// NormX and NormY encode the surface normal (128 = 0).
	NormX, NormY []uint8
}

// BuildDisplacement computes the height field of a glass slab with a
// superelliptic bevel profile and derives from it the surface normals and
// the refraction offsets. radius, bevel and maxShift are in pixels.
func BuildDisplacement(w, h int, radius, bevel, strength, maxShift float32) *Displacement {
	n := w * h
	d := &Displacement{
		W: w, H: h,
		Dx: make([]int16, n), Dy: make([]int16, n),
		Edge: make([]uint8, n), Inside: make([]uint8, n),
		NormX: make([]uint8, n), NormY: make([]uint8, n),
	}
	fw, fh := float32(w), float32(h)

	// Height field first (the thickness of the glass).
	height := make([]float32, n)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			sd := SquircleSDF(float32(x)+0.5, float32(y)+0.5, fw, fh, radius)

			cov := 0.5 - sd // 1 px feather
			cov = min(max(cov, 0), 1)
			d.Inside[i] = uint8(cov * 255)

			if sd >= 0 {
				d.Edge[i] = 255
				continue
			}
			// t: 0 at the rim, 1 at bevel depth and beyond.
			t := min(-sd/bevel, 1)
			// Superelliptic bevel profile: (1 - (1-t)^p)^(1/p).
			e := 1 - t
			height[i] = float32(math.Pow(math.Max(0, 1-math.Pow(float64(e), bevelP)), 1/bevelP))
			d.Edge[i] = uint8(min(max(e, 0), 1) * 255) // 1 at the rim -> 0 at the centre
		}
	}

	// Normals and refraction offsets from the height gradient.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if d.Inside[i] == 0 {
				d.NormX[i], d.NormY[i] = 128, 128
				continue
			}
			xm, xp, ym, yp := i, i, i, i
			if x > 0 {
				xm = i - 1
			}
			if x < w-1 {
				xp = i + 1
			}
			if y > 0 {
				ym = i - w
			}
			if y < h-1 {
				yp = i + w
			}
			gx := (height[xp] - height[xm]) * 0.5
			gy := (height[yp] - height[ym]) * 0.5

			nx, ny := -gx, -gy
			nl := float32(math.Sqrt(float64(nx*nx + ny*ny + 0.0025)))
			d.NormX[i] = uint8(min(max(int(128+nx/nl*127), 0), 255))
			d.NormY[i] = uint8(min(max(int(128+ny/nl*127), 0), 255))

			// Refraction: the direction comes from the gradient, the magnitude
			// from an explicit rim profile. A magnitude derived from the
			// gradient alone gives only a couple of pixels of shift, which reads
			// as a blur, not as glass.
			gl := float32(math.Sqrt(float64(gx*gx + gy*gy)))
			if gl < 1e-6 {
				continue
			}
			e := float32(d.Edge[i]) / 255 // 1 at the rim -> 0 at bevel depth
			// Snell-like falloff: strongest right at the rim, quick decay inward.
			mag := maxShift * strength * (1 - 1/IOR) * 3 * float32(math.Pow(float64(e), 1.55))
			d.Dx[i] = toFixed16(gx / gl * mag)
			d.Dy[i] = toFixed16(gy / gl * mag)
		}
	}
	return d
}

// bevelP is the exponent of the bevel profile. It is a float32 constant in
// the prototype, widened to float64 for Math.Pow.
var bevelP = float64(float32(2.4))

func toFixed16(v float32) int16 {
	return int16(min(max(int(v*16), math.MinInt16), math.MaxInt16))
}

// RefractParams are the material and lighting inputs of Refract.
type RefractParams struct {
	// LensAmount (0..1) ramps the lensing, tint and darkening, so the
	// material can "materialise"; Render always uses 1.
	LensAmount float32
	// Tint is the base wash colour and TintAlpha its opacity.
	Tint      color.RGBA
	TintAlpha float32
	// LightX and LightY give the direction of the virtual key light.
	LightX, LightY float32
	// Spec and Rim scale the specular highlight and the Fresnel rim.
	Spec, Rim float32
	// Darken multiplies the colour so white text stays legible.
	Darken float32
}

// Per-edge-byte powers used by the dispersion, sharp cross-fade and rim terms.
var (
	edgePow11, edgePow025, edgePow22 [256]float32
)

func init() {
	for i := range 256 {
		e := float64(i) / 255
		edgePow11[i] = float32(math.Pow(e, 1.1))
		edgePow025[i] = float32(math.Pow(e, 0.25))
		edgePow22[i] = float32(math.Pow(e, 2.2))
	}
}

// Refract composites the lens: it refracts blurred through d with per-channel
// chromatic dispersion (the shift grows strictly R < G < B), cross-fades to
// sharp across the bevel so the rim shows structure being bent, then applies
// the cool glass tint, the tint wash, darkening, the directional Fresnel rim,
// the Blinn-Phong specular and the bevel shading. blurred and sharp (which may
// be nil) must be d.W×d.H with their origin at (0, 0). The result carries
// the squircle coverage in its alpha channel (premultiplied, as image.RGBA
// requires).
func Refract(blurred *image.RGBA, d *Displacement, p RefractParams, sharp *image.RGBA) *image.RGBA {
	w, h := d.W, d.H
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sp, sStride := blurred.Pix, blurred.Stride
	sw, sh := blurred.Rect.Dx(), blurred.Rect.Dy()
	var hp []uint8
	hStride := 0
	if sharp != nil {
		hp, hStride = sharp.Pix, sharp.Stride
	}

	lens := p.LensAmount
	disp := float32(Dispersion) * lens
	tr, tg, tb := float32(p.Tint.R), float32(p.Tint.G), float32(p.Tint.B)
	// The glass must thicken as it materialises, not just fade a border up:
	// tint and darkening ramp with the lens amount so the slab gains substance.
	tA := p.TintAlpha * (0.35 + 0.65*lens)
	dk := 1 - (1-p.Darken)*lens

	for y := 0; y < h; y++ {
		orow := out.Pix[y*out.Stride : y*out.Stride+w*4]
		fy := float32(y)
		for x := 0; x < w; x++ {
			i := y*w + x
			inside := d.Inside[i]
			if inside == 0 {
				continue // already transparent
			}
			fx := float32(x)
			ox := float32(d.Dx[i]) / 16 * lens
			oy := float32(d.Dy[i]) / 16 * lens
			eb := d.Edge[i]
			edge := float32(eb) / 255

			// Chromatic dispersion: shorter wavelengths refract more, so the
			// per-channel shift is scaled strictly monotonically R < G < B.
			k := edgePow11[eb] * disp * 0.01
			rS, bS := 1-k, 1+k
			rr := sample(sp, sStride, sw, sh, fx+ox*rS, fy+oy*rS, 0)
			gg := sample(sp, sStride, sw, sh, fx+ox, fy+oy, 1)
			bb := sample(sp, sStride, sw, sh, fx+ox*bS, fy+oy*bS, 2)

			// Cross-fade to the sharp capture across the bevel so the lensed
			// rim shows structure being bent, not a smooth wash.
			if hp != nil && edge > 0.001 {
				m := edgePow025[eb] * lens
				hr := sample(hp, hStride, sw, sh, fx+ox*rS, fy+oy*rS, 0)
				hg := sample(hp, hStride, sw, sh, fx+ox, fy+oy, 1)
				hb := sample(hp, hStride, sw, sh, fx+ox*bS, fy+oy*bS, 2)
				rr = int(float32(rr) + float32(hr-rr)*m)
				gg = int(float32(gg) + float32(hg-gg)*m)
				bb = int(float32(bb) + float32(hb-bb)*m)
			}

			fr, fg, fb := float32(rr), float32(gg), float32(bb)
			// Cool glass tint (blue shift), strongest where thick.
			fr *= 0.955
			fg *= 0.985
			fb *= 1.045
			// Base tint wash for legibility.
			fr += (tr - fr) * tA
			fg += (tg - fg) * tA
			fb += (tb - fb) * tA
			// Darken overall so white text reads.
			fr *= dk
			fg *= dk
			fb *= dk

			// Lighting.
			nx := float32(int(d.NormX[i])-128) / 127
			ny := float32(int(d.NormY[i])-128) / 127
			s2 := nx*nx + ny*ny
			// Fresnel (Schlick): grazing angles reflect more. slope^4 = (slope²)².
			fres := 0.04 + 0.96*s2*s2
			// Blinn-Phong specular against the key light, gated by n·l so the
			// unlit rim stays dark.
			ndl := nx*p.LightX + ny*p.LightY
			nl := max(ndl, 0)
			nl2 := nl * nl
			nl4 := nl2 * nl2
			spec := nl4 * nl4 * nl // nl^9
			wrap := nl2 * nl       // nl^3
			// The Fresnel rim stays directional, or it becomes the uniform
			// outline of a flat overlay.
			add := spec*p.Spec*255*lens + fres*p.Rim*120*edgePow22[eb]*wrap*lens
			// Bevel shading: the inner wall of a thick slab catches light on the
			// lit side and falls into shadow on the unlit side across the whole
			// bevel.
			bevelRamp := edge * (1 - edge) * 4 // peaks mid-bevel
			add += bevelRamp * ndl * 26 * lens

			fr += add
			fg += add
			fb += add * 1.03

			o := orow[x*4 : x*4+4 : x*4+4]
			r8, g8, b8 := clampByte(fr), clampByte(fg), clampByte(fb)
			if inside != 255 { // premultiply the antialiased edge
				a := uint32(inside)
				r8 = uint8((uint32(r8)*a + 127) / 255)
				g8 = uint8((uint32(g8)*a + 127) / 255)
				b8 = uint8((uint32(b8)*a + 127) / 255)
			}
			o[0], o[1], o[2], o[3] = r8, g8, b8, inside
		}
	}
	return out
}

// sample returns channel ch (0 = R, 1 = G, 2 = B) of p at (fx, fy) with
// bilinear filtering and clamping to the image, truncated like the prototype.
func sample(p []uint8, stride, w, h int, fx, fy float32, ch int) int {
	fx = max(fx, 0)
	fy = max(fy, 0)
	fx = min(fx, float32(w)-1.001)
	fy = min(fy, float32(h)-1.001)
	if fx < 0 { // images narrower than 2 px
		fx = 0
	}
	if fy < 0 {
		fy = 0
	}
	x0, y0 := int(fx), int(fy)
	x1, y1 := x0, y0
	if x0+1 < w {
		x1 = x0 + 1
	}
	if y0+1 < h {
		y1 = y0 + 1
	}
	tx, ty := fx-float32(x0), fy-float32(y0)
	r0 := y0 * stride
	r1 := y1 * stride
	a := float32(p[r0+x0*4+ch])
	b := float32(p[r0+x1*4+ch])
	c := float32(p[r1+x0*4+ch])
	dd := float32(p[r1+x1*4+ch])
	top := a + (b-a)*tx
	bot := c + (dd-c)*tx
	return int(top + (bot-top)*ty)
}
