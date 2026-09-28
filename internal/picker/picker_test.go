package picker

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestInitialDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":                               "",
		dir:                              dir,
		dir + string(filepath.Separator): dir,
		filepath.Join(dir, "sub", ".."):  dir,
		file:                             "", // not a directory
		filepath.Join(dir, "missing"):    "",
		"relative/path":                  "",
		dir + "\x00evil":                 "",
	}
	for in, want := range cases {
		if got := initialDir(in); got != want {
			t.Errorf("initialDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResult(t *testing.T) {
	abs := t.TempDir()
	cases := []struct {
		in, want string
		err      bool
	}{
		{"", "", false},
		{"\n", "", false},
		{abs + "\n", abs, false},
		{abs + string(filepath.Separator) + "\n", abs, false}, // osascript's trailing slash
		{"relative\n", "", true},
		{abs + "\n" + abs + "\n", "", true}, // two lines
	}
	for _, c := range cases {
		got, err := result(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("result(%q) = %q, %v; want %q, error %v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestArgs(t *testing.T) {
	// Titles and paths are separate argv entries; nothing is ever spliced
	// into a script or a shell line.
	title := `Pick "a" folder; rm -rf ~`
	got := osascriptArgs(title, "/Users/test/Sync")
	if len(got) != 4 || got[0] != "-e" || got[1] != osascriptScript || got[2] != title || got[3] != "/Users/test/Sync" {
		t.Errorf("osascriptArgs = %q", got)
	}
	if got := osascriptArgs(title, ""); len(got) != 3 {
		t.Errorf("osascriptArgs without initial = %q", got)
	}

	want := []string{"--file-selection", "--directory", "--title=" + title, "--filename=/home/test/Sync/"}
	if got := zenityArgs(title, "/home/test/Sync"); !reflect.DeepEqual(got, want) {
		t.Errorf("zenityArgs = %q, want %q", got, want)
	}
	if got := zenityArgs(title, ""); len(got) != 3 {
		t.Errorf("zenityArgs without initial = %q", got)
	}

	want = []string{"--title", title, "--getexistingdirectory", "/home/test/Sync"}
	if got := kdialogArgs(title, "/home/test/Sync"); !reflect.DeepEqual(got, want) {
		t.Errorf("kdialogArgs = %q, want %q", got, want)
	}
	if got := kdialogArgs(title, ""); !filepath.IsAbs(got[3]) && got[3] != "/" {
		t.Errorf("kdialogArgs without initial starts in %q", got[3])
	}
}

func TestFolderWithoutPickerOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the no-picker fallback is Linux-only")
	}
	t.Setenv("PATH", t.TempDir()) // neither zenity nor kdialog
	got, err := Folder("Choose", "")
	if got != "" || err != nil {
		t.Errorf("Folder with no picker = %q, %v; want \"\", nil", got, err)
	}
}
