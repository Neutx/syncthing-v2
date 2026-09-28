package status

import (
	"math"
	"strconv"
	"strings"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// Detail texts for the three connection failure states (§3.3).
const (
	detailUnauthorized = "Syncthing rejected the API key — run " + brand.BinaryName + " doctor"
	detailBadResponse  = "Unexpected response from Syncthing"
)

// view is everything a snapshot's state and text lines are derived from.
type view struct {
	err     error          // last client error; nil when Syncthing answered
	host    string         // dialled host:port, for the Down detail
	folders []model.Folder // with their latest counters
	peers   []model.Peer   // configured remote devices, Connected set
	inRate  float64
	outRate float64
}

// totals aggregates folder counters (F12). Paused folders are left out of
// the aggregate unless every folder is paused, so a paused folder's backlog
// never keeps the other folders in "Syncing".
type totals struct {
	global, need, inSync           int64
	globalFiles, localFiles, items int64
	errors                         int
	syncing, scanning, allPaused   bool
}

func aggregate(folders []model.Folder) totals {
	var t totals
	t.allPaused = len(folders) > 0
	for _, f := range folders {
		if !f.Paused {
			t.allPaused = false
		}
	}
	for _, f := range folders {
		if f.Paused && !t.allPaused {
			continue
		}
		t.global += f.GlobalBytes
		t.need += f.NeedBytes
		t.inSync += f.InSyncBytes
		t.globalFiles += f.GlobalFiles
		t.localFiles += f.LocalFiles
		t.items += f.NeedItems
		t.errors += f.Errors
		switch f.State {
		case "syncing", "sync-preparing":
			t.syncing = true
		case "scanning", "scan-waiting":
			t.scanning = true
		}
	}
	return t
}

// Percent is the sync progress rule (F13): floor(100·(global−need)/global),
// capped at 99 while anything is needed, and 100 when there is no data.
func Percent(global, need int64) int {
	p := 100
	if global > 0 {
		p = int(math.Floor(100 * float64(global-need) / float64(global)))
	}
	if p > 99 && need > 0 {
		p = 99
	}
	return max(0, min(p, 100))
}

// decide applies the state machine (§3.3); the first matching rule wins.
func decide(err error, nFolders, peersUp int, t totals) model.State {
	if err != nil {
		switch k, _ := stclient.KindOf(err); k {
		case stclient.ErrUnauthorized:
			return model.StateUnauthorized
		case stclient.ErrBadResponse:
			return model.StateError
		default:
			return model.StateDown
		}
	}
	switch {
	case nFolders == 0:
		return model.StateInSync
	case t.allPaused:
		return model.StatePaused
	case t.errors > 0:
		return model.StateError
	case peersUp == 0:
		return model.StateNoPeer
	case t.syncing || t.need > 0:
		return model.StateSyncing
	case t.scanning:
		return model.StateScanning
	default:
		return model.StateInSync
	}
}

// compute builds the status part of a snapshot: state, texts, peers,
// folders, progress and rates. Activity, startup, GUI URL, pending lists and
// the timestamp are added by the engine.
func compute(v view) model.Snapshot {
	var s model.Snapshot
	if v.err != nil {
		s.State = decide(v.err, 0, 0, totals{})
		s.Label, s.Headline = Label(s.State), Headline(s.State)
		s.FilesLine = "-"
		switch s.State {
		case model.StateUnauthorized:
			s.Detail = detailUnauthorized
			s.Subline = "Syncthing is running but did not accept the API key."
			s.PeerLine = "API key rejected"
			s.SizeLine = "Not authorized"
		case model.StateError:
			s.Detail = detailBadResponse
			s.Subline = detailBadResponse + "."
			s.PeerLine = detailBadResponse
			s.SizeLine = "Service error"
		default:
			s.Detail = "Syncthing is not responding on " + v.host
			s.Subline = "Start Syncthing to resume syncing."
			s.PeerLine = "Syncthing not running"
			s.SizeLine = "Service offline"
		}
		s.Pct = 100
		return s
	}

	s.Peers = v.peers
	s.Folders = v.folders
	var up []string
	for _, p := range v.peers {
		if p.Connected {
			up = append(up, p.Name+" ("+string(p.Transport)+")")
		}
	}
	remotes := len(v.peers)

	t := aggregate(v.folders)
	s.State = decide(nil, len(v.folders), len(up), t)
	s.Label, s.Headline = Label(s.State), Headline(s.State)
	s.Pct = Percent(t.global, t.need)
	s.NeedBytes, s.NeedItems = t.need, t.items
	s.InRate, s.OutRate = v.inRate, v.outRate
	s.Moving = v.inRate > 1024 || v.outRate > 1024
	s.ETA = ETA(t.need, v.inRate)

	if len(up) == 0 {
		s.PeerLine = "No devices connected"
	} else {
		s.PeerLine = strings.Join(up, ", ")
	}

	progress := strconv.Itoa(s.Pct) + "% - " + Size(t.need) + " left"
	pairHint := ""
	if remotes == 0 {
		pairHint = " Pair a device to start."
	}
	switch s.State {
	case model.StateInSync:
		if len(v.folders) == 0 {
			s.FolderLine = "No folders configured"
			s.Subline = "No folders configured." + pairHint
		} else {
			s.FolderLine = "Up to date"
			s.Subline = "All devices hold the same files."
		}
	case model.StatePaused:
		s.FolderLine = "Paused"
		s.Subline = "Syncing is paused."
	case model.StateError:
		s.FolderLine = strconv.Itoa(t.errors) + " error(s)"
		s.Subline = strconv.Itoa(t.errors) + " item(s) could not be synced."
		s.Detail = folderErrorDetail(v.folders, t.errors)
	case model.StateNoPeer:
		s.FolderLine = "Up to date"
		if t.need > 0 {
			s.FolderLine = progress
		}
		s.Subline = "No paired device is reachable right now." + pairHint
	case model.StateSyncing:
		s.FolderLine = progress
		s.Subline = strconv.Itoa(s.Pct) + "% complete"
		if s.ETA != "" {
			s.Subline += "  -  about " + s.ETA + " left"
		}
	case model.StateScanning:
		s.FolderLine = "Checking for changes"
		s.Subline = "Routine verification - nothing is wrong."
	}

	s.FilesLine = Grp(t.localFiles) + " of " + Grp(t.globalFiles) + " files"
	s.SizeLine = Size(t.inSync) + " of " + Size(t.global) + " in sync"
	if s.Moving {
		s.SpeedLine = "v " + Rate(v.inRate) + "   ^ " + Rate(v.outRate)
	} else {
		s.SpeedLine = "idle"
	}
	return s
}

func folderErrorDetail(folders []model.Folder, errs int) string {
	n := 0
	for _, f := range folders {
		if f.Errors > 0 && !f.Paused {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return strconv.Itoa(errs) + " item(s) could not be synced in " + strconv.Itoa(n) +
		" folder(s). Open the Syncthing web UI for the file list."
}
