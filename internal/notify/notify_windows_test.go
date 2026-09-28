//go:build windows

package notify

import "testing"

type fakeTray struct {
	plain, withClick []string
	onClick          func()
	accept           bool
}

func (f *fakeTray) Balloon(title, body string) bool {
	f.plain = append(f.plain, title+"|"+body)
	return f.accept
}

func (f *fakeTray) BalloonWithClick(title, body string, onClick func()) bool {
	f.withClick = append(f.withClick, title+"|"+body)
	f.onClick = onClick
	return f.accept
}

type plainTray struct{ got []string }

func (p *plainTray) Balloon(title, body string) bool {
	p.got = append(p.got, title+"|"+body)
	return true
}

func TestShowRoutesThroughTray(t *testing.T) {
	defer Use(nil)

	Use(nil)
	Show("no tray", "must not panic", nil)

	f := &fakeTray{accept: true}
	Use(f)
	Show("Syncthing in sync", "All files are up to date.", nil)
	if len(f.plain) != 1 || f.plain[0] != "Syncthing in sync|All files are up to date." || len(f.withClick) != 0 {
		t.Fatalf("nil onClick must use Balloon: plain=%v withClick=%v", f.plain, f.withClick)
	}

	clicked := false
	Show("Pairing request", "A device asks to pair.", func() { clicked = true })
	if len(f.withClick) != 1 || f.withClick[0] != "Pairing request|A device asks to pair." {
		t.Fatalf("onClick must use BalloonWithClick: %v", f.withClick)
	}
	f.onClick()
	if !clicked {
		t.Fatal("the balloon callback is not the caller's onClick")
	}

	f.accept = false
	Show("refused", "logged only", nil) // must not panic when the tray refuses

	p := &plainTray{}
	Use(p)
	Show("plain", "tray without click support", func() {})
	if len(p.got) != 1 {
		t.Fatalf("a tray without BalloonWithClick must still get the balloon: %v", p.got)
	}

	if !HasStatusNotifierWatcher() {
		t.Error("HasStatusNotifierWatcher must be true on Windows")
	}
}
