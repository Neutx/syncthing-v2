package pairing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

func TestShareFolderMergeIdempotent(t *testing.T) {
	st, c := newFakeST(t)
	st.folders["abcde-12345"] = map[string]any{
		"id": "abcde-12345", "label": "Docs", "path": "/synthetic/docs", "type": "sendreceive",
		"rescanIntervalS": 3600.0,
		"devices": []any{
			map[string]any{"deviceID": idSelf, "introducedBy": "", "encryptionPassword": ""},
			map[string]any{"deviceID": idMac, "introducedBy": "", "encryptionPassword": "synthetic-secret"},
		},
	}
	s := newService(c, nil, newProbeTable(nil), nil)
	ctx := context.Background()
	if err := s.ShareFolder(ctx, "abcde-12345", "ignored", "/ignored", []string{idLaptop, idMac}); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 || w[0] != "PUT /rest/config/folders/abcde-12345" {
		t.Fatalf("writes = %v", w)
	}
	got := st.lastBody()
	wantDevices := []any{
		map[string]any{"deviceID": idSelf, "introducedBy": "", "encryptionPassword": ""},
		map[string]any{"deviceID": idMac, "introducedBy": "", "encryptionPassword": "synthetic-secret"},
		map[string]any{"deviceID": idLaptop},
	}
	if !reflect.DeepEqual(got["devices"], wantDevices) {
		t.Fatalf("devices = %v\nwant %v", got["devices"], wantDevices)
	}
	if got["path"] != "/synthetic/docs" || got["label"] != "Docs" || got["rescanIntervalS"] != 3600.0 {
		t.Fatalf("existing folder settings changed: %v", got)
	}

	// Sharing the same devices again changes nothing and writes nothing.
	if err := s.ShareFolder(ctx, "abcde-12345", "", "", []string{idLaptop, idMac, idLaptop}); err != nil {
		t.Fatal(err)
	}
	if w := st.writeLog(); len(w) != 1 {
		t.Fatalf("idempotent share wrote again: %v", w)
	}
}

func TestShareFolderCreatesNew(t *testing.T) {
	st, c := newFakeST(t)
	s := newService(c, nil, newProbeTable(nil), nil)
	dir := filepath.Join(t.TempDir(), "Sync")
	if err := s.ShareFolder(context.Background(), "fghij-67890", "", dir, []string{idLaptop}); err != nil {
		t.Fatal(err)
	}
	got := st.lastBody()
	want := map[string]any{
		"id": "fghij-67890", "label": "Sync", "path": dir, "type": "sendreceive",
		"devices": []any{map[string]any{"deviceID": idSelf}, map[string]any{"deviceID": idLaptop}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT body = %v\nwant %v", got, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("folder directory not created: %v", err)
	}
	if err := s.ShareFolder(context.Background(), "new-id", "x", "relative/dir", []string{idLaptop}); err == nil {
		t.Fatal("relative path accepted for a new folder")
	}
	if err := s.ShareFolder(context.Background(), "new-id", "x", dir, []string{"bogus"}); err == nil {
		t.Fatal("invalid device ID accepted")
	}
	if err := s.ShareFolder(context.Background(), " ", "x", dir, []string{idLaptop}); err == nil {
		t.Fatal("empty folder ID accepted")
	}
}

func TestAcceptFolder(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{"deviceID": idLaptop, "name": "laptop"})
	st.pendingFolders["offer-0001"] = map[string]string{idLaptop: "Holiday Photos", idFriend: "Holiday Photos"}
	s := newService(c, nil, newProbeTable(nil), nil)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "Holiday Photos")

	// From a device that is not configured: refused.
	err := s.AcceptFolder(ctx, model.PendingFolder{FolderID: "offer-0001", FromDevice: idFriend}, dir)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured sender: %v", err)
	}
	// Not offered (anymore): refused.
	err = s.AcceptFolder(ctx, model.PendingFolder{FolderID: "offer-9999", FromDevice: idLaptop}, dir)
	if !errors.Is(err, ErrNotOffered) {
		t.Fatalf("not offered: %v", err)
	}
	if w := st.writeLog(); len(w) != 0 {
		t.Fatalf("refused accepts wrote: %v", w)
	}

	if err := s.AcceptFolder(ctx, model.PendingFolder{FolderID: "offer-0001", FromDevice: idLaptop}, dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": "offer-0001", "label": "Holiday Photos", "path": dir, "type": "sendreceive",
		"devices": []any{map[string]any{"deviceID": idSelf}, map[string]any{"deviceID": idLaptop}},
	}
	if got := st.lastBody(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT body = %v\nwant %v", got, want)
	}
}

