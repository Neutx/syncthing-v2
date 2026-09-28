package stclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// ConfigFile is the name of Syncthing's configuration file.
const ConfigFile = "config.xml"

// ErrConfigNotFound is wrapped by Discover when no config.xml exists in any
// of the locations it checks.
var ErrConfigNotFound = errors.New("syncthing config.xml not found")

// pathsTimeout bounds each `syncthing paths` / `--paths` call.
const pathsTimeout = 10 * time.Second

// Discover finds the active Syncthing config.xml and reads its endpoint.
// The config path is looked up in this order, taking the first file that
// exists:
//
//  1. $STHOMEDIR/config.xml, then $STCONFDIR/config.xml
//  2. `<syncthingBin> paths` (Syncthing v2), the "Configuration file:" entry
//  3. `<syncthingBin> --paths` (Syncthing v1)
//  4. the per-OS default location
//
// Steps 2 and 3 are skipped when syncthingBin is empty.
func Discover(ctx context.Context, syncthingBin string) (Endpoint, string, error) {
	p, err := defaultFinder().find(ctx, syncthingBin)
	if err != nil {
		return Endpoint{}, "", err
	}
	ep, _, err := ReadConfig(p)
	if err != nil {
		return Endpoint{}, p, err
	}
	return ep, p, nil
}

// finder holds the environment the lookup depends on, so tests can replace it.
type finder struct {
	goos   string
	getenv func(string) string
	home   func() (string, error)
	run    func(ctx context.Context, bin string, args ...string) ([]byte, error)
	exists func(string) bool
}

func defaultFinder() finder {
	return finder{
		goos:   runtime.GOOS,
		getenv: os.Getenv,
		home:   osutil.HomeDir,
		run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			return osutil.Output(ctx, pathsTimeout, bin, args...)
		},
		exists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.Mode().IsRegular()
		},
	}
}

func (f finder) find(ctx context.Context, bin string) (string, error) {
	var tried []string
	check := func(p string) bool {
		if p == "" {
			return false
		}
		p = filepath.Clean(p)
		tried = append(tried, p)
		return f.exists(p)
	}

	for _, env := range []string{"STHOMEDIR", "STCONFDIR"} {
		if d := strings.TrimSpace(f.getenv(env)); d != "" {
			if p := filepath.Join(d, ConfigFile); check(p) {
				return filepath.Clean(p), nil
			}
		}
	}

	if bin != "" {
		for _, args := range [][]string{{"paths"}, {"--paths"}} {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			out, err := f.run(ctx, bin, args...)
			if err != nil {
				continue
			}
			if p := parsePaths(out); check(p) {
				return filepath.Clean(p), nil
			}
		}
	}

	for _, p := range f.defaults() {
		if check(p) {
			return filepath.Clean(p), nil
		}
	}
	if len(tried) == 0 {
		return "", ErrConfigNotFound
	}
	return "", fmt.Errorf("%w (checked %s)", ErrConfigNotFound, strings.Join(tried, ", "))
}

// defaults lists the per-OS default config.xml locations, most likely first.
func (f finder) defaults() []string {
	home, _ := f.home()
	join := func(base string, elem ...string) string {
		if base == "" {
			return ""
		}
		return filepath.Join(append([]string{base}, elem...)...)
	}
	switch f.goos {
	case "windows":
		lad := f.getenv("LOCALAPPDATA")
		if lad == "" {
			lad = join(home, "AppData", "Local")
		}
		return []string{join(lad, "Syncthing", ConfigFile)}
	case "darwin":
		return []string{join(home, "Library", "Application Support", "Syncthing", ConfigFile)}
	default:
		var out []string
		if x := f.getenv("XDG_STATE_HOME"); x != "" {
			out = append(out, join(x, "syncthing", ConfigFile))
		}
		out = append(out, join(home, ".local", "state", "syncthing", ConfigFile))
		if x := f.getenv("XDG_CONFIG_HOME"); x != "" {
			out = append(out, join(x, "syncthing", ConfigFile))
		}
		out = append(out, join(home, ".config", "syncthing", ConfigFile))
		return out
	}
}

// parsePaths extracts the config file path from `syncthing paths` output:
//
//	Configuration file:
//		/home/user/.local/state/syncthing/config.xml
//
// A value on the same line as the label is accepted too.
func parsePaths(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	found := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if found {
			if line != "" {
				return line
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Configuration file:"); ok {
			if rest = strings.TrimSpace(rest); rest != "" {
				return rest
			}
			found = true
		}
	}
	return ""
}
