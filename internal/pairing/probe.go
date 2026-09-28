// Package pairing pairs Syncthing devices over a Tailscale tailnet: it probes
// tailnet peers for their Syncthing device ID, decides which pending devices
// may be shown to the user, writes the device and folder configuration, and
// watches for incoming pairing requests. Nothing is accepted without an
// explicit user action on the machine doing the accepting.
package pairing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"syscall"
	"time"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
)

// SyncPort is Syncthing's default sync protocol listen port, the only port
// probed and configured.
const SyncPort = 22000

// ProbeTimeout bounds one probe when the context has no earlier deadline.
const ProbeTimeout = 3 * time.Second

// Probe errors.
var (
	// ErrNoSyncthing: something answered but it did not present a TLS
	// certificate the way Syncthing does (or the handshake timed out after
	// connecting, or failed).
	ErrNoSyncthing = errors.New("no Syncthing on this address")
	// ErrRefused: the connection was actively refused.
	ErrRefused = errors.New("connection refused")
	// ErrTimeout: no answer within the timeout.
	ErrTimeout = errors.New("connection timed out")
)

// errProbeDone aborts the TLS handshake at the moment the server asks for a
// client certificate. By then the server has proven it holds the private key
// of the certificate it presented (its CertificateVerify signature and its
// Finished message have been checked), and the client has not yet written
// anything of its own. The remote Syncthing therefore never learns a device
// ID from the probe and never records a pending device.
var errProbeDone = errors.New("probe: server identity proven")

// Probe connects to a Syncthing sync listener at ap, reads the server's TLS
// certificate, checks that the server holds the matching private key, and
// returns the device ID derived from the certificate. It never sends a
// client certificate and never completes the handshake with a Syncthing
// server (which always requests one).
func Probe(ctx context.Context, ap netip.AddrPort) (string, error) {
	if !ap.IsValid() {
		return "", ErrNoSyncthing
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
	}

	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", ap.String())
	if err != nil {
		return "", classify(ctx, err)
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}

	var (
		der    []byte
		proved bool
	)
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"bep/1.0"},
		InsecureSkipVerify: true, //nolint:gosec // identity is the certificate hash itself, taken below
		// Called on receipt of the server's Certificate message, before its
		// CertificateVerify signature is checked: only remember the DER.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) > 0 {
				der = append([]byte(nil), rawCerts[0]...)
			}
			return nil
		},
		// In TLS 1.3 this runs only after the server's CertificateVerify and
		// Finished have both been verified, and before the client writes its
		// own Certificate flight. Aborting here proves key possession without
		// ever presenting a client certificate. Certificates stays unset.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			proved = true
			return nil, errProbeDone
		},
	}
	conn := tls.Client(raw, cfg)
	herr := conn.HandshakeContext(ctx)
	// herr == nil: the server asked for no client certificate (Syncthing
	// always does), but the completed handshake still verified its signature.
	if der != nil && (proved || herr == nil) {
		return deviceid.FromCert(der), nil
	}
	if herr == nil {
		// Never report success without a certificate.
		return "", ErrNoSyncthing
	}
	// The TCP connection was already up, so a refusal cannot happen here;
	// anything but a timeout or cancellation means "not Syncthing".
	switch cerr := classify(ctx, herr); {
	case errors.Is(cerr, ErrTimeout), errors.Is(cerr, context.Canceled):
		return "", cerr
	}
	return "", ErrNoSyncthing
}

// classify maps a dial or handshake error to ErrRefused, ErrTimeout or ErrNoSyncthing.
func classify(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if isConnRefused(err) {
		return ErrRefused
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return ErrNoSyncthing
}

// wsaeConnRefused is WSAECONNREFUSED, which Windows reports instead of ECONNREFUSED.
const wsaeConnRefused = syscall.Errno(10061)

func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var en syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &en) && en == wsaeConnRefused
}
