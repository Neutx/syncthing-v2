package icon

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

var update = flag.Bool("update", false, "rewrite the golden PNGs in testdata")

// tolerance allows for last-bit differences between architectures (for
// example fused multiply-add on arm64) in the rasteriser's float maths.
const tolerance = 2

var snapshots = []struct {
	name  string
	state model.State
	pct   int
	size  int
}{
	{"insync-32", model.StateInSync, 100, 32},
	{"syncing-0-32", model.StateSyncing, 0, 32},
	{"syncing-25-32", model.StateSyncing, 25, 32},
	{"syncing-60-32", model.StateSyncing, 60, 32},
	{"syncing-100-32", model.StateSyncing, 100, 32},
	{"scanning-32", model.StateScanning, 0, 32},
	{"paused-32", model.StatePaused, 0, 32},
	{"nopeer-32", model.StateNoPeer, 0, 32},
	{"error-32", model.StateError, 0, 32},
	{"down-32", model.StateDown, 0, 32},
	{"unauthorized-32", model.StateUnauthorized, 0, 32},
	{"insync-16", model.StateInSync, 100, 16},
	{"syncing-40-20", model.StateSyncing, 40, 20},
	{"nopeer-24", model.StateNoPeer, 0, 24},
}

func TestRingSnapshots(t *testing.T) {
	for _, tc := range snapshots {
		t.Run(tc.name, func(t *testing.T) {
			got := Ring(tc.state, tc.pct, tc.size)
			path := filepath.Join("testdata", "ring-"+tc.name+".png")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, PNG(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			defer f.Close()
			want, err := png.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds() != want.Bounds() {
				t.Fatalf("bounds %v, golden %v", got.Bounds(), want.Bounds())
			}
			for y := 0; y < tc.size; y++ {
				for x := 0; x < tc.size; x++ {
					g := got.RGBAAt(x, y)
					w := color.RGBAModel.Convert(want.At(x, y)).(color.RGBA)
					if diff(g.R, w.R) > tolerance || diff(g.G, w.G) > tolerance || diff(g.B, w.B) > tolerance || diff(g.A, w.A) > tolerance {
						t.Fatalf("pixel (%d,%d) = %v, golden %v", x, y, g, w)
					}
				}
			}
		})
	}
}

func diff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// Semantic checks that do not depend on the goldens. Coordinates are pixel
// centres in the 32 px icon.
func TestRingGeometry(t *testing.T) {
	alpha := func(img *image.RGBA, x, y int) uint8 { return img.RGBAAt(x, y).A }
	opaque := func(t *testing.T, img *image.RGBA, x, y int) {
		t.Helper()
		if a := alpha(img, x, y); a < 240 {
			t.Errorf("pixel (%d,%d) alpha %d, want opaque", x, y, a)
		}
	}
	clear := func(t *testing.T, img *image.RGBA, x, y int) {
		t.Helper()
		if a := alpha(img, x, y); a != 0 {
			t.Errorf("pixel (%d,%d) alpha %d, want transparent", x, y, a)
		}
	}
	track := func(t *testing.T, img *image.RGBA, x, y int) {
		t.Helper()
		if a := alpha(img, x, y); a < trackAlpha-3 || a > trackAlpha+3 {
			t.Errorf("pixel (%d,%d) alpha %d, want track alpha %d", x, y, a, trackAlpha)
		}
	}

	// The ring band covers radii 10.5..15.5 around (16,16).
	ringPoints := [][2]int{{16, 2}, {29, 16}, {16, 29}, {2, 16}}
	corners := [][2]int{{0, 0}, {31, 0}, {0, 31}, {31, 31}}

	t.Run("full ring states", func(t *testing.T) {
		for _, s := range []model.State{model.StateInSync, model.StateScanning, model.StatePaused, model.StateNoPeer, model.StateError, model.StateDown, model.StateUnauthorized} {
			img := Ring(s, 0, 32)
			for _, p := range ringPoints {
				opaque(t, img, p[0], p[1])
			}
			for _, p := range corners {
				clear(t, img, p[0], p[1])
			}
			// Between the ring and the glyphs (radius about 8.5) nothing is drawn.
			clear(t, img, 16, 7)
		}
	})

	t.Run("dot states", func(t *testing.T) {
		for _, s := range []model.State{model.StateScanning, model.StatePaused, model.StateDown} {
			img := Ring(s, 0, 32)
			opaque(t, img, 15, 15)
			opaque(t, img, 16, 16)
			clear(t, img, 12, 12) // the dot has radius 3 only
		}
	})

	t.Run("x states", func(t *testing.T) {
		for _, s := range []model.State{model.StateNoPeer, model.StateError, model.StateUnauthorized} {
			img := Ring(s, 0, 32)
			opaque(t, img, 12, 12) // on both diagonals
			opaque(t, img, 19, 12)
			opaque(t, img, 12, 19)
			opaque(t, img, 19, 19)
			clear(t, img, 16, 11) // between the arms, above the crossing
		}
	})

	t.Run("tick", func(t *testing.T) {
		img := Ring(model.StateInSync, 100, 32)
		opaque(t, img, 14, 19) // the elbow
		opaque(t, img, 21, 12) // the long arm
		clear(t, img, 12, 12)
		clear(t, img, 16, 12) // above the elbow, inside the V
	})

	t.Run("syncing arc", func(t *testing.T) {
		img := Ring(model.StateSyncing, 25, 32)
		opaque(t, img, 16, 2)  // start at 12 o'clock
		opaque(t, img, 25, 6)  // about 1:30, inside the arc
		opaque(t, img, 29, 15) // 3 o'clock is the end (with a round cap)
		track(t, img, 16, 29)  // 6 o'clock is track only
		track(t, img, 2, 16)   // 9 o'clock is track only
		clear(t, img, 16, 16)  // no glyph while syncing

		zero := Ring(model.StateSyncing, 0, 32)
		opaque(t, zero, 16, 2) // the 2% minimum is visible
		track(t, zero, 29, 16)

		full := Ring(model.StateSyncing, 100, 32)
		for _, p := range ringPoints {
			opaque(t, full, p[0], p[1])
		}
		over := Ring(model.StateSyncing, 250, 32)
		if !bytes.Equal(over.Pix, full.Pix) {
			t.Error("pct above 100 must draw like 100")
		}
		under := Ring(model.StateSyncing, -5, 32)
		if !bytes.Equal(under.Pix, zero.Pix) {
			t.Error("negative pct must draw like the 2% minimum")
		}
	})

	t.Run("colours", func(t *testing.T) {
		want := map[model.State]color.NRGBA{
			model.StateInSync:       {0x2E, 0xBE, 0x64, 0xFF},
			model.StateSyncing:      {0x38, 0x96, 0xF0, 0xFF},
			model.StateScanning:     {0xF0, 0xB4, 0x28, 0xFF},
			model.StateNoPeer:       {0xE6, 0x50, 0x46, 0xFF},
			model.StateError:        {0xE6, 0x50, 0x46, 0xFF},
			model.StatePaused:       {0x96, 0x96, 0x9B, 0xFF},
			model.StateDown:         {0x6E, 0x6E, 0x73, 0xFF},
			model.StateUnauthorized: {0x6E, 0x6E, 0x73, 0xFF},
		}
		for s, c := range want {
			if got := StateColor(s); got != c {
				t.Errorf("StateColor(%d) = %v, want %v", s, got, c)
			}
			px := Ring(s, 100, 32).RGBAAt(16, 3)
			if diff(px.R, c.R) > 1 || diff(px.G, c.G) > 1 || diff(px.B, c.B) > 1 || px.A != 255 {
				t.Errorf("state %d ring pixel %v, want %v", s, px, c)
			}
		}
	})

	t.Run("sizes", func(t *testing.T) {
		for _, n := range []int{1, 16, 20, 24, 36, 64, 256} {
			if b := Ring(model.StateInSync, 0, n).Bounds(); b.Dx() != n || b.Dy() != n {
				t.Errorf("size %d gave %v", n, b)
			}
		}
		if b := Ring(model.StateInSync, 0, 0).Bounds(); b.Dx() != 1 {
			t.Errorf("size 0 gave %v, want 1x1", b)
		}
	})
}

func TestPNGRoundTrip(t *testing.T) {
	img := Ring(model.StateSyncing, 40, 48)
	data := PNG(img)
	if !bytes.Equal(data, PNG(img)) {
		t.Fatal("PNG output is not deterministic")
	}
	dec, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	assertSamePixels(t, img, dec, 0)
	if PNG(image.NewRGBA(image.Rect(0, 0, 0, 0))) != nil {
		t.Error("PNG of an empty image must be nil")
	}
}

func TestBGRA(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, color.RGBA{0x10, 0x20, 0x30, 0xFF})
	img.SetRGBA(1, 0, color.RGBA{0x40, 0x20, 0x00, 0x80}) // premultiplied
	got := BGRA(img)
	want := []byte{0x30, 0x20, 0x10, 0xFF, 0x00, 0x3F, 0x7F, 0x80} // 0x40/0x80 un-premultiplies to 0x7F
	if !bytes.Equal(got, want) {
		t.Fatalf("BGRA = % x, want % x", got, want)
	}
}

