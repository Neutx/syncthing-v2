package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// fakeBackend records actions and serves whatever snapshots a test pushes.
type fakeBackend struct {
	mu       sync.Mutex
	calls    []fakeCall
	err      error
	result   any
	backdrop []byte
	snaps    chan model.Snapshot
	unsubs   int
}

type fakeCall struct {
	name string
	args string
}

func newFake() *fakeBackend { return &fakeBackend{snaps: make(chan model.Snapshot, 8)} }

func (f *fakeBackend) Snapshots() (<-chan model.Snapshot, func()) {
	return f.snaps, func() {
		f.mu.Lock()
		f.unsubs++
		f.mu.Unlock()
	}
}

func (f *fakeBackend) Action(_ context.Context, name string, args json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{name, string(args)})
	return f.result, f.err
}

func (f *fakeBackend) Backdrop() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backdrop
}

// set changes the fake's canned answers while the server may be using it.
func (f *fakeBackend) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeBackend) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.name)
	}
	return out
}

func startServer(t *testing.T, b Backend) *Server {
	t.Helper()
	s, err := NewServer(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var testClient = &http.Client{
	Transport:     &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       10 * time.Second,
}

type reqOpts struct {
	host, origin, cookie, xstv2, bearer, secFetch, ctype string
	body                                                 string
}

func do(t *testing.T, s *Server, method, path string, o reqOpts) *http.Response {
	t.Helper()
	var body io.Reader
	if o.body != "" {
		body = strings.NewReader(o.body)
	}
	req, err := http.NewRequest(method, s.URL()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if o.host != "" {
		req.Host = o.host
	}
	if o.origin != "" {
		req.Header.Set("Origin", o.origin)
	}
	if o.cookie != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: o.cookie})
	}
	if o.xstv2 != "" {
		req.Header.Set("X-STV2", o.xstv2)
	}
	if o.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+o.bearer)
	}
	if o.secFetch != "" {
		req.Header.Set("Sec-Fetch-Site", o.secFetch)
	}
	if o.ctype != "" {
		req.Header.Set("Content-Type", o.ctype)
	} else if o.body != "" && method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// login exchanges a fresh launch token for a session cookie value.
func login(t *testing.T, s *Server) string {
	t.Helper()
	resp := postLogin(t, s, s.NewLaunchToken(), "", "null")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			return c.Value
		}
	}
	t.Fatal("login: no session cookie")
	return ""
}

func postLogin(t *testing.T, s *Server, token, next, origin string) *http.Response {
	t.Helper()
	form := url.Values{"token": {token}}
	if next != "" {
		form.Set("next", next)
	}
	return do(t, s, http.MethodPost, "/login", reqOpts{
		origin: origin, body: form.Encode(), ctype: "application/x-www-form-urlencoded",
	})
}

func assertSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Content-Security-Policy"); got != csp {
		t.Errorf("CSP = %q, want %q", got, csp)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("CORS header %s must never be sent", k)
		}
	}
}

