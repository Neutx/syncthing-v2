package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/prefs"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/update"
)

// Synthetic identities only: documentation addresses and generated IDs.
const (
	selfID = "AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	peerID = "ZZZZZZZ-YYYYYYY-XXXXXXX-WWWWWWW-VVVVVVV-UUUUUUU-TTTTTTT-SSSSSSS"
)

var (
	peerA = netip.MustParseAddr("100.64.0.2")
	peerB = netip.MustParseAddr("100.64.0.3")
	other = netip.MustParseAddr("100.64.0.4")
)

// fakeAPI answers REST GETs from a table of JSON bodies or errors.
type fakeAPI struct {
	bodies map[string]string
	errs   map[string]error
}

func (f fakeAPI) Get(_ context.Context, path string, out any) error {
	if err, ok := f.errs[path]; ok {
		return err
	}
	body, ok := f.bodies[path]
	if !ok {
		return &stclient.Error{Kind: stclient.ErrBadResponse, Status: 404}
	}
	return json.Unmarshal([]byte(body), out)
}

type probeLog struct {
	mu    sync.Mutex
	addrs []string
}

func tsStatus() tailnet.Status {
	return tailnet.Status{
		BackendState: "Running",
		Self:         tailnet.Node{HostName: "desk-example", OS: "windows", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}, Online: true, UserID: 1},
		Peers: []tailnet.Node{
			{HostName: "laptop-example", OS: "windows", IPs: []netip.Addr{peerA, netip.MustParseAddr("fd7a:115c:a1e0::2")}, Online: true, UserID: 1},
			{HostName: "mini-example", OS: "macOS", IPs: []netip.Addr{peerB}, Online: true, UserID: 1},
			{HostName: "friend-example", OS: "linux", IPs: []netip.Addr{other}, Online: true, UserID: 2},                // other owner: not probed
			{HostName: "offline-example", OS: "linux", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.5")}, UserID: 1}, // offline
			{HostName: "server-example", OS: "linux", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.6")}, Online: true, UserID: 1, Tags: []string{"tag:server"}},
			{HostName: "phone-example", OS: "iOS", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.7")}, Online: true, UserID: 1},
		},
	}
}

// healthy returns an Env in which every check passes, plus the probe log.
func healthy(goos string) (Env, *probeLog) {
	pl := &probeLog{}
	base, _ := url.Parse("http://127.0.0.1:8384")
	ep := stclient.Endpoint{BaseURL: base, APIKey: "synthetic-key"}
	return Env{
		GOOS: goos,
		Syncthing: func(context.Context) (stinstall.Install, error) {
			return stinstall.Install{Bin: "/fake/syncthing", Version: "v2.1.5"}, nil
		},
		Discover: func(_ context.Context, bin string) (stclient.Endpoint, string, error) {
			if bin != "/fake/syncthing" {
				return stclient.Endpoint{}, "", fmt.Errorf("unexpected bin %q", bin)
			}
			return ep, "/fake/home/config.xml", nil
		},
		ReadConfig: func(string) (stclient.Endpoint, stclient.GUIConfig, error) {
			return ep, stclient.GUIConfig{Address: "127.0.0.1:8384", Port: 8384}, nil
		},
		API: func(stclient.Endpoint) API {
			return fakeAPI{bodies: map[string]string{
				"/rest/system/status":  `{"myID":"` + selfID + `"}`,
				"/rest/config/options": `{"listenAddresses":["default"]}`,
			}}
		},
		Tailscale: func(context.Context) (tailnet.Status, error) { return tsStatus(), nil },
		Probe: func(_ context.Context, ap netip.AddrPort) (string, error) {
			pl.mu.Lock()
			pl.addrs = append(pl.addrs, ap.String())
			pl.mu.Unlock()
			return peerID, nil
		},
		FirewallRule:   func(context.Context) (bool, error) { return true, nil },
		WebView2:       func() bool { return true },
		StatusNotifier: func() bool { return true },
		Update:         func(context.Context) (update.Result, error) { return update.Result{Checked: true}, nil },
	}, pl
}

func statuses(rep Report) map[string]string {
	m := map[string]string{}
	for _, c := range rep.Checks {
		m[c.Code] = c.Status
	}
	return m
}

func codesOf(rep Report) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.Code)
	}
	return out
}

