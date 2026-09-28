package stclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
)

func TestNormalizeAddress(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"0.0.0.0:8384", "127.0.0.1", 8384, false},
		{"[::]:9999", "127.0.0.1", 9999, false},
		{":::9998", "127.0.0.1", 9998, false},
		{"*:8385", "127.0.0.1", 8385, false},
		{"", "127.0.0.1", 8384, false},
		{"   ", "127.0.0.1", 8384, false},
		{":8386", "127.0.0.1", 8386, false},
		{"0.0.0.0", "127.0.0.1", 8384, false},
		{"0.0.0.0:", "127.0.0.1", 8384, false},
		{"127.0.0.1:18384", "127.0.0.1", 18384, false},
		{"127.0.0.2:8384", "127.0.0.2", 8384, false},
		// An IP this machine does not own never receives the API key.
		{"192.0.2.10:8384", "127.0.0.1", 8384, false},
		{"192.0.2.10", "127.0.0.1", 8384, false},
		{"[2001:db8::10]:8384", "127.0.0.1", 8384, false},
		{"[::1]:8384", "::1", 8384, false},
		{"::1", "::1", 8384, false},
		{"localhost:8384", "127.0.0.1", 8384, false},
		{"example.invalid:8390", "127.0.0.1", 8390, false},
		{"http://127.0.0.1:8384/", "127.0.0.1", 8384, false},
		{"127.0.0.1:abc", "", 0, true},
		{"127.0.0.1:0", "", 0, true},
		{"127.0.0.1:65536", "", 0, true},
		{"/var/run/syncthing.sock", "", 0, true},
		{"unix:///run/st.sock", "", 0, true},
	}
	for _, c := range cases {
		h, p, err := NormalizeAddress(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeAddress(%q) = %q, %d; want error", c.in, h, p)
			}
			continue
		}
		if err != nil || h != c.wantHost || p != c.wantPort {
			t.Errorf("NormalizeAddress(%q) = %q, %d, %v; want %q, %d", c.in, h, p, err, c.wantHost, c.wantPort)
		}
	}
}

func TestReadConfigWildcard(t *testing.T) {
	ep, g, err := ReadConfig(filepath.Join("testdata", "wildcard.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ep.BaseURL.String(); got != "http://127.0.0.1:18384" {
		t.Errorf("BaseURL = %q", got)
	}
	if ep.APIKey != "synthetic-api-key-0123456789abcd" {
		t.Errorf("APIKey = %q", ep.APIKey)
	}
	if ep.CAPool != nil {
		t.Error("CAPool set without TLS")
	}
	want := GUIConfig{Address: "0.0.0.0:18384", InsecureAdminAccess: true, Port: 18384}
	if g != want {
		t.Errorf("GUIConfig = %+v, want %+v", g, want)
	}
}

func TestReadConfigLoopbackWithUser(t *testing.T) {
	ep, g, err := ReadConfig(filepath.Join("testdata", "loopback-auth.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL.String() != "http://127.0.0.1:28384" || ep.APIKey != "synthetic-key-with-spaces" {
		t.Errorf("endpoint = %s %q", ep.BaseURL, ep.APIKey)
	}
	if !g.HasUser || g.InsecureAdminAccess || g.TLS || g.Port != 28384 {
		t.Errorf("GUIConfig = %+v", g)
	}
}

// fakeInterfaces makes the given CIDRs this machine's interface addresses
// for one test.
func fakeInterfaces(t *testing.T, cidrs ...string) {
	t.Helper()
	old := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = old })
	interfaceAddrs = func() ([]net.Addr, error) {
		out := make([]net.Addr, 0, len(cidrs))
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			n.IP = ip
			out = append(out, n)
		}
		return out, nil
	}
}

func TestReadConfigLANAddressWithoutPort(t *testing.T) {
	fakeInterfaces(t, "192.0.2.10/24", "fe80::1/64")
	ep, g, err := ReadConfig(filepath.Join("testdata", "lan-noport.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL.String() != "http://192.0.2.10:8384" || g.Port != 8384 || g.HasUser {
		t.Errorf("endpoint %s, gui %+v", ep.BaseURL, g)
	}
}

func TestNormalizeAddressKeepsOnlyLocalIPs(t *testing.T) {
	fakeInterfaces(t, "192.0.2.10/24", "2001:db8::5/64", "fe80::1/64")
	for in, want := range map[string]string{
		"192.0.2.10:8384":          "192.0.2.10",
		"[::ffff:192.0.2.10]:8384": "192.0.2.10",
		"[2001:db8::5]:8384":       "2001:db8::5",
		"[fe80::1%eth0]:8384":      "fe80::1%eth0",
		"192.0.2.11:8384":          "127.0.0.1", // same subnet, but not this machine
		"198.51.100.7:8384":        "127.0.0.1",
		"[2001:db8::6]:8384":       "127.0.0.1",
		"127.0.0.1:8384":           "127.0.0.1",
	} {
		if h, _, err := NormalizeAddress(in); err != nil || h != want {
			t.Errorf("NormalizeAddress(%q) = %q, %v; want %q", in, h, err, want)
		}
	}

	// A remote GUI address in config.xml never gets the API key.
	_, path := writeConfig(t, `<gui enabled="true"><address>198.51.100.7:8384</address><apikey>remote-synthetic-key</apikey></gui>`)
	ep, g, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL.String() != "http://127.0.0.1:8384" || g.Address != "198.51.100.7:8384" {
		t.Errorf("remote address: endpoint %s, gui %+v", ep.BaseURL, g)
	}

	// When the interfaces cannot be listed, only loopback is trusted.
	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("synthetic failure") }
	if h, _, _ := NormalizeAddress("192.0.2.10:8384"); h != "127.0.0.1" {
		t.Errorf("unlisted interfaces: host %q, want 127.0.0.1", h)
	}
	if h, _, _ := NormalizeAddress("[::1]:8384"); h != "::1" {
		t.Errorf("unlisted interfaces: host %q, want ::1", h)
	}
}

