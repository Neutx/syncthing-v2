package stclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "synthetic-api-key-for-tests"

func endpointFor(t *testing.T, rawURL string) Endpoint {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return Endpoint{BaseURL: u, APIKey: testKey}
}

func TestGetDecodesAndSendsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		if r.URL.Path != "/rest/db/status" || r.URL.Query().Get("folder") != "exmpl-00001" {
			t.Errorf("request = %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"globalBytes":1024,"state":"idle"}`))
	}))
	defer srv.Close()

	c := New(endpointFor(t, srv.URL))
	var st struct {
		GlobalBytes int64  `json:"globalBytes"`
		State       string `json:"state"`
	}
	if err := c.Get(context.Background(), "/rest/db/status?folder=exmpl-00001", &st); err != nil {
		t.Fatal(err)
	}
	if st.GlobalBytes != 1024 || st.State != "idle" {
		t.Errorf("decoded %+v", st)
	}
	if c.Host() != strings.TrimPrefix(srv.URL, "http://") || c.URL() != srv.URL {
		t.Errorf("Host %q URL %q", c.Host(), c.URL())
	}
}

func TestErrorKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/401":
			w.WriteHeader(http.StatusUnauthorized)
		case "/rest/403":
			http.Error(w, "CSRF Error", http.StatusForbidden)
		case "/rest/500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/rest/404":
			http.NotFound(w, r)
		case "/rest/badjson":
			_, _ = w.Write([]byte(`{"state":`))
		case "/rest/html":
			_, _ = w.Write([]byte(`<html>not json</html>`))
		case "/rest/redirect":
			http.Redirect(w, r, "http://192.0.2.1/steal", http.StatusFound)
		}
	}))
	defer srv.Close()
	c := New(endpointFor(t, srv.URL))

	cases := []struct {
		path   string
		kind   ErrKind
		status int
	}{
		{"/rest/401", ErrUnauthorized, 401},
		{"/rest/403", ErrUnauthorized, 403},
		{"/rest/500", ErrBadResponse, 500},
		{"/rest/404", ErrBadResponse, 404},
		{"/rest/badjson", ErrBadResponse, 200},
		{"/rest/html", ErrBadResponse, 200},
		{"/rest/redirect", ErrBadResponse, 302},
	}
	for _, cs := range cases {
		var out map[string]any
		err := c.Get(context.Background(), cs.path, &out)
		k, ok := KindOf(err)
		if !ok || k != cs.kind || StatusOf(err) != cs.status {
			t.Errorf("Get(%s) = %v (kind %v, status %d); want %v %d", cs.path, err, k, StatusOf(err), cs.kind, cs.status)
		}
		if err != nil && strings.Contains(err.Error(), testKey) {
			t.Errorf("error text leaks the API key: %v", err)
		}
	}
}

func TestConnectionRefusedIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	c := New(endpointFor(t, "http://"+addr))
	err = c.Get(context.Background(), "/rest/system/status", nil)
	if k, ok := KindOf(err); !ok || k != ErrUnreachable || StatusOf(err) != 0 {
		t.Fatalf("Get on closed port = %v, want ErrUnreachable", err)
	}
	if err := c.Send(context.Background(), http.MethodPost, "/rest/db/scan", nil); err == nil {
		t.Fatal("Send on closed port succeeded")
	} else if k, _ := KindOf(err); k != ErrUnreachable {
		t.Fatalf("Send on closed port = %v", err)
	}
	if _, _, err := c.Events(context.Background(), 5, 2*time.Second); err == nil {
		t.Fatal("Events on closed port succeeded")
	} else if k, _ := KindOf(err); k != ErrUnreachable {
		t.Fatalf("Events on closed port = %v", err)
	}
}

