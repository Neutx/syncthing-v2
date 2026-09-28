//go:build e2e

// Package e2e runs two real Syncthing instances on loopback and drives the
// pairing flow of internal/pairing against them (spec §10.3). It is built
// only with the e2e tag:
//
//	go test -tags e2e -count=1 -timeout 15m ./e2e/...
//
// Sandbox rules, so a developer's own Syncthing is never touched:
//
//   - Each instance has its own --home under t.TempDir(), and its child
//     environment points HOME, USERPROFILE, LOCALAPPDATA, APPDATA and the XDG
//     dirs into that sandbox. Every inherited Syncthing variable (ST*) is
//     dropped.
//   - config.xml is rewritten before the first start: GUI on 127.0.0.1 with
//     a sandbox port, sync listeners only on 127.0.0.2 or 127.0.0.3, and
//     global discovery, local discovery, relays, NAT, STUN, usage reporting,
//     crash reporting and auto-upgrade all off.
//   - The sync port is Syncthing's default 22000, because pairing.SyncPort
//     is a protocol constant that Probe and Pair always use. A developer's
//     live Syncthing usually listens on the wildcard address 0.0.0.0:22000.
//     A socket bound to a specific loopback address takes precedence for
//     that address on Linux and Windows. preflight checks this before any
//     instance starts, and it fails with an explanation if another process
//     owns 127.0.0.2:22000 or 127.0.0.3:22000 itself.
//   - Each instance listens on both tcp:// and quic:// at its own address,
//     so the quic:// address that Pair configures (UDP 22000) reaches the
//     sandbox peer and never a wildcard UDP listener of another Syncthing.
//   - GUI ports prefer 18384 and 28384. When one is taken, pickGUIPort
//     picks a free port and logs the adjustment.
//   - Discover probes every candidate. The candidate that plays the "other
//     owner" (127.0.0.4) has no sandbox Syncthing, and on a machine with a
//     wildcard :22000 listener any other 127.x address would reach that
//     listener. So the Services use sandboxProbe, which probes only the two
//     sandbox listeners and reports ErrRefused (the result on an idle
//     address) for everything else.
package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/stinstall"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
)

// Sandbox addresses and the preferred GUI ports (spec §10.3).
var (
	ipA     = netip.MustParseAddr("127.0.0.2")
	ipB     = netip.MustParseAddr("127.0.0.3")
	ipOther = netip.MustParseAddr("127.0.0.4") // the "other owner" node; no Syncthing there
)

const (
	guiPortA = 18384
	guiPortB = 28384

	// waitLimit bounds every "within 60 s" assertion of spec §10.3.
	waitLimit = 60 * time.Second
	// startLimit bounds one instance's start-up (API up, listeners bound).
	startLimit = 90 * time.Second
	// stopLimit is how long a clean shutdown may take before the process is killed.
	stopLimit = 30 * time.Second
)

// loopback is the e2e pairing policy: every sandbox address is "tailnet".
var loopback = pairing.Policy{Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}

// ---------------------------------------------------------------------------
// Preflight and ports

// preflight makes sure the two sandbox sync addresses can be bound on TCP and
// UDP port 22000 and that traffic to them reaches the specific binding. It
// runs before any Syncthing instance starts.
func preflight(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("macOS only configures 127.0.0.1 on lo0; the e2e pairing test runs on Linux and Windows (spec §10.3)")
	}
	for _, ip := range []netip.Addr{ipA, ipB} {
		ap := netip.AddrPortFrom(ip, pairing.SyncPort)
		if err := checkTCP(ap); err != nil {
			t.Fatalf("preflight: TCP %s: %v\nThe pairing protocol uses the fixed port %d (pairing.SyncPort), so the harness cannot move it. Stop the process that owns %s and run the test again.", ap, err, pairing.SyncPort, ap)
		}
		if err := checkUDP(ap); err != nil {
			t.Fatalf("preflight: UDP %s: %v\nThe pairing protocol uses the fixed port %d (pairing.SyncPort), so the harness cannot move it. Stop the process that owns %s and run the test again.", ap, err, pairing.SyncPort, ap)
		}
	}
}

