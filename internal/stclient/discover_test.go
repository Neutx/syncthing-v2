package stclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The test binary doubles as a fake `syncthing` for the discovery tests.
const (
	fakeModeEnv = "STV2_STCLIENT_FAKE_SYNCTHING"
	fakePathEnv = "STV2_STCLIENT_FAKE_CONFIG"
)

func TestMain(m *testing.M) {
	mode := os.Getenv(fakeModeEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	switch {
	case mode == "v2" && arg == "paths":
		fmt.Printf("Configuration file:\n\t%s\n\nDevice private key & certificate files:\n\t/nonexistent/key.pem\n", os.Getenv(fakePathEnv))
		os.Exit(0)
	case mode == "v1" && arg == "--paths":
		fmt.Printf("Configuration file:\r\n\t%s\r\n\r\nDatabase location:\r\n\t/nonexistent/index\r\n", os.Getenv(fakePathEnv))
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(1)
	}
}

func TestParsePaths(t *testing.T) {
	cases := map[string]string{
		"Configuration file:\n\t/home/u/.local/state/syncthing/config.xml\n":               "/home/u/.local/state/syncthing/config.xml",
		"Configuration file:\r\n\tC:\\Users\\u\\AppData\\Local\\Syncthing\\config.xml\r\n": `C:\Users\u\AppData\Local\Syncthing\config.xml`,
		"Configuration file: /srv/st/config.xml\n":                                         "/srv/st/config.xml",
		"Configuration file:\n\n   /x/config.xml\n":                                        "/x/config.xml",
		"Database location:\n\t/x/index\n":                                                 "",
		"":                                                                                 "",
	}
	for in, want := range cases {
		if got := parsePaths([]byte(in)); got != want {
			t.Errorf("parsePaths(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeEnv struct {
	env    map[string]string
	home   string
	files  map[string]bool
	output map[string]string // first arg → stdout; missing = command fails
	calls  []string
}

func (f *fakeEnv) finder(goos string) finder {
	return finder{
		goos:   goos,
		getenv: func(k string) string { return f.env[k] },
		home:   func() (string, error) { return f.home, nil },
		run: func(_ context.Context, bin string, args ...string) ([]byte, error) {
			f.calls = append(f.calls, bin+" "+strings.Join(args, " "))
			out, ok := f.output[args[0]]
			if !ok {
				return nil, errors.New("exit status 1")
			}
			return []byte(out), nil
		},
		exists: func(p string) bool { return f.files[p] },
	}
}

func TestFindOrder(t *testing.T) {
	home := filepath.Join("h", "user")
	homeCfg := filepath.Join("h", "st-home", ConfigFile)
	confCfg := filepath.Join("h", "st-conf", ConfigFile)
	v2Cfg := filepath.Join("h", "v2", ConfigFile)
	v1Cfg := filepath.Join("h", "v1", ConfigFile)
	linuxDefault := filepath.Join(home, ".local", "state", "syncthing", ConfigFile)

	base := func() *fakeEnv {
		return &fakeEnv{
			env:  map[string]string{"STHOMEDIR": filepath.Join("h", "st-home"), "STCONFDIR": filepath.Join("h", "st-conf")},
			home: home,
			files: map[string]bool{
				homeCfg: true, confCfg: true, v2Cfg: true, v1Cfg: true, linuxDefault: true,
			},
			output: map[string]string{
				"paths":   "Configuration file:\n\t" + v2Cfg + "\n",
				"--paths": "Configuration file:\n\t" + v1Cfg + "\n",
			},
		}
	}
	ctx := context.Background()

	f := base()
	if p, err := f.finder("linux").find(ctx, "syncthing"); err != nil || p != homeCfg {
		t.Errorf("STHOMEDIR: %q %v", p, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("binary run although STHOMEDIR matched: %v", f.calls)
	}

	f = base()
	f.files[homeCfg] = false
	if p, err := f.finder("linux").find(ctx, "syncthing"); err != nil || p != confCfg {
		t.Errorf("STCONFDIR: %q %v", p, err)
	}

	f = base()
	f.env = nil
	if p, err := f.finder("linux").find(ctx, "syncthing"); err != nil || p != v2Cfg {
		t.Errorf("paths: %q %v", p, err)
	}

	f = base()
	f.env = nil
	delete(f.output, "paths")
	if p, err := f.finder("linux").find(ctx, "syncthing"); err != nil || p != v1Cfg {
		t.Errorf("--paths: %q %v", p, err)
	}
	if want := []string{"syncthing paths", "syncthing --paths"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}

	// A path reported by the binary that does not exist falls through.
	f = base()
	f.env = nil
	f.files[v2Cfg], f.files[v1Cfg] = false, false
	if p, err := f.finder("linux").find(ctx, "syncthing"); err != nil || p != linuxDefault {
		t.Errorf("default: %q %v", p, err)
	}

	// No binary: straight to the defaults.
	f = base()
	f.env = nil
	if p, err := f.finder("linux").find(ctx, ""); err != nil || p != linuxDefault || len(f.calls) != 0 {
		t.Errorf("no binary: %q %v %v", p, err, f.calls)
	}

	// Nothing anywhere.
	f = &fakeEnv{home: home}
	_, err := f.finder("linux").find(ctx, "syncthing")
	if !errors.Is(err, ErrConfigNotFound) || !strings.Contains(err.Error(), linuxDefault) {
		t.Errorf("not found error = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	home := filepath.Join("h", "user")
	cases := []struct {
		goos string
		env  map[string]string
		want []string
	}{
		{"windows", map[string]string{"LOCALAPPDATA": filepath.Join("h", "lad")},
			[]string{filepath.Join("h", "lad", "Syncthing", ConfigFile)}},
		{"windows", nil,
			[]string{filepath.Join(home, "AppData", "Local", "Syncthing", ConfigFile)}},
		{"darwin", nil,
			[]string{filepath.Join(home, "Library", "Application Support", "Syncthing", ConfigFile)}},
		{"linux", nil, []string{
			filepath.Join(home, ".local", "state", "syncthing", ConfigFile),
			filepath.Join(home, ".config", "syncthing", ConfigFile),
		}},
		{"linux", map[string]string{"XDG_STATE_HOME": filepath.Join("h", "state"), "XDG_CONFIG_HOME": filepath.Join("h", "cfg")}, []string{
			filepath.Join("h", "state", "syncthing", ConfigFile),
			filepath.Join(home, ".local", "state", "syncthing", ConfigFile),
			filepath.Join("h", "cfg", "syncthing", ConfigFile),
			filepath.Join(home, ".config", "syncthing", ConfigFile),
		}},
	}
	for _, c := range cases {
		f := &fakeEnv{env: c.env, home: home}
		if got := f.finder(c.goos).defaults(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: defaults = %v, want %v", c.goos, c.env, got, c.want)
		}
	}
}

// isolateEnv points every variable discovery reads at empty temp dirs, so
// the tests never see a real Syncthing installation.
func isolateEnv(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	for _, k := range []string{"STHOMEDIR", "STCONFDIR"} {
		t.Setenv(k, "")
	}
	for _, k := range []string{"LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, empty)
	}
}

func TestDiscoverWithFakeBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"v2", "v1"} {
		t.Run(mode, func(t *testing.T) {
			isolateEnv(t)
			_, cfg := writeConfig(t, `<gui enabled="true" tls="false"><address>[::]:18385</address><apikey>synthetic-discover-key</apikey></gui>`)
			t.Setenv(fakeModeEnv, mode)
			t.Setenv(fakePathEnv, cfg)

			ep, p, err := Discover(context.Background(), exe)
			if err != nil {
				t.Fatal(err)
			}
			if p != filepath.Clean(cfg) || ep.BaseURL.String() != "http://127.0.0.1:18385" || ep.APIKey != "synthetic-discover-key" {
				t.Errorf("Discover = %s %q %q", ep.BaseURL, ep.APIKey, p)
			}
		})
	}
}

func TestDiscoverHomeDirEnv(t *testing.T) {
	isolateEnv(t)
	dir, _ := writeConfig(t, `<gui enabled="true"><address>127.0.0.1:18386</address><apikey>synthetic-env-key</apikey></gui>`)
	t.Setenv("STHOMEDIR", dir)
	t.Setenv(fakeModeEnv, "")
	ep, p, err := Discover(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, ConfigFile) || ep.BaseURL.Host != "127.0.0.1:18386" {
		t.Errorf("Discover = %s %q", ep.BaseURL, p)
	}
}

func TestDiscoverNotFound(t *testing.T) {
	isolateEnv(t)
	_, _, err := Discover(context.Background(), "")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Discover in empty env = %v, want ErrConfigNotFound", err)
	}
}
