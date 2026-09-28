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

// errProbeDone aborts the TLS handshake right after the server certificate
// arrives, before the client could send any certificate of its own. The
// remote Syncthing therefore never learns a device ID from the probe and
// never records a pending device.
var errProbeDone = errors.New("probe: server certificate captured")

// Probe connects to a Syncthing sync listener at ap, reads the server's TLS
// certificate and returns the device ID derived from it. It never sends a
// client certificate and never completes the handshake.
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

	var der []byte
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"bep/1.0"},
		InsecureSkipVerify: true, //nolint:gosec // identity is the certificate hash itself, taken below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) > 0 {
				der = append([]byte(nil), rawCerts[0]...)
			}
			return errProbeDone
		},
		// Certificates and GetClientCertificate are deliberately left unset.
	}
	conn := tls.Client(raw, cfg)
	herr := conn.HandshakeContext(ctx)
	if der != nil {
		return deviceid.FromCert(der), nil
	}
	if herr == nil {
		// Unreachable with VerifyPeerCertificate returning an error, but
		// never report success without a certificate.
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
