// Package notify shows desktop notifications for state changes, pairing
// prompts and updates.
//
//   - Windows: a tray balloon (NIF_INFO) through the running tray; a click
//     runs onClick, or the tray's own click handler when onClick is nil.
//   - macOS: osascript "display notification", with the text passed only as
//     argv. macOS does not report clicks on these notifications, so onClick
//     is not called.
//   - Linux: org.freedesktop.Notifications over the D-Bus session bus, with a
//     "default" action; invoking it calls onClick.
//
// Show never blocks and never fails loudly: a notification that cannot be
// shown is written to the application log.
package notify

// Balloonist is the part of a tray that shows Windows balloons. tray.Tray
// implements it.
type Balloonist interface {
	Balloon(title, body string) bool
}

// Use sets the tray that carries notifications on Windows. On macOS and Linux
// notifications do not go through the tray and Use has no effect.
func Use(t Balloonist) { use(t) }

// Show shows a notification with a title and body. onClick may be nil.
func Show(title, body string, onClick func()) { show(title, body, onClick) }

// HasStatusNotifierWatcher reports whether the desktop can show a tray icon.
// On Linux this is whether org.kde.StatusNotifierWatcher is on the session
// bus (GNOME needs the AppIndicator extension for it); on Windows and macOS
// the notification area always exists and it returns true.
func HasStatusNotifierWatcher() bool { return hasStatusNotifierWatcher() }
