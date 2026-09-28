package glass

import (
	"bytes"
	"errors"
	"flag"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden images in testdata/")

// synthetic returns a deterministic test backdrop with the kind of structure
// refraction needs to be visible: colour gradients, a checkerboard and thin
// diagonal stripes. The bright variant has a mean luminance above 0.5.
func synthetic(w, h int, bright bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8(x * 255 / max(1, w-1))
			g := uint8(y * 255 / max(1, h-1))
			b := uint8(30)
			if (x/4+y/4)%2 == 0 {
				b = 200
			}
			if (x+y)%16 < 3 {
				r, g, b = 250, 250, 250
			}
			if bright {
				r, g, b = 128+r/2, 128+g/2, 128+b/2
			} else {
				r, g, b = r/2, g/2, b/2
			}
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, g, b, 255
		}
	}
	return img
}

func TestSquircleSDF(t *testing.T) {
	const w, h, r = 200, 300, 40
	cases := []struct {
		name string
		x, y float32
		want float32
	}{
		{"centre", 100, 150, -100},
		{"top edge midpoint", 100, 0, 0},
		{"left edge midpoint", 0, 150, 0},
		{"inset by 10 on the right", 190, 150, -10},
		{"outside the corner", 0, 0, float32(40*math.Pow(2, 0.25) - 40)},
	}
	for _, c := range cases {
		got := SquircleSDF(c.x, c.y, w, h, r)
		if math.Abs(float64(got-c.want)) > 1e-3 {
			t.Errorf("%s: SquircleSDF(%v, %v) = %v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}
	// A point on the superellipse diagonal lies on the edge.
	k := float32(r / math.Pow(2, 0.25))
	x := float32(r) - k
	if got := SquircleSDF(x, x, w, h, r); math.Abs(float64(got)) > 1e-3 {
		t.Errorf("diagonal corner point: SDF = %v, want 0", got)
	}
}

func TestSquirclePath(t *testing.T) {
	const x, y, w, h, r = 10.0, 20.0, 460.0, 640.0, 40.0
	pts := SquirclePath(x, y, w, h, r)
	if len(pts) < 4*7 {
		t.Fatalf("path has only %d points", len(pts))
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	var area float64
	for i, p := range pts {
		minX, minY = math.Min(minX, p.X), math.Min(minY, p.Y)
		maxX, maxY = math.Max(maxX, p.X), math.Max(maxY, p.Y)
		// Every vertex lies on the zero set of the SDF the lighting uses.
		if d := SquircleSDF(float32(p.X-x), float32(p.Y-y), w, h, r); math.Abs(float64(d)) > 0.01 {
			t.Fatalf("vertex %d %v is %v px off the SDF edge", i, p, d)
		}
		q := pts[(i+1)%len(pts)]
		area += p.X*q.Y - q.X*p.Y
	}
	if minX != x || minY != y || math.Abs(maxX-(x+w)) > 1e-9 || math.Abs(maxY-(y+h)) > 1e-9 {
		t.Errorf("bounds = (%v,%v)-(%v,%v), want (%v,%v)-(%v,%v)", minX, minY, maxX, maxY, x, y, x+w, y+h)
	}
	area /= 2
	if area <= 0 {
		t.Fatalf("path is not clockwise in screen coordinates (signed area %v)", area)
	}
	// Quarter area of an n=4 superellipse is Γ(1.25)²/Γ(1.5)·r².
	g125, _ := math.Lgamma(1.25)
	g15, _ := math.Lgamma(1.5)
	quarter := math.Exp(2*g125-g15) * r * r
	want := w*h - 4*(r*r-quarter)
	if math.Abs(area-want)/want > 0.001 {
		t.Errorf("area = %v, want about %v", area, want)
	}

	// The radius is clamped to half the shorter side; tiny radii give a rectangle.
	if p := SquirclePath(0, 0, 20, 100, 40); len(p) == 4 {
		t.Error("clamped radius should still trace a curve")
	}
	if p := SquirclePath(0, 0, 20, 20, 0.4); len(p) != 4 {
		t.Errorf("radius 0.4 gave %d points, want a 4-point rectangle", len(p))
	}
}

func TestDisplacement(t *testing.T) {
	const w, h = 460, 640
	d := BuildDisplacement(w, h, Radius, Bevel, Strength, MaxShift)
	at := func(x, y int) int { return y*w + x }

	if d.Inside[at(0, 0)] != 0 {
		t.Errorf("corner pixel coverage = %d, want 0", d.Inside[at(0, 0)])
	}
	c := at(w/2, h/2)
	if d.Inside[c] != 255 || d.Edge[c] != 0 || d.Dx[c] != 0 || d.Dy[c] != 0 || d.NormX[c] != 128 {
		t.Errorf("centre: inside=%d edge=%d dx=%d dy=%d nx=%d; want a flat, fully covered pixel",
			d.Inside[c], d.Edge[c], d.Dx[c], d.Dy[c], d.NormX[c])
	}
	// The rim pulls the backdrop inward from every side.
	if v := d.Dx[at(1, h/2)]; v <= 0 {
		t.Errorf("left rim Dx = %d, want > 0", v)
	}
	if v := d.Dx[at(w-2, h/2)]; v >= 0 {
		t.Errorf("right rim Dx = %d, want < 0", v)
	}
	if v := d.Dy[at(w/2, 1)]; v <= 0 {
		t.Errorf("top rim Dy = %d, want > 0", v)
	}
	if v := d.Dy[at(w/2, h-2)]; v >= 0 {
		t.Errorf("bottom rim Dy = %d, want < 0", v)
	}
	// The peak shift is MaxShift·(1-1/IOR)·3 = 30 px and decays inward.
	peak := 0.0
	for i := range d.Dx {
		peak = math.Max(peak, math.Hypot(float64(d.Dx[i]), float64(d.Dy[i]))/16)
	}
	if peak > 30.01 || peak < 25 {
		t.Errorf("peak shift = %.2f px, want (25, 30]", peak)
	}
	rim, mid := d.Dx[at(1, h/2)], d.Dx[at(15, h/2)]
	if !(rim > mid && mid > 0) || d.Dx[at(int(Bevel)+2, h/2)] != 0 {
		t.Errorf("shift should decay across the bevel: rim %d, mid %d, past bevel %d", rim, mid, d.Dx[at(int(Bevel)+2, h/2)])
	}
	// The upper-left rim faces the light: its normal points up-left.
	ul := at(1, h/2)
	if d.NormX[ul] >= 128 {
		t.Errorf("left rim normal x = %d, want < 128", d.NormX[ul])
	}
}

func TestDispersionOrder(t *testing.T) {
	// A horizontal ramp with equal channels: where the lens pulls the
	// backdrop from further right, a larger shift samples a brighter value, so
	// at the left rim the channels must come out ordered R < G < B (and the
	// reverse on a falling ramp), which proves the per-channel shift is
	// strictly monotonic.
	const w, h = 200, 200
	d := BuildDisplacement(w, h, Radius, Bevel, Strength, MaxShift)
	p := RefractParams{LensAmount: 1, Tint: TintColor, TintAlpha: 0, LightX: 0, LightY: 0, Spec: 0, Rim: 0, Darken: 1}
	for _, rising := range []bool{true, false} {
		src := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := uint8(min(255, 20+x*4))
				if !rising {
					v = uint8(max(0, 235-x*4))
				}
				o := src.PixOffset(x, y)
				src.Pix[o], src.Pix[o+1], src.Pix[o+2], src.Pix[o+3] = v, v, v, 255
			}
		}
		out := Refract(src, d, p, nil)
		o := out.PixOffset(2, h/2)
		r, g, b := out.Pix[o], out.Pix[o+1], out.Pix[o+2]
		if rising && !(r < g && g < b) {
			t.Errorf("rising ramp at the left rim: R=%d G=%d B=%d, want R < G < B", r, g, b)
		}
		if !rising && !(r > g && g > b) {
			t.Errorf("falling ramp at the left rim: R=%d G=%d B=%d, want R > G > B", r, g, b)
		}
		// With the lens off the pixel is the source value, only cool-tinted.
		flat := Refract(src, d, RefractParams{LensAmount: 0, Darken: 1}, nil)
		o = flat.PixOffset(w/2, h/2)
		sv := float32(src.Pix[src.PixOffset(w/2, h/2)])
		if g := flat.Pix[o+1]; g != clampByte(sv*0.985) {
			t.Errorf("lens amount 0: G = %d, want %d", g, clampByte(sv*0.985))
		}
	}
}

func TestBlurAndSaturate(t *testing.T) {
	// A uniform grey stays that grey through blur, saturation and resampling.
	img := image.NewRGBA(image.Rect(0, 0, 37, 23))
	for i := range img.Pix {
		img.Pix[i] = 131
	}
	out := BlurAndSaturate(img, BlurRadius, Saturation)
	if out.Rect != img.Rect {
		t.Fatalf("size = %v, want %v", out.Rect, img.Rect)
	}
	for i, v := range out.Pix {
		if v != 131 {
			t.Fatalf("byte %d = %d, want 131", i, v)
		}
	}

	// The box blur spreads one bright pixel symmetrically and preserves the
	// window size 2r+1.
	small := image.NewRGBA(image.Rect(0, 0, 11, 11))
	small.Pix[small.PixOffset(5, 5)] = 250
	boxBlur(small, 2)
	want := uint8(250 / 5 / 5)
	for _, pt := range []image.Point{{3, 3}, {7, 7}, {3, 7}, {7, 3}, {5, 5}} {
		if v := small.Pix[small.PixOffset(pt.X, pt.Y)]; v != want {
			t.Errorf("blurred value at %v = %d, want %d", pt, v, want)
		}
	}
	if v := small.Pix[small.PixOffset(2, 5)]; v != 0 {
		t.Errorf("value outside the window = %d, want 0", v)
	}

	// Saturation 2 doubles the distance from luminance.
	px := image.NewRGBA(image.Rect(0, 0, 1, 1))
	copy(px.Pix, []uint8{150, 100, 100, 255})
	saturate(px, 2)
	lum := float32(0.2126*150 + 0.7152*100 + 0.0722*100)
	if px.Pix[0] != clampByte(lum+(150-lum)*2) || px.Pix[1] != clampByte(lum+(100-lum)*2) {
		t.Errorf("saturate = %v", px.Pix[:3])
	}
}

func TestMeanLuminance(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:], []uint8{255, 255, 255, 255})
	}
	if l := MeanLuminance(img); math.Abs(float64(l)-1) > 1e-4 {
		t.Errorf("white: %v, want 1", l)
	}
	if l := MeanLuminance(image.NewRGBA(image.Rect(0, 0, 10, 10))); l != 0 {
		t.Errorf("black: %v, want 0", l)
	}
	if l := MeanLuminance(image.NewRGBA(image.Rectangle{})); l != 0.5 {
		t.Errorf("empty: %v, want 0.5", l)
	}
	if MeanLuminance(synthetic(64, 64, true)) <= 0.5 || MeanLuminance(synthetic(64, 64, false)) >= 0.5 {
		t.Error("synthetic inputs must fall on either side of the 0.5 tint threshold")
	}
}

