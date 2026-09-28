//go:build linux

package picker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// folder runs zenity or kdialog, whichever is installed (kdialog first on
// KDE Plasma). Without either it returns ErrUnavailable, and the dashboard
// asks for a typed path instead.
func folder(title, initial string) (string, error) {
	type tool struct {
		name string
		args []string
	}
	tools := []tool{
		{"zenity", zenityArgs(title, initial)},
		{"kdialog", kdialogArgs(title, initial)},
	}
	if strings.Contains(strings.ToUpper(os.Getenv("XDG_CURRENT_DESKTOP")), "KDE") {
		tools[0], tools[1] = tools[1], tools[0]
	}
	for _, t := range tools {
		bin, err := exec.LookPath(t.name)
		if err != nil || !filepath.IsAbs(bin) {
			continue
		}
		return run(bin, t.args)
	}
	return "", ErrUnavailable
}

// run executes a picker. Both zenity and kdialog exit with status 1 on cancel.
func run(bin string, args []string) (string, error) {
	cmd := osutil.Command(context.Background(), bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("picker: %s: %w: %s", filepath.Base(bin), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