func TestICO(t *testing.T) {
	sizes := []int{16, 32, 48, 256}
	imgs := make([]*image.RGBA, len(sizes))
	for i, n := range sizes {
		imgs[i] = Ring(model.StateInSync, 100, n)
	}
	data := ICO(imgs...)
	if !bytes.Equal(data, ICO(imgs...)) {
		t.Fatal("ICO output is not deterministic")
	}
	u16 := func(off int) int { return int(binary.LittleEndian.Uint16(data[off:])) }
	u32 := func(off int) int { return int(binary.LittleEndian.Uint32(data[off:])) }
	if u16(0) != 0 || u16(2) != 1 || u16(4) != len(sizes) {
		t.Fatalf("bad ICONDIR % x", data[:6])
	}
	end := 6 + 16*len(sizes)
	for i, n := range sizes {
		e := 6 + 16*i
		w, h := int(data[e]), int(data[e+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		if w != n || h != n {
			t.Errorf("entry %d is %dx%d, want %d", i, w, h, n)
		}
		if u16(e+4) != 1 || u16(e+6) != 32 {
			t.Errorf("entry %d planes/bpp = %d/%d", i, u16(e+4), u16(e+6))
		}
		size, off := u32(e+8), u32(e+12)
		if off != end {
			t.Errorf("entry %d offset %d, want %d", i, off, end)
		}
		end = off + size
		payload := data[off : off+size]
		var dec image.Image
		if n >= 256 {
			var err error
			if dec, err = png.Decode(bytes.NewReader(payload)); err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
		} else {
			dec = decodeDIB(t, payload, n)
		}
		assertSamePixels(t, imgs[i], dec, 1)
	}
	if end != len(data) {
		t.Errorf("file has %d trailing bytes", len(data)-end)
	}

	for _, bad := range []*image.RGBA{image.NewRGBA(image.Rect(0, 0, 0, 0)), image.NewRGBA(image.Rect(0, 0, 512, 512))} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ICO accepted a %v image", bad.Bounds())
				}
			}()
			ICO(bad)
		}()
	}
}