func TestAuthMatrix(t *testing.T) {
	f := newFake()
	s := startServer(t, f)
	sess := login(t, s)
	port := strings.TrimPrefix(s.URL(), "http://127.0.0.1:")
	action := `{"name":"rescan"}`

	usedTok := s.NewLaunchToken()
	if r := postLogin(t, s, usedTok, "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("first use of token: %d", r.StatusCode)
	}

	expiredTok := s.NewLaunchToken()
	base := time.Now()
	s.auth.mu.Lock()
	s.auth.now = func() time.Time { return base.Add(launchTokenTTL + time.Second) }
	s.auth.mu.Unlock()
	expiredResp := postLogin(t, s, expiredTok, "", "")
	s.auth.mu.Lock()
	s.auth.now = time.Now
	s.auth.mu.Unlock()

	cases := []struct {
		name string
		resp func() *http.Response
	}{
		{"bad Host", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/info", reqOpts{host: "evil.example:" + port, cookie: sess})
		}},
		{"localhost Host", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/info", reqOpts{host: "localhost:" + port, cookie: sess})
		}},
		{"bad Host on login", func() *http.Response {
			return do(t, s, http.MethodPost, "/login", reqOpts{host: "rebind.example:" + port,
				body: "token=" + s.NewLaunchToken(), ctype: "application/x-www-form-urlencoded"})
		}},
		{"bad Origin", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{origin: "http://evil.example", cookie: sess, xstv2: "1", body: action})
		}},
		{"other loopback port Origin", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{origin: "http://127.0.0.1:8384", cookie: sess, xstv2: "1", body: action})
		}},
		{"null Origin off /login", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{origin: "null", cookie: sess, xstv2: "1", body: action})
		}},
		{"cross-site fetch", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/info", reqOpts{cookie: sess, secFetch: "cross-site"})
		}},
		{"same-site fetch", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/state", reqOpts{cookie: sess, secFetch: "same-site"})
		}},
		{"missing cookie on state", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/state", reqOpts{})
		}},
		{"missing cookie on page", func() *http.Response {
			return do(t, s, http.MethodGet, "/", reqOpts{})
		}},
		{"missing cookie on action", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{xstv2: "1", body: action})
		}},
		{"forged cookie", func() *http.Response {
			return do(t, s, http.MethodGet, "/api/info", reqOpts{cookie: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
		}},
		{"missing X-STV2", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{cookie: sess, body: action})
		}},
		{"wrong X-STV2", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/action", reqOpts{cookie: sess, xstv2: "0", body: action})
		}},
		{"reused token", func() *http.Response { return postLogin(t, s, usedTok, "", "") }},
		{"expired token", func() *http.Response { return expiredResp }},
		{"unknown token", func() *http.Response { return postLogin(t, s, "not-a-token", "", "null") }},
		{"empty token", func() *http.Response { return postLogin(t, s, "", "", "") }},
		{"control token as launch token", func() *http.Response { return postLogin(t, s, s.ControlToken(), "", "") }},
		{"show without bearer", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/show", reqOpts{cookie: sess, xstv2: "1"})
		}},
		{"quit with wrong bearer", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/quit", reqOpts{bearer: "wrong", xstv2: "1"})
		}},
		{"launch with session cookie only", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/launch", reqOpts{cookie: sess, xstv2: "1"})
		}},
		{"show with bearer but foreign Origin", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/show", reqOpts{bearer: s.ControlToken(), origin: "http://evil.example", xstv2: "1"})
		}},
		{"show with bearer but no X-STV2", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/show", reqOpts{bearer: s.ControlToken()})
		}},
		{"quit with bearer but wrong X-STV2", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/quit", reqOpts{bearer: s.ControlToken(), xstv2: "0"})
		}},
		{"launch with bearer but no X-STV2", func() *http.Response {
			return do(t, s, http.MethodPost, "/api/launch", reqOpts{bearer: s.ControlToken(), body: `{}`})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := c.resp()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			assertSecurityHeaders(t, resp)
			for _, sc := range resp.Cookies() {
				if sc.Name == CookieName {
					t.Fatal("a denied request must not set a session cookie")
				}
			}
		})
	}
	if calls := f.callNames(); len(calls) != 0 {
		t.Fatalf("denied requests reached the backend: %v", calls)
	}
}

func TestLoginCookieAndRedirect(t *testing.T) {
	s := startServer(t, newFake())
	resp := postLogin(t, s, s.NewLaunchToken(), "/?mode=glass&demo=status#pair", "null")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)
	var c *http.Cookie
	for _, sc := range resp.Cookies() {
		if sc.Name == CookieName {
			c = sc
		}
	}
	if c == nil {
		t.Fatal("no stv2s cookie")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie attributes: HttpOnly=%v SameSite=%v Path=%q", c.HttpOnly, c.SameSite, c.Path)
	}
	raw := resp.Header.Get("Set-Cookie")
	if !strings.Contains(raw, "HttpOnly") || !strings.Contains(raw, "SameSite=Strict") {
		t.Fatalf("Set-Cookie = %q", raw)
	}
	if len(c.Value) < 43 {
		t.Fatalf("session value too short: %d chars", len(c.Value))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `content="0;url=/?demo=status&amp;mode=glass#pair"`) {
		t.Fatalf("login page does not refresh to the requested view: %s", body)
	}

	// GET login works the same way (tooling and the manual demo).
	tok := s.NewLaunchToken()
	u, _ := url.Parse(s.LoginURL(tok, "/?demo=pair"))
	resp = do(t, s, http.MethodGet, u.RequestURI(), reqOpts{})
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) == 0 {
		t.Fatalf("GET login: status %d, cookies %d", resp.StatusCode, len(resp.Cookies()))
	}
	// ...and is single-use too.
	if resp := do(t, s, http.MethodGet, u.RequestURI(), reqOpts{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET login reuse: status %d", resp.StatusCode)
	}
}

