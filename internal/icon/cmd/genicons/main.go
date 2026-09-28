// Command genicons writes the generated app icons: icon-<N>.png for every
// size in 16..1024 and a multi-size icon.ico. With -strip it also writes a
// PNG strip of every tray state, used by the documentation.
//
//	go run ./internal/icon/cmd/genicons -out assets/icons
//	go run ./internal/icon/cmd/genicons -out assets/icons -strip docs/tray-states.png
//
// The output is deterministic, so a second run leaves the tree unchanged.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"os"
	"path/filepath"

	"github.com/Neutx/syncthing-v2/internal/icon"
	"github.com/Neutx/syncthing-v2/internal/model"
)

// pngSizes covers Windows, the macOS iconset (16..1024) and the Linux hicolor
// theme (16, 22, 24, 32, 48, 64, 128, 256, 512).
var pngSizes = []int{16, 20, 22, 24, 32, 40, 48, 64, 128, 256, 512, 1024}

// icoSizes are the sizes Windows Explorer and the shell ask for.
var icoSizes = []int{16, 20, 24, 32, 40, 48, 64, 256}

// stripStates is the order of states in the documentation strip.
var stripStates = []struct {
	s   model.State
	pct int
}{
	{model.StateInSync, 100},
	{model.StateSyncing, 40},
	{model.StateScanning, 0},
	{model.StatePaused, 0},
	{model.StateNoPeer, 0},
	{model.StateError, 0},
	{model.StateUnauthorized, 0},
	{model.StateDown, 0},
}

func main() {
	out := flag.String("out", "assets/icons", "output directory for icon-<N>.png and icon.ico")
	strip := flag.String("strip", "", "also write a PNG strip of every tray state to this file")
	stripSize := flag.Int("strip-size", 64, "icon size in the state strip, in pixels")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "genicons: unexpected arguments: %v\n", flag.Args())
		os.Exit(2)
	}
	if err := run(*out, *strip, *stripSize); err != nil {
		fmt.Fprintln(os.Stderr, "genicons:", err)
		os.Exit(1)
	}
}

func run(out, strip string, stripSize int) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, n := range pngSizes {
		if err := writeIfChanged(filepath.Join(out, fmt.Sprintf("icon-%d.png", n)), icon.PNG(icon.Ring(icon.AppState, 100, n))); err != nil {
			return err
		}
	}
	imgs := make([]*image.RGBA, 0, len(icoSizes))
	for _, n := range icoSizes {
		imgs = append(imgs, icon.Ring(icon.AppState, 100, n))
	}
	if err := writeIfChanged(filepath.Join(out, "icon.ico"), icon.ICO(imgs...)); err != nil {
		return err
	}
	if strip != "" {
		if stripSize < 8 || stripSize > 1024 {
			return fmt.Errorf("-strip-size %d is outside 8..1024", stripSize)
		}
		if dir := filepath.Dir(strip); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := writeIfChanged(strip, icon.PNG(stateStrip(stripSize))); err != nil {
			return err
		}
	}
	return nil
}

// stateStrip lays the state icons out left to right with a gap of a quarter
// icon on a transparent background.
func stateStrip(size int) *image.RGBA {
	gap := size / 4
	w := len(stripStates)*size + (len(stripStates)-1)*gap
	dst := image.NewRGBA(image.Rect(0, 0, w, size))
	for i, st := range stripStates {
		x := i * (size + gap)
		draw.Draw(dst, image.Rect(x, 0, x+size, size), icon.Ring(st.s, st.pct, size), image.Point{}, draw.Src)
	}
	return dst
}

// writeIfChanged writes data to path unless the file already holds exactly
// those bytes, so re-running the generator does not touch timestamps.
func writeIfChanged(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%s: encoder produced no data", path)
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}
