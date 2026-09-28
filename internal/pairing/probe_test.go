package pairing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"math/big"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

var (
	live         = flag.Bool("live", false, "run TestLiveProbe against the Syncthing running on this machine (read-only)")
	syncthingBin = flag.String("syncthing-bin", "", "syncthing binary for TestLiveProbe (default: PATH, then the per-user install location)")
)

// selfSignedCert returns a Syncthing-style self-signed certificate.
func selfSignedCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// bepServer mimics a Syncthing sync listener: TLS 1.3, ALPN bep/1.0, and a
// client certificate required. It records every client certificate it ever
// receives.
type bepServer struct {
	addr        netip.AddrPort
	der         []byte
	hellos      atomic.Int32
	clientCerts atomic.Int32
	completed   atomic.Int32
	results     chan error
}

func newBEPServer(t *testing.T) *bepServer {
	t.Helper()
	cert := selfSignedCert(t, "syncthing")
	s := &bepServer{der: cert.Certificate[0], results: make(chan error, 16)}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"bep/1.0"},
		ClientAuth:   tls.RequireAnyClientCert,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			s.hellos.Add(1)
			return nil, nil
		},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			s.clientCerts.Add(int32(len(rawCerts)))
			return nil
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.addr = netip.MustParseAddrPort(ln.Addr().String())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				tc := c.(*tls.Conn)
				herr := tc.Handshake()
				if herr == nil {
					s.completed.Add(1)
					s.clientCerts.Add(int32(len(tc.ConnectionState().PeerCertificates)))
				}
				s.results <- herr
			}(c)
		}
	}()
	return s
}

func (s *bepServer) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("server handshake did not finish")
		return nil
	}
}

func TestProbeNeverSendsClientCertificate(t *testing.T) {
	srv := newBEPServer(t)
	id, err := Probe(context.Background(), srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	if want := deviceid.FromCert(srv.der); id != want {
		t.Fatalf("Probe = %s, want %s", deviceid.Short(id), deviceid.Short(want))
	}
	if herr := srv.wait(t); herr == nil {
		t.Fatal("the server-side handshake completed; the probe must abort it")
	}
	if n := srv.clientCerts.Load(); n != 0 {
		t.Fatalf("server received %d client certificates, want 0", n)
	}
	if n := srv.completed.Load(); n != 0 {
		t.Fatalf("%d handshakes completed", n)
	}
	if n := srv.hellos.Load(); n != 1 {
		t.Fatalf("server saw %d ClientHellos, want 1", n)
	}
}

// The server's certificate counter must actually detect a client
// certificate, or the test above would pass vacuously.
func TestBEPServerDetectsClientCertificate(t *testing.T) {
	srv := newBEPServer(t)
	cert := selfSignedCert(t, "client")
	c, err := tls.Dial("tcp", srv.addr.String(), &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"bep/1.0"},
		InsecureSkipVerify: true, //nolint:gosec // test client
		Certificates:       []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exchange data so the TLS 1.3 client certificate flight is processed.
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte{0})
	c.Close()
	if herr := srv.wait(t); herr != nil {
		t.Fatalf("control handshake failed: %v", herr)
	}
	if n := srv.clientCerts.Load(); n < 1 {
		t.Fatalf("server counted %d client certificates for a client that sent one", n)
	}
}

// A listener that replays someone else's certificate cannot sign the TLS 1.3
// CertificateVerify for it, so the probe must not return that certificate's
// device ID.
func TestProbeRejectsCertificateWithoutKey(t *testing.T) {
	victim := selfSignedCert(t, "syncthing")
	impostor := selfSignedCert(t, "syncthing")
	replayed := tls.Certificate{Certificate: victim.Certificate, PrivateKey: impostor.PrivateKey}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{replayed},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"bep/1.0"},
		ClientAuth:   tls.RequireAnyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	id, err := Probe(context.Background(), netip.MustParseAddrPort(ln.Addr().String()))
	if !errors.Is(err, ErrNoSyncthing) {
		t.Fatalf("Probe(replayed certificate) = %q, %v; want ErrNoSyncthing", deviceid.Short(id), err)
	}
}

