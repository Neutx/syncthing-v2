// Package doctor runs the `stv2 doctor` checks (spec §3.11) and reports
// them as text or JSON. Every finding carries a stable code that maps to a
// section of docs/troubleshooting.md. The exit code is 0 when there is no
// "high" finding and 1 otherwise.
//
// The checks only read: they never change Syncthing, Tailscale or the
// firewall (the one write is the release check recording its time in
// SyncThing V2's own prefs). Everything they touch comes through Env, so
// tests inject fakes.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/brand"
)

// Severities, from most to least serious.
const (
	High = "high"
	Warn = "warn"
	Info = "info"
)

// Check statuses.
const (
	StatusPass = "pass" // checked, no problem
	StatusFail = "fail" // checked, at least one finding
	StatusSkip = "skip" // not applicable, or a prerequisite failed
)

// Code describes one doctor code.
type Code struct {
	Code     string
	Severity string
	Title    string
}

// Codes lists every code in report order (§3.11).
var Codes = []Code{
	{"TS001", High, "Tailscale CLI not found"},
	{"TS002", High, "Tailscale is not connected"},
	{"ST001", High, "Syncthing binary not found"},
	{"ST002", High, "Syncthing is not responding"},
	{"ST003", High, "Syncthing rejected the API key"},
	{"ST004", Warn, "Syncthing is older than the minimum supported version"},
	{"ST005", Info, "Syncthing does not listen on the default sync port 22000"},
	{"SEC001", High, "Syncthing control panel reachable from the network without protection"},
	{"NET001", Warn, "Syncthing on a tailnet device is not reachable on port 22000"},
	{"FW001", Info, "Windows Firewall rule for Syncthing is missing"},
	{"UI001", Warn, "WebView2 runtime missing"},
	{"UI002", Warn, "No tray host (StatusNotifierWatcher) on the session bus"},
	{"UPD001", Info, "A newer SyncThing V2 release exists"},
}

// SchemaVersion is the version of the JSON report layout. It changes only
// when a field is removed or changes meaning.
const SchemaVersion = 1

// DocsURL returns the troubleshooting section for a code.
func DocsURL(code string) string {
	return brand.RepoURL + "/blob/main/docs/troubleshooting.md#" + strings.ToLower(code)
}

// Finding is one problem.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Docs     string `json:"docs"`
}

// Check is the outcome of one code's check.
type Check struct {
	Code   string `json:"code"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"` // why a check was skipped
}

// Summary counts findings per severity.
type Summary struct {
	High int `json:"high"`
	Warn int `json:"warn"`
	Info int `json:"info"`
}

// Report is the result of Run. Findings and Checks follow the order of
// Codes; Findings is never null in JSON.
type Report struct {
	Schema   int       `json:"schema"`
	Version  string    `json:"version"`
	OS       string    `json:"os"`
	OK       bool      `json:"ok"` // no high findings
	Summary  Summary   `json:"summary"`
	Findings []Finding `json:"findings"`
	Checks   []Check   `json:"checks"`
}

// ExitCode is 0 without high findings and 1 otherwise.
func (r Report) ExitCode() int {
	if r.OK {
		return 0
	}
	return 1
}

// Run executes every check with env and returns the report.
func Run(ctx context.Context, env Env) Report {
	env = env.withDefaults()
	res := runChecks(ctx, env)
	rep := Report{Schema: SchemaVersion, Version: brand.Version, OS: env.GOOS, Findings: []Finding{}}
	for _, c := range Codes {
		chk := Check{Code: c.Code, Status: StatusPass}
		if reason, skipped := res.skipped[c.Code]; skipped {
			chk.Status, chk.Reason = StatusSkip, reason
		}
		for _, msg := range res.found[c.Code] {
			chk.Status, chk.Reason = StatusFail, ""
			rep.Findings = append(rep.Findings, Finding{Code: c.Code, Severity: c.Severity, Title: c.Title, Message: msg, Docs: DocsURL(c.Code)})
			switch c.Severity {
			case High:
				rep.Summary.High++
			case Warn:
				rep.Summary.Warn++
			default:
				rep.Summary.Info++
			}
		}
		rep.Checks = append(rep.Checks, chk)
	}
	rep.OK = rep.Summary.High == 0
	return rep
}

// WriteJSON writes the report as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// WriteText writes the report for a terminal.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s doctor (%s, %s)\n\n", brand.DisplayName, r.Version, r.OS)
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "%-4s  %-6s  %s\n", strings.ToUpper(f.Severity), f.Code, f.Title)
		for _, line := range wrap(f.Message, 70) {
			fmt.Fprintf(&b, "              %s\n", line)
		}
		fmt.Fprintf(&b, "              %s\n\n", f.Docs)
	}
	pass, skip := 0, 0
	for _, c := range r.Checks {
		switch c.Status {
		case StatusPass:
			pass++
		case StatusSkip:
			skip++
		}
	}
	if n := len(r.Findings); n == 0 {
		b.WriteString("No problems found.")
	} else {
		fmt.Fprintf(&b, "%d %s found (%d high, %d warn, %d info).", n, plural(n, "problem"), r.Summary.High, r.Summary.Warn, r.Summary.Info)
	}
	fmt.Fprintf(&b, " %d %s passed, %d skipped.\n", pass, plural(pass, "check"), skip)
	_, err := io.WriteString(w, b.String())
	return err
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// wrap splits s into lines of at most width runes at spaces.
func wrap(s string, width int) []string {
	var lines []string
	var cur strings.Builder
	for _, word := range strings.Fields(s) {
		if cur.Len() > 0 && len([]rune(cur.String()))+1+len([]rune(word)) > width {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}
