// Package ui serves the dashboard: a loopback-only HTTP server with the
// embedded web app, token-to-cookie login, a Server-Sent Events state stream
// and a small JSON action API (spec §3.9, §8).
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// Backend is what the dashboard needs from the application.
type Backend interface {
	// Snapshots subscribes to status snapshots. The channel should deliver
	// the latest snapshot first; the returned func unsubscribes.
	Snapshots() (<-chan model.Snapshot, func())
	// Action runs a dashboard command (see Actions) and returns a
	// JSON-marshalable result.
	Action(ctx context.Context, name string, args json.RawMessage) (any, error)
	// Backdrop returns the glass backdrop PNG, or nil when there is none.
	Backdrop() []byte
}

// ViewBackend is implemented by backends that serve a different snapshot
// stream per view. The demo backend uses it for ?demo=<view>.
type ViewBackend interface {
	SnapshotsFor(view string) (<-chan model.Snapshot, func())
}

//go:embed web
var webFS embed.FS

// CookieName is the session cookie.
const CookieName = "stv2s"

// DefaultNext is where a successful login lands when no target is given.
const DefaultNext = "/?mode=browser"

const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"

// Server is the dashboard's loopback HTTP server.
type Server struct {
	b       Backend
	ln      net.Listener
	srv     *http.Server
	port    int
	host    string // exact Host header every request must carry
	origin  string // the only accepted Origin (besides "null" on /login)
	control string // bearer token for /api/show, /api/quit, /api/launch
	auth    *authStore
	static  map[string]staticFile

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	closeOnce sync.Once
}

type staticFile struct {
	data  []byte
	ctype string
}

// NewServer starts the dashboard server on 127.0.0.1 with an OS-chosen port.
func NewServer(b Backend) (*Server, error) { return NewServerPort(b, 0) }

// NewServerPort starts the dashboard server on 127.0.0.1:port (0 = any free
// port). It never binds anything but the IPv4 loopback address.
func NewServerPort(b Backend, port int) (*Server, error) {
	if b == nil {
		return nil, errors.New("ui: nil backend")
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("ui: invalid port %d", port)
	}
	static, err := loadStatic()
	if err != nil {
		return nil, err
	}
	control, err := randToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("ui: listen: %w", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		b:       b,
		ln:      ln,
		port:    p,
		host:    "127.0.0.1:" + strconv.Itoa(p),
		origin:  "http://127.0.0.1:" + strconv.Itoa(p),
		control: control,
		auth:    newAuthStore(),
		static:  static,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

func loadStatic() (map[string]staticFile, error) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil, err
	}
	types := map[string]string{
		".html": "text/html; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
	}
	out := make(map[string]staticFile, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ct, ok := types[path.Ext(e.Name())]
		if !ok {
			continue
		}
		data, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			return nil, err
		}
		out["/"+e.Name()] = staticFile{data: data, ctype: ct}
	}
	if _, ok := out["/index.html"]; !ok {
		return nil, errors.New("ui: embedded index.html missing")
	}
	out["/"] = out["/index.html"]
	return out, nil
}

// URL returns the server's base URL, http://127.0.0.1:<port>.
func (s *Server) URL() string { return s.origin }

// Port returns the listening port.
func (s *Server) Port() int { return s.port }

// NewLaunchToken mints a single-use login token that expires after 30 s.
func (s *Server) NewLaunchToken() string { return s.auth.newLaunchToken() }

// ControlToken is the bearer token for /api/show, /api/quit and /api/launch
// (POST, with X-STV2: 1 like every other POST). It belongs in instance.json
// (0600) and nowhere else.
func (s *Server) ControlToken() string { return s.control }

// LoginURL returns a GET login URL for token that lands on next (see SafeNext).
func (s *Server) LoginURL(token, next string) string {
	q := url.Values{}
	q.Set("token", token)
	q.Set("next", SafeNext(next))
	return s.origin + "/login?" + q.Encode()
}

