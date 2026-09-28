package icon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// PNG encodes img as a PNG with best compression. The output is deterministic
// for a given image and toolchain. It returns nil for an empty image.
func PNG(img *image.RGBA) []byte {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// BGRA returns img as top-down rows of straight (non-premultiplied) BGRA
// pixels, the layout Windows expects for 32-bit icon bitmaps.
func BGRA(img *image.RGBA) []byte {
	b := img.Bounds()
	n := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(n, n.Bounds(), img, b.Min, draw.Src)
	out := make([]byte, 0, len(n.Pix))
	for y := 0; y < n.Rect.Dy(); y++ {
		row := n.Pix[y*n.Stride : y*n.Stride+4*n.Rect.Dx()]
		for x := 0; x < len(row); x += 4 {
			out = append(out, row[x+2], row[x+1], row[x], row[x+3])
		}
	}
	return out
}

// icoPNGMin is the smallest size stored as PNG inside an ICO; smaller images
// use the classic 32-bit DIB form, which every Windows component reads.
const icoPNGMin = 256

// ICO encodes one or more images as a Windows .ico file. Images of 256 px
// and more are stored as PNG, smaller ones as 32-bit BGRA bitmaps with an
// AND mask. It panics on an empty image or one larger than 256 px, which the
// format cannot describe; callers pass fixed, known sizes.
func ICO(imgs ...*image.RGBA) []byte {
	type entry struct {
		w, h int
		data []byte
	}
	entries := make([]entry, 0, len(imgs))
	for _, img := range imgs {
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		if w < 1 || h < 1 || w > 256 || h > 256 {
			panic(fmt.Sprintf("icon: ICO image size %dx%d is outside 1..256", w, h))
		}
		var data []byte
		if w >= icoPNGMin || h >= icoPNGMin {
			data = PNG(img)
		} else {
			data = dib(img)
		}
		entries = append(entries, entry{w, h, data})
	}

	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le(uint16(0))            // reserved
	le(uint16(1))            // type: icon
	le(uint16(len(entries))) // count
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		buf.WriteByte(byte(e.w & 0xFF)) // 256 is written as 0
		buf.WriteByte(byte(e.h & 0xFF))
		buf.WriteByte(0) // palette colours
		buf.WriteByte(0) // reserved
		le(uint16(1))    // planes
		le(uint16(32))   // bits per pixel
		le(uint32(len(e.data)))
		le(uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		buf.Write(e.data)
	}
	return buf.Bytes()
}

// dib returns the ICO bitmap form of img: a BITMAPINFOHEADER with doubled
// height, bottom-up BGRA rows, then a bottom-up 1-bit AND mask (1 where the
// pixel is fully transparent), rows padded to 32 bits.
func dib(img *image.RGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	px := BGRA(img)
	maskStride := ((w + 31) / 32) * 4
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le(uint32(40))                   // biSize
	le(int32(w))                     // biWidth
	le(int32(2 * h))                 // biHeight: colour + mask
	le(uint16(1))                    // biPlanes
	le(uint16(32))                   // biBitCount
	le(uint32(0))                    // biCompression: BI_RGB
	le(uint32(w*h*4 + maskStride*h)) // biSizeImage
	le(int32(0))                     // biXPelsPerMeter
	le(int32(0))                     // biYPelsPerMeter
	le(uint32(0))                    // biClrUsed
	le(uint32(0))                    // biClrImportant
	for y := h - 1; y >= 0; y-- {
		buf.Write(px[y*w*4 : (y+1)*w*4])
	}
	for y := h - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := 0; x < w; x++ {
			if px[(y*w+x)*4+3] == 0 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		buf.Write(row)
	}
	return buf.Bytes()
}
