//go:build linux

package notify

import (
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestEscapeMarkup(t *testing.T) {
	got := escapeMarkup(`<b>bold</b> & "quotes" 'single'`)
	want := `&lt;b&gt;bold&lt;/b&gt; &amp; "quotes" 'single'`
	if got != want {
		t.Errorf("escapeMarkup = %q, want %q", got, want)
	}
}

func TestNotifyArgs(t *testing.T) {
	args := notifyArgs("Title <x>", "Body <i>", true, true)
	if len(args) != 8 {
		t.Fatalf("Notify takes 8 arguments, got %d", len(args))
	}
	if args[0] != "SyncThing V2" || args[1] != uint32(0) || args[2] != "stv2" || args[7] != int32(-1) {
		t.Errorf("fixed arguments %v", args)
	}
	if args[3] != "Title <x>" {
		t.Errorf("summary is plain text and must not be escaped: %v", args[3])
	}
	if args[4] != "Body &lt;i&gt;" {
		t.Errorf("body must be escaped for markup servers: %v", args[4])
	}
	if !reflect.DeepEqual(args[5], []string{"default", "Open SyncThing V2"}) {
		t.Errorf("clickable actions %v", args[5])
	}
	hints := args[6].(map[string]dbus.Variant)
	if hints["desktop-entry"].Value() != "stv2" {
		t.Errorf("hints %v", hints)
	}

	plain := notifyArgs("t", "a <b>", false, false)
	if plain[4] != "a <b>" {
		t.Errorf("body must stay raw without markup support: %v", plain[4])
	}
	if !reflect.DeepEqual(plain[5], []string{}) {
		t.Errorf("no actions without onClick: %v", plain[5])
	}
}

const serverName = ":1.42" // unique name of the fake notification server

func TestHandleSignals(t *testing.T) {
	n := &notifier{owner: serverName}
	clicks := map[uint32]int{}
	n.mu.Lock()
	for id := uint32(1); id <= 3; id++ {
		n.addCallbackLocked(id, func() { clicks[id]++ })
	}
	n.mu.Unlock()

	sig := func(name string, body ...any) *dbus.Signal {
		return &dbus.Signal{Sender: serverName, Path: busPath, Name: name, Body: body}
	}
	n.handle(sig(sigAction, uint32(1), "other"))   // not the default action
	n.handle(sig(sigAction, uint32(1), "default")) // click
	n.handle(sig(sigAction, uint32(1), "default")) // already consumed
	n.handle(sig(sigClosed, uint32(2), uint32(1))) // closed: forget
	n.handle(sig(sigAction, uint32(2), "default")) // too late
	n.handle(&dbus.Signal{Sender: serverName, Path: "/other", Name: sigAction, Body: []any{uint32(3), "default"}})
	n.handle(sig(sigAction, "not-a-uint", "default")) // malformed
	n.handle(sig(sigAction, uint32(3)))               // malformed
	n.handle(nil)

	if clicks[1] != 1 || clicks[2] != 0 || clicks[3] != 0 {
		t.Errorf("clicks %v", clicks)
	}
	n.handle(sig(sigAction, uint32(3), "default"))
	if clicks[3] != 1 {
		t.Errorf("callback 3 lost: %v", clicks)
	}
	if len(n.callbacks) != 0 || len(n.order) != 0 {
		t.Errorf("callbacks left: %v %v", n.callbacks, n.order)
	}
}

func TestCallbackBound(t *testing.T) {
	n := &notifier{}
	n.mu.Lock()
	for id := uint32(0); id < maxCallbacks+10; id++ {
		n.addCallbackLocked(id, func() {})
	}
	n.mu.Unlock()
	if len(n.callbacks) != maxCallbacks || len(n.order) != maxCallbacks {
		t.Fatalf("kept %d/%d callbacks, want %d", len(n.callbacks), len(n.order), maxCallbacks)
	}
	if _, ok := n.callbacks[0]; ok {
		t.Error("oldest callback not evicted")
	}
	if _, ok := n.callbacks[maxCallbacks+9]; !ok {
		t.Error("newest callback evicted")
	}
}

// TestHandleRejectsSpoofedSender checks that ActionInvoked and
// NotificationClosed from any process other than the notification server
// are ignored.
func TestHandleRejectsSpoofedSender(t *testing.T) {
	n := &notifier{}
	clicks := 0
	n.mu.Lock()
	n.addCallbackLocked(7, func() { clicks++ })
	n.mu.Unlock()
	click := func(sender string) {
		n.handle(&dbus.Signal{Sender: sender, Path: busPath, Name: sigAction, Body: []any{uint32(7), "default"}})
	}

	click(serverName) // owner not known yet: nothing is trusted
	if clicks != 0 {
		t.Fatal("a signal was accepted before the server owner was known")
	}
	n.mu.Lock()
	n.owner = serverName
	n.mu.Unlock()
	for _, spoof := range []string{"", ":1.99", busName} {
		click(spoof)
		n.handle(&dbus.Signal{Sender: spoof, Path: busPath, Name: sigClosed, Body: []any{uint32(7), uint32(2)}})
	}
	if clicks != 0 {
		t.Fatal("a spoofed ActionInvoked ran the click callback")
	}
	if _, ok := n.callbacks[7]; !ok {
		t.Fatal("a spoofed NotificationClosed dropped the callback")
	}
	click(serverName)
	if clicks != 1 {
		t.Fatalf("the server's own ActionInvoked ran the callback %d times, want 1", clicks)
	}
}
