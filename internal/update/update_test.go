package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/prefs"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"1.0.1", "1.0.0", true},
		{"v1.0.1", "v1.0.0", true},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "1.0.0", false},
		{"1.1.0", "1.0.9", true},
		{"2.0.0", "1.99.99", true},
		{"1.10.0", "1.9.0", true},
		{"1.0.0", "1.0.0-rc.1", true},     // a release is newer than its prerelease
		{"1.0.1-rc.1", "1.0.0", false},    // prereleases are never offered
		{"1.0.0", "0.0.0-dev", true},      // dev builds see the first release
		{"1.0.0+build.5", "1.0.0", false}, // build metadata is ignored
	}
	for _, c := range cases {
		got, err := Newer(c.cand, c.cur)
		if err != nil {
			t.Fatalf("Newer(%q, %q): %v", c.cand, c.cur, err)
		}
		if got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.cand, c.cur, got, c.want)
		}
	}
	for _, bad := range []string{"", "1.0", "1.0.0.0", "a.b.c", "01.0.0", "1.0.0-", "1.0.0-a..b"} {
		if _, err := Newer(bad, "1.0.0"); err == nil {
			t.Errorf("Newer(%q) accepted an invalid version", bad)
		}
	}
}

func TestComparePrerelease(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0"}
	for i := 0; i+1 < len(order); i++ {
		a, _ := Parse(order[i])
		b, _ := Parse(order[i+1])
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Errorf("want %s < %s", order[i], order[i+1])
		}
	}
}

type fakeAPI struct {
	srv   *httptest.Server
	hits  atomic.Int32
	body  atomic.Value // string
	code  atomic.Int32
	agent atomic.Value
}

