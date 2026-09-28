package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestMissingFileGivesDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Existed() {
		t.Fatal("Existed() = true without a file")
	}
	p := s.Get()
	if !p.Notifications || !p.CheckForUpdates || p.LegacyAsked || p.ProfileChosen != "" {
		t.Fatalf("defaults = %+v", p)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open must not create prefs.json")
	}
}

func TestUpdatePersistsWithExactFieldNames(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	err = s.Update(func(p *Prefs) error {
		p.Notifications = false
		p.CheckForUpdates = false
		p.ProfileChosen = ProfileHybrid
		p.LegacyAsked = true
		p.LastUpdateCheck = checked
		p.UpdateNotified = "v1.2.3"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DismissDevice("AAAAAAA-SYNTHETIC-DEVICE-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissNotice("gui-exposed"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWidened("BBBBBBB-SYNTHETIC-DEVICE-2"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"notifications", "checkForUpdates", "profileChosen", "dismissedDevices", "widened", "noticesDismissed", "legacyAsked", "lastUpdateCheck", "updateNotified"} {
		if _, ok := m[k]; !ok {
			t.Errorf("prefs.json lacks key %q: %s", k, raw)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Existed() {
		t.Fatal("Existed() = false after save")
	}
	p := s2.Get()
	if p.Notifications || p.CheckForUpdates || !p.LegacyAsked || p.ProfileChosen != ProfileHybrid ||
		!p.LastUpdateCheck.Equal(checked) || p.UpdateNotified != "v1.2.3" {
		t.Fatalf("reloaded = %+v", p)
	}
	if !p.DeviceDismissed("AAAAAAA-SYNTHETIC-DEVICE-1") || p.DeviceDismissed("other") {
		t.Fatal("DeviceDismissed mismatch")
	}
	if !p.NoticeDismissed("gui-exposed") || p.NoticeDismissed("legacy-tray") {
		t.Fatal("NoticeDismissed mismatch")
	}
	if !p.IsWidened("BBBBBBB-SYNTHETIC-DEVICE-2") || p.IsWidened("AAAAAAA-SYNTHETIC-DEVICE-1") {
		t.Fatal("IsWidened mismatch")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %d entries", len(entries))
	}
}

func TestAbsentFieldsKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"legacyAsked":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Get()
	if !p.Notifications || !p.CheckForUpdates || !p.LegacyAsked {
		t.Fatalf("got %+v", p)
	}
}

func TestCorruptFileIsMovedAside(t *testing.T) {
	for name, content := range map[string]string{
		"syntax":  `{"notifications": tru`,
		"profile": `{"profileChosen":"everything"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			if s == nil {
				t.Fatal("Open must still return a usable Store")
			}
			if s.Existed() || !s.Get().Notifications {
				t.Fatal("store must hold defaults")
			}
			bad, err := os.ReadFile(filepath.Join(dir, FileName+".corrupt"))
			if err != nil || string(bad) != content {
				t.Fatalf("corrupt copy = %q, %v", bad, err)
			}
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); err != nil {
				t.Fatalf("reopen after save: %v", err)
			}
		})
	}
}

func TestFailedUpdateChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(p *Prefs) error { p.ProfileChosen = ProfileTailnet; return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Update(func(p *Prefs) error { p.ProfileChosen = "bogus"; return nil }); err == nil {
		t.Fatal("invalid profile accepted")
	}
	boom := errors.New("boom")
	if err := s.Update(func(p *Prefs) error { p.Notifications = false; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	p := s.Get()
	if p.ProfileChosen != ProfileTailnet || !p.Notifications {
		t.Fatalf("in-memory prefs changed: %+v", p)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("file changed after a failed update")
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DismissDevice("CCCCCCC-SYNTHETIC"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWidened("CCCCCCC-SYNTHETIC"); err != nil {
		t.Fatal(err)
	}
	p := s.Get()
	p.DismissedDevices[0] = "mutated"
	p.Widened["other"] = true
	q := s.Get()
	if q.DismissedDevices[0] != "CCCCCCC-SYNTHETIC" || q.Widened["other"] {
		t.Fatal("Get leaked internal state")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("DEV%02d-SYNTHETIC", i)
			errs <- s.DismissDevice(id)
			errs <- s.DismissDevice(id) // idempotent
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get().DismissedDevices
	sort.Strings(got)
	if len(got) != n {
		t.Fatalf("got %d dismissed devices, want %d: %v", len(got), n, got)
	}
	for i, id := range got {
		if id != fmt.Sprintf("DEV%02d-SYNTHETIC", i) {
			t.Fatalf("unexpected id %q at %d", id, i)
		}
	}
}