func TestRenderAdaptiveMaterial(t *testing.T) {
	for _, bright := range []bool{false, true} {
		src := synthetic(96, 128, bright)
		got := Render(src, 1)
		p := RefractParams{LensAmount: 1, Tint: TintColor, TintAlpha: 0.12, LightX: -0.62, LightY: -0.78,
			Spec: 0.90, Rim: 0.40, Darken: 0.78}
		if bright {
			p.TintAlpha, p.Darken = 0.20, 0.62
		}
		d := BuildDisplacement(96, 128, 40, 30, 1, 30)
		want := Refract(BlurAndSaturate(src, 5, 2), d, p, src)
		if !bytes.Equal(got.Pix, want.Pix) {
			t.Errorf("bright=%v: Render differs from the explicit pipeline", bright)
		}
	}
}

func TestRenderScaleAndShape(t *testing.T) {
	src := synthetic(575, 800, false)
	got := Render(src, 1.25)
	d := BuildDisplacement(575, 800, 50, 37.5, 1, 37.5)
	want := Refract(BlurAndSaturate(src, 6, 2), d, RefractParams{LensAmount: 1, Tint: TintColor, TintAlpha: 0.12,
		LightX: -0.62, LightY: -0.78, Spec: 0.90, Rim: 0.40, Darken: 0.78}, src)
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Fatal("scale 1.25 must scale radius, bevel, shift and blur radius")
	}
	for _, pt := range []image.Point{{0, 0}, {574, 0}, {0, 799}, {574, 799}} {
		if a := got.Pix[got.PixOffset(pt.X, pt.Y)+3]; a != 0 {
			t.Errorf("corner %v alpha = %d, want 0", pt, a)
		}
	}
	if a := got.Pix[got.PixOffset(287, 400)+3]; a != 255 {
		t.Errorf("centre alpha = %d, want 255", a)
	}
	// image.RGBA is premultiplied: no colour channel may exceed alpha.
	for i := 0; i < len(got.Pix); i += 4 {
		a := got.Pix[i+3]
		if got.Pix[i] > a || got.Pix[i+1] > a || got.Pix[i+2] > a {
			t.Fatalf("pixel %d is not premultiplied: %v", i/4, got.Pix[i:i+4])
		}
	}

	// Non-zero origins and degenerate inputs are handled.
	sub := synthetic(80, 80, false).SubImage(image.Rect(8, 8, 72, 72)).(*image.RGBA)
	if r := Render(sub, 1); r.Rect != image.Rect(0, 0, 64, 64) {
		t.Errorf("sub-image render has bounds %v", r.Rect)
	}
	if r := Render(nil, 1); !r.Rect.Empty() {
		t.Error("nil source should give an empty image")
	}
	if r := Render(synthetic(3, 2, false), math.NaN()); r.Rect.Dx() != 3 {
		t.Error("tiny source with an invalid scale should still render")
	}
}