func TestTimeoutIsUnreachable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c := New(endpointFor(t, srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Get(ctx, "/rest/system/status", nil)
	if k, ok := KindOf(err); !ok || k != ErrUnreachable {
		t.Fatalf("Get past deadline = %v, want ErrUnreachable", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Get ignored the context deadline")
	}
}

func TestEventsSeedAndSince(t *testing.T) {
	var mu sync.Mutex
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if r.URL.Path != "/rest/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") == "1" {
			_, _ = w.Write([]byte(`[{"id":41,"type":"StartupComplete","time":"2026-01-02T03:04:05Z","data":{}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":42,"type":"ConfigSaved","data":{}},{"id":44,"type":"DeviceConnected","data":{}},{"id":43,"type":"Ping"}]`))
	}))
	defer srv.Close()
	c := New(endpointFor(t, srv.URL))

	evs, last, err := c.Events(context.Background(), 0, EventsTimeout)
	if err != nil || len(evs) != 1 || last != 41 {
		t.Fatalf("seed = %d events, last %d, %v", len(evs), last, err)
	}
	evs, last, err = c.Events(context.Background(), 41, 0)
	if err != nil || len(evs) != 3 || last != 44 {
		t.Fatalf("since = %d events, last %d, %v", len(evs), last, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if q := queries[0]; q.Get("limit") != "1" || q.Get("since") != "" || q.Get("timeout") != "50" {
		t.Errorf("seed query = %v", q)
	}
	if q := queries[1]; q.Get("since") != "41" || q.Get("limit") != "" || q.Get("timeout") != "50" {
		t.Errorf("long-poll query = %v", q)
	}
}

func TestEventsEmptyKeepsSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timeout") != "1" {
			t.Errorf("timeout = %q, want the 1 s floor", r.URL.Query().Get("timeout"))
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	evs, last, err := New(endpointFor(t, srv.URL)).Events(context.Background(), 7, 3*time.Second)
	if err != nil || len(evs) != 0 || last != 7 {
		t.Fatalf("empty batch = %d, %d, %v", len(evs), last, err)
	}
}

func TestSend(t *testing.T) {
	type rec struct {
		method, path, ctype, body string
		length                    int64
	}
	var mu sync.Mutex
	var got []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, rec{r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), string(b), r.ContentLength})
		mu.Unlock()
		if r.Header.Get("X-API-Key") != testKey {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c := New(endpointFor(t, srv.URL))
	ctx := context.Background()

	if err := c.Send(ctx, http.MethodPost, "/rest/db/scan", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, http.MethodPatch, "/rest/config/folders/"+url.PathEscape("a b/c"), map[string]bool{"paused": true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, http.MethodGet, "/rest/db/scan", nil); err == nil {
		t.Error("Send accepted GET")
	}
	if err := c.Send(ctx, http.MethodPost, "http://192.0.2.1/rest/db/scan", nil); err == nil {
		t.Error("Send accepted an absolute URL")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("server saw %d requests", len(got))
	}
	if g := got[0]; g.method != "POST" || g.path != "/rest/db/scan" || g.length != 0 || g.body != "" || g.ctype != "" {
		t.Errorf("POST = %+v", g)
	}
	var body map[string]bool
	if g := got[1]; g.method != "PATCH" || g.path != "/rest/config/folders/a%20b%2Fc" || g.ctype != "application/json" ||
		json.Unmarshal([]byte(g.body), &body) != nil || !body["paused"] {
		t.Errorf("PATCH = %+v", g)
	}
}

func TestErrorText(t *testing.T) {
	cases := []struct {
		e    *Error
		want string
	}{
		{&Error{Kind: ErrUnreachable}, "Syncthing is not responding"},
		{&Error{Kind: ErrUnauthorized, Status: 403}, "Syncthing rejected the API key"},
		{&Error{Kind: ErrBadResponse, Status: 500}, "unexpected response from Syncthing (HTTP 500)"},
		{&Error{Kind: ErrBadResponse, Status: 200}, "unexpected response from Syncthing"},
	}
	for _, c := range cases {
		if got := c.e.Summary(); got != c.want {
			t.Errorf("Summary() = %q, want %q", got, c.want)
		}
		if got := c.e.Error(); got != c.want {
			t.Errorf("Error() without cause = %q, want %q", got, c.want)
		}
	}
	if _, ok := KindOf(io.EOF); ok {
		t.Error("KindOf matched a foreign error")
	}
}
