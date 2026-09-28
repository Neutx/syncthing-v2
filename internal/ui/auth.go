package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// launchTokenTTL is how long a launch token stays valid.
	launchTokenTTL = 30 * time.Second
	// maxLaunchTokens bounds outstanding tokens; the oldest is dropped first.
	maxLaunchTokens = 64
	// maxSessions bounds live sessions; the oldest is dropped first.
	maxSessions = 32
)

// randToken returns 256 random bits, base64url-encoded without padding.
func randToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// authStore holds single-use launch tokens and in-memory sessions. Sessions
// never outlive the process, so they rotate with every tray start.
type authStore struct {
	mu       sync.Mutex
	tokens   map[string]time.Time // token -> expiry
	sessions map[string]time.Time // session -> creation
	now      func() time.Time     // replaced in tests
}

func newAuthStore() *authStore {
	return &authStore{
		tokens:   map[string]time.Time{},
		sessions: map[string]time.Time{},
		now:      time.Now,
	}
}

// newLaunchToken mints a token valid for one login within launchTokenTTL.
// crypto/rand does not fail on supported platforms; if it ever does, the
// empty string returned here can never be consumed.
func (a *authStore) newLaunchToken() string {
	tok, err := randToken()
	if err != nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for t, exp := range a.tokens {
		if !now.Before(exp) {
			delete(a.tokens, t)
		}
	}
	if len(a.tokens) >= maxLaunchTokens {
		dropOldest(a.tokens)
	}
	a.tokens[tok] = now.Add(launchTokenTTL)
	return tok
}

// consume reports whether tok is a live launch token and invalidates it.
func (a *authStore) consume(tok string) bool {
	if tok == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tokens[tok]
	delete(a.tokens, tok)
	return ok && a.now().Before(exp)
}

func (a *authStore) newSession() (string, error) {
	sess, err := randToken()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= maxSessions {
		dropOldest(a.sessions)
	}
	a.sessions[sess] = a.now()
	return sess, nil
}

func (a *authStore) validSession(v string) bool {
	if v == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[v]
	return ok
}

func dropOldest(m map[string]time.Time) {
	var oldest string
	var at time.Time
	for k, t := range m {
		if oldest == "" || t.Before(at) {
			oldest, at = k, t
		}
	}
	delete(m, oldest)
}

// checkBearer compares the Authorization bearer token in constant time.
func checkBearer(r *http.Request, want string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