func TestHealthy(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		env, pl := healthy(goos)
		rep := Run(context.Background(), env)
		if len(rep.Findings) != 0 || !rep.OK || rep.ExitCode() != 0 {
			t.Fatalf("%s: findings %+v", goos, rep.Findings)
		}
		st := statuses(rep)
		wantSkip := map[string][]string{"windows": {"UI002"}, "linux": {"FW001", "UI001"}, "darwin": {"FW001", "UI001", "UI002"}}[goos]
		for _, c := range Codes {
			want := StatusPass
			if slices.Contains(wantSkip, c.Code) {
				want = StatusSkip
			}
			if st[c.Code] != want {
				t.Errorf("%s: %s = %s, want %s", goos, c.Code, st[c.Code], want)
			}
		}
		sort.Strings(pl.addrs)
		if !slices.Equal(pl.addrs, []string{"100.64.0.2:22000", "100.64.0.3:22000"}) {
			t.Errorf("%s: probed %q, want only the online same-owner desktop peers on IPv4:22000", goos, pl.addrs)
		}
	}
}

// oneCode runs env and checks that exactly the wanted code was found with
// its table severity and that the message contains want.
func oneCode(t *testing.T, env Env, code, want string) Report {
	t.Helper()
	rep := Run(context.Background(), env)
	if got := codesOf(rep); !slices.Equal(got, []string{code}) {
		t.Fatalf("findings = %q, want [%s]: %+v", got, code, rep.Findings)
	}
	f := rep.Findings[0]
	var sev string
	for _, c := range Codes {
		if c.Code == code {
			sev = c.Severity
		}
	}
	if f.Severity != sev || f.Docs != DocsURL(code) || !strings.HasSuffix(f.Docs, "/docs/troubleshooting.md#"+strings.ToLower(code)) {
		t.Errorf("%s: finding %+v", code, f)
	}
	if !strings.Contains(f.Message, want) {
		t.Errorf("%s: message %q does not contain %q", code, f.Message, want)
	}
	if wantExit := map[bool]int{true: 1, false: 0}[sev == High]; rep.ExitCode() != wantExit {
		t.Errorf("%s: exit code %d, want %d", code, rep.ExitCode(), wantExit)
	}
	if statuses(rep)[code] != StatusFail {
		t.Errorf("%s: status %s", code, statuses(rep)[code])
	}
	return rep
}

func TestTS001(t *testing.T) {
	env, pl := healthy("windows")
	env.Tailscale = func(context.Context) (tailnet.Status, error) { return tailnet.Status{}, tailnet.ErrNotInstalled }
	rep := oneCode(t, env, "TS001", "tailscale.com/download")
	if st := statuses(rep); st["TS002"] != StatusSkip || st["NET001"] != StatusSkip {
		t.Errorf("dependent checks: %v", st)
	}
	if len(pl.addrs) != 0 {
		t.Error("probed without Tailscale")
	}
}

func TestTS002(t *testing.T) {
	env, _ := healthy("windows")
	env.Tailscale = func(context.Context) (tailnet.Status, error) {
		return tailnet.Status{BackendState: "NeedsLogin"}, nil
	}
	oneCode(t, env, "TS002", "NeedsLogin")

	env.Tailscale = func(context.Context) (tailnet.Status, error) {
		return tailnet.Status{}, errors.New("backend not reachable")
	}
	oneCode(t, env, "TS002", "backend not reachable")
}