// A TLS 1.3 server that requests no client certificate completes the
// handshake, which also verified its signature, so its ID is accepted.
func TestProbeServerWithoutClientAuth(t *testing.T) {
	cert := selfSignedCert(t, "syncthing")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	id, err := Probe(context.Background(), netip.MustParseAddrPort(ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if want := deviceid.FromCert(cert.Certificate[0]); id != want {
		t.Fatalf("Probe = %s, want %s", deviceid.Short(id), deviceid.Short(want))
	}
}

func TestProbeRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Probe(ctx, addr); !errors.Is(err, ErrRefused) {
		t.Fatalf("Probe(closed port) = %v, want ErrRefused", err)
	}
}

func TestProbeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // accept, then never answer
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Probe(ctx, netip.MustParseAddrPort(ln.Addr().String())); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Probe(silent server) = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Probe took %v despite a 300 ms deadline", d)
	}
}

func TestProbeNotSyncthing(t *testing.T) {
	// A plain TCP service that speaks first and hangs up.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-Synthetic\r\n"))
			c.Close()
		}
	}()
	if _, err := Probe(context.Background(), netip.MustParseAddrPort(ln.Addr().String())); !errors.Is(err, ErrNoSyncthing) {
		t.Fatalf("Probe(non-TLS) = %v, want ErrNoSyncthing", err)
	}

	// A TLS 1.2-only server fails version negotiation.
	cert := selfSignedCert(t, "old")
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer tln.Close()
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	if _, err := Probe(context.Background(), netip.MustParseAddrPort(tln.Addr().String())); !errors.Is(err, ErrNoSyncthing) {
		t.Fatalf("Probe(TLS 1.2 server) = %v, want ErrNoSyncthing", err)
	}

	if _, err := Probe(context.Background(), netip.AddrPort{}); !errors.Is(err, ErrNoSyncthing) {
		t.Fatalf("Probe(invalid) = %v", err)
	}
}

func TestProbeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := newBEPServer(t)
	if _, err := Probe(ctx, srv.addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe(canceled) = %v", err)
	}
}

// TestLiveProbe is a read-only manual check against the Syncthing running on
// this machine: go test ./internal/pairing -run TestLiveProbe -live
// It probes 127.0.0.1:22000, compares the result with `syncthing device-id`,
// and verifies that the local pending-device list did not change. Only the
// short (7-character) IDs are printed.
func TestLiveProbe(t *testing.T) {
	if !*live {
		t.Skip("manual check; run with -live")
	}
	bin := findSyncthing(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ep, _, err := stclient.Discover(ctx, bin)
	if err != nil {
		t.Fatalf("discover the local Syncthing: %v", err)
	}
	c := stclient.New(ep)
	pendingCount := func() int {
		var m map[string]any
		if err := c.Get(ctx, "/rest/cluster/pending/devices", &m); err != nil {
			t.Fatalf("read pending devices: %v", err)
		}
		return len(m)
	}
	before := pendingCount()

	probed, err := Probe(ctx, netip.MustParseAddrPort("127.0.0.1:22000"))
	if err != nil {
		t.Fatalf("Probe(127.0.0.1:22000): %v", err)
	}
	want := cliDeviceID(t, ctx, bin)
	t.Logf("probe: %s  syncthing device-id: %s", deviceid.Short(probed), deviceid.Short(want))
	if probed != want {
		t.Fatalf("probe returned %s, syncthing device-id says %s", deviceid.Short(probed), deviceid.Short(want))
	}

	time.Sleep(2 * time.Second) // give Syncthing time to (wrongly) record a pending device
	after := pendingCount()
	t.Logf("pending devices before: %d, after: %d", before, after)
	if after != before {
		t.Fatalf("pending device count changed from %d to %d after the probe", before, after)
	}
}

func findSyncthing(t *testing.T) string {
	t.Helper()
	if *syncthingBin != "" {
		return *syncthingBin
	}
	if p, err := exec.LookPath("syncthing"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			p := filepath.Join(la, "Programs", "Syncthing", "syncthing.exe")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	t.Fatal("syncthing binary not found; pass -syncthing-bin")
	return ""
}

// cliDeviceID runs `syncthing device-id` (v2), falling back to
// `syncthing --device-id` (v1). Both only read the local certificate.
func cliDeviceID(t *testing.T, ctx context.Context, bin string) string {
	t.Helper()
	for _, args := range [][]string{{"device-id"}, {"--device-id"}} {
		out, err := exec.CommandContext(ctx, bin, args...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
			if id, err := deviceid.Parse(strings.TrimSpace(line)); err == nil {
				return id
			}
		}
	}
	t.Fatal("could not read the device ID from syncthing")
	return ""
}
