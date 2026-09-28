//go:build linux

// Package deps anchors every runtime module in go.mod and go.sum, so the
// module files stay tidy while the packages that use them are being built.
// It contains no code and nothing imports it.
package deps

import (
	_ "fyne.io/systray"
	_ "github.com/godbus/dbus/v5"
	_ "golang.org/x/image/vector"
	_ "golang.org/x/sys/unix"
)
