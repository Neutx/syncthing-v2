package pairing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/deviceid"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// Folder errors.
var (
	// ErrUnsafeName: neither the label nor the folder ID yields a safe directory name.
	ErrUnsafeName = errors.New("no safe directory name for this folder")
	// ErrNotOffered: the device does not currently offer this folder.
	ErrNotOffered = errors.New("this folder is not offered by that device")
	// ErrFolderExists: AcceptFolder was asked to put an offered folder at a
	// path, but a folder with that ID already exists locally somewhere else.
	// The concrete error is a *FolderExistsError carrying the existing path.
	ErrFolderExists = errors.New("this folder already exists on this computer")
)

// FolderExistsError is returned by AcceptFolder when the offered folder ID is
// already configured locally at a different path. The sender is not added;
// accepting again with Path as the destination adds the sender to that
// existing folder.
type FolderExistsError struct {
	FolderID string
	Path     string // the existing folder's path, as configured in Syncthing
}

func (e *FolderExistsError) Error() string {
	return fmt.Sprintf("%v at %s: choose that location to share it with this device", ErrFolderExists, e.Path)
}

// Is makes errors.Is(err, ErrFolderExists) match.
func (e *FolderExistsError) Is(target error) bool { return target == ErrFolderExists }

// maxNameLen caps the sanitised directory name (before a " (n)" suffix).
const maxNameLen = 64

// maxCollisions bounds the " (2)", " (3)", ... search in SafeFolderDir.
const maxCollisions = 1000

// ShareFolder shares folderID with deviceIDs. An existing folder keeps its
// configuration: the devices are merged into its device list by ID and the
// folder is written back only when something was added. A folder that does
// not exist yet is created at path (which must be absolute) as a
// send-receive folder shared with this device and deviceIDs.
func (s *Service) ShareFolder(ctx context.Context, folderID, label, path string, deviceIDs []string) error {
	if strings.TrimSpace(folderID) == "" {
		return errors.New("share folder: empty folder ID")
	}
	ids := make([]string, 0, len(deviceIDs))
	for _, d := range deviceIDs {
		id, err := deviceid.Parse(d)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	myID, err := s.myID(ctx)
	if err != nil {
		return err
	}
	return s.mergeOrCreateFolder(ctx, folderID, label, path, append([]string{myID}, ids...), false)
}

// AcceptFolder accepts a folder offered by a configured device into path
// (normally SafeFolderDir(~/Sync, label, id) or a user-chosen directory).
// The offer must still be pending in Syncthing.
//
// When a folder with the offered ID already exists locally, the sender is
// added to it only if path is that folder's path; otherwise a
// *FolderExistsError (errors.Is ErrFolderExists) names the existing path and
// nothing is written, so an offer can never silently attach the sender to a
// local folder the user did not choose.
func (s *Service) AcceptFolder(ctx context.Context, pf model.PendingFolder, path string) error {
	if strings.TrimSpace(pf.FolderID) == "" {
		return errors.New("accept folder: empty folder ID")
	}
	from, err := deviceid.Parse(pf.FromDevice)
	if err != nil {
		return err
	}
	if _, found, err := s.getDevice(ctx, from); err != nil {
		return err
	} else if !found {
		return ErrNotConfigured
	}
	offer, ok, err := s.pendingOffer(ctx, pf.FolderID, from)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotOffered
	}
	label := pf.Label
	if label == "" {
		label = offer.Label
	}
	myID, err := s.myID(ctx)
	if err != nil {
		return err
	}
	return s.mergeOrCreateFolder(ctx, pf.FolderID, label, path, []string{myID, from}, true)
}

// DeclineFolder removes a pending folder offer from Syncthing.
func (s *Service) DeclineFolder(ctx context.Context, pf model.PendingFolder) error {
	if strings.TrimSpace(pf.FolderID) == "" {
		return errors.New("decline folder: empty folder ID")
	}
	from, err := deviceid.Parse(pf.FromDevice)
	if err != nil {
		return err
	}
	q := url.Values{"folder": {pf.FolderID}, "device": {from}}
	return s.C.Send(ctx, http.MethodDelete, "/rest/cluster/pending/folders?"+q.Encode(), nil)
}

type pendingOffer struct {
	Label string `json:"label"`
}

// pendingOffer reports whether device currently offers folderID.
func (s *Service) pendingOffer(ctx context.Context, folderID, device string) (pendingOffer, bool, error) {
	var pending map[string]struct {
		OfferedBy map[string]pendingOffer `json:"offeredBy"`
	}
	if err := s.C.Get(ctx, "/rest/cluster/pending/folders?"+url.Values{"device": {device}}.Encode(), &pending); err != nil {
		return pendingOffer{}, false, err
	}
	o, ok := pending[folderID].OfferedBy[device]
	return o, ok, nil
}

// mergeOrCreateFolder merges devices into an existing folder (GET, merge,
// PUT; never replacing the list) or creates the folder at path. With
// samePathOnly, an existing folder is only changed when its path is path;
// otherwise a *FolderExistsError is returned.
func (s *Service) mergeOrCreateFolder(ctx context.Context, folderID, label, path string, devices []string, samePathOnly bool) error {
	var folder map[string]any
	err := s.C.Get(ctx, folderPath(folderID), &folder)
	switch {
	case isNotFound(err) || (err == nil && folder == nil):
		return s.createFolder(ctx, folderID, label, path, devices)
	case err != nil:
		return err
	}
	list, changed := mergeDevices(folder["devices"], devices)
	if !changed {
		return nil
	}
	if existing, _ := folder["path"].(string); samePathOnly && !samePath(existing, path) {
		return &FolderExistsError{FolderID: folderID, Path: existing}
	}
	folder["devices"] = list
	folder["id"] = folderID
	return s.C.Send(ctx, http.MethodPut, folderPath(folderID), folder)
}

