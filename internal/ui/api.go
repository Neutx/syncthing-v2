package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// actionArgs documents every action POST /api/action accepts, with its
// arguments and result. Names outside this table are rejected with 400
// before reaching the Backend; "show" and "quit" are reserved for the
// bearer-authenticated /api/show and /api/quit.
var actionArgs = map[string]string{
	// Status view (§3.9)
	"rescan":           "",
	"pause":            "",
	"resume":           "",
	"restart":          "",
	"start-syncthing":  "",
	"open-folder":      `{"id"?:string}`,
	"open-webui":       "",
	"copy-diagnostics": `-> string or {"text":string}; the page copies it to the clipboard`,
	"set-autostart":    `{"target":"tray"|"syncthing","on":bool}`,
	// Pair view
	"discover":       `-> []model.Candidate`,
	"pair":           `{"deviceID":string}`,
	"accept-device":  `{"deviceID":string}`,
	"decline-device": `{"deviceID":string}`,
	"share-folder":   `{"folderID"?:string,"path"?:string,"label"?:string,"deviceIDs":[]string}`,
	"pick-folder":    `-> string or {"path":string}; "" when cancelled`,
	"accept-folder":  `{"folderID":string,"fromDevice":string,"path"?:string}`,
	"decline-folder": `{"folderID":string,"fromDevice":string}`,
	// Notices
	"fix-gui-exposure":  "",
	"dismiss-notice":    `{"id":string,"forever"?:bool}`,
	"migrate-legacy":    `{"yes":bool}`,
	"install-tailscale": "",
	"open-update":       "opens the release page of Snapshot.UpdateAvailable",
	// Settings view (the §8.1 toggles that §3.9 does not name)
	"get-settings": `-> {"notifications":bool,"checkForUpdates":bool,"profile":"tailnet"|"hybrid"|""}`,
	"set-pref":     `{"key":"notifications"|"checkForUpdates","on":bool}`,
	"set-profile":  `{"profile":"tailnet"|"hybrid"}`,
	"open-logs":    "",
	// Help (§4.2 docs links)
	"open-docs": `{"page":string}; one of DocsPages()`,
	// Welcome view (§5.5, §6.1): the Windows Firewall rule, after a UAC prompt
	"allow-firewall": `-> {"already":bool}`,
	// Window
	"hide": "",
}

// Actions returns every action name POST /api/action accepts, sorted.
func Actions() []string {
	out := make([]string, 0, len(actionArgs))
	for k := range actionArgs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// docsPages are the documentation pages the dashboard may open. The page
// names a key, never a URL, so it cannot send the browser anywhere else.
var docsPages = map[string]string{
	// §4.2: "Tailscale is not connected. Open Tailscale and sign in"
	"tailscale-not-connected": brand.RepoURL + "/blob/main/docs/troubleshooting.md#ts002",
	// Tailscale CLI missing
	"tailscale-setup": brand.RepoURL + "/blob/main/docs/tailscale-setup.md",
	// §4.2: a "blocked" candidate (port 22000 refused: ACLs or firewall)
	"blocked": brand.RepoURL + "/blob/main/docs/troubleshooting.md#net001",
}

// DocsURL returns the address of a documentation page for open-docs, and
// false for a page that is not in the list.
func DocsURL(page string) (string, bool) {
	u, ok := docsPages[page]
	return u, ok
}

// DocsPages returns every page DocsURL knows, sorted.
func DocsPages() []string {
	out := make([]string, 0, len(docsPages))
	for k := range docsPages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// docsError is an action error with a documentation page.
type docsError struct {
	err  error
	page string
}

func (e *docsError) Error() string    { return e.err.Error() }
func (e *docsError) Unwrap() error    { return e.err }
func (e *docsError) DocsPage() string { return e.page }

// WithDocs tags an action error with a documentation page (see DocsURL).
// POST /api/action then answers {"ok":false,"error":...,"docs":page} and the
// page shows a help link next to the message. An unknown page is dropped.
func WithDocs(err error, page string) error {
	if err == nil {
		return nil
	}
	if _, ok := docsPages[page]; !ok {
		return err
	}
	return &docsError{err: err, page: page}
}

// maxActionBody bounds POST /api/action bodies.
const maxActionBody = 64 << 10

type actionRequest struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "request too large"})
		return
	}
	var req actionRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if _, ok := actionArgs[req.Name]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action " + strconv.Quote(req.Name)})
		return
	}
	args := req.Args
	if len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = nil
	} else if !json.Valid(args) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid args"})
		return
	}
	res, err := s.b.Action(r.Context(), req.Name, args)
	if err != nil {
		msg := err.Error()
		if msg == "" {
			msg = "action failed"
		}
		out := map[string]any{"ok": false, "error": msg}
		var de interface{ DocsPage() string }
		if errors.As(err, &de) {
			if _, ok := docsPages[de.DocsPage()]; ok {
				out["docs"] = de.DocsPage()
			}
		}
		writeJSON(w, http.StatusInternalServerError, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) handleBackdrop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	png := s.b.Backdrop()
	if len(png) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(png)
	}
}

// info is static product information for the About view and per-OS wording.
type info struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Disclaimer string `json:"disclaimer"`
	AtLogin    string `json:"atLogin"`
	OS         string `json:"os"`
	RepoURL    string `json:"repoURL"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, info{
		Name:       brand.DisplayName,
		Version:    brand.Version,
		Commit:     brand.Commit(),
		Disclaimer: brand.Disclaimer,
		AtLogin:    brand.AtLogin(),
		OS:         runtime.GOOS,
		RepoURL:    brand.RepoURL,
	})
}
