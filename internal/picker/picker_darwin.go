//go:build darwin

package picker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// folder runs AppleScript's "choose folder" through osascript. The prompt and
// the initial folder travel as argv, never as script source.
func folder(title, initial string) (string, error) {
	cmd := osutil.Command(context.Background(), "/usr/bin/osascript", osascriptArgs(title, initial)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		// "User canceled. (-128)" is a cancel, not a failure.
		if errors.As(err, &ee) && strings.Contains(stderr.String(), "-128") {
			return "", nil
		}
		return "", fmt.Errorf("picker: osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
