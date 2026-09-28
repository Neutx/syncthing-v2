package single

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const (
	helperEnv     = "STV2_SINGLE_TEST_HELPER"
	helperNameEnv = "STV2_SINGLE_TEST_NAME"
	helperDirEnv  = "STV2_SINGLE_TEST_DIR"
	token         = "synthetic-control-token-0123456789abcdef"
)

// The test binary doubles as a second process that tries to take the lock.
// Exit codes: 0 acquired, 10 already running, 20 other error.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "acquire" {
		os.Exit(m.Run())
	}
	l, err := Acquire(os.Getenv(helperNameEnv), os.Getenv(helperDirEnv))
	switch {
	case errors.Is(err, ErrAlreadyRunning):
		os.Exit(10)
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(20)
	}
	_ = l.Release()
	os.Exit(0)
}

func testName() string {
	return fmt.Sprintf(`Local\SyncThingV2.Test.%d.%d`, os.Getpid(), time.Now().UnixNano())
}

func acquireInOtherProcess(t *testing.T, name, dir string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"=acquire", helperNameEnv+"="+name, helperDirEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	return 0
}

func TestSecondAcquireFailsWhileFirstIsHeld(t *testing.T) {
	name, dir := testName(), t.TempDir()

	first, err := Acquire(name, dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	second, err := Acquire(name, dir)
	if !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			_ = second.Release()
		}
		t.Fatalf("second acquire in the same process: err = %v, want ErrAlreadyRunning", err)
	}
	if code := acquireInOtherProcess(t, name, dir); code != 10 {
		t.Fatalf("acquire from another process exited %d, want 10 (already running)", code)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	if code := acquireInOtherProcess(t, name, dir); code != 0 {
		t.Fatalf("acquire from another process after release exited %d, want 0", code)
	}
	third, err := Acquire(name, dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestDifferentNamesDoNotConflict(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(testName()+".a", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Acquire(testName()+".b", dir)
	if err != nil {
		t.Fatalf("a different name must not conflict: %v", err)
	}
	defer b.Release()
}

func TestEmptyNameRejected(t *testing.T) {
	if _, err := Acquire("", t.TempDir()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestInstanceRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	in := Instance{Port: 49152, PID: 4242, Token: token}
	if err := WriteInstance(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, InstanceFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("instance.json mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	// Another PID must not remove this instance's file.
	if err := RemoveInstance(dir, 9999); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstance(dir); err != nil {
		t.Fatalf("file removed by a foreign pid: %v", err)
	}
	if err := RemoveInstance(dir, 4242); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstance(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present: %v", err)
	}
	if err := RemoveInstance(dir, 4242); err != nil {
		t.Fatalf("removing a missing file: %v", err)
	}
}

func TestInstanceValidation(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []Instance{
		{Port: 0, PID: 1, Token: token},
		{Port: 70000, PID: 1, Token: token},
		{Port: 1234, PID: 0, Token: token},
		{Port: 1234, PID: 1, Token: "short"},
	} {
		if err := WriteInstance(dir, in); err == nil {
			t.Errorf("WriteInstance(%+v) accepted invalid data", in)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, InstanceFile), []byte(`{"port":"x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstance(dir); err == nil {
		t.Fatal("ReadInstance accepted malformed JSON")
	}
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestSignal(t *testing.T) {
	type seen struct{ method, path, auth, csrf, host string }
	got := make(chan seen, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-STV2"), r.Host}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	port := serverPort(t, srv)
	dir := t.TempDir()

	if err := WriteInstance(dir, Instance{Port: port, PID: os.Getpid(), Token: token}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"show", "quit"} {
		if err := Signal(context.Background(), dir, action); err != nil {
			t.Fatalf("Signal(%s): %v", action, err)
		}
		s := <-got
		want := seen{http.MethodPost, "/api/" + action, "Bearer " + token, "1", "127.0.0.1:" + strconv.Itoa(port)}
		if s != want {
			t.Fatalf("request = %+v, want %+v", s, want)
		}
	}

	if err := Signal(context.Background(), dir, "exec"); err == nil {
		t.Fatal("unknown action accepted")
	}

	// A wrong token is reported as an error.
	if err := WriteInstance(dir, Instance{Port: port, PID: os.Getpid(), Token: "wrong-token-0123456789"}); err != nil {
		t.Fatal(err)
	}
	if err := Signal(context.Background(), dir, "show"); err == nil {
		t.Fatal("a 403 answer must be an error")
	}
	<-got
}

func TestSignalWithoutInstance(t *testing.T) {
	if err := Signal(context.Background(), t.TempDir(), "show"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}