func TestReadConfigRegistersAPIKeyAsSecret(t *testing.T) {
	const key = "BareConfigKey-Synthetic-4Rf8Tg2Yh6" // synthetic; gitleaks:allow
	_, path := writeConfig(t, `<gui enabled="true"><address>127.0.0.1:18384</address><apikey>`+key+`</apikey></gui>`)
	if _, _, err := ReadConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := applog.Redact("dial with " + key + " failed"); got != "dial with "+applog.Redacted+" failed" {
		t.Errorf("applog.Redact = %q; the API key was not registered", got)
	}
}

func TestReadConfigErrors(t *testing.T) {
	for _, name := range []string{"tls-nocert.xml", "disabled.xml", "broken.xml", "missing.xml"} {
		if _, _, err := ReadConfig(filepath.Join("testdata", name)); err == nil {
			t.Errorf("ReadConfig(%s) succeeded, want error", name)
		}
	}
}

// writeConfig writes a synthetic config.xml into a new temp dir.
func writeConfig(t *testing.T, gui string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, ConfigFile)
	body := "<configuration version=\"37\">\n" + gui + "\n</configuration>\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTLSWithPinnedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "synthetic-tls-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"myID":"SYNTHET-IC00000"}`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	dir, cfg := writeConfig(t, `<gui enabled="true" tls="true"><address>`+host+`</address><apikey>synthetic-tls-key</apikey></gui>`)
	if err := os.WriteFile(filepath.Join(dir, CertFile), pemCert(srv.Certificate().Raw), 0o600); err != nil {
		t.Fatal(err)
	}

	ep, g, err := ReadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !g.TLS || ep.CAPool == nil || ep.BaseURL.Scheme != "https" {
		t.Fatalf("TLS not picked up: %+v %s", g, ep.BaseURL)
	}
	var st struct {
		MyID string `json:"myID"`
	}
	if err := New(ep).Get(context.Background(), "/rest/system/status", &st); err != nil {
		t.Fatalf("Get over pinned TLS: %v", err)
	}
	if st.MyID != "SYNTHET-IC00000" {
		t.Errorf("myID = %q", st.MyID)
	}

	// A pool holding some other certificate must not trust the server.
	other := x509.NewCertPool()
	other.AddCert(selfSigned(t))
	ep.CAPool = other
	err = New(ep).Get(context.Background(), "/rest/system/status", nil)
	if k, ok := KindOf(err); !ok || k != ErrUnreachable {
		t.Fatalf("Get with foreign pool = %v, want ErrUnreachable", err)
	}
}

func selfSigned(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "syncthing"},
		DNSNames:     []string{"syncthing"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAudit(t *testing.T) {
	cases := []struct {
		name string
		g    GUIConfig
		want bool
	}{
		{"loopback v4", GUIConfig{Address: "127.0.0.1:8384"}, false},
		{"loopback v6", GUIConfig{Address: "[::1]:8384"}, false},
		{"localhost", GUIConfig{Address: "localhost:8384"}, false},
		{"wildcard no password", GUIConfig{Address: "0.0.0.0:8384"}, true},
		{"v6 wildcard no password", GUIConfig{Address: "[::]:8384"}, true},
		{"empty address no password", GUIConfig{Address: ""}, true},
		{"LAN no password", GUIConfig{Address: "192.0.2.10:8384"}, true},
		{"host name no password", GUIConfig{Address: "example.invalid:8384"}, true},
		{"wildcard with password", GUIConfig{Address: "0.0.0.0:8384", HasUser: true}, false},
		{"loopback insecure admin", GUIConfig{Address: "127.0.0.1:8384", InsecureAdminAccess: true}, true},
		{"wildcard password insecure admin", GUIConfig{Address: "0.0.0.0:8384", HasUser: true, InsecureAdminAccess: true}, true},
	}
	for _, c := range cases {
		f := Audit(c.g)
		if !c.want {
			if len(f) != 0 {
				t.Errorf("%s: findings %+v, want none", c.name, f)
			}
			continue
		}
		if len(f) != 1 || f[0].Code != "SEC001" || f[0].Severity != "high" || f[0].Text == "" {
			t.Errorf("%s: findings %+v, want one SEC001 high", c.name, f)
		}
	}

	both := Audit(GUIConfig{Address: "0.0.0.0:8384", InsecureAdminAccess: true})
	if len(both) != 1 || !strings.Contains(both[0].Text, "0.0.0.0:8384") || !strings.Contains(both[0].Text, "insecureAdminAccess") {
		t.Errorf("combined finding = %+v", both)
	}
}
