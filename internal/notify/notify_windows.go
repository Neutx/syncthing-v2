//go:build windows

package notify

import (
	"sync"

	"github.com/Neutx/syncthing-v2/internal/applog"
)

// clickBalloonist is implemented by the Windows tray: a balloon whose click
// runs its own callback.
type clickBalloonist interface {
	BalloonWithClick(title, body string, onClick func()) bool
}

var (
	mu      sync.Mutex
	current Balloonist
)

func use(t Balloonist) {
	mu.Lock()
	current = t
	mu.Unlock()
}

func show(title, body string, onClick func()) {
	mu.Lock()
	t := current
	mu.Unlock()
	if t == nil {
		applog.Printf("notify: no tray to show %q", title)
		return
	}
	var ok bool
	if c, is := t.(clickBalloonist); is && onClick != nil {
		ok = c.BalloonWithClick(title, body, onClick)
	} else {
		ok = t.Balloon(title, body)
	}
	if !ok {
		applog.Printf("notify: balloon %q not shown (tray icon not in the notification area)", title)
	}
}

func hasStatusNotifierWatcher() bool { return true }