func (s *Service) createFolder(ctx context.Context, folderID, label, path string, devices []string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("folder path %q is not absolute", path)
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create folder directory: %w", err)
	}
	if label == "" {
		label = filepath.Base(path)
	}
	list, _ := mergeDevices(nil, devices)
	return s.C.Send(ctx, http.MethodPut, folderPath(folderID), map[string]any{
		"id":      folderID,
		"label":   label,
		"path":    path,
		"type":    "sendreceive",
		"devices": list,
	})
}

// samePath reports whether a folder path from the Syncthing config (which may
// start with "~") and a local absolute path name the same directory.
// Comparison ignores case on Windows and macOS, whose default file systems
// are case-insensitive.
func samePath(configured, path string) bool {
	if configured == "" || path == "" {
		return false
	}
	// Syncthing expands a leading "~" followed by the OS path separator.
	configured = filepath.FromSlash(configured)
	if configured == "~" || strings.HasPrefix(configured, "~"+string(filepath.Separator)) {
		h, err := osutil.HomeDir()
		if err != nil {
			return false
		}
		configured = filepath.Join(h, configured[1:])
	}
	a, b := filepath.Clean(configured), filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// mergeDevices adds each of ids to a decoded folder "devices" array unless an
// entry with that deviceID is already there. Existing entries (and their
// extra fields, such as encryption passwords) are kept unchanged and in order.
func mergeDevices(existing any, ids []string) ([]any, bool) {
	arr, _ := existing.([]any)
	out := append([]any(nil), arr...)
	have := make(map[string]bool, len(out)+len(ids))
	for _, e := range out {
		if m, ok := e.(map[string]any); ok {
			if id, ok := m["deviceID"].(string); ok {
				have[id] = true
			}
		}
	}
	changed := false
	for _, id := range ids {
		if have[id] {
			continue
		}
		have[id] = true
		out = append(out, map[string]any{"deviceID": id})
		changed = true
	}
	return out, changed
}

// SafeFolderDir returns a directory under base for an incoming folder with
// the given label and ID. The label comes from a remote device and is not
// trusted:
//
//   - a label that is an absolute path, contains a ".." element or is a
//     Windows reserved device name is rejected;
//   - otherwise only [A-Za-z0-9 ._-] is kept (other characters become
//     spaces), runs of spaces and dots are collapsed, leading and trailing
//     dots and spaces are trimmed, and the name is capped at 64 characters;
//   - when the label yields nothing usable, the folder ID is used under the
//     same rules; when that fails too, ErrUnsafeName is returned;
//   - an existing, non-empty path gets " (2)", " (3)", ... appended;
//   - the result must stay inside base after filepath.Clean.
//
// base must be absolute. The directory is not created.
func SafeFolderDir(base, label, id string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("base directory %q is not absolute", base)
	}
	base = filepath.Clean(base)
	name := safeName(label)
	if name == "" {
		name = safeName(id)
	}
	if name == "" {
		return "", ErrUnsafeName
	}
	for n := 1; n <= maxCollisions; n++ {
		cand := name
		if n > 1 {
			cand = fmt.Sprintf("%s (%d)", name, n)
		}
		p := filepath.Clean(filepath.Join(base, cand))
		if !within(base, p) {
			return "", fmt.Errorf("%w: %q escapes %q", ErrUnsafeName, cand, base)
		}
		free, err := usableDir(p)
		if err != nil {
			return "", err
		}
		if free {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free directory name for %q under %q", name, base)
}

// safeName sanitises one path element, or returns "" when s must be rejected.
func safeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || looksAbsolute(s) {
		return ""
	}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.TrimSpace(part) == ".." {
			return ""
		}
	}
	var b strings.Builder
	var last rune
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		case r == '.':
			if last == '.' {
				continue
			}
		default:
			r = ' '
			if last == ' ' {
				continue
			}
		}
		b.WriteRune(r)
		last = r
	}
	name := strings.TrimRight(strings.TrimLeft(b.String(), ". "), ". ")
	if len(name) > maxNameLen {
		name = strings.TrimRight(name[:maxNameLen], ". ")
	}
	if name == "" || reservedName(name) {
		return ""
	}
	return name
}

// looksAbsolute reports absolute paths of any OS: "/x", "\x", "C:x", "C:\x", UNC.
func looksAbsolute(s string) bool {
	if filepath.IsAbs(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
		return true
	}
	return len(s) >= 2 && s[1] == ':' && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z'))
}

// reservedName reports Windows device names, which are reserved with any
// extension ("CON", "con.txt", "COM1.tar.gz").
func reservedName(name string) bool {
	stem := strings.ToUpper(strings.TrimSpace(strings.SplitN(name, ".", 2)[0]))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// usableDir reports whether p does not exist or is an empty directory.
func usableDir(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// NewFolderID returns a random folder ID in Syncthing's "xxxxx-xxxxx" style
// (lowercase letters and digits, from crypto/rand).
func NewFolderID() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	max := big.NewInt(int64(len(chars)))
	var b strings.Builder
	for i := 0; i < 10; i++ {
		if i == 5 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(chars[n.Int64()])
	}
	return b.String(), nil
}
