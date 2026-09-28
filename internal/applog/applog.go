// Package applog is SyncThing V2's own log: a rolling file of at most
// 1 MiB plus one previous generation (stv2.log and stv2.log.1), with
// secrets redacted before anything reaches the disk.
//
// Redaction removes every value registered with AddSecret (the Syncthing API
// key), the value of any X-API-Key header, and apikey values written as
// key=value, "apiKey": "value" or <apikey>value</apikey>.
package applog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// FileName is the current log file inside the log directory.
	FileName = "stv2.log"
	// MaxSize is the size at which the log rolls over to FileName + ".1".
	MaxSize = 1 << 20
	// Redacted replaces every secret.
	Redacted = "[REDACTED]"
	// minSecretLen stops trivially short strings from being registered as
	// secrets, which would shred unrelated log text.
	minSecretLen = 8
)

var patterns = []*regexp.Regexp{
	// X-API-Key: value   /   "X-Api-Key":["value"]   /   X-API-Key=value
	regexp.MustCompile(`(?i)(x-api-key"?\s*[:=]\s*\[?\s*"?)[^\s"',\]}&]+`),
	// apikey=value, api_key: value, "apiKey": "value", api-key='value'
	regexp.MustCompile(`(?i)(\bapi[_-]?key"?\s*[:=]\s*["']?)[^\s"',}&<]+`),
	// <apikey>value</apikey>
	regexp.MustCompile(`(?i)(<apikey>)[^<]*`),
	// Authorization: Bearer value
	regexp.MustCompile(`(?i)(authorization"?\s*[:=]\s*"?bearer\s+)[^\s"',}&]+`),
}

// Logger writes redacted, timestamped lines to a rolling file. It is safe for
// concurrent use and implements io.Writer, so it can back a log.Logger.
type Logger struct {
	mu      sync.Mutex
	dir     string
	f       *os.File
	size    int64
	closed  bool // set by Close; until then a lost file handle is reopened
	secrets []string
	now     func() time.Time
}

