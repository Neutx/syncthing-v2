package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/single"
)

// fullID matches a device ID in its dashed or compact form.
var fullID = regexp.MustCompile(`[A-Z2-7]{7}-[A-Z2-7]{7}|[A-Z2-7]{50,}`)

func synthID(seed string) string {
	return deviceid.FromCert([]byte("synthetic certificate " + seed))
}

// candidates is a synthetic tailnet: this computer, a device of the same
// owner, a device of another user, one without a known owner name, and one
// without Syncthing.
func candidates() []model.Candidate {
	return []model.Candidate{
		{NodeName: "this-laptop", OS: "windows", IP: netip.MustParseAddr("100.64.0.1"), SameOwner: true,
			DeviceID: synthID("self"), Status: pairing.StatusSelf},
		{NodeName: "studio-desktop", DNSName: "studio-desktop.example.ts.net", OS: "linux",
			IP: netip.MustParseAddr("100.64.0.2"), SameOwner: true, DeviceID: synthID("studio"), Status: pairing.StatusReady},
		{NodeName: "shared-nas", OS: "linux", IP: netip.MustParseAddr("100.64.0.3"), LoginName: "friend@example.com",
			DeviceID: synthID("nas"), Status: pairing.StatusPaired},
		{NodeName: "guest-box", OS: "macOS", IP: netip.MustParseAddr("100.64.0.4"),
			DeviceID: synthID("guest"), Status: pairing.StatusBlocked},
		{NodeName: "phone", OS: "iOS", IP: netip.MustParseAddr("100.64.0.5"), SameOwner: true, Status: "no-syncthing"},
	}
}

// assertNoFullIDs fails when out carries more than the 7-character short
// form of any synthetic device ID.
func assertNoFullIDs(t *testing.T, out string) {
	t.Helper()
	if m := fullID.FindString(out); m != "" {
		t.Errorf("output contains a full device ID %q:\n%s", m, out)
	}
	for _, c := range candidates() {
		if len(c.DeviceID) > 8 && strings.Contains(out, c.DeviceID[:8]) {
			t.Errorf("output contains more than the short ID of %s:\n%s", c.NodeName, out)
		}
	}
}

func TestPrintCandidatesTable(t *testing.T) {
	var buf bytes.Buffer
	if code := printCandidates(&buf, candidates(), false); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	out := buf.String()
	assertNoFullIDs(t, out)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want a header and 4 rows (no self row):\n%s", len(lines), out)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "NAME OS IP OWNER ID STATUS" {
		t.Errorf("header = %q", lines[0])
	}
	if strings.Contains(out, "this-laptop") {
		t.Error("the self row is listed")
	}
	want := map[string][]string{
		"studio-desktop": {"linux", "100.64.0.2", "you", synthID("studio")[:7], pairing.StatusReady},
		"shared-nas":     {"linux", "100.64.0.3", "friend@example.com", synthID("nas")[:7], pairing.StatusPaired},
		"guest-box":      {"macOS", "100.64.0.4", "other", synthID("guest")[:7], pairing.StatusBlocked},
		"phone":          {"iOS", "100.64.0.5", "you", "-", "no-syncthing"},
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 6 {
			t.Errorf("row %q has %d columns, want 6", l, len(f))
			continue
		}
		w, ok := want[f[0]]
		if !ok {
			t.Errorf("unexpected row %q", l)
			continue
		}
		if strings.Join(f[1:], " ") != strings.Join(w, " ") {
			t.Errorf("row %q, want %v", l, w)
		}
		if id := f[4]; id != "-" && len(id) != 7 {
			t.Errorf("ID %q is not the 7-character short form", id)
		}
	}
}

func TestPrintCandidatesJSON(t *testing.T) {
	var buf bytes.Buffer
	if code := printCandidates(&buf, candidates(), true); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	assertNoFullIDs(t, buf.String())
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (no self row)", len(rows))
	}
	allowed := map[string]bool{"name": true, "dnsName": true, "os": true, "ip": true, "sameOwner": true, "owner": true, "id": true, "status": true}
	for _, r := range rows {
		for k := range r {
			if !allowed[k] {
				t.Errorf("unexpected key %q in %v", k, r)
			}
		}
		for _, k := range []string{"name", "os", "ip", "sameOwner", "status"} {
			if _, ok := r[k]; !ok {
				t.Errorf("row %v lacks %q", r, k)
			}
		}
		if r["status"] == pairing.StatusSelf {
			t.Errorf("self row listed: %v", r)
		}
		if id, ok := r["id"].(string); ok && len(id) != 7 {
			t.Errorf("id %q is not 7 characters", id)
		}
		_, hasOwner := r["owner"]
		if r["sameOwner"] == true && hasOwner {
			t.Errorf("owner shown for the user's own device: %v", r)
		}
	}
	byName := map[string]map[string]any{}
	for _, r := range rows {
		byName[r["name"].(string)] = r
	}
	if r := byName["shared-nas"]; r["owner"] != "friend@example.com" || r["id"] != synthID("nas")[:7] {
		t.Errorf("shared-nas row = %v", r)
	}
	if _, ok := byName["phone"]["id"]; ok {
		t.Errorf("a device without Syncthing has an id: %v", byName["phone"])
	}

	// Only this computer online: an empty list, never null.
	buf.Reset()
	printCandidates(&buf, candidates()[:1], true)
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Errorf("JSON for no candidates = %q, want []", buf.String())
	}
	buf.Reset()
	printCandidates(&buf, nil, false)
	if !strings.Contains(buf.String(), "No other computers are online") {
		t.Errorf("text for no candidates = %q", buf.String())
	}
}

