package stinstall

import (
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
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// generateTimeout bounds `syncthing generate` (it creates a key pair).
const generateTimeout = 2 * time.Minute

// generateEnvDrop are Syncthing environment variables that would conflict
// with --home or turn port probing off.
var generateEnvDrop = []string{"STHOMEDIR", "STCONFDIR", "STDATADIR", "STNOPORTPROBING"}

// Generate runs `<bin> generate --home <home>`, which creates Syncthing's key
// pair and config.xml in home. Port probing stays on, so Syncthing picks free
// GUI and listen ports when 8384 or 22000 are taken; callers read the GUI
// address and API key from the generated config.xml instead of assuming them.
// An existing config in home is kept by Syncthing.
func Generate(ctx context.Context, bin, home string) error {
	if bin == "" {
		return errors.New("generate: no syncthing binary")
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("generate: home %q is not absolute", home)
	}
	home = filepath.Clean(home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()
	cmd := osutil.Command(ctx, bin, "generate", "--home", home)
	cmd.Env = filterEnv(os.Environ(), generateEnvDrop)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			if len(msg) > 512 {
				msg = msg[:512] + "..."
			}
			return fmt.Errorf("syncthing generate: %w: %s", err, msg)
		}
		return fmt.Errorf("syncthing generate: %w", err)
	}
	cfg := filepath.Join(home, stclient.ConfigFile)
	if fi, err := os.Stat(cfg); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("syncthing generate did not create %s", cfg)
	}
	return nil
}

// filterEnv returns env without the named variables. Names compare without
// regard to case, as Windows does.
func filterEnv(env, drop []string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, d := range drop {
			if strings.EqualFold(k, d) {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// DefaultHome returns Syncthing's default home directory for a new install
// on this OS, the first location stclient.Discover checks:
//
//	Windows: %LOCALAPPDATA%\Syncthing
//	macOS:   ~/Library/Application Support/Syncthing
//	Linux:   $XDG_STATE_HOME/syncthing (default ~/.local/state/syncthing)
func DefaultHome() (string, error) {
	return defaultHome(runtime.GOOS, os.Getenv, osutil.HomeDir)
}

func defaultHome(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	switch goos {
	case "windows":
		lad, err := localAppData(getenv, home)
		if err != nil {
			return "", err
		}
		return filepath.Join(lad, "Syncthing"), nil
	case "darwin":
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "Library", "Application Support", "Syncthing"), nil
	default:
		if x := getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
			return filepath.Join(x, "syncthing"), nil
		}
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".local", "state", "syncthing"), nil
	}
}
