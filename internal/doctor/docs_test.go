package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTroubleshootingDocCoversEveryCode keeps docs/troubleshooting.md in step
// with Codes: every finding links to "#<code>", so each code needs its own
// "## CODE" section whose first line repeats the title and the severity.
func TestTroubleshootingDocCoversEveryCode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(b), "\r\n", "\n")
	for _, c := range Codes {
		head := "\n## " + c.Code + "\n\n"
		i := strings.Index(doc, head)
		if i < 0 {
			t.Errorf("%s: no %q section in docs/troubleshooting.md", c.Code, "## "+c.Code)
			continue
		}
		first, _, _ := strings.Cut(doc[i+len(head):], "\n")
		if want := "**" + c.Title + "** (" + c.Severity; !strings.HasPrefix(first, want) {
			t.Errorf("%s: section starts with %q, want the prefix %q", c.Code, first, want)
		}
	}
}
