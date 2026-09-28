package stclient

import (
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/applog"
)

// DefaultGUIPort is Syncthing's default GUI and REST port.
const DefaultGUIPort = 8384

// CertFile is the GUI certificate Syncthing keeps next to config.xml.
const CertFile = "https-cert.pem"

// GUIConfig is the part of config.xml's <gui> element SyncThing V2 needs.
// Address is the configured listen address, verbatim (trimmed); Port is its
// port, or DefaultGUIPort when the address has none.
type GUIConfig struct {
	Address                           string
	TLS, InsecureAdminAccess, HasUser bool
	Port                              int
}

type xmlConfig struct {
	GUI struct {
		Enabled             string `xml:"enabled,attr"`
		TLS                 string `xml:"tls,attr"`
		Address             string `xml:"address"`
		APIKey              string `xml:"apikey"`
		User                string `xml:"user"`
		Password            string `xml:"password"`
		AuthMode            string `xml:"authMode"`
		InsecureAdminAccess string `xml:"insecureAdminAccess"`
	} `xml:"gui"`
}

// maxConfigSize bounds how much of config.xml is read; real configs are a few
// hundred KiB at most, and a longer file fails to parse.
const maxConfigSize = 16 << 20

// ReadConfig reads the GUI section of a Syncthing config.xml and returns the
// endpoint to reach its REST API. Wildcard listen addresses (0.0.0.0, ::, *
// or empty), host names and IP addresses this machine does not own all dial
// 127.0.0.1, so the API key is only ever sent to this machine. With
// tls="true" the endpoint trusts only the https-cert.pem stored next to
// config.xml. The API key is registered with applog as a secret, so logs and
// diagnostics never show it.
func ReadConfig(configPath string) (Endpoint, GUIConfig, error) {
	f, err := os.Open(configPath)
	if err != nil {
		return Endpoint{}, GUIConfig{}, fmt.Errorf("read Syncthing config: %w", err)
	}
	defer f.Close()

	var cfg xmlConfig
	dec := xml.NewDecoder(io.LimitReader(f, maxConfigSize))
	if err := dec.Decode(&cfg); err != nil {
		return Endpoint{}, GUIConfig{}, fmt.Errorf("parse Syncthing config %s: %w", filepath.Base(configPath), err)
	}

	gui := cfg.GUI
	if isFalse(gui.Enabled) {
		return Endpoint{}, GUIConfig{}, errors.New("the Syncthing GUI and REST API are disabled in config.xml (<gui enabled=\"false\">)")
	}

	g := GUIConfig{
		Address:             strings.TrimSpace(gui.Address),
		TLS:                 isTrue(gui.TLS),
		InsecureAdminAccess: isTrue(gui.InsecureAdminAccess),
		HasUser: strings.TrimSpace(gui.User) != "" &&
			(strings.TrimSpace(gui.Password) != "" || strings.EqualFold(strings.TrimSpace(gui.AuthMode), "ldap")),
	}

	host, port, err := NormalizeAddress(g.Address)
	if err != nil {
		return Endpoint{}, GUIConfig{}, err
	}
	g.Port = port

	scheme := "http"
	var pool *x509.CertPool
	if g.TLS {
		scheme = "https"
		certPath := filepath.Join(filepath.Dir(configPath), CertFile)
		pem, err := os.ReadFile(certPath)
		if err != nil {
			return Endpoint{}, GUIConfig{}, fmt.Errorf("the Syncthing GUI uses HTTPS but its certificate could not be read: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return Endpoint{}, GUIConfig{}, fmt.Errorf("the Syncthing GUI certificate %s holds no PEM certificate", CertFile)
		}
	}

	ep := Endpoint{
		BaseURL: &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port))},
		APIKey:  strings.TrimSpace(gui.APIKey),
		CAPool:  pool,
	}
	applog.AddSecret(ep.APIKey)
	return ep, g, nil
}

// NormalizeAddress turns a Syncthing GUI listen address into the host and port
// to dial (F2). Wildcards and host names map to 127.0.0.1. An IP literal is
// kept only when it is loopback or assigned to one of this machine's
// interfaces (Syncthing then listens only there); any other IP also maps to
// 127.0.0.1, so the API key never leaves this machine. An empty address or
// one without a port uses DefaultGUIPort.
func NormalizeAddress(addr string) (host string, port int, err error) {
	h, p, err := splitAddress(addr)
	if err != nil {
		return "", 0, err
	}

	port = DefaultGUIPort
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("the Syncthing GUI address %q has an invalid port", addr)
		}
		port = n
	}

	switch h {
	case "", "0.0.0.0", "::", "*":
		return "127.0.0.1", port, nil
	}
	ip, err := netip.ParseAddr(h)
	if err != nil {
		// A host name: only ever dial this machine.
		return "127.0.0.1", port, nil
	}
	if ip.IsUnspecified() || !isLocalIP(ip) {
		return "127.0.0.1", port, nil
	}
	return ip.Unmap().String(), port, nil
}

// interfaceAddrs lists this machine's interface addresses; tests replace it.
var interfaceAddrs = net.InterfaceAddrs

// isLocalIP reports whether ip is a loopback address or one assigned to an
// interface of this machine. If the interfaces cannot be listed, only
// loopback counts as local.
func isLocalIP(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if ip.IsLoopback() {
		return true
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var raw net.IP
		switch v := a.(type) {
		case *net.IPNet:
			raw = v.IP
		case *net.IPAddr:
			raw = v.IP
		default:
			continue
		}
		if have, ok := netip.AddrFromSlice(raw); ok && have.Unmap() == ip {
			return true
		}
	}
	return false
}

// splitAddress splits a GUI listen address into host (without brackets) and
// port (possibly empty). It rejects Unix socket addresses.
func splitAddress(addr string) (host, port string, err error) {
	a := strings.TrimSpace(addr)
	a = strings.TrimPrefix(strings.TrimPrefix(a, "http://"), "https://")
	a = strings.TrimSuffix(a, "/")
	if strings.HasPrefix(a, "/") || strings.HasPrefix(strings.ToLower(a), "unix:") {
		return "", "", fmt.Errorf("the Syncthing GUI address %q is a Unix socket, which is not supported", addr)
	}
	host = a
	if h, p, err := net.SplitHostPort(a); err == nil {
		host, port = h, p
	} else if i := strings.LastIndex(a, ":"); i >= 0 && splitsAt(a[:i]) {
		// "host:" with an empty port, or an unbracketed IPv6 host such as
		// ":::8384" (split at the last colon, as the prototype did).
		host, port = a[:i], a[i+1:]
	}
	return strings.Trim(host, "[]"), port, nil
}

func splitsAt(h string) bool {
	if !strings.Contains(h, ":") {
		return true
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}

func isTrue(s string) bool  { return strings.EqualFold(strings.TrimSpace(s), "true") }
func isFalse(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "false") }
