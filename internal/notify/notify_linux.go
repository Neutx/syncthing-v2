//go:build linux

package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Neutx/syncthing-v2/internal/applog"
	"github.com/Neutx/syncthing-v2/internal/brand"
)

const (
	busName     = "org.freedesktop.Notifications"
	busPath     = dbus.ObjectPath("/org/freedesktop/Notifications")
	busIface    = "org.freedesktop.Notifications"
	sigAction   = busIface + ".ActionInvoked"
	sigClosed   = busIface + ".NotificationClosed"
	callTimeout = 5 * time.Second

	// maxCallbacks bounds the click callbacks kept for notifications whose
	// server never reports them closed.
	maxCallbacks = 64
)

// notifier owns one private session-bus connection that receives the
// ActionInvoked and NotificationClosed signals.
type notifier struct {
	mu        sync.Mutex
	conn      *dbus.Conn
	markup    bool              // the server renders body markup, so text is escaped
	owner     string            // unique bus name of the notification server; "" until known
	callbacks map[uint32]func() // notification ID → onClick
	order     []uint32          // callback IDs, oldest first
}

var linux = &notifier{}

func use(Balloonist) {}

func show(title, body string, onClick func()) {
	go func() {
		if err := linux.notify(title, body, onClick); err != nil {
			applog.Printf("notify: D-Bus notification %q: %v", title, err)
		}
	}()
}

func (n *notifier) notify(title, body string, onClick func()) error {
	n.mu.Lock()
	defer n.mu.Unlock() // held until the callback is stored, so no click is missed
	conn, err := n.connectLocked()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var id uint32
	err = conn.Object(busName, busPath).CallWithContext(ctx, busIface+".Notify", 0,
		notifyArgs(title, body, onClick != nil, n.markup)...).Store(&id)
	if err != nil {
		if !conn.Connected() {
			n.conn = nil
		}
		return err
	}
	if onClick != nil {
		// Signals are accepted only from the server that owns busName, so
		// look the owner up now that Notify has certainly started it. It is
		// looked up on every notification because the server may have been
		// restarted under a new unique name.
		owner, err := nameOwner(ctx, conn)
		if err != nil {
			return fmt.Errorf("resolving the owner of %s: %w", busName, err)
		}
		n.owner = owner
		n.addCallbackLocked(id, onClick)
	}
	return nil
}

// nameOwner returns the unique bus name (":1.42") that owns busName.
func nameOwner(ctx context.Context, conn *dbus.Conn) (string, error) {
	var owner string
	err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&owner)
	if err == nil && owner == "" {
		err = errors.New("empty owner")
	}
	return owner, err
}

// notifyArgs returns the arguments of Notify(app_name, replaces_id,
// app_icon, summary, body, actions, hints, expire_timeout).
func notifyArgs(title, body string, clickable, markup bool) []any {
	if markup {
		body = escapeMarkup(body)
	}
	actions := []string{}
	if clickable {
		actions = []string{"default", "Open " + brand.DisplayName}
	}
	hints := map[string]dbus.Variant{
		"desktop-entry": dbus.MakeVariant(brand.BinaryName),
	}
	return []any{brand.DisplayName, uint32(0), brand.BinaryName, title, body, actions, hints, int32(-1)}
}

// escapeMarkup escapes the characters that the notification body markup
// subset would interpret.
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (n *notifier) connectLocked() (*dbus.Conn, error) {
	if n.conn != nil && n.conn.Connected() {
		return n.conn, nil
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	// The bus delivers only signals sent by the current owner of busName;
	// handle checks the sender again, since this connection may also receive
	// signals through other match rules.
	if err := conn.AddMatchSignal(dbus.WithMatchSender(busName), dbus.WithMatchObjectPath(busPath), dbus.WithMatchInterface(busIface)); err != nil {
		conn.Close()
		return nil, err
	}
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	go func() {
		for sig := range ch { // closed when the connection closes
			n.handle(sig)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var caps []string
	if err := conn.Object(busName, busPath).CallWithContext(ctx, busIface+".GetCapabilities", 0).Store(&caps); err == nil {
		n.markup = slices.Contains(caps, "body-markup")
	} else {
		n.markup = true // unknown server: escaping is always safe
	}
	n.conn = conn
	return conn, nil
}

func (n *notifier) addCallbackLocked(id uint32, f func()) {
	if n.callbacks == nil {
		n.callbacks = map[uint32]func(){}
	}
	if _, ok := n.callbacks[id]; !ok {
		n.order = append(n.order, id)
	}
	n.callbacks[id] = f
	for len(n.order) > maxCallbacks {
		delete(n.callbacks, n.order[0])
		n.order = n.order[1:]
	}
}

func (n *notifier) takeCallback(id uint32) func() {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.callbacks[id]
	delete(n.callbacks, id)
	if i := slices.Index(n.order, id); i >= 0 {
		n.order = slices.Delete(n.order, i, i+1)
	}
	return f
}

// handle runs the click callback for a "default" ActionInvoked signal and
// forgets callbacks of closed notifications. Signals not sent by the
// notification server's unique name are ignored, so another process on the
// session bus cannot spoof a click.
func (n *notifier) handle(sig *dbus.Signal) {
	if sig == nil || sig.Path != busPath || len(sig.Body) < 1 {
		return
	}
	n.mu.Lock()
	owner := n.owner
	n.mu.Unlock()
	if owner == "" || sig.Sender != owner {
		return
	}
	id, ok := sig.Body[0].(uint32)
	if !ok {
		return
	}
	switch sig.Name {
	case sigAction:
		if len(sig.Body) < 2 {
			return
		}
		if key, _ := sig.Body[1].(string); key != "default" {
			return
		}
		if f := n.takeCallback(id); f != nil {
			f()
		}
	case sigClosed:
		n.takeCallback(id)
	}
}

func hasStatusNotifierWatcher() bool {
	conn, err := dbus.ConnectSessionBus() // a new connection, closed below
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var has bool
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&has)
	return err == nil && has
}
