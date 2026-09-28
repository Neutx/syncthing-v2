// Package status turns Syncthing's REST API into model.Snapshot values: the
// polling and event engine, the state machine, the text lines and
// formatters, the transport classifier, the event-to-activity map,
// notification rules and the redacted diagnostics report.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
	"github.com/Neutx/syncthing-v2/internal/stclient"
)

// Engine polls one Syncthing instance and publishes snapshots. A single
// goroutine started by Run owns all mutable status state; other goroutines
// only call Refresh, Note and the command methods, which never touch it.
type Engine struct {
	c       *stclient.Client
	startup func() model.Startup

	refresh chan struct{} // coalesced "reconcile now"
	wake    chan struct{} // coalesced "notes queued"

	mu    sync.Mutex
	inbox []model.ActivityItem

	started atomic.Bool

	// Timing; NewEngine sets the production values (§3.3, F6).
	pollEvery     time.Duration // connections poll
	slowEvery     time.Duration // full reconcile and pending lists
	backoff       time.Duration // after an event long-poll error
	eventsTimeout time.Duration // HTTP deadline of one long-poll
	now           func() time.Time

	st engineState // owned by the Run goroutine
}

type engineState struct {
	loaded bool
	// connErr is the result of the last connections poll, or the error that
	// made Syncthing unreachable or rejected the API key. fetchErr is an
	// unexpected response to a startup, config, folder or pending read; it
	// stays until a full reconcile succeeds, so a read that keeps failing
	// keeps the Error state instead of being hidden by the next
	// connections poll.
	connErr  error
	fetchErr error
	myID     string
	devices  []deviceCfg
	folders  []folderCfg
	status   map[string]dbStatus
	conns    map[string]connInfo
	rates    rateMeter
	pending  []model.PendingDevice
	pendingF []model.PendingFolder
	activity []model.ActivityItem
}

type deviceCfg struct {
	DeviceID string `json:"deviceID"`
	Name     string `json:"name"`
}

type folderCfg struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Path   string `json:"path"`
	Paused bool   `json:"paused"`
}

// dbStatus is /rest/db/status and the summary in FolderSummary events.
type dbStatus struct {
	GlobalBytes int64  `json:"globalBytes"`
	NeedBytes   int64  `json:"needBytes"`
	InSyncBytes int64  `json:"inSyncBytes"`
	GlobalFiles int64  `json:"globalFiles"`
	LocalFiles  int64  `json:"localFiles"`
	NeedFiles   int64  `json:"needFiles"`
	NeedDeletes int64  `json:"needDeletes"`
	Errors      int    `json:"errors"`
	PullErrors  int    `json:"pullErrors"`
	State       string `json:"state"`
	Error       string `json:"error"`
}

type connInfo struct {
	Connected bool   `json:"connected"`
	Address   string `json:"address"`
	Type      string `json:"type"`
}

type connections struct {
	Total struct {
		InBytesTotal  int64 `json:"inBytesTotal"`
		OutBytesTotal int64 `json:"outBytesTotal"`
	} `json:"total"`
	Connections map[string]connInfo `json:"connections"`
}

// rateMeter derives transfer rates from Syncthing's byte counters (F10):
// only over intervals longer than 0.5 s, and a counter that went down (a
// Syncthing restart) never produces a rate.
type rateMeter struct {
	prevIn, prevOut int64
	prevAt          time.Time
	in, out         float64
}

func (r *rateMeter) update(in, out int64, at time.Time) {
	if !r.prevAt.IsZero() {
		secs := at.Sub(r.prevAt).Seconds()
		if secs <= 0.5 {
			return // too short to measure; keep the previous rates and baseline
		}
		r.in, r.out = 0, 0
		if in >= r.prevIn {
			r.in = float64(in-r.prevIn) / secs
		}
		if out >= r.prevOut {
			r.out = float64(out-r.prevOut) / secs
		}
	}
	r.prevIn, r.prevOut, r.prevAt = in, out, at
}

func (r *rateMeter) reset() { *r = rateMeter{} }

