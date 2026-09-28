//go:build windows

// Package deps anchors every runtime module in go.mod and go.sum, so the
// module files stay tidy while the packages that use them are being built.
// It contains no code and nothing imports it.
package deps

import (
	_ "github.com/go-ole/go-ole"
	_ "github.com/go-ole/go-ole/oleutil"
	_ "github.com/wailsapp/go-webview2/pkg/edge"
	_ "golang.org/x/image/vector"
	_ "golang.org/x/sys/windows"
	_ "golang.org/x/sys/windows/registry"
)