// decodeDIB reads the bitmap form written by dib, checking the AND mask.
func decodeDIB(t *testing.T, b []byte, n int) image.Image {
	t.Helper()
	if binary.LittleEndian.Uint32(b) != 40 || int(int32(binary.LittleEndian.Uint32(b[4:]))) != n ||
		int(int32(binary.LittleEndian.Uint32(b[8:]))) != 2*n || binary.LittleEndian.Uint16(b[14:]) != 32 {
		t.Fatalf("bad BITMAPINFOHEADER % x", b[:16])
	}
	maskStride := ((n + 31) / 32) * 4
	if len(b) != 40+n*n*4+maskStride*n {
		t.Fatalf("DIB length %d, want %d", len(b), 40+n*n*4+maskStride*n)
	}
	px, mask := b[40:40+n*n*4], b[40+n*n*4:]
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		src := n - 1 - y // bottom-up
		for x := 0; x < n; x++ {
			p := px[(src*n+x)*4:]
			img.SetNRGBA(x, y, color.NRGBA{p[2], p[1], p[0], p[3]})
			masked := mask[src*maskStride+x/8]&(0x80>>(x%8)) != 0
			if masked != (p[3] == 0) {
				t.Fatalf("mask bit at (%d,%d) = %v for alpha %d", x, y, masked, p[3])
			}
		}
	}
	return img
}

// assertSamePixels compares in straight alpha, allowing tol per channel for
// the premultiplied round trip.
func assertSamePixels(t *testing.T, want *image.RGBA, got image.Image, tol int) {
	t.Helper()
	if got.Bounds().Size() != want.Bounds().Size() {
		t.Fatalf("size %v, want %v", got.Bounds().Size(), want.Bounds().Size())
	}
	for y := 0; y < want.Rect.Dy(); y++ {
		for x := 0; x < want.Rect.Dx(); x++ {
			w := color.NRGBAModel.Convert(want.At(x, y)).(color.NRGBA)
			g := color.NRGBAModel.Convert(got.At(got.Bounds().Min.X+x, got.Bounds().Min.Y+y)).(color.NRGBA)
			if w.A == 0 && g.A == 0 {
				continue
			}
			if diff(w.R, g.R) > tol || diff(w.G, g.G) > tol || diff(w.B, g.B) > tol || diff(w.A, g.A) > tol {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, g, w)
			}
		}
	}
}

func ExampleRing() {
	img := Ring(model.StateSyncing, 42, 24)
	fmt.Println(img.Bounds().Dx(), img.Bounds().Dy())
	// Output: 24 24
}
