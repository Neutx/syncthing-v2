// Package prefs stores SyncThing V2's per-user preferences in prefs.json in
// the app data directory. Every change is written atomically (temporary file
// plus rename), so a crash never leaves a half-written file.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Neutx/syncthing-v2/internal/osutil"
)

// FileName is the preferences file inside the app data directory.
const FileName = "prefs.json"

// Profile values for ProfileChosen.
const (
	ProfileTailnet = "tailnet"
	ProfileHybrid  = "hybrid"
)

// ErrCorrupt is returned (wrapped) by Open when prefs.json cannot be parsed.
// The unreadable file is moved aside to prefs.json.corrupt and the returned
// Store holds the defaults, so the app keeps working.
var ErrCorrupt = errors.New("prefs.json is corrupt")

// Prefs is the persisted preference set.
type Prefs struct {
	// Notifications enables desktop notifications.
	Notifications bool `json:"notifications"`
	// CheckForUpdates enables the daily read-only release check.
	CheckForUpdates bool `json:"checkForUpdates"`
	// ProfileChosen is the transport profile the user picked ("tailnet" or
	// "hybrid"), or "" when none was chosen.
	ProfileChosen string `json:"profileChosen,omitempty"`
	// DismissedDevices lists device IDs whose pairing requests were declined.
	DismissedDevices []string `json:"dismissedDevices,omitempty"`
	// Widened records devices whose addresses were widened after first connect.
	Widened map[string]bool `json:"widened,omitempty"`
	// NoticesDismissed lists one-time notice IDs the user chose not to see again.
	NoticesDismissed []string `json:"noticesDismissed,omitempty"`
	// LegacyAsked is set once the legacy-tray migration question was answered.
	LegacyAsked bool `json:"legacyAsked"`
	// LastUpdateCheck is when the release check last ran.
	LastUpdateCheck time.Time `json:"lastUpdateCheck,omitzero"`
	// UpdateNotified is the release version already announced by notification.
	UpdateNotified string `json:"updateNotified,omitempty"`
	// LatestRelease is the newest release the last answered check found, or
	// "" when there was none; it answers within the 24 h check interval.
	LatestRelease string `json:"latestRelease,omitempty"`
}

// Defaults returns the preferences used when no file exists.
func Defaults() Prefs {
	return Prefs{Notifications: true, CheckForUpdates: true}
}

// Clone returns a deep copy.
func (p Prefs) Clone() Prefs {
	c := p
	c.DismissedDevices = slices.Clone(p.DismissedDevices)
	c.NoticesDismissed = slices.Clone(p.NoticesDismissed)
	if p.Widened != nil {
		c.Widened = make(map[string]bool, len(p.Widened))
		for k, v := range p.Widened {
			c.Widened[k] = v
		}
	}
	return c
}

// DeviceDismissed reports whether pairing requests from id were declined.
func (p Prefs) DeviceDismissed(id string) bool { return slices.Contains(p.DismissedDevices, id) }

// NoticeDismissed reports whether the notice id was dismissed for good.
func (p Prefs) NoticeDismissed(id string) bool { return slices.Contains(p.NoticesDismissed, id) }

// IsWidened reports whether the device's addresses were already widened.
func (p Prefs) IsWidened(id string) bool { return p.Widened[id] }

func (p *Prefs) validate() error {
	switch p.ProfileChosen {
	case "", ProfileTailnet, ProfileHybrid:
		return nil
	default:
		return fmt.Errorf("unknown profile %q", p.ProfileChosen)
	}
}

// Store is the loaded preference file. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	p       Prefs
	existed bool
}

// Open loads dir/prefs.json, creating dir (0700) if needed. A missing file
// yields the defaults. Fields absent from the file keep their defaults.
func Open(dir string) (*Store, error) {
	if err := osutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName), p: Defaults()}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	loaded := Defaults()
	perr := json.Unmarshal(data, &loaded)
	if perr == nil {
		perr = loaded.validate()
	}
	if perr != nil {
		bad := s.path + ".corrupt"
		_ = os.Remove(bad)
		if rerr := os.Rename(s.path, bad); rerr != nil {
			return nil, fmt.Errorf("%w: %v; moving it aside failed: %v", ErrCorrupt, perr, rerr)
		}
		return s, fmt.Errorf("%w (%v); kept as %s, using defaults", ErrCorrupt, perr, filepath.Base(bad))
	}
	s.p, s.existed = loaded, true
	return s, nil
}

// Path returns the preferences file path.
func (s *Store) Path() string { return s.path }

// Existed reports whether a valid prefs.json was present when the Store was
// opened. A first run (no prefs) triggers the setup flow.
func (s *Store) Existed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existed
}

// Get returns a copy of the current preferences.
func (s *Store) Get() Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p.Clone()
}

// Update applies fn to a copy of the preferences and saves the result
// atomically. If fn returns an error, or the new value is invalid, or saving
// fails, the stored preferences are left unchanged.
func (s *Store) Update(fn func(*Prefs) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.p.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := osutil.WriteFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.p, s.existed = next, true
	return nil
}

// Save writes the current preferences (used to create the file on first run).
func (s *Store) Save() error { return s.Update(func(*Prefs) error { return nil }) }

// DismissDevice records that pairing requests from id were declined.
func (s *Store) DismissDevice(id string) error {
	return s.Update(func(p *Prefs) error {
		if !slices.Contains(p.DismissedDevices, id) {
			p.DismissedDevices = append(p.DismissedDevices, id)
		}
		return nil
	})
}

// DismissNotice records that the one-time notice id must not be shown again.
func (s *Store) DismissNotice(id string) error {
	return s.Update(func(p *Prefs) error {
		if !slices.Contains(p.NoticesDismissed, id) {
			p.NoticesDismissed = append(p.NoticesDismissed, id)
		}
		return nil
	})
}

// MarkWidened records that the device's addresses were widened.
func (s *Store) MarkWidened(id string) error {
	return s.Update(func(p *Prefs) error {
		if p.Widened == nil {
			p.Widened = map[string]bool{}
		}
		p.Widened[id] = true
		return nil
	})
}
