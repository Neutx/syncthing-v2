//go:build darwin

package osutil

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFolderOpenArgsRevealsBundles(t *testing.T) {
	dir := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plain := mk("Photos")
	app := mk("Calculator.app")
	pkg := mk("Setup.pkg")
	pane := mk("Evil.prefPane")
	bare := mk("NoExtension")
	mk("NoExtension", "Contents", "MacOS")

	for path, want := range map[string][]string{
		plain:                          {plain},
		app:                            {"-R", app},
		pkg:                            {"-R", pkg},
		pane:                           {"-R", pane},
		bare:                           {"-R", bare},
		"/Applications/Calculator.app": {"-R", "/Applications/Calculator.app"},
	} {
		if got := folderOpenArgs(path); !reflect.DeepEqual(got, want) {
			t.Errorf("folderOpenArgs(%q) = %q, want %q", path, got, want)
		}
	}
}
