package app

import (
	"errors"
	"image"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/glass"
	"github.com/Neutx/syncthing-v2/internal/status"
	"github.com/Neutx/syncthing-v2/internal/uihost"
)

// startHost starts the dashboard host. On Windows the ui-host child is
// prewarmed: it logs in at once and stays hidden until the first Show.
// On macOS and Linux the dashboard opens in the default browser.
func (a *App) startHost() {
	h, err := uihost.StartWith(uihost.Config{
		ServerURL:   a.server.URL(),
		LaunchToken: a.server.NewLaunchToken(),
		NewToken:    a.server.NewLaunchToken,
		OnFallback:  a.onFallback,
	})
	if err != nil {
		applog.Printf("app: starting the dashboard host: %v", err)
		return
	}
	a.mu.Lock()
	a.host = h
	a.mu.Unlock()
}

// onFallback is called once when the Windows host switches to the browser
// for the rest of the session (doctor reports UI001 for a missing WebView2).
func (a *App) onFallback(reason error) {
	applog.Printf("app: dashboard falls back to the browser: %v", reason)
	msg := "The dashboard opens in your browser for this session"
	if errors.Is(reason, uihost.ErrWebView2Missing) {
		msg = "WebView2 is not available, so the dashboard opens in your browser (see stv2 doctor, UI001)"
	}
	a.note(msg, status.TintGrey)
}

// Show opens the dashboard (§6.4 show sequence). On Windows, with the glass
// host, the desktop behind the dashboard is captured while the popup is
// hidden, rendered through the glass pipeline and cached for
// /api/backdrop.png before the popup is shown. In browser mode the dashboard
// opens in the default browser without a capture.
func (a *App) Show() { a.ShowView("") }

// Dashboard views the tray opens directly (uihost.ValidView names).
const (
	ViewPair     = "pair"
	ViewWelcome  = "welcome"
	ViewSettings = "settings"
)

// ShowView is Show on a given view: ViewPair for "Pair Devices…" and the
// pairing and folder-offer notifications, ViewWelcome for --setup and the
// first run, ViewSettings (which carries the About section) for "About"
// where there is no native message box. An empty view keeps the page on the
// view it shows.
func (a *App) ShowView(view string) {
	a.showMu.Lock()
	defer a.showMu.Unlock()
	a.mu.Lock()
	h := a.host
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.engine.Refresh() // F4: refresh when the dashboard opens
	}
	if h == nil {
		applog.Printf("app: no dashboard host")
		return
	}
	if h.Browser() {
		h.ShowView(image.Rectangle{}, view)
		return
	}
	r, scale := a.place(uihost.DashboardSize)
	if h.Visible() {
		h.Hide()
		time.Sleep(captureSettle)
	}
	a.renderBackdrop(r, scale)
	h.ShowView(r, view)
}

// renderBackdrop captures r and caches the rendered glass backdrop PNG. On
// failure the previous backdrop is dropped, so the page shows its plain
// material rather than a stale picture of another place on the screen.
func (a *App) renderBackdrop(r image.Rectangle, scale float64) {
	var png []byte
	src, err := a.capture(r)
	if err == nil {
		png, err = glass.PNG(a.render(src, scale))
	}
	if err != nil && !errors.Is(err, glass.ErrUnsupported) {
		applog.Printf("app: glass backdrop: %v", err)
	}
	a.mu.Lock()
	a.backdrop = png
	a.mu.Unlock()
}

// hide hides the popup (the page asks for it after its exit animation).
func (a *App) hide() {
	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	if h != nil {
		h.Hide()
	}
}

// browserMode reports whether the dashboard opens in the browser.
func (a *App) browserMode() bool {
	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	return h == nil || h.Browser()
}
