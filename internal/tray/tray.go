// Package tray shows SyncThing V2's notification-area icon, tooltip and menu.
//
// Windows uses a native Shell_NotifyIconW implementation on a locked OS
// thread (tray_windows.go); macOS and Linux use fyne.io/systray
// (tray_other.go). The menu model and tooltip text are built by the pure
// functions BuildMenu and Tooltip (menu.go), so every platform shows the same
// content.
package tray

import (
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
)

// startingTooltip is the hover text until the first SetTooltip.
const startingTooltip = brand.DisplayName + ": starting..."

// MenuItem is one entry of the tray menu. An item whose ID is Separator is a
// separator line. Children turn an item into a submenu. Items with Visible
// false stay in the model (so the platform menu keeps a stable shape) but are
// not shown.
type MenuItem struct {
	ID, Text                  string
	Enabled, Checked, Visible bool
	Children                  []MenuItem
}

// Tray is the platform tray icon.
//
// Run blocks until Quit and owns the calling OS thread. onReady runs once, on
// its own goroutine, when the tray accepts updates. onClick runs on a left
// click of the icon or a click on a balloon (Windows only; on macOS and
// Linux a click opens the menu, by platform convention). onMenu runs with the
// ID of a chosen menu item. On Windows onClick and onMenu run on the tray
// thread, so they must not block for long; on macOS and Linux they run on a
// systray goroutine. All other methods are safe to call from any goroutine,
// before or after Run starts. A Tray runs once; after Quit it cannot be
// started again.
type Tray interface {
	Run(onReady func(), onClick func(), onMenu func(id string))
	// SetIcon shows the status ring for s. Off Windows, pct is quantised to
	// 5% steps so the icon is not re-sent for every small change.
	SetIcon(s model.State, pct int)
	// SetTooltip sets the hover text. Windows shows at most 127 UTF-16 units.
	SetTooltip(text string)
	// SetMenu replaces the menu model.
	SetMenu(items []MenuItem)
	// Balloon shows a notification balloon on Windows and reports whether it
	// was queued. It returns false on other platforms and before Run.
	Balloon(title, body string) bool
	// Quit removes the icon and makes Run return.
	Quit()
}

// New returns the tray for the running platform. A process has at most one
// running tray.
func New() Tray { return newTray() }