// NewEngine returns an engine for c. startup, which may be nil, is called for
// every snapshot to report the autostart entries.
func NewEngine(c *stclient.Client, startup func() model.Startup) *Engine {
	return &Engine{
		c:             c,
		startup:       startup,
		refresh:       make(chan struct{}, 1),
		wake:          make(chan struct{}, 1),
		pollEvery:     2 * time.Second,
		slowEvery:     30 * time.Second,
		backoff:       3 * time.Second,
		eventsTimeout: stclient.EventsTimeout,
		now:           time.Now,
	}
}

// Run starts the engine and returns the snapshot channel. The channel holds
// at most one snapshot: a slow reader always gets the newest one. It is
// closed when ctx is done. Run must be called only once.
func (e *Engine) Run(ctx context.Context) <-chan model.Snapshot {
	if !e.started.CompareAndSwap(false, true) {
		panic("status: Engine.Run called twice")
	}
	out := make(chan model.Snapshot, 1)
	events := make(chan []json.RawMessage)
	go e.eventLoop(ctx, events)
	go e.loop(ctx, events, out)
	return out
}

// Refresh asks for an immediate full reconcile (F4). It never blocks.
func (e *Engine) Refresh() { nudge(e.refresh) }

// Note adds a line to Recent Activity (F28); tint is one of the Tint
// constants. It never blocks.
func (e *Engine) Note(text, tint string) {
	e.mu.Lock()
	e.inbox = append(e.inbox, model.ActivityItem{At: e.now(), Text: text, Tint: tint})
	e.mu.Unlock()
	nudge(e.wake)
}

func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Rescan asks Syncthing to rescan every folder.
func (e *Engine) Rescan(ctx context.Context) error {
	return e.command(ctx, "/rest/db/scan", "Rescan requested", TintAmber)
}

// Restart restarts Syncthing.
func (e *Engine) Restart(ctx context.Context) error {
	return e.command(ctx, "/rest/system/restart", "Restarting Syncthing", TintAmber)
}

// Pause pauses every folder (F27).
func (e *Engine) Pause(ctx context.Context) error { return e.setPaused(ctx, true) }

// Resume resumes every folder (F27).
func (e *Engine) Resume(ctx context.Context) error { return e.setPaused(ctx, false) }

func (e *Engine) command(ctx context.Context, path, done, tint string) error {
	defer e.Refresh()
	if err := e.c.Send(ctx, http.MethodPost, path, nil); err != nil {
		e.Note("Command failed: "+summary(err), TintRed)
		return err
	}
	e.Note(done, tint)
	return nil
}

func (e *Engine) setPaused(ctx context.Context, paused bool) error {
	defer e.Refresh()
	var folders []folderCfg
	err := e.c.Get(ctx, "/rest/config/folders", &folders)
	for i := 0; err == nil && i < len(folders); i++ {
		err = e.c.Send(ctx, http.MethodPatch, "/rest/config/folders/"+url.PathEscape(folders[i].ID),
			map[string]bool{"paused": paused})
	}
	if err != nil {
		e.Note("Pause failed: "+summary(err), TintRed)
		return err
	}
	if paused {
		e.Note("Syncing paused", TintGrey)
	} else {
		e.Note("Syncing resumed", TintGreen)
	}
	return nil
}

func summary(err error) string {
	var se *stclient.Error
	if errors.As(err, &se) {
		return se.Summary()
	}
	return err.Error()
}

// ---------- owner goroutine ----------

func (e *Engine) loop(ctx context.Context, events <-chan []json.RawMessage, out chan model.Snapshot) {
	defer close(out)
	e.st.status = map[string]dbStatus{}
	e.st.conns = map[string]connInfo{}

	e.fullRead(ctx)
	e.drainNotes()
	e.publish(out)

	fast := time.NewTicker(e.pollEvery)
	defer fast.Stop()
	slow := time.NewTicker(e.slowEvery)
	defer slow.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			if e.st.loaded && e.st.fetchErr == nil {
				e.pollConnections(ctx)
			} else {
				// Not loaded, or a read failed: retry the full reads.
				e.fullRead(ctx)
			}
		case <-slow.C:
			e.fullRead(ctx)
		case <-e.refresh:
			e.fullRead(ctx)
		case batch := <-events:
			e.handleEvents(ctx, batch)
		case <-e.wake:
		}
		if ctx.Err() != nil {
			return
		}
		e.drainNotes()
		e.publish(out)
	}
}