// checkTCP binds ap, connects to it and confirms that the connection arrives
// at this listener. The client closes first, so the listening side never
// holds a TIME_WAIT entry on ap.
func checkTCP(ap netip.AddrPort) error {
	ln, err := net.Listen("tcp", ap.String())
	if err != nil {
		return fmt.Errorf("cannot bind: %w", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	c, err := net.DialTimeout("tcp", ap.String(), 3*time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to own listener: %w", err)
	}
	local := c.LocalAddr().String()
	_ = c.Close()
	select {
	case s, ok := <-accepted:
		if !ok {
			return errors.New("listener failed while accepting")
		}
		defer s.Close()
		if s.RemoteAddr().String() != local {
			return fmt.Errorf("listener accepted %s instead of the test connection from %s", s.RemoteAddr(), local)
		}
		_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.Copy(io.Discard, s) // wait for the client's FIN
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("the connection was taken by another listener (a wildcard socket with exclusive use?)")
	}
}

// checkUDP binds ap and confirms that a datagram sent to it arrives there.
func checkUDP(ap netip.AddrPort) error {
	pc, err := net.ListenPacket("udp", ap.String())
	if err != nil {
		return fmt.Errorf("cannot bind: %w", err)
	}
	defer pc.Close()
	c, err := net.Dial("udp", ap.String())
	if err != nil {
		return fmt.Errorf("cannot address own socket: %w", err)
	}
	defer c.Close()
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := c.Write(token); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if string(buf[:n]) == string(token) {
			return nil
		}
	}
	return errors.New("datagrams to this address do not reach the specific binding")
}

// pickGUIPort returns preferred when 127.0.0.1:preferred is free, and
// otherwise a free port chosen by the OS. The adjustment is logged.
func pickGUIPort(t *testing.T, preferred int) int {
	t.Helper()
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred)); err == nil {
		_ = ln.Close()
		return preferred
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no free loopback port for a sandbox GUI: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	t.Logf("GUI port %d is in use on this machine; the sandbox uses %d instead", preferred, port)
	return port
}

// ---------------------------------------------------------------------------
// Syncthing binary

// syncthingBinary returns the pinned Syncthing (brand.BootstrapSyncthing),
// downloaded and SHA-256 verified through stinstall.Download into a cache
// directory under the system temp dir, and reused by later runs.
func syncthingBinary(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := filepath.Join(os.TempDir(), "stv2-e2e-cache", "syncthing-"+brand.BootstrapSyncthing)
	bin := filepath.Join(dir, stinstall.ExeName())
	if v := stinstall.BinVersion(ctx, bin); v == brand.BootstrapSyncthing {
		return bin
	}
	got, err := stinstall.Download(ctx, dir, nil)
	if err != nil {
		t.Fatalf("download pinned Syncthing %s: %v", brand.BootstrapSyncthing, err)
	}
	if v := stinstall.BinVersion(ctx, got); v != brand.BootstrapSyncthing {
		t.Fatalf("downloaded Syncthing reports version %q, want %s", v, brand.BootstrapSyncthing)
	}
	return got
}

// ---------------------------------------------------------------------------
// Instances

// instance is one sandboxed Syncthing.
type instance struct {
	name   string     // device name and fake tailnet host name
	ip     netip.Addr // sync listener address
	dir    string     // sandbox root (HOME for the child)
	home   string     // Syncthing --home
	id     string     // device ID printed by `syncthing device-id`
	client *stclient.Client
	log    string // Syncthing's stdout and stderr
	cmd    *exec.Cmd
	exited chan struct{} // closed when the process has exited
	err    error         // cmd.Wait result, valid after exited is closed
}

// syncAddr is the instance's sync listener address.
func (in *instance) syncAddr() netip.AddrPort {
	return netip.AddrPortFrom(in.ip, pairing.SyncPort)
}

// listenAddresses are the only sync listeners the instance may open.
func (in *instance) listenAddresses() []string {
	ap := in.syncAddr().String()
	return []string{"tcp://" + ap, "quic://" + ap}
}

