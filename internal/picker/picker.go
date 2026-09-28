// Package picker shows the platform's native folder picker:
// SHBrowseForFolderW on Windows, `osascript` "choose folder" on macOS, and
// zenity or kdialog on Linux when one is installed. Every external program
// gets its arguments as an argument list, never through a shell.
package picker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Folder asks the user to choose a folder and returns its absolute path. It
// returns "" with a nil error if the user cancels or no picker is available.
// title is the prompt; initial, if it is an existing absolute directory, is
// preselected.
func Folder(title, initial string) (string, error) {
	title = strings.TrimSpace(strings.ReplaceAll(title, "\x00", ""))
	if title == "" {
		title = "Choose a folder"
	}
	p, err := folder(title, initialDir(initial))
	if err != nil || p == "" {
		return "", err
	}
	return result(p)
}

// initialDir returns initial cleaned if it names an existing directory by
// absolute path, otherwise "".
func initialDir(initial string) string {
	if initial == "" || strings.ContainsRune(initial, 0) || !filepath.IsAbs(initial) {
		return ""
	}
	initial = filepath.Clean(initial)
	if fi, err := os.Stat(initial); err != nil || !fi.IsDir() {
		return ""
	}
	return initial
}

// result validates a picker's output: one line holding an absolute path.
func result(out string) (string, error) {
	p := strings.TrimRight(out, "\r\n")
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "\r\n\x00") || !filepath.IsAbs(p) {
		return "", fmt.Errorf("picker: unexpected result %q", p)
	}
	return filepath.Clean(p), nil // also drops the trailing slash osascript adds
}

// osascriptScript is run by osascript with the prompt and, optionally, the
// initial folder as argv, so neither is ever parsed as AppleScript source.
const osascriptScript = `on run argv
	if (count of argv) > 1 then
		set f to choose folder with prompt (item 1 of argv) default location (POSIX file (item 2 of argv))
	else
		set f to choose folder with prompt (item 1 of argv)
	end if
	return POSIX path of f
end run`

// osascriptArgs builds the osascript argument list.
func osascriptArgs(title, initial string) []string {
	args := []string{"-e", osascriptScript, title}
	if initial != "" {
		args = append(args, initial)
	}
	return args
}

// zenityArgs builds the zenity argument list. A trailing separator makes
// zenity open inside the initial folder rather than select it in its parent.
func zenityArgs(title, initial string) []string {
	args := []string{"--file-selection", "--directory", "--title=" + title}
	if initial != "" {
		args = append(args, "--filename="+strings.TrimRight(initial, "/")+"/")
	}
	return args
}

// kdialogArgs builds the kdialog argument list; initial must be absolute (or
// empty, which starts in the home folder).
func kdialogArgs(title, initial string) []string {
	start := initial
	if start == "" {
		if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
			start = h
		} else {
			start = "/"
		}
	}
	return []string{"--title", title, "--getexistingdirectory", start}
}