func TestST001(t *testing.T) {
	env, _ := healthy("windows")
	env.Syncthing = func(context.Context) (stinstall.Install, error) { return stinstall.Install{}, stinstall.ErrNotFound }
	rep := oneCode(t, env, "ST001", "Syncthing was not found")
	st := statuses(rep)
	for _, c := range []string{"ST002", "ST003", "ST004", "ST005", "SEC001"} {
		if st[c] != StatusSkip {
			t.Errorf("%s = %s, want skip", c, st[c])
		}
	}
}

func TestST002(t *testing.T) {
	env, _ := healthy("windows")
	env.API = func(stclient.Endpoint) API {
		return fakeAPI{errs: map[string]error{"/rest/system/status": &stclient.Error{Kind: stclient.ErrUnreachable, Err: errors.New("connection refused")}}}
	}
	rep := oneCode(t, env, "ST002", "not responding on 127.0.0.1:8384")
	if st := statuses(rep); st["ST003"] != StatusSkip || st["ST005"] != StatusSkip {
		t.Errorf("dependent checks: %v", st)
	}

	env, _ = healthy("windows")
	env.Discover = func(context.Context, string) (stclient.Endpoint, string, error) {
		return stclient.Endpoint{}, "", fmt.Errorf("lookup: %w", stclient.ErrConfigNotFound)
	}
	rep = oneCode(t, env, "ST002", "no configuration yet")
	if statuses(rep)["SEC001"] != StatusSkip {
		t.Error("SEC001 must be skipped without a config")
	}

	env, _ = healthy("windows")
	env.API = func(stclient.Endpoint) API {
		return fakeAPI{errs: map[string]error{"/rest/system/status": &stclient.Error{Kind: stclient.ErrBadResponse, Status: 500}}}
	}
	oneCode(t, env, "ST002", "unexpected response")
}

func TestST003(t *testing.T) {
	env, _ := healthy("windows")
	env.API = func(stclient.Endpoint) API {
		return fakeAPI{errs: map[string]error{"/rest/system/status": &stclient.Error{Kind: stclient.ErrUnauthorized, Status: 403}}}
	}
	rep := oneCode(t, env, "ST003", "rejected the API key")
	if st := statuses(rep); st["ST002"] != StatusPass || st["ST005"] != StatusSkip {
		t.Errorf("statuses: %v", st)
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Message, "synthetic-key") {
			t.Fatal("the API key leaked into the report")
		}
	}
}

