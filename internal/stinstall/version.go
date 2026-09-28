package stinstall

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// semver is a parsed "vMAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]" version.
type semver struct {
	major, minor, patch int
	pre                 []string // dot-separated pre-release identifiers; nil for a release
}

// versionRe finds a Syncthing version inside `syncthing --version` output,
// for example `syncthing v2.1.5 "Hafnium Hornet" (go1.27.1 windows-amd64) ...`.
var versionRe = regexp.MustCompile(`\bv?(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?`)

// parseVersion parses a version such as "v2.1.5", "1.27.0" or
// "v2.0.0-rc.1+3-gabcdef". Leading and trailing text is not allowed.
func parseVersion(s string) (semver, error) {
	s = strings.TrimSpace(s)
	m := versionRe.FindStringSubmatchIndex(s)
	if m == nil || m[0] != 0 || m[1] != len(s) {
		return semver{}, fmt.Errorf("cannot parse version %q", s)
	}
	return semverFrom(s, m)
}

func semverFrom(s string, m []int) (semver, error) {
	num := func(i int) (int, error) {
		return strconv.Atoi(s[m[2*i]:m[2*i+1]])
	}
	var v semver
	var err error
	if v.major, err = num(1); err != nil {
		return semver{}, fmt.Errorf("cannot parse version %q: %w", s, err)
	}
	if v.minor, err = num(2); err != nil {
		return semver{}, fmt.Errorf("cannot parse version %q: %w", s, err)
	}
	if v.patch, err = num(3); err != nil {
		return semver{}, fmt.Errorf("cannot parse version %q: %w", s, err)
	}
	if m[8] >= 0 {
		v.pre = strings.Split(s[m[8]+1:m[9]], ".")
	}
	return v, nil
}

// ParseVersionOutput extracts the version (for example "v2.1.5") from the
// output of `syncthing --version`. It returns "" when none is found.
func ParseVersionOutput(out string) string {
	// Only tokens starting with "v" are candidates, which skips the Go
	// version ("(go1.27.1") and the build date.
	for _, w := range strings.Fields(out) {
		if !strings.HasPrefix(w, "v") {
			continue
		}
		if _, err := parseVersion(w); err == nil {
			return w
		}
	}
	return ""
}

// compare returns -1, 0 or +1 following semver precedence (build metadata
// is ignored; a pre-release sorts before its release).
func (a semver) compare(b semver) int {
	for _, d := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if d[0] != d[1] {
			return cmpInt(d[0], d[1])
		}
	}
	switch {
	case a.pre == nil && b.pre == nil:
		return 0
	case a.pre == nil:
		return 1
	case b.pre == nil:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePre(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a.pre), len(b.pre))
}

// comparePre compares two pre-release identifiers: numeric identifiers
// compare numerically and sort before alphanumeric ones.
func comparePre(x, y string) int {
	xn, xerr := strconv.Atoi(x)
	yn, yerr := strconv.Atoi(y)
	switch {
	case xerr == nil && yerr == nil:
		return cmpInt(xn, yn)
	case xerr == nil:
		return -1
	case yerr == nil:
		return 1
	}
	return strings.Compare(x, y)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CompareVersions compares two versions and returns -1, 0 or +1.
func CompareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

// CheckVersion reports whether Syncthing version v is at least
// brand.MinSyncthing. msg explains the result in one sentence, suitable for
// doctor ST004.
func CheckVersion(v string) (ok bool, msg string) {
	return checkVersion(v, brand.MinSyncthing)
}

func checkVersion(v, minimum string) (bool, string) {
	if strings.TrimSpace(v) == "" {
		return false, "The Syncthing version is unknown; SyncThing V2 needs " + minimum + " or newer."
	}
	c, err := CompareVersions(v, minimum)
	if err != nil {
		return false, fmt.Sprintf("Cannot read the Syncthing version %q; SyncThing V2 needs %s or newer.", v, minimum)
	}
	if c < 0 {
		return false, fmt.Sprintf("Syncthing %s is older than %s, the oldest version SyncThing V2 supports. Update Syncthing.", v, minimum)
	}
	return true, fmt.Sprintf("Syncthing %s is supported (%s or newer).", v, minimum)
}