// startInstance generates a sandbox home for name, rewrites its config and
// starts `syncthing serve`. The instance is shut down when the test ends.
func startInstance(t *testing.T, bin, root, name string, ip netip.Addr, guiPort int) *instance {
	t.Helper()
	in := &instance{name: name, ip: ip, dir: filepath.Join(root, name)}
	in.home = filepath.Join(in.dir, "syncthing-home")
	in.log = filepath.Join(in.dir, "syncthing.log")
	if err := os.MkdirAll(in.home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := sandboxEnv(in.dir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := runSyncthing(ctx, bin, env, "generate", "--home", in.home, "--no-port-probing"); err != nil {
		t.Fatalf("%s: syncthing generate: %v\n%s", name, err, out)
	}
	out, err := runSyncthing(ctx, bin, env, "device-id", "--home", in.home)
	if err != nil {
		t.Fatalf("%s: syncthing device-id: %v\n%s", name, err, out)
	}
	id, err := deviceid.Parse(strings.TrimSpace(lastLine(out)))
	if err != nil {
		t.Fatalf("%s: syncthing device-id printed %q: %v", name, out, err)
	}
	in.id = id

	cfgPath := filepath.Join(in.home, stclient.ConfigFile)
	if err := sandboxConfig(cfgPath, in, guiPort); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	ep, _, err := stclient.ReadConfig(cfgPath)
	if err != nil {
		t.Fatalf("%s: read sandbox config: %v", name, err)
	}
	if ep.BaseURL.Host != fmt.Sprintf("127.0.0.1:%d", guiPort) {
		t.Fatalf("%s: sandbox GUI address is %s, want 127.0.0.1:%d", name, ep.BaseURL.Host, guiPort)
	}
	in.client = stclient.New(ep)

	logf, err := os.Create(in.log)
	if err != nil {
		t.Fatal(err)
	}
	in.cmd = exec.Command(bin, "serve", "--home", in.home, "--no-browser", "--no-restart", "--no-upgrade")
	in.cmd.Env = env
	in.cmd.Dir = in.dir
	in.cmd.Stdout = logf
	in.cmd.Stderr = logf
	if err := in.cmd.Start(); err != nil {
		_ = logf.Close()
		t.Fatalf("%s: start syncthing: %v", name, err)
	}
	in.exited = make(chan struct{})
	go func() {
		in.err = in.cmd.Wait()
		_ = logf.Close()
		close(in.exited)
	}()
	t.Cleanup(func() { in.stop(t) })

	in.waitReady(t)
	return in
}

// sandboxEnv returns the child environment: the current one without any
// Syncthing variable, with every per-user location moved into dir.
func sandboxEnv(dir string) []string {
	moved := map[string]string{
		"HOME":            dir,
		"USERPROFILE":     dir,
		"LOCALAPPDATA":    filepath.Join(dir, "AppData", "Local"),
		"APPDATA":         filepath.Join(dir, "AppData", "Roaming"),
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"XDG_STATE_HOME":  filepath.Join(dir, ".local", "state"),
		"XDG_DATA_HOME":   filepath.Join(dir, ".local", "share"),
		"XDG_CACHE_HOME":  filepath.Join(dir, ".cache"),
	}
	env := make([]string, 0, len(os.Environ())+len(moved)+2)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := moved[strings.ToUpper(k)]; ok || isSyncthingVar(k) {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range moved {
		env = append(env, k+"="+v)
	}
	return append(env, "STNOUPGRADE=1", "STNODEFAULTFOLDER=1")
}

// isSyncthingVar reports Syncthing's environment variables (STHOMEDIR,
// STGUIADDRESS, STNORESTART, ...): upper-case names starting with "ST".
func isSyncthingVar(k string) bool {
	return len(k) > 2 && strings.HasPrefix(k, "ST") && k == strings.ToUpper(k)
}

func runSyncthing(ctx context.Context, bin string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// configEdits are the <options> values every sandbox needs before its first
// start. Each element must occur exactly once in the generated config.
var configEdits = []struct{ tag, value string }{
	{"globalAnnounceEnabled", "false"},
	{"localAnnounceEnabled", "false"},
	{"relaysEnabled", "false"},
	{"natEnabled", "false"},
	{"startBrowser", "false"},
	{"urAccepted", "-1"},
	{"autoUpgradeIntervalH", "0"},
	{"crashReportingEnabled", "false"},
	{"stunKeepaliveStartS", "0"},
}

var (
	reListen  = regexp.MustCompile(`([ \t]*)<listenAddress>[^<]*</listenAddress>`)
	reGUIAddr = regexp.MustCompile(`(<gui\b[^>]*>\s*<address>)[^<]*(</address>)`)
)

// sandboxConfig rewrites a freshly generated config.xml for in. It fails
// when any expected element is missing or repeated, so an instance can
// never start with upstream's network defaults.
func sandboxConfig(path string, in *instance, guiPort int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read generated config: %w", err)
	}
	cfg := string(b)
	replaceOnce := func(re *regexp.Regexp, what string, repl func([]string) string) error {
		m := re.FindAllStringSubmatchIndex(cfg, -1)
		if len(m) != 1 {
			return fmt.Errorf("generated config has %d %s elements, want exactly 1", len(m), what)
		}
		groups := make([]string, len(m[0])/2)
		for i := range groups {
			if m[0][2*i] >= 0 {
				groups[i] = cfg[m[0][2*i]:m[0][2*i+1]]
			}
		}
		cfg = cfg[:m[0][0]] + repl(groups) + cfg[m[0][1]:]
		return nil
	}

	for _, e := range configEdits {
		re := regexp.MustCompile(`<` + e.tag + `>[^<]*</` + e.tag + `>`)
		v := e.value
		if err := replaceOnce(re, "<"+e.tag+">", func([]string) string { return "<" + e.tag + ">" + v + "</" + e.tag + ">" }); err != nil {
			return err
		}
	}
	if err := replaceOnce(reListen, "<listenAddress>", func(g []string) string {
		var sb strings.Builder
		for i, a := range in.listenAddresses() {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(g[1] + "<listenAddress>" + a + "</listenAddress>")
		}
		return sb.String()
	}); err != nil {
		return err
	}
	if err := replaceOnce(reGUIAddr, "<gui><address>", func(g []string) string {
		return fmt.Sprintf("%s127.0.0.1:%d%s", g[1], guiPort, g[2])
	}); err != nil {
		return err
	}
	// The generated device name is the machine's host name; use the sandbox name.
	reSelf := regexp.MustCompile(`(<device id="` + regexp.QuoteMeta(in.id) + `" name=")[^"]*(")`)
	if err := replaceOnce(reSelf, "own <device>", func(g []string) string { return g[1] + in.name + g[2] }); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(cfg), 0o600)
}

// waitReady waits until the REST API answers, then checks that the running
// options are the sandbox ones and that both sync listeners are up.
func (in *instance) waitReady(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	type listener struct {
		Error        *string  `json:"error"`
		LANAddresses []string `json:"lanAddresses"`
	}
	var st struct {
		MyID                    string              `json:"myID"`
		ConnectionServiceStatus map[string]listener `json:"connectionServiceStatus"`
	}
	var last error
	for {
		select {
		case <-in.exited:
			t.Fatalf("%s: syncthing exited during start-up (%v)\n%s", in.name, in.err, in.logTail(60))
		case <-ctx.Done():
			t.Fatalf("%s: not ready after %s: %v\n%s", in.name, startLimit, last, in.logTail(60))
		case <-time.After(300 * time.Millisecond):
		}
		if last = in.client.Get(ctx, "/rest/system/status", &st); last != nil {
			continue
		}
		if st.MyID != in.id {
			t.Fatalf("%s: REST reports device ID %s, `syncthing device-id` printed %s", in.name, st.MyID, in.id)
		}
		up := 0
		for _, a := range in.listenAddresses() {
			l, ok := st.ConnectionServiceStatus[a]
			switch {
			case ok && l.Error != nil && *l.Error != "":
				t.Fatalf("%s: listener %s failed: %s\n%s", in.name, a, *l.Error, in.logTail(60))
			case ok && len(l.LANAddresses) > 0:
				up++
			}
		}
		if up == len(in.listenAddresses()) {
			break
		}
		last = fmt.Errorf("%d of %d listeners up", up, len(in.listenAddresses()))
	}

	var opts struct {
		ListenAddresses       []string `json:"listenAddresses"`
		GlobalAnnounceEnabled bool     `json:"globalAnnounceEnabled"`
		LocalAnnounceEnabled  bool     `json:"localAnnounceEnabled"`
		RelaysEnabled         bool     `json:"relaysEnabled"`
		NATEnabled            bool     `json:"natEnabled"`
		AutoUpgradeIntervalH  int      `json:"autoUpgradeIntervalH"`
	}
	if err := in.client.Get(ctx, "/rest/config/options", &opts); err != nil {
		t.Fatalf("%s: read options: %v", in.name, err)
	}
	if strings.Join(opts.ListenAddresses, ",") != strings.Join(in.listenAddresses(), ",") ||
		opts.GlobalAnnounceEnabled || opts.LocalAnnounceEnabled || opts.RelaysEnabled || opts.NATEnabled || opts.AutoUpgradeIntervalH != 0 {
		t.Fatalf("%s: running options are not the sandbox options: %+v", in.name, opts)
	}
}

// stop shuts the instance down through the API and kills it if it does not
// exit in time. On a failed test it logs the tail of Syncthing's output.
func (in *instance) stop(t *testing.T) {
	select {
	case <-in.exited:
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := in.client.Send(ctx, http.MethodPost, "/rest/system/shutdown", nil)
		cancel()
		if err != nil {
			t.Logf("%s: shutdown request: %v", in.name, err)
		}
		select {
		case <-in.exited:
		case <-time.After(stopLimit):
			t.Logf("%s: no exit %s after shutdown; killing it", in.name, stopLimit)
			_ = in.cmd.Process.Kill()
			<-in.exited
		}
	}
	if t.Failed() {
		t.Logf("%s: last lines of Syncthing's output:\n%s", in.name, in.logTail(80))
	}
}

func (in *instance) logTail(n int) string {
	f, err := os.Open(in.log)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}

// certID derives the device ID from the instance's cert.pem.
func (in *instance) certID(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(in.home, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatalf("%s: cert.pem holds no certificate", in.name)
	}
	return deviceid.FromCert(blk.Bytes)
}

// ---------------------------------------------------------------------------
// REST views used by the assertions

type pendingDevice struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (in *instance) pendingDevices(ctx context.Context) (map[string]pendingDevice, error) {
	var out map[string]pendingDevice
	err := in.client.Get(ctx, "/rest/cluster/pending/devices", &out)
	return out, err
}

type deviceConfig struct {
	DeviceID          string   `json:"deviceID"`
	Name              string   `json:"name"`
	Addresses         []string `json:"addresses"`
	Compression       string   `json:"compression"`
	Introducer        bool     `json:"introducer"`
	AutoAcceptFolders bool     `json:"autoAcceptFolders"`
}

func (in *instance) devices(ctx context.Context) ([]deviceConfig, error) {
	var out []deviceConfig
	err := in.client.Get(ctx, "/rest/config/devices", &out)
	return out, err
}

func (in *instance) device(ctx context.Context, id string) (deviceConfig, error) {
	var out deviceConfig
	err := in.client.Get(ctx, "/rest/config/devices/"+url.PathEscape(id), &out)
	return out, err
}

type folderConfig struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Devices []struct {
		DeviceID string `json:"deviceID"`
	} `json:"devices"`
}

func (in *instance) folder(ctx context.Context, id string) (folderConfig, error) {
	var out folderConfig
	err := in.client.Get(ctx, "/rest/config/folders/"+url.PathEscape(id), &out)
	return out, err
}

func (in *instance) connected(ctx context.Context, id string) (bool, error) {
	var out struct {
		Connections map[string]struct {
			Connected bool `json:"connected"`
		} `json:"connections"`
	}
	if err := in.client.Get(ctx, "/rest/system/connections", &out); err != nil {
		return false, err
	}
	return out.Connections[id].Connected, nil
}

func (in *instance) scan(ctx context.Context, folderID string) error {
	return in.client.Send(ctx, http.MethodPost, "/rest/db/scan?"+url.Values{"folder": {folderID}}.Encode(), nil)
}

// waitFor polls cond every 500 ms until it reports true, failing the test
// after waitLimit with the last error or state it returned.
func waitFor(t *testing.T, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	var last error
	for {
		ok, err := cond()
		if ok && err == nil {
			return
		}
		last = err
		if time.Now().After(deadline) {
			if last != nil {
				t.Fatalf("%s: not reached within %s: %v", what, waitLimit, last)
			}
			t.Fatalf("%s: not reached within %s", what, waitLimit)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Fake tailnet

// fakeTailnet is a tailnet.Source with a fixed status.
type fakeTailnet struct{ st tailnet.Status }

func (f fakeTailnet) Status(context.Context) (tailnet.Status, error) { return f.st, nil }

// node is a fake tailnet node owned by user.
func node(name string, ip netip.Addr, user int64, login string) tailnet.Node {
	return tailnet.Node{
		ID:        "node-" + name,
		HostName:  name,
		DNSName:   name + ".tailnet.example",
		OS:        "linux",
		LoginName: login,
		IPs:       []netip.Addr{ip},
		Online:    true,
		UserID:    user,
	}
}

// view is the tailnet as seen from self.
func view(self tailnet.Node, peers ...tailnet.Node) tailnet.Status {
	return tailnet.Status{BackendState: tailnet.BackendRunning, Self: self, Peers: peers}
}

// sandboxProbe probes only the given sandbox listeners and reports
// ErrRefused for any other address without dialling it (see the package
// comment).
func sandboxProbe(allowed ...netip.AddrPort) func(context.Context, netip.AddrPort) (string, error) {
	return func(ctx context.Context, ap netip.AddrPort) (string, error) {
		for _, a := range allowed {
			if a == ap {
				return pairing.Probe(ctx, ap)
			}
		}
		return "", pairing.ErrRefused
	}
}

// randomText returns a short random string for file contents.
func randomText(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
