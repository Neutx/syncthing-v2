// Package stclient talks to the local Syncthing REST API: it discovers
// config.xml, reads the GUI address and API key from it, and performs
// requests with the prototype's timeouts (5 s GET, 60 s event long-poll,
// 6 s commands). Every failure is an *Error with a Kind that separates "not
// running" from "API key rejected" and "unexpected response".
package stclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Endpoint is where and how to reach Syncthing's REST API.
// CAPool is set when the GUI uses TLS; it then holds only Syncthing's own
// https-cert.pem.
type Endpoint struct {
	BaseURL *url.URL
	APIKey  string
	CAPool  *x509.CertPool
}

// Request timeouts (F3).
const (
	GetTimeout    = 5 * time.Second
	EventsTimeout = 60 * time.Second
	SendTimeout   = 6 * time.Second
)

// eventsMargin is the gap between the HTTP deadline of an event long-poll and
// the timeout Syncthing is asked to hold the request for (60 s → timeout=50).
const eventsMargin = 10 * time.Second

// maxBody bounds a response body; /rest/events batches are the largest.
const maxBody = 64 << 20

// Client performs REST calls against one Syncthing instance. It is safe for
// concurrent use.
type Client struct {
	ep Endpoint
	hc *http.Client
}

// New returns a client for ep. Proxies are never used and redirects are never
// followed, so the API key only goes to the configured local address.
func New(ep Endpoint) *Client {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: GetTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: GetTimeout,
	}
	if ep.CAPool != nil {
		tr.TLSClientConfig = pinnedTLS(ep.CAPool)
	}
	return &Client{
		ep: ep,
		hc: &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// pinnedTLS trusts exactly the certificates in pool. Syncthing's generated
// GUI certificate names "syncthing" or the host name rather than the loopback
// address being dialled, so the chain is verified against pool without a host
// name check; any certificate not issued by (or equal to) a pooled one fails.
func pinnedTLS(pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verification is done in VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("syncthing presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: inter,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			})
			return err
		},
	}
}

// URL returns the base URL of the Syncthing GUI (never containing the API key).
func (c *Client) URL() string { return c.ep.BaseURL.String() }

// Host returns the dialled host:port, for messages such as
// "Syncthing is not responding on 127.0.0.1:8384".
func (c *Client) Host() string { return c.ep.BaseURL.Host }

// Get fetches path (for example "/rest/system/status") and decodes the JSON
// body into out. A nil out discards the body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, GetTimeout, out)
}

// Events long-polls /rest/events. With since <= 0 it seeds: it asks for the
// latest event only (limit=1). Otherwise it asks for events after since.
// timeout is the HTTP deadline (EventsTimeout when <= 0); Syncthing is asked
// to hold the request 10 s less (timeout=50 for 60 s). It returns the raw
// events and the highest event ID seen, or since when there were none.
func (c *Client) Events(ctx context.Context, since int, timeout time.Duration) ([]json.RawMessage, int, error) {
	if timeout <= 0 {
		timeout = EventsTimeout
	}
	hold := (timeout - eventsMargin) / time.Second
	if hold < 1 {
		hold = 1
	}
	q := url.Values{}
	if since <= 0 {
		q.Set("limit", "1")
		since = 0
	} else {
		q.Set("since", strconv.Itoa(since))
	}
	q.Set("timeout", strconv.Itoa(int(hold)))

	var raw []json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/rest/events?"+q.Encode(), nil, timeout, &raw); err != nil {
		return nil, since, err
	}
	last := since
	for _, ev := range raw {
		var h struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(ev, &h); err != nil {
			return nil, since, &Error{Kind: ErrBadResponse, Status: http.StatusOK, Err: fmt.Errorf("decode event: %w", err)}
		}
		if h.ID > last {
			last = h.ID
		}
	}
	return raw, last, nil
}

// Send performs a POST, PUT, PATCH or DELETE on path. A nil body sends an
// empty request; anything else is sent as JSON. The response body is ignored.
func (c *Client) Send(ctx context.Context, method, path string, body any) error {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("stclient: unsupported method %q", method)
	}
	return c.do(ctx, method, path, body, SendTimeout, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, timeout time.Duration, out any) error {
	ref, err := url.Parse(path)
	if err != nil || ref.Scheme != "" || ref.Host != "" || !strings.HasPrefix(ref.Path, "/") {
		return fmt.Errorf("stclient: invalid API path %q", path)
	}
	target := c.ep.BaseURL.ResolveReference(ref)

	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("stclient: encode request body: %w", err)
		}
		rd = bytes.NewReader(b)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target.String(), rd)
	if err != nil {
		return fmt.Errorf("stclient: %w", err)
	}
	req.Header.Set("X-API-Key", c.ep.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return &Error{Kind: ErrUnreachable, Err: stripURL(err)}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &Error{Kind: ErrUnauthorized, Status: resp.StatusCode, Err: errors.New(resp.Status)}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := resp.Status
		if m := strings.TrimSpace(string(msg)); m != "" {
			detail += ": " + m
		}
		return &Error{Kind: ErrBadResponse, Status: resp.StatusCode, Err: errors.New(detail)}
	}

	lr := io.LimitReader(resp.Body, maxBody)
	if out == nil {
		if _, err := io.Copy(io.Discard, lr); err != nil {
			return &Error{Kind: ErrUnreachable, Status: resp.StatusCode, Err: stripURL(err)}
		}
		return nil
	}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return &Error{Kind: ErrUnreachable, Status: resp.StatusCode, Err: stripURL(err)}
		}
		return &Error{Kind: ErrBadResponse, Status: resp.StatusCode, Err: fmt.Errorf("decode %s: %w", ref.Path, err)}
	}
	return nil
}

// stripURL drops the request URL from a *url.Error so messages stay short;
// the URL never holds the API key, but it adds nothing for the user.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