func newFakeAPI(t *testing.T, body string) *fakeAPI {
	f := &fakeAPI{}
	f.body.Store(body)
	f.code.Store(200)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.agent.Store(r.Header.Get("User-Agent"))
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/releases/latest" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.code.Load()))
		fmt.Fprint(w, f.body.Load().(string))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func release(tag string, draft, pre bool) string {
	return fmt.Sprintf(`{"tag_name":%q,"name":"SyncThing V2 %s","html_url":"https://github.com/o/r/releases/tag/%s","draft":%v,"prerelease":%v}`, tag, tag, tag, draft, pre)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newChecker(t *testing.T, f *fakeAPI, current string) (*Checker, *prefs.Store, *clock) {
	t.Helper()
	st, err := prefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	c := New(st)
	c.URL = f.srv.URL + "/repos/o/r/releases/latest"
	c.Current = current
	c.Client = f.srv.Client()
	c.Now = clk.now
	return c, st, clk
}

func TestCheckFindsNewerRelease(t *testing.T) {
	f := newFakeAPI(t, release("v1.0.1", false, false))
	c, st, clk := newChecker(t, f, "1.0.0")
	res, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Checked || !res.Available || !res.Notify || res.Latest.Version != "v1.0.1" {
		t.Fatalf("result = %+v", res)
	}
	if res.Latest.URL != "https://github.com/o/r/releases/tag/v1.0.1" {
		t.Errorf("URL = %q", res.Latest.URL)
	}
	if got := f.agent.Load(); got != brand.UserAgent() || !strings.HasPrefix(got.(string), "stv2/") {
		t.Errorf("User-Agent = %v, want %q", got, brand.UserAgent())
	}
	if !st.Get().LastUpdateCheck.Equal(clk.t) {
		t.Errorf("LastUpdateCheck = %v, want %v", st.Get().LastUpdateCheck, clk.t)
	}

	// Within 24 h: no second query, cached result.
	clk.t = clk.t.Add(23 * time.Hour)
	res, err = c.Check(context.Background())
	if err != nil || res.Checked || !res.Available || res.Latest.Version != "v1.0.1" {
		t.Fatalf("cached result = %+v, %v", res, err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 per 24 h", f.hits.Load())
	}

	// Announced once: Notify turns off, also for a fresh Checker (restart).
	if err := c.MarkNotified("v1.0.1"); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Check(context.Background())
	if res.Notify {
		t.Error("Notify still set after MarkNotified")
	}
	c2 := New(st)
	c2.URL, c2.Current, c2.Client, c2.Now = c.URL, "1.0.0", f.srv.Client(), clk.now
	res, err = c2.Check(context.Background())
	if err != nil || res.Checked || !res.Available || res.Notify || res.Latest.Version != "v1.0.1" {
		t.Fatalf("restart result = %+v, %v", res, err)
	}

	// After 24 h: one new query.
	clk.t = clk.t.Add(2 * time.Hour)
	if !c.Due() {
		t.Fatal("check not due after 25 h")
	}
	if res, err = c.Check(context.Background()); err != nil || !res.Checked {
		t.Fatalf("second check = %+v, %v", res, err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", f.hits.Load())
	}
}

func TestCheckSameOrOlder(t *testing.T) {
	for _, cur := range []string{"1.0.1", "1.2.0"} {
		f := newFakeAPI(t, release("v1.0.1", false, false))
		c, _, _ := newChecker(t, f, cur)
		res, err := c.Check(context.Background())
		if err != nil || res.Available || res.Notify || !res.Checked {
			t.Errorf("current %s: %+v, %v", cur, res, err)
		}
	}
}

func TestPrereleasesAndDraftsIgnored(t *testing.T) {
	for _, body := range []string{
		release("v1.0.1", false, true),       // flagged prerelease
		release("v1.0.1", true, false),       // draft
		release("v1.0.1-rc.1", false, false), // prerelease tag without the flag
	} {
		f := newFakeAPI(t, body)
		c, st, _ := newChecker(t, f, "1.0.0")
		res, err := c.Check(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if res.Available || res.Latest.Version != "" || !res.Checked {
			t.Errorf("%s: result = %+v", body, res)
		}
		if st.Get().LastUpdateCheck.IsZero() {
			t.Errorf("%s: an answered check must be recorded", body)
		}
		if _, err := c.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
			t.Errorf("%s: Latest error = %v, want ErrNoRelease", body, err)
		}
	}
}

func TestNoReleaseAndErrors(t *testing.T) {
	f := newFakeAPI(t, `{"message":"Not Found"}`)
	f.code.Store(404)
	c, _, _ := newChecker(t, f, "1.0.0")
	if res, err := c.Check(context.Background()); err != nil || res.Available {
		t.Fatalf("404: %+v, %v", res, err)
	}

	f = newFakeAPI(t, `{"message":"rate limited"}`)
	f.code.Store(403)
	c, st, _ := newChecker(t, f, "1.0.0")
	_, err := c.Check(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("403: err = %v", err)
	}
	if st.Get().LastUpdateCheck.IsZero() {
		t.Error("an HTTP answer must count as the day's query")
	}

	f = newFakeAPI(t, `not json`)
	c, _, _ = newChecker(t, f, "1.0.0")
	if _, err := c.Check(context.Background()); err == nil {
		t.Fatal("bad JSON accepted")
	}

	// A network failure is not recorded, so the next attempt retries.
	f = newFakeAPI(t, release("v1.0.1", false, false))
	c, st, _ = newChecker(t, f, "1.0.0")
	f.srv.Close()
	if _, err := c.Check(context.Background()); err == nil {
		t.Fatal("closed server: no error")
	}
	if !st.Get().LastUpdateCheck.IsZero() || !c.Due() {
		t.Error("a network failure must not block the next attempt")
	}
}

func TestDisabledMakesNoRequest(t *testing.T) {
	f := newFakeAPI(t, release("v9.0.0", false, false))
	c, st, _ := newChecker(t, f, "1.0.0")
	if err := st.Update(func(p *prefs.Prefs) error { p.CheckForUpdates = false; return nil }); err != nil {
		t.Fatal(err)
	}
	res, err := c.Check(context.Background())
	if err != nil || res.Available || res.Checked {
		t.Fatalf("disabled: %+v, %v", res, err)
	}
	if c.Due() {
		t.Error("Due must be false when checks are off")
	}
	if f.hits.Load() != 0 {
		t.Fatalf("disabled checker made %d requests", f.hits.Load())
	}
}

func TestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	if New(nil).RequestTimeout != Timeout {
		t.Fatalf("production request timeout is not %s", Timeout)
	}
	// The caller's context has no deadline: only the Checker's own timeout
	// can end the request.
	c := &Checker{URL: srv.URL, Current: "1.0.0", Client: srv.Client(), RequestTimeout: 150 * time.Millisecond, Interval: Interval, Now: time.Now}
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := c.Latest(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hung server: err = %v, want a deadline error", err)
		}
		if el := time.Since(start); el > time.Second {
			t.Fatalf("request took %s, not bounded by the 150ms timeout", el)
		}
	case <-time.After(2 * time.Second):
		// Unblock the handler so the goroutine and the server can finish.
		srv.CloseClientConnections()
		t.Fatal("request not bounded by the Checker's timeout")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	f := newFakeAPI(t, release("v1.0.1", false, false))
	c, _, _ := newChecker(t, f, "1.0.0")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Result, 1)
	done := make(chan struct{})
	go func() {
		c.Run(ctx, func(r Result) { got <- r })
		close(done)
	}()
	select {
	case r := <-got:
		if !r.Available {
			t.Errorf("Run result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not check at start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestLatestReleaseSharedAcrossProcesses covers a second process (the tray
// restarting, or `stv2 doctor` next to it) inside the 24 h window: it must
// learn the release the first process found from prefs, without querying,
// also before that release was announced.
func TestLatestReleaseSharedAcrossProcesses(t *testing.T) {
	f := newFakeAPI(t, release("v1.0.1", false, false))
	dir := t.TempDir()
	st, err := prefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	checker := func(st *prefs.Store) *Checker {
		c := New(st)
		c.URL, c.Current, c.Client, c.Now = f.srv.URL+"/repos/o/r/releases/latest", "1.0.0", f.srv.Client(), clk.now
		return c
	}
	if res, err := checker(st).Check(context.Background()); err != nil || !res.Checked || !res.Available {
		t.Fatalf("first check = %+v, %v", res, err)
	}

	// Another process: prefs re-read from disk, the notice not shown yet.
	clk.t = clk.t.Add(time.Hour)
	st2, err := prefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Get().LatestRelease; got != "v1.0.1" {
		t.Fatalf("prefs LatestRelease = %q, want v1.0.1", got)
	}
	c2 := checker(st2)
	res, err := c2.Check(context.Background())
	if err != nil || res.Checked || !res.Available || !res.Notify || res.Latest.Version != "v1.0.1" || res.Latest.URL != ReleasesPage {
		t.Fatalf("second process = %+v, %v", res, err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 per 24 h across processes", f.hits.Load())
	}
	if err := c2.MarkNotified("v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if res, _ := checker(st2).Check(context.Background()); !res.Available || res.Notify {
		t.Fatalf("after MarkNotified = %+v", res)
	}

	// A running version that caught up hides the recorded release.
	c3 := checker(st2)
	c3.Current = "1.0.1"
	if res, _ := c3.Check(context.Background()); res.Available {
		t.Fatalf("current 1.0.1: %+v", res)
	}
}

// TestLatestReleaseRecording: an HTTP error keeps the release recorded
// before, "no release" clears it.
func TestLatestReleaseRecording(t *testing.T) {
	f := newFakeAPI(t, release("v1.0.1", false, false))
	c, st, clk := newChecker(t, f, "1.0.0")
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.code.Store(403)
	f.body.Store(`{"message":"rate limited"}`)
	clk.t = clk.t.Add(25 * time.Hour)
	if _, err := c.Check(context.Background()); err == nil {
		t.Fatal("403 gave no error")
	}
	if got := st.Get().LatestRelease; got != "v1.0.1" {
		t.Fatalf("after 403: LatestRelease = %q, want v1.0.1 kept", got)
	}

	f.code.Store(404)
	f.body.Store(`{"message":"Not Found"}`)
	clk.t = clk.t.Add(25 * time.Hour)
	if res, err := c.Check(context.Background()); err != nil || res.Available {
		t.Fatalf("404: %+v, %v", res, err)
	}
	if got := st.Get().LatestRelease; got != "" {
		t.Fatalf("after 404: LatestRelease = %q, want cleared", got)
	}
}