func TestAcceptFolderExistingID(t *testing.T) {
	st, c := newFakeST(t)
	st.addDevice(map[string]any{"deviceID": idLaptop, "name": "laptop"})
	st.pendingFolders["offer-0001"] = map[string]string{idLaptop: "Holiday Photos"}
	existingDir := filepath.Join(t.TempDir(), "My Unrelated Folder")
	st.folders["offer-0001"] = map[string]any{
		"id": "offer-0001", "label": "Mine", "path": existingDir, "type": "sendonly",
		"devices": []any{map[string]any{"deviceID": idSelf}},
	}
	s := newService(c, nil, newProbeTable(nil), nil)
	ctx := context.Background()
	pf := model.PendingFolder{FolderID: "offer-0001", FromDevice: idLaptop}

	// Accepting "into ~/Sync/<label>" must not silently share the existing,
	// possibly unrelated folder: a typed error names the existing path.
	dest := filepath.Join(t.TempDir(), "Holiday Photos")
	err := s.AcceptFolder(ctx, pf, dest)
	if !errors.Is(err, ErrFolderExists) {
		t.Fatalf("AcceptFolder = %v, want ErrFolderExists", err)
	}
	var fe *FolderExistsError
	if !errors.As(err, &fe) || fe.Path != existingDir || fe.FolderID != "offer-0001" {
		t.Fatalf("error = %#v, want the existing path", err)
	}
	if !strings.Contains(err.Error(), existingDir) {
		t.Fatalf("message %q does not name the existing path", err)
	}
	if w := st.writeLog(); len(w) != 0 {
		t.Fatalf("refused accept wrote: %v", w)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused accept created %s: %v", dest, err)
	}

	// Choosing the existing folder's location adds the sender to it and keeps
	// its settings (a trailing separator and, on case-insensitive systems,
	// different letter case still match).
	same := existingDir + string(filepath.Separator)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		same = strings.ToUpper(existingDir)
	}
	if err := s.AcceptFolder(ctx, pf, same); err != nil {
		t.Fatal(err)
	}
	got := st.lastBody()
	if got["path"] != existingDir || got["type"] != "sendonly" || got["label"] != "Mine" {
		t.Fatalf("existing folder settings changed: %v", got)
	}
	want := []any{map[string]any{"deviceID": idSelf}, map[string]any{"deviceID": idLaptop}}
	if !reflect.DeepEqual(got["devices"], want) {
		t.Fatalf("devices = %v\nwant %v", got["devices"], want)
	}

	// The sender is already in the folder: accepting again anywhere is a no-op.
	if err := s.AcceptFolder(ctx, pf, dest); err != nil {
		t.Fatalf("no-op accept: %v", err)
	}
	if w := st.writeLog(); len(w) != 1 {
		t.Fatalf("writes = %v, want exactly one PUT", w)
	}
}

func TestSamePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory:", err)
	}
	abs := filepath.Join(t.TempDir(), "Docs")
	sep := string(filepath.Separator)
	cases := []struct {
		configured, path string
		want             bool
	}{
		{abs, abs, true},
		{abs + sep, abs, true},
		{filepath.Join(abs, "..", "Docs"), abs, true},
		{abs, filepath.Join(filepath.Dir(abs), "Other"), false},
		{"~" + sep + "Sync" + sep + "Docs", filepath.Join(home, "Sync", "Docs"), true},
		{"~/Sync/Docs", filepath.Join(home, "Sync", "Docs"), true},
		{"~", home, true},
		{"~other" + sep + "Docs", filepath.Join(home, "Docs"), false},
		{"", abs, false},
		{abs, "", false},
	}
	for _, c := range cases {
		if got := samePath(c.configured, c.path); got != c.want {
			t.Errorf("samePath(%q, %q) = %v, want %v", c.configured, c.path, got, c.want)
		}
	}
	upper := strings.ToUpper(abs)
	if got, want := samePath(abs, upper), runtime.GOOS == "windows" || runtime.GOOS == "darwin"; upper != abs && got != want {
		t.Errorf("samePath(%q, %q) = %v, want %v on %s", abs, upper, got, want, runtime.GOOS)
	}
}

