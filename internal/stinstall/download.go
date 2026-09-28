package stinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// ReleaseBaseURL is where upstream Syncthing releases are downloaded from.
const ReleaseBaseURL = "https://github.com/syncthing/syncthing/releases/download"

// ErrChecksumMismatch is wrapped by Download when the downloaded archive's
// SHA-256 differs from its pin. The archive has been deleted by then.
var ErrChecksumMismatch = errors.New("syncthing download does not match its pinned SHA-256")

// ErrUnsupportedPlatform is returned by Download for an OS or architecture
// without a pinned upstream asset.
var ErrUnsupportedPlatform = errors.New("no pinned Syncthing download for this platform")

const (
	maxDownload  = 256 << 20 // upstream archives are about 25 MB
	stallTimeout = 60 * time.Second
)

// Download fetches the pinned Syncthing release (brand.BootstrapSyncthing)
// for this OS and architecture, verifies its SHA-256 against pins.go and
// installs the binary with upstream's LICENSE.txt, README.txt and AUTHORS.txt
// into dir (normally ManagedDir). It returns the binary's path. progress, if
// not nil, is called as bytes arrive; total is -1 when the size is unknown.
// On a checksum mismatch nothing is installed, the archive is deleted, and
// the error wraps ErrChecksumMismatch. The binary is installed before the
// notices: when it cannot be replaced (for example a running syncthing.exe
// on Windows) no file in dir changes; when a notice fails after that, the
// binary's path is returned together with an error saying so.
func Download(ctx context.Context, dir string, progress func(done, total int64)) (string, error) {
	return defaultDownloader().download(ctx, dir, progress)
}

// Asset returns the upstream archive name for goos/goarch at version v.
// Windows on arm64 uses the amd64 build, which runs under emulation.
func Asset(goos, goarch, v string) (string, error) {
	switch {
	case goos == "windows" && (goarch == "amd64" || goarch == "arm64"):
		return "syncthing-windows-amd64-" + v + ".zip", nil
	case goos == "darwin" && (goarch == "amd64" || goarch == "arm64"):
		return "syncthing-macos-universal-" + v + ".zip", nil
	case goos == "linux" && (goarch == "amd64" || goarch == "arm64"):
		return "syncthing-linux-" + goarch + "-" + v + ".tar.gz", nil
	}
	return "", fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, goos, goarch)
}

type downloader struct {
	client  *http.Client
	baseURL string
	version string
	pins    map[string]string
	goos    string
	goarch  string
}

func defaultDownloader() downloader {
	return downloader{
		client:  newHTTPClient(),
		baseURL: ReleaseBaseURL,
		version: pinnedVersion,
		pins:    pins,
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
	}
}

// newHTTPClient returns a client for release downloads: environment proxies
// are honoured, and redirects (GitHub sends one to its CDN) must keep the
// original scheme, so an https download is never downgraded.
func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != via[0].URL.Scheme {
				return fmt.Errorf("refusing redirect from %s to %s", via[0].URL.Scheme, req.URL.Scheme)
			}
			return nil
		},
	}
}

func (d downloader) download(ctx context.Context, dir string, progress func(done, total int64)) (string, error) {
	asset, err := Asset(d.goos, d.goarch, d.version)
	if err != nil {
		return "", err
	}
	want, ok := d.pins[asset]
	if !ok {
		return "", fmt.Errorf("%w: %s is not pinned", ErrUnsupportedPlatform, asset)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("download directory %q is not absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	// Keep the archive's extension: extract picks the format by name.
	ext := ".zip"
	if strings.HasSuffix(asset, ".tar.gz") {
		ext = ".tar.gz"
	}
	f, err := os.CreateTemp(dir, ".stv2-download-*"+ext)
	if err != nil {
		return "", err
	}
	archive := f.Name()
	defer os.Remove(archive)

	got, err := d.fetch(ctx, d.baseURL+"/"+d.version+"/"+asset, f, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	if !strings.EqualFold(got, want) {
		if rerr := os.Remove(archive); rerr != nil {
			return "", fmt.Errorf("%w: %s has SHA-256 %s, expected %s (and deleting it failed: %v)", ErrChecksumMismatch, asset, got, want, rerr)
		}
		return "", fmt.Errorf("%w: %s has SHA-256 %s, expected %s", ErrChecksumMismatch, asset, got, want)
	}
	return extract(archive, dir, exeName(d.goos))
}

// fetch streams url into w and returns the hex SHA-256 of what it wrote. A
// body that stops arriving for stallTimeout aborts the download.
func (d downloader) fetch(ctx context.Context, url string, w io.Writer, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errStalled := fmt.Errorf("no data received for %s", stallTimeout)
	watchdog := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", brand.UserAgent())
	resp, err := d.client.Do(req)
	if err != nil {
		return "", causeOr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server answered %s", resp.Status)
	}
	total := resp.ContentLength
	if total > maxDownload {
		return "", fmt.Errorf("download is %d bytes, more than the %d allowed", total, maxDownload)
	}

	h := sha256.New()
	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			done += int64(n)
			if done > maxDownload {
				return "", fmt.Errorf("download exceeds %d bytes", maxDownload)
			}
			h.Write(buf[:n])
			if _, err := w.Write(buf[:n]); err != nil {
				return "", err
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", causeOr(ctx, rerr)
		}
	}
	if total >= 0 && done != total {
		return "", fmt.Errorf("download ended after %d of %d bytes", done, total)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// causeOr returns the context's cancellation cause when there is one (for
// example the stall watchdog), and err otherwise.
func causeOr(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return err
}
