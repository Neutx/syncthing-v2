package status

import "github.com/Neutx/syncthing-v2/internal/model"

// NoticeFor returns the notification for a state change (F23), or ok=false
// when the change is not worth a notification. The rules, first match wins:
//
//   - no change → none
//   - → NoPeer, unless coming from Down or Unauthorized → "Syncthing disconnected"
//   - → Down → "Syncthing stopped"
//   - → Unauthorized → "API key rejected"
//   - → Error → "Syncthing error" with the folder line (or the detail)
//   - → InSync, coming from Syncing or NoPeer → "Syncthing in sync"
func NoticeFor(prev, next model.State, s model.Snapshot) (title, body string, ok bool) {
	if prev == next {
		return "", "", false
	}
	switch next {
	case model.StateNoPeer:
		if prev == model.StateDown || prev == model.StateUnauthorized {
			return "", "", false
		}
		return "Syncthing disconnected", "No devices are connected.", true
	case model.StateDown:
		return "Syncthing stopped", "The Syncthing process is not running.", true
	case model.StateUnauthorized:
		body := s.Detail
		if body == "" {
			body = detailUnauthorized
		}
		return "API key rejected", body, true
	case model.StateError:
		body := s.FolderLine
		if body == "" {
			body = s.Detail
		}
		return "Syncthing error", body, true
	case model.StateInSync:
		if prev == model.StateSyncing || prev == model.StateNoPeer {
			return "Syncthing in sync", "All files are up to date.", true
		}
	}
	return "", "", false
}
