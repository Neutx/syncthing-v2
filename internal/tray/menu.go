package tray

import (
	"strings"
	"unicode/utf16"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
)

// Menu item IDs passed to the onMenu callback.
const (
	Separator = "-"

	IDInfoState          = "info-state"
	IDInfoTransfer       = "info-transfer"
	IDInfoStartup        = "info-startup"
	IDOpenStatus         = "open-status"
	IDPair               = "pair"
	IDOpenWebUI          = "open-webui"
	IDOpenFolder         = "open-folder" // the parent item; it is disabled when there is no folder
	IDRescan             = "rescan"
	IDPause              = "pause"
	IDResume             = "resume"
	IDRestart            = "restart"
	IDStartSyncthing     = "start-syncthing"
	IDAutostartTray      = "autostart-tray"
	IDAutostartSyncthing = "autostart-syncthing"
	IDUpdate             = "update"
	IDAbout              = "about"
	IDExit               = "exit"

	// FolderPrefix precedes a Syncthing folder ID in "Open Sync Folder" items.
	FolderPrefix = IDOpenFolder + ":"
)

// FolderID returns the Syncthing folder ID of an "Open Sync Folder" menu ID.
func FolderID(menuID string) (string, bool) {
	id, ok := strings.CutPrefix(menuID, FolderPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// TooltipMax is the longest tooltip, in UTF-16 units, that the Windows shell
// shows (NOTIFYICONDATAW.szTip holds 128 WCHARs including the terminator).
const TooltipMax = 127

// Tooltip returns the tray hover text (feature F20):
//
//	SyncThing V2 - <Label>
//	<PeerLine>
//	<FolderLine>
//
// While syncing with data moving, the last line becomes
// "<FolderLine>  ETA <eta>" followed by a fourth line with the speed. Empty
// lines are left out, and the text is cut to TooltipMax UTF-16 units with
// "..." at the end.
func Tooltip(s model.Snapshot) string {
	lines := []string{brand.DisplayName + " - " + label(s)}
	add := func(l string) {
		if l != "" {
			lines = append(lines, l)
		}
	}
	add(s.PeerLine)
	if s.State == model.StateSyncing && s.Moving {
		third := s.FolderLine
		if s.ETA != "" {
			third += "  ETA " + s.ETA
		}
		add(strings.TrimSpace(third))
		add(s.SpeedLine)
	} else {
		add(s.FolderLine)
	}
	return truncateUTF16(strings.Join(lines, "\n"), TooltipMax)
}

// truncateUTF16 cuts s to at most max UTF-16 units, ending in "..." when it
// had to cut, and never splits a surrogate pair.
func truncateUTF16(s string, max int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= max {
		return s
	}
	n := max - 3
	if n > 0 && utf16.IsSurrogate(rune(u[n-1])) && u[n-1] < 0xDC00 {
		n-- // do not keep a lone high surrogate
	}
	return string(utf16.Decode(u[:n])) + "..."
}

// BuildMenu returns the tray menu for snapshot s (spec §8.2, features F22,
// F24 and F25).
//
// s must be a snapshot the app has built (s.At is set). The zero Snapshot has
// State == StateDown, so building a menu from it would offer "Start
// Syncthing" before anything is known. Until the first SetMenu and SetIcon the
// tray has no menu and shows the app icon (icon.AppState) with a "starting"
// tooltip, so callers wait for the first snapshot instead.
func BuildMenu(s model.Snapshot) []MenuItem { return buildMenu(s, brand.AtLogin()) }

func buildMenu(s model.Snapshot, atLogin string) []MenuItem {
	running := s.State != model.StateDown && s.State != model.StateUnauthorized
	sep := MenuItem{ID: Separator, Visible: true}
	item := func(id, text string, enabled bool) MenuItem {
		return MenuItem{ID: id, Text: text, Enabled: enabled, Visible: true}
	}

	hdr := label(s)
	if s.FolderLine != "" {
		hdr += "  -  " + s.FolderLine
	}
	transfer := "Transfer: n/a"
	if s.SpeedLine != "" {
		transfer = "Transfer: " + s.SpeedLine
	}
	if s.ETA != "" {
		transfer += "   (ETA " + s.ETA + ")"
	}
	startup := "Startup: Syncthing " + onOff(s.Startup.Syncthing) + " | Tray " + onOff(s.Startup.Tray)

	pause := item(IDPause, "Pause All", running && len(s.Folders) > 0)
	if s.State == model.StatePaused {
		pause = item(IDResume, "Resume All", running)
	}

	start := item(IDStartSyncthing, "Start Syncthing", true)
	start.Visible = s.State == model.StateDown

	trayAuto := item(IDAutostartTray, "Start "+brand.DisplayName+" "+atLogin, true)
	trayAuto.Checked = s.Startup.Tray
	// Syncthing's autostart can only be changed here when SyncThing V2 manages
	// it, or when there is none yet.
	stAuto := item(IDAutostartSyncthing, "Start Syncthing "+atLogin, !s.Startup.Syncthing || s.Startup.SyncthingManagedByUs)
	stAuto.Checked = s.Startup.Syncthing

	upd := item(IDUpdate, "Update available…", true)
	if s.UpdateAvailable != "" {
		upd.Text = "Update available (" + s.UpdateAvailable + ")…"
	} else {
		upd.Visible = false
	}

	return []MenuItem{
		item(IDInfoState, hdr, false),
		item(IDInfoTransfer, transfer, false),
		item(IDInfoStartup, startup, false),
		sep,
		item(IDOpenStatus, "Open Status Window", true),
		item(IDPair, "Pair Devices…", true),
		item(IDOpenWebUI, "Open Web UI", s.GUIURL != ""),
		folderItem(s.Folders),
		sep,
		item(IDRescan, "Rescan All", running),
		pause,
		item(IDRestart, "Restart Syncthing", running),
		start,
		sep,
		trayAuto,
		stAuto,
		sep,
		upd,
		item(IDAbout, "About "+brand.DisplayName, true),
		item(IDExit, "Exit", true),
	}
}

// folderItem is "Open Sync Folder": one action for a single folder, or a
// submenu listing every folder when there are several (F24).
func folderItem(folders []model.Folder) MenuItem {
	const text = "Open Sync Folder"
	switch len(folders) {
	case 0:
		return MenuItem{ID: IDOpenFolder, Text: text, Visible: true}
	case 1:
		return MenuItem{ID: FolderPrefix + folders[0].ID, Text: text, Enabled: true, Visible: true}
	}
	children := make([]MenuItem, 0, len(folders))
	for _, f := range folders {
		name := f.Label
		if name == "" {
			name = f.ID
		}
		children = append(children, MenuItem{ID: FolderPrefix + f.ID, Text: name, Enabled: true, Visible: true})
	}
	return MenuItem{ID: IDOpenFolder, Text: text, Enabled: true, Visible: true, Children: children}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// label is the snapshot's state label, with the prototype's wording as a
// fallback for a snapshot that has none yet (before the first poll).
func label(s model.Snapshot) string {
	if s.Label != "" {
		return s.Label
	}
	switch s.State {
	case model.StateUnauthorized:
		return "API key rejected"
	case model.StateError:
		return "Error"
	case model.StatePaused:
		return "Paused"
	case model.StateSyncing:
		return "Syncing"
	case model.StateScanning:
		return "Checking"
	case model.StateNoPeer:
		return "Disconnected"
	case model.StateInSync:
		return "In sync"
	default:
		return "Not running"
	}
}