func TestDeclineFolder(t *testing.T) {
	st, c := newFakeST(t)
	st.pendingFolders["offer-0001"] = map[string]string{idLaptop: "X"}
	s := newService(c, nil, newProbeTable(nil), nil)
	if err := s.DeclineFolder(context.Background(), model.PendingFolder{FolderID: "offer-0001", FromDevice: idLaptop}); err != nil {
		t.Fatal(err)
	}
	w := st.writeLog()
	if len(w) != 1 || !strings.HasPrefix(w[0], "DELETE /rest/cluster/pending/folders?") ||
		!strings.Contains(w[0], "folder=offer-0001") || !strings.Contains(w[0], "device="+idLaptop) {
		t.Fatalf("writes = %v", w)
	}
	if len(st.pendingFolders["offer-0001"]) != 0 {
		t.Fatal("offer not removed")
	}
}

func TestSafeFolderDir(t *testing.T) {
	base := t.TempDir()
	const id = "abcde-12345"
	cases := []struct {
		label, want string
	}{
		{"Holiday Photos", "Holiday Photos"},
		{`..\x`, id},
		{"../x", id},
		{"a/../../x", id},
		{"..", id},
		{"CON", id},
		{"con.txt", id},
		{"Com1", id},
		{"LPT9.tar.gz", id},
		{"/etc/passwd", id},
		{`C:\Windows`, id},
		{`c:relative`, id},
		{`\\server\share`, id},
		{"Photos: 2024/*?", "Photos 2024"},
		{"...hidden", "hidden"},
		{"trailing dots...", "trailing dots"},
		{"a....b", "a.b"},
		{"tab\tand\nnewline", "tab and newline"},
		{"émoji 🎉 ok", "moji ok"},
		{"", id},
		{"   ", id},
		{"COM0", "COM0"},
		{"CONSOLE", "CONSOLE"},
		{strings.Repeat("x", 100), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		got, err := SafeFolderDir(base, tc.label, id)
		if err != nil {
			t.Errorf("SafeFolderDir(%q): %v", tc.label, err)
			continue
		}
		if want := filepath.Join(base, tc.want); got != want {
			t.Errorf("SafeFolderDir(%q) = %q, want %q", tc.label, got, want)
		}
		if rel, err := filepath.Rel(base, got); err != nil || strings.HasPrefix(rel, "..") || strings.ContainsAny(rel, `/\`) {
			t.Errorf("SafeFolderDir(%q) = %q is not a direct child of base", tc.label, got)
		}
	}
}

func TestSafeFolderDirRejects(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct{ label, id string }{
		{"CON", "CON"},
		{`..\x`, `..\y`},
		{"/abs", "/abs"},
		{"", ""},
		{"...", "..."},
	} {
		if got, err := SafeFolderDir(base, tc.label, tc.id); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("SafeFolderDir(%q, %q) = %q, %v; want ErrUnsafeName", tc.label, tc.id, got, err)
		}
	}
	if _, err := SafeFolderDir("relative/base", "Docs", "id"); err == nil {
		t.Error("relative base accepted")
	}
}

func TestSafeFolderDirCollisions(t *testing.T) {
	base := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Existing empty directory: reused.
	mk(filepath.Join(base, "Empty"))
	if got, _ := SafeFolderDir(base, "Empty", "id"); got != filepath.Join(base, "Empty") {
		t.Errorf("empty dir: %q", got)
	}
	// Existing non-empty directories: " (2)", then " (3)".
	mk(filepath.Join(base, "Docs"))
	if err := os.WriteFile(filepath.Join(base, "Docs", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := SafeFolderDir(base, "Docs", "id"); got != filepath.Join(base, "Docs (2)") {
		t.Errorf("first collision: %q", got)
	}
	mk(filepath.Join(base, "Docs (2)", "sub"))
	if got, _ := SafeFolderDir(base, "Docs", "id"); got != filepath.Join(base, "Docs (3)") {
		t.Errorf("second collision: %q", got)
	}
	// An existing file with the name also collides.
	if err := os.WriteFile(filepath.Join(base, "Notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := SafeFolderDir(base, "Notes", "id"); got != filepath.Join(base, "Notes (2)") {
		t.Errorf("file collision: %q", got)
	}
	if runtime.GOOS == "windows" {
		// Case-insensitive file system: "docs" collides with "Docs".
		if got, _ := SafeFolderDir(base, "docs", "id"); got != filepath.Join(base, "docs (3)") {
			t.Errorf("case-insensitive collision: %q", got)
		}
	}
}

func TestNewFolderID(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{5}-[a-z0-9]{5}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewFolderID()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(id) {
			t.Fatalf("NewFolderID = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate folder ID %q", id)
		}
		seen[id] = true
	}
}
