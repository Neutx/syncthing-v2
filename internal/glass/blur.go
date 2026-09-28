package glass

import (
	"image"
	"math"
)

// BlurAndSaturate returns a frosted copy of src: it is downsampled to half
// resolution, box-blurred twice with radius max(1, radius/2) (two passes
// approximate a Gaussian), saturated by the given factor, and upsampled back
// to the size of src with bilinear filtering. src must have its origin at
// (0, 0).
//
// Half resolution, not quarter: a quarter-size copy loses the backdrop
// structure that makes refraction legible and leaves a flat wash.
func BlurAndSaturate(src *image.RGBA, radius int, saturation float32) *image.RGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	small := downsample2(src)
	r := max(1, radius/2)
	boxBlur(small, r)
	boxBlur(small, r)
	if abs32(saturation-1) > 0.001 {
		saturate(small, saturation)
	}
	return upsample(small, w, h)
}

// downsample2 halves src with a 2×2 box filter (rounded average).
func downsample2(src *image.RGBA) *image.RGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := max(1, w/2), max(1, h/2)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := min(2*y, h-1)
		y1 := min(2*y+1, h-1)
		r0 := src.Pix[y0*src.Stride:]
		r1 := src.Pix[y1*src.Stride:]
		o := dst.Pix[y*dst.Stride:]
		for x := 0; x < dw; x++ {
			x0 := min(2*x, w-1) * 4
			x1 := min(2*x+1, w-1) * 4
			for c := 0; c < 4; c++ {
				s := uint32(r0[x0+c]) + uint32(r0[x1+c]) + uint32(r1[x0+c]) + uint32(r1[x1+c])
				o[x*4+c] = uint8((s + 2) / 4)
			}
		}
	}
	return dst
}

// boxBlur applies a separable box blur of radius r in place, with the edge
// pixels repeated (clamp-to-edge), exactly as the prototype's running sums.
func boxBlur(img *image.RGBA, r int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	stride := img.Stride
	p := img.Pix
	tmp := make([]uint8, len(p))
	n := int32(2*r + 1)
	clamp := func(v, hi int) int {
		if v < 0 {
			return 0
		}
		if v > hi {
			return hi
		}
		return v
	}

	// Horizontal pass: p -> tmp.
	for y := 0; y < h; y++ {
		row := p[y*stride : y*stride+w*4]
		trow := tmp[y*stride : y*stride+w*4]
		var s [4]int32
		for x := -r; x <= r; x++ {
			q := clamp(x, w-1) * 4
			for c := 0; c < 4; c++ {
				s[c] += int32(row[q+c])
			}
		}
		for x := 0; x < w; x++ {
			for c := 0; c < 4; c++ {
				trow[x*4+c] = uint8(s[c] / n)
			}
			a := clamp(x-r, w-1) * 4
			b := clamp(x+r+1, w-1) * 4
			for c := 0; c < 4; c++ {
				s[c] += int32(row[b+c]) - int32(row[a+c])
			}
		}
	}

	// Vertical pass: tmp -> p.
	for x := 0; x < w; x++ {
		var s [4]int32
		xo := x * 4
		for y := -r; y <= r; y++ {
			q := clamp(y, h-1)*stride + xo
			for c := 0; c < 4; c++ {
				s[c] += int32(tmp[q+c])
			}
		}
		for y := 0; y < h; y++ {
			o := y*stride + xo
			for c := 0; c < 4; c++ {
				p[o+c] = uint8(s[c] / n)
			}
			a := clamp(y-r, h-1)*stride + xo
			b := clamp(y+r+1, h-1)*stride + xo
			for c := 0; c < 4; c++ {
				s[c] += int32(tmp[b+c]) - int32(tmp[a+c])
			}
		}
	}
}

// saturate scales each pixel's chroma around its Rec. 709 luminance.
func saturate(img *image.RGBA, s float32) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < len(row); x += 4 {
			r, g, b := float32(row[x]), float32(row[x+1]), float32(row[x+2])
			lum := 0.2126*r + 0.7152*g + 0.0722*b
			row[x] = clampByte(lum + (r-lum)*s)
			row[x+1] = clampByte(lum + (g-lum)*s)
			row[x+2] = clampByte(lum + (b-lum)*s)
		}
	}
}

// upsample scales src to w×h with bilinear filtering, aligning pixel centres.
func upsample(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	type tap struct {
		i0, i1 int
		f      float32
	}
	taps := func(n, sn int) []tap {
		t := make([]tap, n)
		scale := float64(sn) / float64(n)
		for i := range t {
			s := (float64(i)+0.5)*scale - 0.5
			s = math.Max(0, math.Min(s, float64(sn-1)))
			i0 := int(s)
			i1 := min(i0+1, sn-1)
			t[i] = tap{i0, i1, float32(s - float64(i0))}
		}
		return t
	}
	xt, yt := taps(w, sw), taps(h, sh)

	for y := 0; y < h; y++ {
		ty := yt[y]
		r0 := src.Pix[ty.i0*src.Stride:]
		r1 := src.Pix[ty.i1*src.Stride:]
		o := dst.Pix[y*dst.Stride:]
		for x := 0; x < w; x++ {
			tx := xt[x]
			a, b := tx.i0*4, tx.i1*4
			for c := 0; c < 4; c++ {
				top := float32(r0[a+c]) + (float32(r0[b+c])-float32(r0[a+c]))*tx.f
				bot := float32(r1[a+c]) + (float32(r1[b+c])-float32(r1[a+c]))*tx.f
				o[x*4+c] = uint8(top + (bot-top)*ty.f + 0.5)
			}
		}
	}
	return dst
}

// clampByte converts to a byte the way the prototype does: values are
// truncated toward zero and clamped to 0..255.
func clampByte(v float32) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}