func TestST004(t *testing.T) {
	env, _ := healthy("windows")
	env.Syncthing = func(context.Context) (stinstall.Install, error) {
		return stinstall.Install{Bin: "/fake/syncthing", Version: "v1.26.0"}, nil
	}
	oneCode(t, env, "ST004", "v1.26.0")

	// `syncthing --version` gave nothing: the running Syncthing's REST answer
	// decides, in both directions.
	for restVersion, want := range map[string][]string{"v2.1.5": nil, "v1.26.0": {"ST004"}} {
		env, _ = healthy("windows")
		env.Syncthing = func(context.Context) (stinstall.Install, error) {
			return stinstall.Install{Bin: "/fake/syncthing"}, nil
		}
		env.API = func(stclient.Endpoint) API {
			return fakeAPI{bodies: map[string]string{
				"/rest/system/status":  `{"myID":"` + selfID + `"}`,
				"/rest/system/version": `{"version":"` + restVersion + `","os":"windows","arch":"amd64"}`,
				"/rest/config/options": `{"listenAddresses":["default"]}`,
			}}
		}
		rep := Run(context.Background(), env)
		if got := codesOf(rep); !slices.Equal(got, want) {
			t.Errorf("REST version %s: findings = %q, want %q", restVersion, got, want)
		}
		if want != nil && !strings.Contains(rep.Findings[0].Message, restVersion) {
			t.Errorf("REST version %s: message %q", restVersion, rep.Findings[0].Message)
		}
	}

	// Neither source knows the version while Syncthing answers: ST004 says so.
	env, _ = healthy("windows")
	env.Syncthing = func(context.Context) (stinstall.Install, error) {
		return stinstall.Install{Bin: "/fake/syncthing"}, nil
	}
	oneCode(t, env, "ST004", "version is unknown")

	// Syncthing is not answering (ST002 or ST003): the unknown version has
	// the same cause, so ST004 is skipped with a reason, not reported again.
	unknown := func(context.Context) (stinstall.Install, error) {
		return stinstall.Install{Bin: "/fake/syncthing"}, nil
	}
	notAnswering := map[string]func(*Env){
		"ST002 unreachable": func(e *Env) {
			e.API = func(stclient.Endpoint) API {
				return fakeAPI{errs: map[string]error{"/rest/system/status": &stclient.Error{Kind: stclient.ErrUnreachable, Err: errors.New("connection refused")}}}
			}
		},
		"ST002 no config": func(e *Env) {
			e.Discover = func(context.Context, string) (stclient.Endpoint, string, error) {
				return stclient.Endpoint{}, "", fmt.Errorf("lookup: %w", stclient.ErrConfigNotFound)
			}
		},
		"ST003": func(e *Env) {
			e.API = func(stclient.Endpoint) API {
				return fakeAPI{errs: map[string]error{"/rest/system/status": &stclient.Error{Kind: stclient.ErrUnauthorized, Status: 403}}}
			}
		},
	}
	for name, breakIt := range notAnswering {
		env, _ = healthy("windows")
		env.Syncthing = unknown
		breakIt(&env)
		rep := Run(context.Background(), env)
		want := []string{name[:5]}
		if got := codesOf(rep); !slices.Equal(got, want) {
			t.Errorf("%s: findings = %q, want %q", name, got, want)
		}
		for _, c := range rep.Checks {
			if c.Code == "ST004" && (c.Status != StatusSkip || !strings.Contains(c.Reason, "not answering")) {
				t.Errorf("%s: ST004 = %+v, want a skip because Syncthing is not answering", name, c)
			}
		}
	}

	// A known but too old version is still reported when Syncthing is down.
	env, _ = healthy("windows")
	env.Syncthing = func(context.Context) (stinstall.Install, error) {
		return stinstall.Install{Bin: "/fake/syncthing", Version: "v1.26.0"}, nil
	}
	notAnswering["ST002 unreachable"](&env)
	if got := codesOf(Run(context.Background(), env)); !slices.Equal(got, []string{"ST002", "ST004"}) {
		t.Errorf("old version, Syncthing down: findings = %q, want [ST002 ST004]", got)
	}
}

func TestST005(t *testing.T) {
	env, _ := healthy("windows")
	env.API = func(stclient.Endpoint) API {
		return fakeAPI{bodies: map[string]string{
			"/rest/system/status":  `{"myID":"` + selfID + `"}`,
			"/rest/config/options": `{"listenAddresses":["tcp://0.0.0.0:22001","quic://0.0.0.0:22001","dynamic+https://relays.example"]}`,
		}}
	}
	oneCode(t, env, "ST005", "tcp://0.0.0.0:22001")
}

func TestListensOnDefault(t *testing.T) {
	cases := []struct {
		addrs []string
		want  bool
	}{
		{[]string{"default"}, true},
		{[]string{"tcp://:22000"}, true},
		{[]string{"quic://[::]:22000"}, true},
		{[]string{"tcp4://0.0.0.0:22000"}, true},
		{[]string{"tcp://0.0.0.0:22001", "dynamic+https://relays.example:22000"}, false},
		{nil, false},
		{[]string{"%zz"}, false},
	}
	for _, c := range cases {
		if got := listensOnDefault(c.addrs); got != c.want {
			t.Errorf("listensOnDefault(%q) = %v", c.addrs, got)
		}
	}
}