// Open creates dir (0700) if needed and opens dir/stv2.log for appending.
func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Logger{dir: dir, now: time.Now}
	if err := l.openCurrent(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path returns the current log file's path.
func (l *Logger) Path() string { return filepath.Join(l.dir, FileName) }

// Dir returns the log directory.
func (l *Logger) Dir() string { return l.dir }

func (l *Logger) openCurrent() error {
	f, err := os.OpenFile(l.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// AddSecret registers a value that must never appear in the log, such as the
// Syncthing API key. Values shorter than 8 characters are ignored.
func (l *Logger) AddSecret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < minSecretLen {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, have := range l.secrets {
		if have == s {
			return
		}
	}
	l.secrets = append(l.secrets, s)
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(l.secrets, func(i, j int) bool { return len(l.secrets[i]) > len(l.secrets[j]) })
}

// Redact returns s with every known secret and API key pattern removed.
func (l *Logger) Redact(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.redact(s)
}

func (l *Logger) redact(s string) string {
	for _, sec := range l.secrets {
		s = strings.ReplaceAll(s, sec, Redacted)
	}
	return RedactPatterns(s)
}

// RedactPatterns removes API key and bearer token values that follow a
// recognisable key name. It does not know registered secrets.
func RedactPatterns(s string) string {
	for _, re := range patterns {
		s = re.ReplaceAllString(s, "${1}"+Redacted)
	}
	return s
}

// Printf formats a message and writes it as one timestamped line.
func (l *Logger) Printf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\r\n")
	_, _ = l.writeLine(msg)
}

// Write implements io.Writer. Each call is redacted and written as-is (a
// log.Logger already supplies its own prefix and newline).
func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.write(l.redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (l *Logger) writeLine(msg string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := l.now().Format("2006-01-02T15:04:05.000Z07:00") + " " + l.redact(msg) + "\n"
	if err := l.write(line); err != nil {
		return 0, err
	}
	return len(line), nil
}

// write appends s, rolling the file first when s would push it past MaxSize.
// The caller holds l.mu.
//
// A failed rollover never stops logging: rotate reopens the current file,
// the entry goes into it (past MaxSize), and the rollover is retried on the
// next write. On Windows the rename fails while another process (a log
// viewer, an editor, an antivirus scanner) holds the file without
// FILE_SHARE_DELETE. If even the reopen fails, this write fails and the next
// one tries to reopen again.
func (l *Logger) write(s string) error {
	if l.closed {
		return os.ErrClosed
	}
	if l.f == nil {
		if err := l.openCurrent(); err != nil {
			return err
		}
	}
	if l.size > 0 && l.size+int64(len(s)) > MaxSize {
		if err := l.rotate(); err != nil && l.f == nil {
			return err
		}
	}
	if len(s) > MaxSize { // a single oversized entry is truncated, never split across files
		s = strings.ToValidUTF8(s[:MaxSize-len("...\n")], "") + "...\n"
	}
	n, err := io.WriteString(l.f, s)
	l.size += int64(n)
	return err
}

// rotate moves the current file to FileName + ".1" and starts a new one. It
// always ends with the current file reopened when that is possible, so l.f is
// nil afterwards only if the reopen failed too.
func (l *Logger) rotate() error {
	cerr := l.f.Close()
	l.f = nil
	prev := l.Path() + ".1"
	_ = os.Remove(prev) // os.Rename cannot replace on every platform and filesystem
	rerr := os.Rename(l.Path(), prev)
	if rerr != nil {
		rerr = fmt.Errorf("applog: rollover: %w", rerr)
	}
	return errors.Join(cerr, rerr, l.openCurrent())
}

// Close closes the log file. Later writes fail with os.ErrClosed.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Package-level logger used by the rest of the application.
var (
	stdMu sync.RWMutex
	std   *Logger
)

// Init opens the application log in dir and makes it the package default.
// Until Init succeeds, Printf writes redacted lines to standard error.
func Init(dir string) error {
	l, err := Open(dir)
	if err != nil {
		return err
	}
	pendingMu.Lock()
	for _, sec := range pending {
		l.AddSecret(sec)
	}
	pendingMu.Unlock()
	stdMu.Lock()
	old := std
	if old != nil {
		old.mu.Lock()
		carried := append([]string(nil), old.secrets...)
		old.mu.Unlock()
		for _, sec := range carried {
			l.AddSecret(sec)
		}
	}
	std = l
	stdMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// Default returns the logger set by Init, or nil.
func Default() *Logger {
	stdMu.RLock()
	defer stdMu.RUnlock()
	return std
}

// Printf logs through the default logger.
func Printf(format string, args ...any) {
	if l := Default(); l != nil {
		l.Printf(format, args...)
		return
	}
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\r\n")
	fmt.Fprintln(os.Stderr, pendingRedact(msg))
}

// AddSecret registers a secret with the default logger (and with any logger
// opened later through Init). Registering the same value again is a no-op.
func AddSecret(s string) {
	if l := Default(); l != nil {
		l.AddSecret(s)
		return
	}
	addPending(s)
}

// Redact removes every registered secret and API key pattern from s, whether
// or not Init has run yet. Use it for text that leaves the process by another
// route than the log, such as a diagnostics report.
func Redact(s string) string {
	if l := Default(); l != nil {
		return l.Redact(s)
	}
	return pendingRedact(s)
}

// Secrets registered while no default logger is open (before Init or after
// Close) are held here and handed to the logger Init opens.
var (
	pendingMu sync.Mutex
	pending   []string
)

func addPending(s string) {
	s = strings.TrimSpace(s)
	if len(s) < minSecretLen {
		return
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for _, have := range pending {
		if have == s {
			return
		}
	}
	pending = append(pending, s)
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(pending, func(i, j int) bool { return len(pending[i]) > len(pending[j]) })
}

func pendingRedact(s string) string {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for _, sec := range pending {
		s = strings.ReplaceAll(s, sec, Redacted)
	}
	return RedactPatterns(s)
}

// Close closes the default logger. The secrets it knew stay registered, so
// Redact and a later Init keep removing them.
func Close() error {
	stdMu.Lock()
	l := std
	if l != nil {
		// Carry the secrets over before the logger stops being the default,
		// so a concurrent Redact always knows them.
		l.mu.Lock()
		carried := append([]string(nil), l.secrets...)
		l.mu.Unlock()
		for _, sec := range carried {
			addPending(sec)
		}
	}
	std = nil
	stdMu.Unlock()
	if l == nil {
		return nil
	}
	return l.Close()
}
