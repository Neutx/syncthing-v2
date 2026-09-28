package stinstall

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrUnsafeArchive is wrapped by extraction errors caused by an archive entry
// that could write outside the destination: an absolute path, a ".." element,
// a drive or stream name, a link pointing outside the archive, or a special
// file.
var ErrUnsafeArchive = errors.New("unsafe archive entry")

// noticeFiles are upstream's notices kept next to the binary (MPL-2.0).
var noticeFiles = []string{"LICENSE.txt", "README.txt", "AUTHORS.txt"}

// maxMember bounds the size of one extracted file.
const maxMember = 512 << 20

// maxLinkTarget bounds the length of a symlink target read from a zip entry.
const maxLinkTarget = 4096

// entryPath validates an archive member name and returns it cleaned, with
// "/" separators. Backslashes count as separators, because Windows would
// treat them so when the name is joined to the destination.
func entryPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\x00:") {
		return "", fmt.Errorf("%w: %q", ErrUnsafeArchive, name)
	}
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafeArchive, name)
	}
	for _, elem := range strings.Split(n, "/") {
		if elem == ".." {
			return "", fmt.Errorf("%w: %q leaves the archive", ErrUnsafeArchive, name)
		}
	}
	return path.Clean(n), nil
}

// linkTarget validates the target of a symbolic link stored at entry (a
// cleaned entryPath): it must be relative and resolve inside the archive.
func linkTarget(entry, target string) error {
	t := strings.ReplaceAll(target, `\`, "/")
	if t == "" || strings.ContainsAny(t, "\x00:") || strings.HasPrefix(t, "/") {
		return fmt.Errorf("%w: link %q -> %q", ErrUnsafeArchive, entry, target)
	}
	r := path.Join(path.Dir(entry), t)
	if r == ".." || strings.HasPrefix(r, "../") {
		return fmt.Errorf("%w: link %q -> %q points outside the archive", ErrUnsafeArchive, entry, target)
	}
	return nil
}

// wanted maps an archive member to the file name it is installed as, or ""
// when the member is not installed. Members are expected one level below the
// release's top directory ("syncthing-linux-amd64-v2.1.5/syncthing").
func wanted(entry, binName string) string {
	parts := strings.Split(entry, "/")
	if len(parts) != 2 {
		return ""
	}
	if parts[1] == binName {
		return binName
	}
	for _, n := range noticeFiles {
		if parts[1] == n {
			return n
		}
	}
	return ""
}

// staging collects extracted members in hidden temporary files inside dest
// and moves them into place only after the whole archive was checked.
type staging struct {
	dest    string
	binName string
	tmp     map[string]string // installed name -> temporary path
}

func newStaging(dest, binName string) *staging {
	return &staging{dest: dest, binName: binName, tmp: map[string]string{}}
}

func (s *staging) add(name string, r io.Reader) error {
	if _, dup := s.tmp[name]; dup {
		return fmt.Errorf("%w: %s appears twice", ErrUnsafeArchive, name)
	}
	f, err := os.CreateTemp(s.dest, ".stv2-extract-*")
	if err != nil {
		return err
	}
	s.tmp[name] = f.Name()
	n, err := io.Copy(f, io.LimitReader(r, maxMember+1))
	if err == nil && n > maxMember {
		err = fmt.Errorf("%s is larger than %d bytes", name, maxMember)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// commit renames the staged files into dest and returns the binary's path.
//
// The binary goes first. If it cannot be replaced (on Windows, typically
// because the old syncthing.exe is running), commit fails before any notice
// file is touched, so dest keeps a consistent previous installation. If a
// notice file fails after the binary was installed, the other notices are
// still installed and commit returns the binary's path together with an
// error that says the binary is in place.
func (s *staging) commit() (string, error) {
	if _, ok := s.tmp[s.binName]; !ok {
		return "", fmt.Errorf("archive does not contain %s", s.binName)
	}
	if _, ok := s.tmp["LICENSE.txt"]; !ok {
		return "", errors.New("archive does not contain LICENSE.txt")
	}
	order := make([]string, 0, len(s.tmp))
	for _, name := range append([]string{s.binName}, noticeFiles...) {
		if _, ok := s.tmp[name]; ok {
			order = append(order, name)
		}
	}
	for _, name := range order {
		mode := os.FileMode(0o644)
		if name == s.binName {
			mode = 0o755
		}
		if err := os.Chmod(s.tmp[name], mode); err != nil {
			return "", err
		}
	}
	bin := filepath.Join(s.dest, s.binName)
	if err := s.install(s.binName); err != nil {
		return "", err
	}
	var errs []error
	for _, name := range order[1:] {
		if err := s.install(name); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return bin, fmt.Errorf("%s was installed, but not all of upstream's notices: %w", bin, errors.Join(errs...))
	}
	return bin, nil
}

// install moves one staged file into place.
func (s *staging) install(name string) error {
	if err := os.Rename(s.tmp[name], filepath.Join(s.dest, name)); err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	delete(s.tmp, name)
	return nil
}

// cleanup removes staged files that were not committed.
func (s *staging) cleanup() {
	for _, tmp := range s.tmp {
		_ = os.Remove(tmp)
	}
}

// extract installs the Syncthing binary and upstream's notices from a
// release archive (.zip or .tar.gz, chosen by name) into dest and returns
// the binary's path. Every entry is checked, not only the installed ones;
// any unsafe entry rejects the whole archive and nothing is installed.
func extract(archive, dest, binName string) (string, error) {
	st := newStaging(dest, binName)
	defer st.cleanup()
	var err error
	switch {
	case strings.HasSuffix(archive, ".zip"):
		err = extractZip(archive, st)
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		err = extractTarGz(archive, st)
	default:
		err = fmt.Errorf("unknown archive type: %s", filepath.Base(archive))
	}
	if err != nil {
		return "", err
	}
	return st.commit()
}

func extractZip(archive string, st *staging) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name, err := entryPath(f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			target, err := readZipLink(f)
			if err != nil {
				return err
			}
			if err := linkTarget(name, target); err != nil {
				return err
			}
			continue
		case mode.IsDir():
			continue
		case !mode.IsRegular():
			return fmt.Errorf("%w: %q is a special file", ErrUnsafeArchive, f.Name)
		}
		inst := wanted(name, st.binName)
		if inst == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = st.add(inst, rc)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func readZipLink(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLinkTarget+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxLinkTarget {
		return "", fmt.Errorf("%w: link %q target too long", ErrUnsafeArchive, f.Name)
	}
	return string(b), nil
}

func extractTarGz(archive string, st *staging) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := entryPath(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink:
			if err := linkTarget(name, h.Linkname); err != nil {
				return err
			}
			continue
		case tar.TypeLink: // hard link: Linkname names another member
			if _, err := entryPath(h.Linkname); err != nil {
				return err
			}
			continue
		case tar.TypeReg, tar.TypeGNUSparse:
		default:
			return fmt.Errorf("%w: %q has type %q", ErrUnsafeArchive, h.Name, h.Typeflag)
		}
		if inst := wanted(name, st.binName); inst != "" {
			if err := st.add(inst, tr); err != nil {
				return err
			}
		}
	}
}
