//go:build !windows

package glass

import "image"

// Capture is only implemented on Windows. On macOS and Linux the dashboard
// opens in the default browser with a flat material, so no backdrop is
// captured.
func Capture(image.Rectangle) (*image.RGBA, error) {
	return nil, ErrUnsupported
}