// LaunchPage returns a small HTML document that immediately POSTs token to
// /login and lands on next. Hosts write it to a 0600 launch file (browser
// path) or load it as a string (WebView2); its Origin is then "null", which
// /login accepts only together with a valid token.
func (s *Server) LaunchPage(token, next string) []byte {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>SyncThing V2</title></head>")
	b.WriteString("<body><form method=\"post\" action=\"" + e(s.origin+"/login") + "\">")
	b.WriteString("<input type=\"hidden\" name=\"token\" value=\"" + e(token) + "\">")
	b.WriteString("<input type=\"hidden\" name=\"next\" value=\"" + e(SafeNext(next)) + "\">")
	b.WriteString("<noscript><button type=\"submit\">Open dashboard</button></noscript></form>")
	b.WriteString("<script>document.forms[0].submit()</script></body></html>")
	return []byte(b.String())
}

// Close stops the server and ends every open state stream.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		err = s.srv.Close()
		<-s.done
	})
	return err
}

// SafeNext reduces a post-login target to "/" plus a re-encoded query, so a
// login can never redirect off the dashboard. Anything else yields DefaultNext.
func SafeNext(next string) string {
	if next == "" || len(next) > 512 || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return DefaultNext
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Path != "/" {
		return DefaultNext
	}
	q := u.Query()
	out := "/"
	if len(q) > 0 {
		out += "?" + q.Encode()
	}
	if u.Fragment != "" && isWord(u.Fragment) {
		out += "#" + u.Fragment
	}
	return out
}

func isWord(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ServeHTTP applies the §8.3 checks, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")

	// DNS rebinding: the Host must be exactly our loopback address.
	if r.Host != s.host {
		deny(w)
		return
	}
	p := r.URL.Path
	// Cross-origin writes: a present Origin must be ours. "null" (a launch
	// file or a string-loaded page) is tolerated only on /login, which
	// still requires a valid single-use token.
	if o := r.Header.Get("Origin"); o != "" && o != s.origin && !(o == "null" && p == "/login") {
		deny(w)
		return
	}
	// Browsers label requests from other sites (including other ports on
	// 127.0.0.1, which count as same-site); only /login may come from one.
	if sf := r.Header.Get("Sec-Fetch-Site"); (sf == "cross-site" || sf == "same-site") && p != "/login" {
		deny(w)
		return
	}

	switch p {
	case "/login":
		s.handleLogin(w, r)
		return
	case "/api/show", "/api/quit", "/api/launch":
		s.handleControl(w, r)
		return
	}

	if !s.auth.validSession(sessionCookie(r)) {
		deny(w)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("X-STV2") != "1" {
		deny(w)
		return
	}

	switch p {
	case "/api/state":
		s.handleState(w, r)
	case "/api/action":
		s.handleAction(w, r)
	case "/api/backdrop.png":
		s.handleBackdrop(w, r)
	case "/api/info":
		s.handleInfo(w, r)
	default:
		s.handleStatic(w, r)
	}
}

func deny(w http.ResponseWriter) {
	http.Error(w, "Forbidden. Open the dashboard from the SyncThing V2 tray icon.", http.StatusForbidden)
}

func sessionCookie(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.auth.consume(r.Form.Get("token")) {
		deny(w)
		return
	}
	sess, err := s.auth.newSession()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	// A same-origin meta refresh rather than a 303: the follow-up request is
	// then initiated by our own page, so the Strict cookie is always sent.
	next := html.EscapeString(SafeNext(r.Form.Get("next")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>SyncThing V2</title>"+
		"<meta http-equiv=\"refresh\" content=\"0;url=%s\"></head><body><a href=\"%s\">Open dashboard</a></body></html>", next, next)
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if !checkBearer(r, s.control) {
		deny(w)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// §8.3: every POST carries X-STV2: 1, the control routes included.
	if r.Header.Get("X-STV2") != "1" {
		deny(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	switch r.URL.Path {
	case "/api/launch":
		var req struct {
			Next string `json:"next"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": s.LoginURL(s.NewLaunchToken(), req.Next)})
	default: // /api/show, /api/quit
		name := strings.TrimPrefix(r.URL.Path, "/api/")
		if _, err := s.b.Action(r.Context(), name, nil); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, ok := s.static[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(f.data)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"ok":false,"error":"result is not serialisable"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