// fail records the error of a startup, config, folder or pending read. An
// unexpected response keeps the loaded state and is held in fetchErr until a
// full reconcile succeeds; any other error goes to drop.
func (e *Engine) fail(err error) {
	if k, _ := stclient.KindOf(err); k == stclient.ErrBadResponse {
		e.st.fetchErr = err
		return
	}
	e.drop(err)
}

// drop records that Syncthing is unreachable or rejected the API key. The
// loaded state is dropped, so recovery starts with the full startup reads.
func (e *Engine) drop(err error) {
	e.st.connErr = err
	e.st.loaded = false
	e.st.conns = map[string]connInfo{}
	e.st.rates.reset()
}

// fullRead reconciles and then, if that succeeded, reads the pending lists.
func (e *Engine) fullRead(ctx context.Context) {
	e.reconcile(ctx)
	if e.st.loaded {
		e.pollPending(ctx)
	}
}

// reconcile performs the startup reads (§3.3): own device ID, devices,
// folders and every folder's db/status, then the connections. Only a
// reconcile that gets past the config reads replaces fetchErr: with nil, or
// with the error of a folder that answered badly.
func (e *Engine) reconcile(ctx context.Context) {
	// Every path below records a fresh outcome before the next snapshot: an
	// error through fail, or connErr from the closing connections poll. An
	// older unreachable or rejected error must not outlive it.
	e.st.connErr = nil
	var sys struct {
		MyID string `json:"myID"`
	}
	if err := e.c.Get(ctx, "/rest/system/status", &sys); err != nil {
		e.fail(err)
		return
	}
	e.st.myID = sys.MyID
	if err := e.loadConfig(ctx); err != nil {
		e.fail(err)
		return
	}
	// One folder answering badly does not stop the other folders or the
	// connections from being read; its error is kept in fetchErr.
	var folderErr error
	for _, f := range e.st.folders {
		if err := e.fetchFolder(ctx, f.ID); err != nil {
			if k, _ := stclient.KindOf(err); k == stclient.ErrBadResponse {
				folderErr = err
				continue
			}
			e.fail(err)
			return
		}
	}
	e.st.loaded = true
	e.st.fetchErr = folderErr
	e.pollConnections(ctx)
}

func (e *Engine) loadConfig(ctx context.Context) error {
	var devices []deviceCfg
	if err := e.c.Get(ctx, "/rest/config/devices", &devices); err != nil {
		return err
	}
	var folders []folderCfg
	if err := e.c.Get(ctx, "/rest/config/folders", &folders); err != nil {
		return err
	}
	e.st.devices, e.st.folders = devices, folders
	known := make(map[string]bool, len(folders))
	for _, f := range folders {
		known[f.ID] = true
	}
	for id := range e.st.status {
		if !known[id] {
			delete(e.st.status, id)
		}
	}
	return nil
}

// fetchFolder reads /rest/db/status for one folder. A 404 (the folder was
// just removed) only drops its counters.
func (e *Engine) fetchFolder(ctx context.Context, id string) error {
	var st dbStatus
	err := e.c.Get(ctx, "/rest/db/status?folder="+url.QueryEscape(id), &st)
	if stclient.StatusOf(err) == http.StatusNotFound {
		delete(e.st.status, id)
		return nil
	}
	if err != nil {
		return err
	}
	e.st.status[id] = st
	return nil
}

func (e *Engine) pollConnections(ctx context.Context) {
	var c connections
	if err := e.c.Get(ctx, "/rest/system/connections", &c); err != nil {
		if k, _ := stclient.KindOf(err); k == stclient.ErrBadResponse {
			e.st.connErr = err // retried by the next poll
		} else {
			e.drop(err)
		}
		return
	}
	e.st.connErr = nil
	e.st.rates.update(c.Total.InBytesTotal, c.Total.OutBytesTotal, e.now())
	if c.Connections == nil {
		c.Connections = map[string]connInfo{}
	}
	e.st.conns = c.Connections
}

