package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// The test binary doubles as a fake tailscale CLI: when helperEnv is set it
// prints the named fixture and exits with the given code.
const (
	helperEnv     = "STV2_TAILNET_TEST_HELPER_FIXTURE"
	helperExitEnv = "STV2_TAILNET_TEST_HELPER_EXIT"
)

func TestMain(m *testing.M) {
	if fixture := os.Getenv(helperEnv); fixture != "" {
		if len(os.Args) < 3 || os.Args[1] != "status" || os.Args[2] != "--json" {
			fmt.Fprintf(os.Stderr, "unexpected args %q", os.Args[1:])
			os.Exit(64)
		}
		if fixture != "-" {
			data, err := os.ReadFile(fixture)
			if err != nil {
				fmt.Fprint(os.Stderr, err)
				os.Exit(65)
			}
			os.Stdout.Write(data)
		}
		fmt.Fprint(os.Stderr, "helper stderr")
		code, _ := strconv.Atoi(os.Getenv(helperExitEnv))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func load(t *testing.T, name string) Status {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	st, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func names(ns []Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.HostName
	}
	return out
}

func TestParseRunning(t *testing.T) {
	st := load(t, "status-running.json")
	if !st.Running() || st.BackendState != "Running" {
		t.Fatalf("BackendState = %q", st.BackendState)
	}
	if st.Self.HostName != "example-desk" || st.Self.UserID != 1001 || st.Self.LoginName != "owner@example.com" {
		t.Fatalf("Self = %+v", st.Self)
	}
	if st.Self.DNSName != "example-desk.tailnet-example.ts.net" {
		t.Errorf("Self.DNSName = %q: trailing dot not trimmed", st.Self.DNSName)
	}
	if len(st.Peers) != 7 {
		t.Fatalf("got %d peers, want 7", len(st.Peers))
	}
	friend, ok := st.FindByIP(netip.MustParseAddr("100.64.0.8"))
	if !ok || friend.HostName != "friend-pc" || friend.LoginName != "friend@example.org" || friend.UserID != 2002 {
		t.Fatalf("friend = %+v, %v", friend, ok)
	}
	if st.SameOwner(friend) {
		t.Error("friend-pc reported as same owner")
	}
	if friend.DNSName != "friend-pc.tailnet-example.ts.net" {
		t.Errorf("DNSName = %q", friend.DNSName)
	}
	sharee, _ := st.FindByIP(netip.MustParseAddr("100.64.0.5"))
	if !sharee.Sharee || sharee.LoginName != "sharer@example.net" {
		t.Errorf("sharee = %+v", sharee)
	}
	tagged, _ := st.FindByIP(netip.MustParseAddr("100.64.0.6"))
	if len(tagged.Tags) != 1 || tagged.Tags[0] != "tag:server" {
		t.Errorf("tagged = %+v", tagged)
	}
	mac, _ := st.FindByIP(netip.MustParseAddr("fd7a:115c:a1e0::3"))
	if ip, ok := mac.IPv4(); !ok || ip != netip.MustParseAddr("100.64.0.3") {
		t.Errorf("mac IPv4 = %v, %v (IPv6 listed first)", ip, ok)
	}
	if ip, _ := mac.PreferredIP(); ip != netip.MustParseAddr("100.64.0.3") {
		t.Errorf("mac PreferredIP = %v", ip)
	}
}

func TestFindByIP(t *testing.T) {
	st := load(t, "status-running.json")
	// This computer's own addresses never resolve to a node.
	if n, ok := st.FindByIP(netip.MustParseAddr("100.64.0.1")); ok {
		t.Errorf("self lookup = %+v, want no match", n)
	}
	if !st.SelfHasIP(netip.MustParseAddr("::ffff:100.64.0.1")) || st.SelfHasIP(netip.MustParseAddr("100.64.0.2")) || st.SelfHasIP(netip.Addr{}) {
		t.Error("SelfHasIP matched the wrong addresses")
	}
	// IPv4-mapped IPv6 input is unmapped before the lookup.
	if n, ok := st.FindByIP(netip.MustParseAddr("::ffff:100.64.0.2")); !ok || n.HostName != "example-laptop" {
		t.Errorf("mapped lookup = %+v, %v", n, ok)
	}
	for _, ip := range []string{"100.64.0.99", "192.168.1.2"} {
		if n, ok := st.FindByIP(netip.MustParseAddr(ip)); ok {
			t.Errorf("FindByIP(%s) = %+v", ip, n)
		}
	}
	if _, ok := st.FindByIP(netip.Addr{}); ok {
		t.Error("FindByIP(invalid) matched")
	}
}

func TestCandidates(t *testing.T) {
	st := load(t, "status-running.json")
	got := names(st.Candidates())
	// Offline, sharee, tagged and iOS nodes are excluded; same owner first,
	// sorted case-insensitively; then other owners.
	want := []string{"example-laptop", "Example-Mac", "friend-pc"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
}

func TestParseNeedsLogin(t *testing.T) {
	st := load(t, "status-needslogin.json")
	if st.Running() || st.BackendState != "NeedsLogin" {
		t.Fatalf("BackendState = %q", st.BackendState)
	}
	if len(st.Peers) != 0 || len(st.Candidates()) != 0 {
		t.Fatalf("peers = %v", st.Peers)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "not json", "{}", `{"Peer":{}}`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
}

func TestPrefixes(t *testing.T) {
	in := []string{"100.64.0.1", "100.127.255.254", "fd7a:115c:a1e0::1", "fd7a:115c:a1e0:ffff::1"}
	out := []string{"100.63.255.255", "100.128.0.0", "192.168.1.1", "10.0.0.1", "fd7a:115c:a1e1::1", "127.0.0.1"}
	contains := func(s string) bool {
		ip := netip.MustParseAddr(s)
		for _, p := range Prefixes {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, s := range in {
		if !contains(s) {
			t.Errorf("%s should be inside the tailnet prefixes", s)
		}
	}
	for _, s := range out {
		if contains(s) {
			t.Errorf("%s should be outside the tailnet prefixes", s)
		}
	}
}

func helperCLI(t *testing.T, fixture string, exit int) CLI {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if fixture != "-" {
		fixture, err = filepath.Abs(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(helperEnv, fixture)
	t.Setenv(helperExitEnv, strconv.Itoa(exit))
	return CLI{Path: exe}
}

func TestCLIStatus(t *testing.T) {
	st, err := helperCLI(t, "status-running.json", 0).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running() || len(st.Peers) != 7 {
		t.Fatalf("status = %+v", st)
	}
}

func TestCLIStatusNonZeroExitWithJSON(t *testing.T) {
	st, err := helperCLI(t, "status-needslogin.json", 1).Status(context.Background())
	if err != nil {
		t.Fatalf("valid JSON with a non-zero exit should parse: %v", err)
	}
	if st.BackendState != "NeedsLogin" {
		t.Fatalf("BackendState = %q", st.BackendState)
	}
}

func TestCLIStatusFailure(t *testing.T) {
	_, err := helperCLI(t, "-", 1).Status(context.Background())
	if err == nil {
		t.Fatal("expected an error when the CLI fails without output")
	}
	if _, err := (CLI{}).Status(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("empty Path: %v", err)
	}
}

func TestLocateFromPATH(t *testing.T) {
	dir := t.TempDir()
	name := "tailscale"
	if runtime.GOOS == "windows" {
		name = "tailscale.exe"
	}
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE")
	}
	got, err := Locate()
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, fake) {
		t.Fatalf("Locate = %q, want %q", got, fake)
	}
}

func TestLocateKnownPathFallback(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing", "tailscale")
	present := filepath.Join(dir, "known", "tailscale")
	if err := os.MkdirAll(filepath.Dir(present), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(present, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	got, err := locate(notOnPath, []string{missing, dir, present})
	if err != nil || got != present {
		t.Fatalf("locate = %q, %v; want %q (directories are skipped)", got, err, present)
	}
	if _, err := locate(notOnPath, []string{missing}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("locate with nothing present: %v", err)
	}
}

func TestKnownPaths(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "ProgramFiles":
			return `C:\Program Files`
		case "ProgramW6432":
			return `C:\Program Files`
		}
		return ""
	}
	win := knownPaths("windows", env)
	if runtime.GOOS == "windows" {
		if len(win) != 1 || win[0] != `C:\Program Files\Tailscale\tailscale.exe` {
			t.Errorf("windows = %v", win)
		}
	}
	if len(knownPaths("windows", func(string) string { return "" })) != 0 {
		t.Error("windows paths without %ProgramFiles% should be empty")
	}
	mac := knownPaths("darwin", env)
	if len(mac) != 3 || mac[0] != "/Applications/Tailscale.app/Contents/MacOS/Tailscale" {
		t.Errorf("darwin = %v", mac)
	}
	lin := knownPaths("linux", env)
	if fmt.Sprint(lin) != "[/usr/bin/tailscale /usr/local/bin/tailscale /snap/bin/tailscale]" {
		t.Errorf("linux = %v", lin)
	}
}

func samePath(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}
