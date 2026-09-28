// Package model holds the data types shared between the status engine, the
// tray, the dashboard server and the pairing service. Values are treated as
// immutable once published in a Snapshot.
package model

import (
	"net/netip"
	"time"
)

// State is the overall sync state shown by the tray and the dashboard.
type State int

const (
	StateDown State = iota
	StateUnauthorized
	StateError
	StatePaused
	StateSyncing
	StateScanning
	StateNoPeer
	StateInSync
)

// Transport is how a peer is connected: "Tailscale", "relay", "local network" or "direct".
type Transport string

// Peer is a configured remote device.
type Peer struct {
	ID, Name, Addr string
	Transport      Transport
	Connected      bool
}

// Folder is a configured Syncthing folder with its latest counters.
type Folder struct {
	ID, Label, Path string
	Paused          bool
	State           string
	Errors          int

	GlobalBytes, NeedBytes, InSyncBytes, GlobalFiles, LocalFiles, NeedItems int64
}

// ActivityItem is one line of Recent Activity. Tint is green, red, blue, amber or grey.
type ActivityItem struct {
	At   time.Time
	Text string
	Tint string
}

// Startup reports which autostart entries exist.
type Startup struct {
	Syncthing, Tray      bool
	SyncthingManagedByUs bool
}

// Snapshot is the complete, immutable status published by the engine.
type Snapshot struct {
	State                    State
	Label, Headline, Subline string
	Detail                   string // error detail for Down/Unauthorized/Error
	Peers                    []Peer // configured remote devices; Connected flag set
	Folders                  []Folder
	Pct                      int
	NeedBytes, NeedItems     int64
	InRate, OutRate          float64
	Moving                   bool
	ETA                      string
	PeerLine, FolderLine     string
	FilesLine, SizeLine      string
	SpeedLine                string
	Activity                 []ActivityItem // newest first, max 40
	Startup                  Startup
	GUIURL                   string // for "Open Web UI"; never contains the API key
	UpdateAvailable          string // "" or "vX.Y.Z"
	Pending                  []PendingDevice
	PendingFolders           []PendingFolder
	Notices                  []string // one-time prompts: "gui-exposed", "legacy-tray", "tailscale-missing"
	At                       time.Time
}

// Candidate is a tailnet node that could be paired.
// Status is "ready", "paired", "no-syncthing", "blocked" or "self".
type Candidate struct {
	NodeName, DNSName, OS, LoginName string
	IP                               netip.Addr
	SameOwner                        bool
	DeviceID                         string
	Status                           string
}

// PendingDevice is a device asking this Syncthing to be added.
type PendingDevice struct {
	DeviceID, Name string
	Addr           netip.AddrPort
	Verified       bool
	Node           *Candidate
	Reason         string
}

// PendingFolder is a folder a paired device offers to share.
type PendingFolder struct {
	FolderID, Label, FromDevice string
}