func TestTokenValidJustBeforeExpiry(t *testing.T) {
	s := startServer(t, newFake())
	tok := s.NewLaunchToken()
	base := time.Now()
	s.auth.mu.Lock()
	s.auth.now = func() time.Time { return base.Add(launchTokenTTL - time.Second) }
	s.auth.mu.Unlock()
	if resp := postLogin(t, s, tok, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("token at 29 s: status %d", resp.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                             DefaultNext,
		"/":                            "/",
		"/?mode=glass":                 "/?mode=glass",
		"/?demo=pair&mode=browser":     "/?demo=pair&mode=browser",
		"/?mode=glass#settings":        "/?mode=glass#settings",
		"/?mode=glass#bad<frag>":       "/?mode=glass",
		"//evil.example/":              DefaultNext,
		"/\\evil.example":              DefaultNext,
		"http://evil.example/":         DefaultNext,
		"javascript:alert(1)":          DefaultNext,
		"/api/action":                  DefaultNext,
		"/login?token=x":               DefaultNext,
		"/?a=\"><script>":              "/?a=%22%3E%3Cscript%3E",
		"/" + strings.Repeat("a", 600): DefaultNext,
	}
	for in, want := range cases {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLaunchPageLogsIn(t *testing.T) {
	s := startServer(t, newFake())
	tok := s.NewLaunchToken()
	page := string(s.LaunchPage(tok, "/?mode=glass"))
	for _, want := range []string{
		`action="` + s.URL() + `/login"`, `name="token" value="` + tok + `"`,
		`name="next" value="/?mode=glass"`, "document.forms[0].submit()",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("launch page lacks %q", want)
		}
	}
	// A launch file posts with Origin: null; that is accepted with the token.
	if resp := postLogin(t, s, tok, "/?mode=glass", "null"); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch login: %d", resp.StatusCode)
	}
}

func TestControlRoutes(t *testing.T) {
	f := newFake()
	s := startServer(t, f)
	for _, name := range []string{"show", "quit"} {
		resp := do(t, s, http.MethodPost, "/api/"+name, reqOpts{bearer: s.ControlToken(), xstv2: "1"})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("/api/%s: status %d", name, resp.StatusCode)
		}
		assertSecurityHeaders(t, resp)
	}
	if got := strings.Join(f.callNames(), ","); got != "show,quit" {
		t.Fatalf("backend calls = %s", got)
	}
	if resp := do(t, s, http.MethodGet, "/api/show", reqOpts{bearer: s.ControlToken()}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/show: %d", resp.StatusCode)
	}

	// /api/launch mints a working single-use login URL.
	resp := do(t, s, http.MethodPost, "/api/launch", reqOpts{bearer: s.ControlToken(), xstv2: "1", body: `{"next":"/?demo=settings"}`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/launch: %d", resp.StatusCode)
	}
	var out struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		t.Fatalf("/api/launch body: %v %+v", err, out)
	}
	u, err := url.Parse(out.URL)
	if err != nil || u.Host != strings.TrimPrefix(s.URL(), "http://") || u.Query().Get("next") != "/?demo=settings" {
		t.Fatalf("launch URL %q", out.URL)
	}
	if resp := do(t, s, http.MethodGet, u.RequestURI(), reqOpts{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch URL login: %d", resp.StatusCode)
	}

	// A failing backend surfaces as 500 with the message.
	f.set(func() { f.err = io.ErrUnexpectedEOF })
	if resp := do(t, s, http.MethodPost, "/api/show", reqOpts{bearer: s.ControlToken(), xstv2: "1"}); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failing show: %d", resp.StatusCode)
	}
}

func TestAuthStoreBounds(t *testing.T) {
	a := newAuthStore()
	first, err := a.newSession()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for i := 1; i <= maxSessions; i++ {
		i := i
		a.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		if _, err := a.newSession(); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.sessions) != maxSessions {
		t.Fatalf("sessions = %d, want %d", len(a.sessions), maxSessions)
	}
	if a.validSession(first) {
		t.Fatal("the oldest session should have been evicted")
	}

	var toks []string
	for i := 0; i < maxLaunchTokens+5; i++ {
		i := i
		a.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		toks = append(toks, a.newLaunchToken())
	}
	if len(a.tokens) > maxLaunchTokens {
		t.Fatalf("tokens = %d, want <= %d", len(a.tokens), maxLaunchTokens)
	}
	if !a.consume(toks[len(toks)-1]) {
		t.Fatal("the newest token must stay valid")
	}
	if a.consume(toks[0]) {
		t.Fatal("the oldest token should have been evicted")
	}
	if a.validSession("") || a.consume("") {
		t.Fatal("empty values must never authenticate")
	}
}