func TestRenderGolden(t *testing.T) {
	for _, g := range []struct {
		name   string
		bright bool
		scale  float64
	}{
		{"render_dark_64.png", false, 1},
		{"render_bright_64.png", true, 1},
		{"render_dark_64_scale050.png", false, 0.5},
	} {
		got := Render(synthetic(64, 64, g.bright), g.scale)
		path := filepath.Join("testdata", g.name)
		if *update {
			b, err := PNG(got)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		want, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if want.Bounds() != got.Rect {
			t.Fatalf("%s: bounds %v, want %v", g.name, got.Rect, want.Bounds())
		}
		bad := 0
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				wr, wg, wb, wa := want.At(x, y).RGBA()
				o := got.PixOffset(x, y)
				gv := got.Pix[o : o+4]
				for c, wv := range []uint32{wr >> 8, wg >> 8, wb >> 8, wa >> 8} {
					if d := int(gv[c]) - int(wv); d > 2 || d < -2 {
						if bad < 5 {
							t.Errorf("%s: pixel (%d,%d) channel %d = %d, want %d±2", g.name, x, y, c, gv[c], wv)
						}
						bad++
					}
				}
			}
		}
		if bad > 0 {
			t.Errorf("%s: %d channel values outside ±2", g.name, bad)
		}
	}
}

func TestPNG(t *testing.T) {
	img := Render(synthetic(32, 32, false), 1)
	b, err := PNG(img)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Bounds() != img.Rect {
		t.Errorf("decoded bounds %v, want %v", dec.Bounds(), img.Rect)
	}
}

func TestCapture(t *testing.T) {
	if _, err := Capture(image.Rect(0, 0, 0, 10)); err == nil {
		t.Error("an empty rectangle must be rejected")
	}
	img, err := Capture(image.Rect(0, 0, 8, 6))
	if runtime.GOOS != "windows" {
		if !errors.Is(err, errors.ErrUnsupported) || img != nil {
			t.Fatalf("Capture on %s = %v, %v; want ErrUnsupported", runtime.GOOS, img, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if img.Rect != image.Rect(0, 0, 8, 6) {
		t.Errorf("capture bounds %v", img.Rect)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 255 {
			t.Fatal("captured pixels must be opaque")
		}
	}
}

func BenchmarkRender(b *testing.B) {
	src := synthetic(460, 640, false)
	Render(src, 1) // the displacement map is cached, as in the running app
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Render(src, 1)
	}
}
