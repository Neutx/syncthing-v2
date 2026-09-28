package app

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/Neutx/syncthing-v2/internal/pairing"
	"github.com/Neutx/syncthing-v2/internal/stclient"
	"github.com/Neutx/syncthing-v2/internal/tailnet"
	"github.com/Neutx/syncthing-v2/internal/ui"
)

// actionCases returns the string labels of the switch statements in
// (*App).Action, read from backend.go.
func actionCases(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "backend.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Action" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		for _, st := range fn.Body.List {
			sw, ok := st.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			for _, c := range sw.Body.List {
				for _, e := range c.(*ast.CaseClause).List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							out[s] = true
						}
					}
				}
			}
		}
	}
	return out
}

// TestBackendHandlesEveryUIAction keeps (*App).Action in step with the
// dashboard's action table, so no button ends in an "unknown action" toast.
func TestBackendHandlesEveryUIAction(t *testing.T) {
	cases := actionCases(t)
	if len(cases) < 20 {
		t.Fatalf("found only %d action cases in backend.go; the scanner is out of date", len(cases))
	}
	for _, name := range append(ui.Actions(), "show", "quit") {
		if !cases[name] {
			t.Errorf("ui action %q is not handled by (*App).Action", name)
		}
	}
}

func TestOpenDocs(t *testing.T) {
	h := newHarness(t, newFakeST(t), stclient.GUIConfig{Port: 8384})
	for _, page := range ui.DocsPages() {
		if _, err := act(t, h.a, "open-docs", map[string]any{"page": page}); err != nil {
			t.Fatalf("open-docs %s: %v", page, err)
		}
	}
	for _, bad := range []string{"", "https://example.com", "../../etc"} {
		if _, err := act(t, h.a, "open-docs", map[string]any{"page": bad}); err == nil {
			t.Errorf("open-docs %q accepted", bad)
		}
	}
	h.mu.Lock()
	opened := append([]string(nil), h.opened...)
	h.mu.Unlock()
	pages := ui.DocsPages()
	if len(opened) != len(pages) {
		t.Fatalf("opened %v, want one URL per page %v", opened, pages)
	}
	for i, page := range pages {
		if want, _ := ui.DocsURL(page); opened[i] != want {
			t.Errorf("open-docs %s opened %q, want %q", page, opened[i], want)
		}
	}
}

// TestTailnetErrorsCarryDocs: the §4.2 Tailscale messages come with a docs
// link for the Pair view.
func TestTailnetErrorsCarryDocs(t *testing.T) {
	for _, c := range []struct {
		err       error
		msg, page string
	}{
		{fmt.Errorf("%w (state %q)", pairing.ErrTailscaleNotRunning, "NeedsLogin"),
			"Tailscale is not connected. Open Tailscale and sign in.", "tailscale-not-connected"},
		{fmt.Errorf("status: %w", tailnet.ErrNotInstalled),
			"Tailscale is not installed. Install it and sign in, then try again.", "tailscale-setup"},
	} {
		got := tailnetError(c.err)
		if got.Error() != c.msg {
			t.Errorf("message %q, want %q", got.Error(), c.msg)
		}
		var de interface{ DocsPage() string }
		if !errors.As(got, &de) || de.DocsPage() != c.page {
			t.Errorf("%q: docs page missing or wrong, want %q", c.msg, c.page)
		}
		if _, ok := ui.DocsURL(c.page); !ok {
			t.Errorf("page %q is not a known docs page", c.page)
		}
	}
	other := errors.New("probe failed")
	if got := tailnetError(other); got != other {
		t.Errorf("unrelated error rewritten: %v", got)
	}
}
