package applog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic secrets only.
const (
	apiKey      = "SyntheticApiKey-7Hq2Lw9Zx4Rt6Vb1" // synthetic; gitleaks:allow
	headerKey   = "HeaderOnlyKey-Qm3Np8Ks2Jd5"
	jsonKey     = "JsonOnlyKey-Bv7Cx1Zn4Lk9"
	xmlKey      = "XmlOnlyKey-Wt5Ey2Ru8Io3"
	queryKey    = "QueryOnlyKey-Pa6Sd9Fg1Hj4"
	bearerToken = "BearerOnlyToken-Ty7Gh2Uj5Ik8"
)

func readAll(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{FileName + ".1", FileName} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestAPIKeyNeverReachesTheFile(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.AddSecret(apiKey)

	l.Printf("GET /rest/system/status with key %s", apiKey)
	l.Printf("request headers: map[X-Api-Key:[%s] Accept:[application/json]]", apiKey)
	l.Printf("X-API-Key: %s", headerKey)
	l.Printf(`sent {"apiKey":"%s","user":"demo"}`, jsonKey)
	l.Printf("config: <gui><apikey>%s</apikey></gui>", xmlKey)
	l.Printf("url http://127.0.0.1:18384/rest/noauth?apikey=%s&x=1", queryKey)
	l.Printf("Authorization: Bearer %s", bearerToken)
	l.Printf("wrapped error: %v", fmt.Errorf("dial failed for key=%s: refused", apiKey))

	// Through the io.Writer path, as a log.Logger would use it.
	std := log.New(l, "std: ", 0)
	std.Printf("stdlib logger saw %s", apiKey)

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	got := readAll(t, dir)
	for _, secret := range []string{apiKey, headerKey, jsonKey, xmlKey, queryKey, bearerToken} {
		if strings.Contains(got, secret) {
			t.Errorf("log file contains secret %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"GET /rest/system/status", Redacted, "Accept:", `"user":"demo"`, "std: stdlib logger saw"} {
		if !strings.Contains(got, want) {
			t.Errorf("log file lost non-secret text %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n"); n != 9 {
		t.Errorf("got %d lines, want 9:\n%s", n, got)
	}
}

func TestPackageDefaultRedactsSecretsRegisteredBeforeInit(t *testing.T) {
	dir := t.TempDir()
	AddSecret(apiKey) // before Init: held as pending
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close() })
	Printf("adopted Syncthing with key %s", apiKey)
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, dir)
	if strings.Contains(got, apiKey) {
		t.Fatalf("log file contains the API key:\n%s", got)
	}
	if !strings.Contains(got, "adopted Syncthing with key "+Redacted) {
		t.Fatalf("expected a redacted line, got:\n%s", got)
	}
}

func TestShortSecretsAreIgnored(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.AddSecret("abc")
	l.AddSecret("   ")
	if got := l.Redact("abc def"); got != "abc def" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestOverlappingSecretsRemovedWhole(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.AddSecret("prefix12")
	l.AddSecret("prefix12-and-more")
	if got := l.Redact("v=prefix12-and-more"); got != "v="+Redacted {
		t.Fatalf("Redact = %q", got)
	}
}

func TestRollsAtOneMiBKeepingTwoGenerations(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	payload := strings.Repeat("x", 1000)
	const lines = 3000 // about 3 MB: forces two rollovers
	for i := 0; i < lines; i++ {
		l.Printf("line %06d %s", i, payload)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	cur, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	prev, err := os.Stat(filepath.Join(dir, FileName+".1"))
	if err != nil {
		t.Fatal(err)
	}
	if cur.Size() > MaxSize || prev.Size() > MaxSize {
		t.Fatalf("sizes %d and %d exceed %d", cur.Size(), prev.Size(), MaxSize)
	}
	if prev.Size() < MaxSize-2048 {
		t.Fatalf("previous generation only %d bytes; rolled too early", prev.Size())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want exactly 2 files, got %d", len(entries))
	}
	all := readAll(t, dir)
	if strings.Contains(all, "line 000000 ") {
		t.Fatal("the oldest generation should have been discarded")
	}
	if !strings.Contains(all, fmt.Sprintf("line %06d ", lines-1)) {
		t.Fatal("the newest line is missing")
	}
	// Every line is whole: rollover never splits one.
	for _, ln := range strings.Split(strings.TrimSuffix(all, "\n"), "\n") {
		if !strings.HasPrefix(ln, "2026-01-02T03:04:05.000Z line ") || !strings.HasSuffix(ln, payload) {
			t.Fatalf("broken line: %.80q", ln)
		}
	}
}

func TestReopenAppendsAndCountsExistingSize(t *testing.T) {
	dir := t.TempDir()
	existing := strings.Repeat("y", MaxSize-10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Printf("this line does not fit in the current file")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(filepath.Join(dir, FileName+".1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(prev) != existing {
		t.Fatal("existing content was not rolled over intact")
	}
	cur, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(cur), "this line does not fit in the current file\n") {
		t.Fatalf("current file = %q", cur)
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("late\n")); err == nil {
		t.Fatal("expected an error after Close")
	}
}

func TestRedactKnowsSecretsWithoutADefaultLogger(t *testing.T) {
	const bare = "BareSyntheticKey-Zc4Xv8Bn2Mq6" // synthetic; gitleaks:allow
	AddSecret(bare)
	AddSecret(bare) // registering twice keeps one entry
	pendingMu.Lock()
	n := 0
	for _, s := range pending {
		if s == bare {
			n++
		}
	}
	pendingMu.Unlock()
	if n != 1 {
		t.Errorf("pending holds the secret %d times, want 1", n)
	}
	if got := Redact("value " + bare + " end"); got != "value "+Redacted+" end" {
		t.Errorf("Redact before Init = %q", got)
	}

	// A secret registered while a logger is open survives Close.
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	const later = "LaterSyntheticKey-Hy3Ju7Ki1Lo5" // synthetic; gitleaks:allow
	AddSecret(later)
	if got := Redact(later); got != Redacted {
		t.Errorf("Redact with a default logger = %q", got)
	}
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	if got := Redact("k " + later + " " + bare); got != "k "+Redacted+" "+Redacted {
		t.Errorf("Redact after Close = %q", got)
	}
}

// fillToEdge fills the current file to 16 bytes short of MaxSize, so any
// longer write must roll over first.
func fillToEdge(t *testing.T, l *Logger) {
	t.Helper()
	payload := strings.Repeat("f", 1000)
	for l.size < MaxSize-4096 {
		l.Printf("fill %s", payload)
	}
	pad := strings.Repeat("p", int(MaxSize-16-l.size-1)) + "\n"
	if _, err := l.Write([]byte(pad)); err != nil {
		t.Fatal(err)
	}
	if l.size != MaxSize-16 {
		t.Fatalf("filled to %d bytes, want %d", l.size, MaxSize-16)
	}
}

func TestFailedRolloverKeepsLogging(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory named stv2.log.1 can be neither removed nor
	// replaced by a rename, on every platform, so every rollover fails.
	blocker := filepath.Join(dir, FileName+".1")
	if err := os.MkdirAll(filepath.Join(blocker, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fillToEdge(t, l)

	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("written while the rollover fails %d\n", i)
		if n, err := l.Write([]byte(msg)); err != nil || n != len(msg) {
			t.Fatalf("Write during a failed rollover = %d, %v; want %d, nil", n, err, len(msg))
		}
	}
	cur, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) <= MaxSize || !strings.HasSuffix(string(cur), "written while the rollover fails 2\n") {
		t.Fatalf("current file (%d bytes) does not end with the lines written during the failed rollover", len(cur))
	}

	// Once the obstacle is gone the next write rolls over normally.
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	l.Printf("after the obstacle is gone")
	prev, err := os.ReadFile(blocker)
	if err != nil {
		t.Fatalf("rollover was not retried: %v", err)
	}
	if !strings.HasSuffix(string(prev), "written while the rollover fails 2\n") {
		t.Fatal("the oversized file was not rolled over intact")
	}
	cur, err = os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(cur), "after the obstacle is gone\n") || len(cur) > 200 {
		t.Fatalf("current file after the retried rollover = %q", cur)
	}
}

func TestLostHandleIsReopenedOnNextWrite(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Simulate a rollover whose reopen failed: no handle, but not Closed.
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()

	l.Printf("logging resumes")
	got, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "logging resumes\n") {
		t.Fatalf("log = %q", got)
	}
}