func (e *Engine) pollPending(ctx context.Context) {
	var devs map[string]struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	if err := e.c.Get(ctx, "/rest/cluster/pending/devices", &devs); err != nil {
		e.fail(err)
		return
	}
	var folders map[string]struct {
		OfferedBy map[string]struct {
			Label string `json:"label"`
		} `json:"offeredBy"`
	}
	if err := e.c.Get(ctx, "/rest/cluster/pending/folders", &folders); err != nil {
		e.fail(err)
		return
	}

	pd := make([]model.PendingDevice, 0, len(devs))
	for id, d := range devs {
		pd = append(pd, model.PendingDevice{DeviceID: id, Name: d.Name, Addr: parseAddrPort(d.Address)})
	}
	sort.Slice(pd, func(i, j int) bool { return pd[i].DeviceID < pd[j].DeviceID })

	var pf []model.PendingFolder
	for fid, f := range folders {
		for dev, o := range f.OfferedBy {
			pf = append(pf, model.PendingFolder{FolderID: fid, Label: o.Label, FromDevice: dev})
		}
	}
	sort.Slice(pf, func(i, j int) bool {
		if pf[i].FolderID != pf[j].FolderID {
			return pf[i].FolderID < pf[j].FolderID
		}
		return pf[i].FromDevice < pf[j].FromDevice
	})
	e.st.pending, e.st.pendingF = pd, pf
}

