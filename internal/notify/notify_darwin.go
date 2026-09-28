//go:build darwin

package notify

import (
	"context"
	"time"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/osutil"
)

const osascript = "/usr/bin/osascript"

func use(Balloonist) {}

func show(title, body string, _ func()) {
	go func() {
		if _, err := osutil.Output(context.Background(), 10*time.Second, osascript, osascriptArgs(title, body)...); err != nil {
			applog.Printf("notify: osascript: %v", err)
		}
	}()
}

// osascriptArgs builds a fixed script that reads the text from argv, so no
// title or body is ever parsed as AppleScript. The first argument is a fixed
// word, so a title that starts with "-" cannot be read as an osascript
// option (option parsing stops at the first non-option argument).
func osascriptArgs(title, body string) []string {
	return []string{
		"-e", "on run argv",
		"-e", "display notification (item 3 of argv) with title (item 2 of argv)",
		"-e", "end run",
		brand.BinaryName, title, body,
	}
}

func hasStatusNotifierWatcher() bool { return true }