// TestRunUsageErrors covers argument errors, which must all return
// exitUsage before any command runs.
func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"},
		{"--bogus"},
		{"pair"},
		{"pair", "--bogus"},
		{"pair", "--list", "extra"},
		{"--background", "extra"},
		{"--setup", "extra"},
		{"", "extra"},
		{"doctor", "extra"},
		{"doctor", "--nope"},
		{"install", "extra"},
		{"uninstall", "--nope"},
		{"firewall"},
		{"firewall", "deny"},
		{"firewall", "allow", "extra"},
		{"profile"},
		{"profile", "bogus"},
		{"profile", "tailnet", "extra"},
		{"demo", "--port", "x"},
		{"demo", "extra"},
	} {
		if code := run(args); code != exitUsage {
			t.Errorf("run(%q) = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestVersion(t *testing.T) {
	var buf bytes.Buffer
	if code := cmdVersion(&buf); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(buf.String(), "commit ") {
		t.Errorf("version output lacks the commit: %q", buf.String())
	}
}

// testLock returns an acquire function for a lock no real tray uses.
func testLock(t *testing.T, dir string) func() (*single.Lock, error) {
	name := fmt.Sprintf("stv2-test-quit-%d-%d", os.Getpid(), time.Now().UnixNano())
	return func() (*single.Lock, error) { return single.Acquire(name, dir) }
}

func noSignal(t *testing.T) func(context.Context, string) error {
	return func(context.Context, string) error {
		t.Error("signalled a tray although none holds the lock")
		return nil
	}
}

// TestQuitStaleInstance: a crashed tray left instance.json but no lock
// holder, so --quit reports "not running", exits 0 and removes the file.
func TestQuitStaleInstance(t *testing.T) {
	for name, content := range map[string]string{
		"valid":   "",
		"corrupt": "{",
		"missing": "-",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, single.InstanceFile)
			switch content {
			case "":
				if err := single.WriteInstance(dir, single.Instance{Port: 1, PID: 999999, Token: strings.Repeat("t", 32)}); err != nil {
					t.Fatal(err)
				}
			case "-":
			default:
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			acquire := testLock(t, dir)
			var out bytes.Buffer
			if code := quitTray(&out, dir, acquire, noSignal(t)); code != exitOK {
				t.Fatalf("exit code %d, want %d", code, exitOK)
			}
			if !strings.Contains(out.String(), "is not running") {
				t.Errorf("output = %q", out.String())
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stale %s left behind: %v", single.InstanceFile, err)
			}
			// The lock was released again.
			l, err := acquire()
			if err != nil {
				t.Fatalf("lock not released: %v", err)
			}
			_ = l.Release()
		})
	}
}

// TestQuitRunningTray: with the lock held by a tray, --quit signals it.
func TestQuitRunningTray(t *testing.T) {
	dir := t.TempDir()
	if err := single.WriteInstance(dir, single.Instance{Port: 1, PID: 999999, Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	held := func() (*single.Lock, error) { return nil, single.ErrAlreadyRunning }
	var got []string
	ok := func(_ context.Context, action string) error { got = append(got, action); return nil }
	if code := quitTray(&bytes.Buffer{}, dir, held, ok); code != exitOK {
		t.Errorf("exit code %d, want %d", code, exitOK)
	}
	if len(got) != 1 || got[0] != "quit" {
		t.Errorf("signalled %v, want [quit]", got)
	}
	if _, err := os.Stat(filepath.Join(dir, single.InstanceFile)); err != nil {
		t.Errorf("the running tray's %s was touched: %v", single.InstanceFile, err)
	}

	failing := func(context.Context, string) error { return errors.New("connection refused") }
	if code := quitTray(&bytes.Buffer{}, dir, held, failing); code != exitFail {
		t.Errorf("unanswered signal: exit code %d, want %d", code, exitFail)
	}
	broken := func() (*single.Lock, error) { return nil, errors.New("lock unavailable") }
	if code := quitTray(&bytes.Buffer{}, dir, broken, noSignal(t)); code != exitFail {
		t.Errorf("lock error: exit code %d, want %d", code, exitFail)
	}
}

// TestQuitRealLockHeld uses a real lock held by this test, standing in for
// a running tray.
func TestQuitRealLockHeld(t *testing.T) {
	dir := t.TempDir()
	acquire := testLock(t, dir)
	l, err := acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	var got []string
	sig := func(_ context.Context, action string) error { got = append(got, action); return nil }
	if code := quitTray(&bytes.Buffer{}, dir, acquire, sig); code != exitOK || len(got) != 1 {
		t.Errorf("exit code %d, signals %v; want %d and one quit", code, got, exitOK)
	}
}