func parseAddrPort(s string) netip.AddrPort {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// handleEvents applies one long-poll batch: activity lines for every event,
// then the targeted reads the events call for.
func (e *Engine) handleEvents(ctx context.Context, batch []json.RawMessage) {
	var needConfig, needConns, needPending bool
	refetch := map[string]bool{}
	for _, raw := range batch {
		if item, ok := Describe(raw); ok {
			e.addActivity(item)
		}
		var ev event
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		var d struct {
			Folder  string    `json:"folder"`
			To      string    `json:"to"`
			Summary *dbStatus `json:"summary"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		switch ev.Type {
		case "ConfigSaved", "FolderPaused", "FolderResumed":
			needConfig = true
		case "FolderSummary":
			if d.Folder != "" && d.Summary != nil {
				e.st.status[d.Folder] = *d.Summary
				delete(refetch, d.Folder)
			}
		case "StateChanged":
			if d.Folder != "" {
				st := e.st.status[d.Folder]
				st.State = d.To
				e.st.status[d.Folder] = st
				refetch[d.Folder] = true
			}
		case "FolderErrors":
			if d.Folder != "" {
				refetch[d.Folder] = true
			}
		case "DeviceConnected", "DeviceDisconnected", "DevicePaused", "DeviceResumed":
			needConns = true
		case "PendingDevicesChanged", "PendingFoldersChanged":
			needPending = true
		}
	}
	if !e.st.loaded {
		return // the next tick performs the full startup reads
	}
	if needConfig {
		if err := e.loadConfig(ctx); err != nil {
			e.fail(err)
			return
		}
		for _, f := range e.st.folders {
			if _, ok := e.st.status[f.ID]; !ok {
				refetch[f.ID] = true
			}
		}
	}
	ids := make([]string, 0, len(refetch))
	for id := range refetch {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := e.fetchFolder(ctx, id); err != nil {
			e.fail(err)
			return
		}
	}
	if needConns {
		e.pollConnections(ctx)
	}
	if needPending {
		e.pollPending(ctx)
	}
}

func (e *Engine) addActivity(item model.ActivityItem) {
	e.st.activity = append([]model.ActivityItem{item}, e.st.activity...)
	if len(e.st.activity) > MaxActivity {
		e.st.activity = e.st.activity[:MaxActivity]
	}
}

func (e *Engine) drainNotes() {
	e.mu.Lock()
	notes := e.inbox
	e.inbox = nil
	e.mu.Unlock()
	for _, n := range notes {
		e.addActivity(n)
	}
}

// snapshot assembles the current snapshot from the owned state.
func (e *Engine) snapshot() model.Snapshot {
	err := e.st.connErr
	if err == nil {
		err = e.st.fetchErr
	}
	v := view{err: err, host: e.c.Host()}
	if v.err == nil {
		v.inRate, v.outRate = e.st.rates.in, e.st.rates.out
		for _, d := range e.st.devices {
			if d.DeviceID == e.st.myID {
				continue
			}
			p := model.Peer{ID: d.DeviceID, Name: d.Name}
			if p.Name == "" {
				p.Name = Short(d.DeviceID)
			}
			if c, ok := e.st.conns[d.DeviceID]; ok && c.Connected {
				p.Connected, p.Addr, p.Transport = true, c.Address, Classify(c.Address, c.Type)
			}
			v.peers = append(v.peers, p)
		}
		for _, f := range e.st.folders {
			st := e.st.status[f.ID]
			mf := model.Folder{
				ID: f.ID, Label: f.Label, Path: f.Path, Paused: f.Paused, State: st.State,
				Errors:      st.Errors + st.PullErrors,
				GlobalBytes: st.GlobalBytes, NeedBytes: st.NeedBytes, InSyncBytes: st.InSyncBytes,
				GlobalFiles: st.GlobalFiles, LocalFiles: st.LocalFiles, NeedItems: st.NeedFiles + st.NeedDeletes,
			}
			if mf.Label == "" {
				mf.Label = f.ID
			}
			if mf.Errors == 0 && (st.State == "error" || st.Error != "") && !f.Paused {
				mf.Errors = 1 // the folder itself failed (for example its path is missing)
			}
			if f.Paused {
				mf.State = "paused"
			}
			v.folders = append(v.folders, mf)
		}
	}

	s := compute(v)
	s.Activity = append([]model.ActivityItem(nil), e.st.activity...)
	if e.startup != nil {
		s.Startup = e.startup()
	}
	s.GUIURL = e.c.URL() + "/"
	if v.err == nil {
		s.Pending = append([]model.PendingDevice(nil), e.st.pending...)
		s.PendingFolders = append([]model.PendingFolder(nil), e.st.pendingF...)
	}
	s.At = e.now()
	return s
}

func (e *Engine) publish(out chan model.Snapshot) {
	s := e.snapshot()
	select {
	case out <- s:
		return
	default:
	}
	// Replace the unread snapshot; this goroutine is the only sender.
	select {
	case <-out:
	default:
	}
	select {
	case out <- s:
	default:
	}
}

// ---------- event long-poll (F6) ----------

// eventLoop seeds with limit=1, then long-polls since=N&timeout=50. A
// transport or HTTP error sleeps 3 s and re-seeds; a malformed batch sleeps
// 3 s and retries from the same ID. The seed batch is not reported.
func (e *Engine) eventLoop(ctx context.Context, out chan<- []json.RawMessage) {
	since := 0
	seeded := false
	for ctx.Err() == nil {
		if !seeded {
			_, id, err := e.c.Events(ctx, 0, e.eventsTimeout)
			if err != nil {
				e.sleep(ctx, e.backoff)
				continue
			}
			since, seeded = id, true
		}
		evs, id, err := e.c.Events(ctx, since, e.eventsTimeout)
		if err != nil {
			if !isDecodeError(err) {
				seeded = false
			}
			e.sleep(ctx, e.backoff)
			continue
		}
		since = id
		if len(evs) == 0 {
			continue
		}
		select {
		case out <- evs:
		case <-ctx.Done():
			return
		}
	}
}

// isDecodeError reports a 2xx response whose body was not valid JSON (the
// prototype's non-WebException case).
func isDecodeError(err error) bool {
	k, ok := stclient.KindOf(err)
	s := stclient.StatusOf(err)
	return ok && k == stclient.ErrBadResponse && s >= 200 && s <= 299
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
