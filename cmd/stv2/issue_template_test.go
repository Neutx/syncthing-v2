package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// readRepoFile reads a file relative to the repository root with LF line ends.
func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestBugReportVersionPlaceholder keeps the issue form's version placeholder in
// the shape of the first line that "stv2 version" really prints.
func TestBugReportVersionPlaceholder(t *testing.T) {
	tmpl := readRepoFile(t, ".github", "ISSUE_TEMPLATE", "bug_report.yml")
	m := regexp.MustCompile(`(?m)^\s+id: version\n(?:.*\n)*?\s+placeholder: "([^"]*)"`).FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatal("bug_report.yml: no placeholder for the version field")
	}
	var buf bytes.Buffer
	cmdVersion(&buf)
	first, _, _ := strings.Cut(buf.String(), "\n")
	prefix := brand.DisplayName + " "
	if !strings.HasPrefix(first, prefix) {
		t.Fatalf("cmdVersion first line %q does not start with %q", first, prefix)
	}
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `\d+\.\d+\.\d+$`).MatchString(m[1]) {
		t.Errorf("version placeholder %q, want %q followed by a version such as 1.0.0", m[1], prefix)
	}
}

// TestBugReportUsesFullPathCommands checks that the issue form tells people to
// run stv2 by the same full paths as docs/troubleshooting.md: stv2 is not on
// PATH on Windows or macOS, and on Windows the output needs "| Out-Host".
func TestBugReportUsesFullPathCommands(t *testing.T) {
	tmpl := readRepoFile(t, ".github", "ISSUE_TEMPLATE", "bug_report.yml")
	doc := readRepoFile(t, "docs", "troubleshooting.md")

	exe := func(os string) string {
		m := regexp.MustCompile("(?m)^\\| " + os + " \\| `(.+?) doctor").FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("docs/troubleshooting.md: no %s row with a doctor command", os)
		}
		return m[1]
	}
	win, mac := exe("Windows"), exe("macOS")
	for _, want := range []string{
		win + " version | Out-Host",
		win + " doctor --json | Out-Host",
		mac + " version",
		mac + " doctor --json",
		"docs/troubleshooting.md#troubleshooting",
	} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("bug_report.yml lacks %q", want)
		}
	}
	if strings.Contains(tmpl, "Run `stv2 doctor") {
		t.Error("bug_report.yml still tells people to run a bare `stv2 doctor`")
	}
}
