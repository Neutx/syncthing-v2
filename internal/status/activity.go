package status

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// Activity tints (the dashboard maps them to the D12 state colours).
const (
	TintGreen = "green"
	TintRed   = "red"
	TintBlue  = "blue"
	TintAmber = "amber"
	TintGrey  = "grey"
)

// MaxActivity is how many Recent Activity items a snapshot keeps (D9).
const MaxActivity = 40

// event is the envelope of one /rest/events entry.
type event struct {
	ID   int             `json:"id"`
	Type string          `json:"type"`
	Time string          `json:"time"`
	Data json.RawMessage `json:"data"`
}

// Describe turns a Syncthing event into a Recent Activity line, following
// the prototype's event map (inventory §2.4). ok is false for events that
// produce no line.
func Describe(ev json.RawMessage) (model.ActivityItem, bool) {
	var e event
	if err := json.Unmarshal(ev, &e); err != nil {
		return model.ActivityItem{}, false
	}
	text, tint := describe(e)
	if text == "" {
		return model.ActivityItem{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, e.Time)
	if err != nil {
		at = time.Now()
	}
	return model.ActivityItem{At: at, Text: text, Tint: tint}, true
}

func describe(e event) (text, tint string) {
	// Events whose line depends on data produce nothing without it, as in the prototype.
	noData := len(e.Data) == 0 || string(e.Data) == "null"
	switch e.Type {
	case "DeviceConnected":
		return "Device connected", TintGreen
	case "DeviceDisconnected":
		return "Device disconnected", TintRed
	case "ItemFinished":
		if noData {
			return "", ""
		}
		var d struct {
			Item   string  `json:"item"`
			Action string  `json:"action"`
			Error  *string `json:"error"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return "", ""
		}
		name := fileName(d.Item)
		if d.Error != nil && *d.Error != "" {
			return "Failed: " + name, TintRed
		}
		switch d.Action {
		case "delete":
			return "Deleted " + name, TintBlue
		case "update":
			return "Updated " + name, TintBlue
		default:
			return "Received " + name, TintBlue
		}
	case "StateChanged":
		if noData {
			return "", ""
		}
		var d struct {
			To string `json:"to"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return "", ""
		}
		switch d.To {
		case "scanning":
			return "Checking for changes", TintAmber
		case "syncing":
			return "Sync started", TintBlue
		case "idle":
			return "Up to date", TintGreen
		}
	case "LocalIndexUpdated":
		if noData {
			return "", ""
		}
		var d struct {
			Filenames []string `json:"filenames"`
		}
		if json.Unmarshal(e.Data, &d) != nil || len(d.Filenames) == 0 {
			return "", ""
		}
		if len(d.Filenames) == 1 {
			return "Local change: " + fileName(d.Filenames[0]), TintBlue
		}
		return "Local changes: " + strconv.Itoa(len(d.Filenames)) + " items", TintBlue
	case "RemoteChangeDetected":
		if noData {
			return "", ""
		}
		var d struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			return "", ""
		}
		return "Remote change: " + fileName(d.Path), TintBlue
	case "FolderErrors":
		return "Folder reported errors", TintRed
	case "ConfigSaved":
		return "Settings updated", TintGrey
	}
	return "", ""
}

// fileName returns the last element of a Syncthing item path, which uses the
// sending OS's separator, like .NET's Path.GetFileName on Windows.
func fileName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