func TestSEC001(t *testing.T) {
	env, _ := healthy("windows")
	env.ReadConfig = func(string) (stclient.Endpoint, stclient.GUIConfig, error) {
		return stclient.Endpoint{}, stclient.GUIConfig{Address: "0.0.0.0:8384", Port: 8384}, nil
	}
	oneCode(t, env, "SEC001", "no password")

	env.ReadConfig = func(string) (stclient.Endpoint, stclient.GUIConfig, error) {
		return stclient.Endpoint{}, stclient.GUIConfig{Address: "127.0.0.1:8384", Port: 8384, InsecureAdminAccess: true}, nil
	}
	oneCode(t, env, "SEC001", "insecureAdminAccess")
}

func TestNET001(t *testing.T) {
	env, _ := healthy("windows")
	env.Probe = func(_ context.Context, ap netip.AddrPort) (string, error) {
		switch ap.Addr() {
		case peerA:
			return "", fmt.Errorf("dial: %w", pairing.ErrRefused)
		case peerB:
			return "", pairing.ErrTimeout
		case other:
			t.Error("probed another owner's device")
		}
		return peerID, nil
	}
	rep := Run(context.Background(), env)
	if got := codesOf(rep); !slices.Equal(got, []string{"NET001", "NET001"}) {
		t.Fatalf("findings = %q", got)
	}
	msgs := rep.Findings[0].Message + "\n" + rep.Findings[1].Message
	for _, want := range []string{"laptop-example (100.64.0.2:22000)", "refused", "mini-example (100.64.0.3:22000)", "timed out"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if !rep.OK || rep.ExitCode() != 0 || rep.Summary.Warn != 2 {
		t.Errorf("NET001 is a warning: %+v", rep.Summary)
	}
}

func TestFW001(t *testing.T) {
	env, _ := healthy("windows")
	env.FirewallRule = func(context.Context) (bool, error) { return false, nil }
	oneCode(t, env, "FW001", "firewall allow")

	env.FirewallRule = func(context.Context) (bool, error) { return false, errors.New("netsh missing") }
	rep := Run(context.Background(), env)
	if len(rep.Findings) != 0 || statuses(rep)["FW001"] != StatusSkip {
		t.Errorf("unreadable firewall: %+v", rep.Checks)
	}

	env, _ = healthy("linux")
	env.FirewallRule = func(context.Context) (bool, error) { t.Error("FW001 checked off Windows"); return false, nil }
	Run(context.Background(), env)
}

func TestUI001(t *testing.T) {
	env, _ := healthy("windows")
	env.WebView2 = func() bool { return false }
	oneCode(t, env, "UI001", "WebView2")

	env, _ = healthy("darwin")
	env.WebView2 = func() bool { return false }
	if rep := Run(context.Background(), env); len(rep.Findings) != 0 {
		t.Errorf("UI001 reported off Windows: %+v", rep.Findings)
	}
}

func TestUI002(t *testing.T) {
	env, _ := healthy("linux")
	env.StatusNotifier = func() bool { return false }
	oneCode(t, env, "UI002", "AppIndicator")

	env, _ = healthy("windows")
	env.StatusNotifier = func() bool { return false }
	if rep := Run(context.Background(), env); len(rep.Findings) != 0 {
		t.Errorf("UI002 reported off Linux: %+v", rep.Findings)
	}
}

func TestUPD001(t *testing.T) {
	env, _ := healthy("windows")
	env.Update = func(context.Context) (update.Result, error) {
		return update.Result{Available: true, Latest: update.Release{Version: "v1.0.1", URL: "https://github.com/Neutx/syncthing-v2/releases/tag/v1.0.1"}}, nil
	}
	oneCode(t, env, "UPD001", "v1.0.1 is available")

	for _, err := range []error{ErrUpdatesOff, ErrNoPrefs, errors.New("offline")} {
		env.Update = func(context.Context) (update.Result, error) { return update.Result{}, err }
		rep := Run(context.Background(), env)
		if len(rep.Findings) != 0 || statuses(rep)["UPD001"] != StatusSkip {
			t.Errorf("%v: %+v", err, rep.Checks)
		}
		if err == ErrNoPrefs {
			for _, c := range rep.Checks {
				if c.Code == "UPD001" && !strings.Contains(c.Reason, "no preferences yet") {
					t.Errorf("UPD001 reason = %q", c.Reason)
				}
			}
		}
	}
}

// TestUpdateFrom covers the default UPD001 source: no GitHub query before the
// first launch or with checks off, and the tray's recorded result within the
// 24 h window.
func TestUpdateFrom(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"tag_name":"v99.0.0","html_url":"https://github.com/o/r/releases/tag/v99.0.0"}`)
	}))
	defer srv.Close()
	newChecker := func(st *prefs.Store) *update.Checker {
		c := update.New(st)
		c.URL, c.Client, c.Current = srv.URL, srv.Client(), "1.0.0"
		return c
	}
	ctx := context.Background()

	// Before the first launch: skipped, nothing created, no query.
	dir := t.TempDir()
	if _, err := updateFrom(ctx, dir, newChecker); !errors.Is(err, ErrNoPrefs) {
		t.Fatalf("no prefs: err = %v, want ErrNoPrefs", err)
	}
	if _, err := os.Stat(filepath.Join(dir, prefs.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created %s: %v", prefs.FileName, err)
	}

	st, err := prefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(p *prefs.Prefs) error { p.CheckForUpdates = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := updateFrom(ctx, dir, newChecker); !errors.Is(err, ErrUpdatesOff) {
		t.Fatalf("checks off: err = %v, want ErrUpdatesOff", err)
	}

	// The tray checked an hour ago and found v1.0.1 but has not announced
	// it: doctor reports it without querying.
	if err := st.Update(func(p *prefs.Prefs) error {
		p.CheckForUpdates = true
		p.LastUpdateCheck = time.Now().Add(-time.Hour)
		p.LatestRelease = "v1.0.1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := updateFrom(ctx, dir, newChecker)
	if err != nil || !res.Available || res.Latest.Version != "v1.0.1" {
		t.Fatalf("within 24 h: %+v, %v", res, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("GitHub queried %d times, want 0", hits.Load())
	}

	// After 24 h one query runs and is recorded.
	if err := st.Update(func(p *prefs.Prefs) error { p.LastUpdateCheck = time.Now().Add(-25 * time.Hour); return nil }); err != nil {
		t.Fatal(err)
	}
	if res, err := updateFrom(ctx, dir, newChecker); err != nil || !res.Checked || res.Latest.Version != "v99.0.0" {
		t.Fatalf("after 24 h: %+v, %v", res, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("GitHub queried %d times, want 1", hits.Load())
	}
}

func TestEmptyEnvSkipsEverything(t *testing.T) {
	rep := Run(context.Background(), Env{GOOS: "windows"})
	if len(rep.Findings) != 0 || !rep.OK {
		t.Fatalf("findings from an empty env: %+v", rep.Findings)
	}
	for _, c := range rep.Checks {
		if c.Status != StatusSkip || c.Reason == "" {
			t.Errorf("%s = %+v, want a skip with a reason", c.Code, c)
		}
	}
}

// TestJSONSchema pins the JSON layout: field names, order and types. A
// change here is a breaking change for scripts that parse `doctor --json`.
func TestJSONSchema(t *testing.T) {
	env, _ := healthy("linux")
	env.StatusNotifier = func() bool { return false }
	env.Syncthing = func(context.Context) (stinstall.Install, error) { return stinstall.Install{}, stinstall.ErrNotFound }
	rep := Run(context.Background(), env)
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if got, want := sortedKeys(top), []string{"checks", "findings", "ok", "os", "schema", "summary", "version"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %q, want %q", got, want)
	}
	// Key order in the document is part of the contract for readers that
	// diff reports.
	order := []string{`"schema"`, `"version"`, `"os"`, `"ok"`, `"summary"`, `"findings"`, `"checks"`}
	last := -1
	for _, k := range order {
		i := strings.Index(buf.String(), k)
		if i <= last {
			t.Fatalf("key %s out of order in:\n%s", k, buf.String())
		}
		last = i
	}

	var doc struct {
		Schema   int               `json:"schema"`
		OK       bool              `json:"ok"`
		OS       string            `json:"os"`
		Summary  map[string]int    `json:"summary"`
		Findings []json.RawMessage `json:"findings"`
		Checks   []json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != 1 || doc.OK || doc.OS != "linux" || !reflect.DeepEqual(doc.Summary, map[string]int{"high": 1, "warn": 1, "info": 0}) {
		t.Fatalf("doc = %+v", doc)
	}
	var f map[string]any
	if err := json.Unmarshal(doc.Findings[0], &f); err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(f), []string{"code", "docs", "message", "severity", "title"}; !slices.Equal(got, want) {
		t.Fatalf("finding keys = %q, want %q", got, want)
	}
	if f["code"] != "ST001" || f["severity"] != "high" {
		t.Fatalf("first finding = %v", f)
	}
	if len(doc.Checks) != len(Codes) {
		t.Fatalf("%d checks, want one per code (%d)", len(doc.Checks), len(Codes))
	}
	for i, raw := range doc.Checks {
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		keys := sortedKeys(c)
		if !slices.Equal(keys, []string{"code", "status"}) && !slices.Equal(keys, []string{"code", "reason", "status"}) {
			t.Errorf("check keys = %q", keys)
		}
		if c["code"] != Codes[i].Code {
			t.Errorf("check %d = %v, want %s", i, c["code"], Codes[i].Code)
		}
	}

	// No findings still yields an empty array, never null.
	env, _ = healthy("windows")
	buf.Reset()
	if err := Run(context.Background(), env).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"findings": []`) {
		t.Errorf("empty findings not an array:\n%s", buf.String())
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestCodesTable(t *testing.T) {
	want := []string{"TS001", "TS002", "ST001", "ST002", "ST003", "ST004", "ST005", "SEC001", "NET001", "FW001", "UI001", "UI002", "UPD001"}
	sev := map[string]string{"TS001": High, "TS002": High, "ST001": High, "ST002": High, "ST003": High, "ST004": Warn, "ST005": Info,
		"SEC001": High, "NET001": Warn, "FW001": Info, "UI001": Warn, "UI002": Warn, "UPD001": Info}
	var got []string
	for _, c := range Codes {
		got = append(got, c.Code)
		if c.Severity != sev[c.Code] || c.Title == "" {
			t.Errorf("%s: severity %s, title %q", c.Code, c.Severity, c.Title)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("codes = %q", got)
	}
}

func TestWriteText(t *testing.T) {
	env, _ := healthy("windows")
	env.Tailscale = func(context.Context) (tailnet.Status, error) { return tailnet.Status{}, tailnet.ErrNotInstalled }
	env.FirewallRule = func(context.Context) (bool, error) { return false, nil }
	var buf bytes.Buffer
	if err := Run(context.Background(), env).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"HIGH  TS001", "INFO  FW001", "troubleshooting.md#ts001", "2 problems found (1 high, 0 warn, 1 info).", "skipped."} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("line too long (%d): %q", len(line), line)
		}
	}

	env, _ = healthy("windows")
	buf.Reset()
	_ = Run(context.Background(), env).WriteText(&buf)
	if !strings.Contains(buf.String(), "No problems found. 12 checks passed, 1 skipped.") {
		t.Errorf("healthy text:\n%s", buf.String())
	}
}
