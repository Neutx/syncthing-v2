//go:build darwin || linux

package tray

import (
	"runtime"
	"strings"
	"sync"

	"fyne.io/systray"

	"github.com/Neutx/syncthing-v2/internal/brand"
	"github.com/Neutx/syncthing-v2/internal/icon"
	"github.com/Neutx/syncthing-v2/internal/model"
)

// iconPixels is the rendered icon size. fyne.io/systray pins the macOS
// status-item image to 16 pt, so 32 px gives a crisp @2x image; Linux
// StatusNotifierItem hosts scale a 24 px pixmap to their panel size.
func iconPixels() int {
	if runtime.GOOS == "darwin" {
		return 32
	}
	return 24
}

// checkable lists the toggles that always carry a check box, so Linux
// menus show an empty box while they are off.
var checkable = map[string]bool{IDAutostartTray: true, IDAutostartSyncthing: true}

// node mirrors one model item and the systray item that shows it.
type node struct {
	it       *systray.MenuItem // nil for separators
	shown    MenuItem
	children []node
}

// fyneTray is the tray on macOS and Linux, built on fyne.io/systray.
type fyneTray struct {
	mu       sync.Mutex
	ready    bool
	started  bool
	quitting bool
	onMenu   func(string)

	iconSet bool
	icon    iconKeyOther
	tip     string
	menu    []MenuItem

	shape string // structure of the menu that is currently built
	nodes []node
}

type iconKeyOther struct {
	state model.State
	pct   int
}

func newTray() Tray { return &fyneTray{tip: startingTooltip} }

func (t *fyneTray) Run(onReady func(), _ func(), onMenu func(id string)) {
	t.mu.Lock()
	if t.started || t.quitting {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.onMenu = onMenu
	t.mu.Unlock()

	if runtime.GOOS == "linux" {
		// The StatusNotifierItem Id is taken from the title when systray
		// exports the item, so it must be set before Run. Without it the Id
		// is "systray_<pid>", which changes every launch, and KDE Plasma
		// forgets the icon's "Always shown/hidden" choice. On macOS a title
		// would be drawn next to the menu bar icon.
		systray.SetTitle(brand.DisplayName)
	}
	systray.Run(func() {
		t.mu.Lock()
		t.ready = true
		// Before the first SetIcon the state is unknown: show the neutral
		// pending icon, which claims neither "in sync" nor an error.
		k := iconKeyOther{state: icon.PendingState}
		if t.iconSet {
			k = t.icon
		}
		systray.SetIcon(icon.PNG(icon.Ring(k.state, k.pct, iconPixels())))
		if t.tip != "" {
			systray.SetTooltip(t.tip)
		}
		t.applyMenuLocked()
		quitting := t.quitting
		t.mu.Unlock()
		if quitting {
			systray.Quit()
			return
		}
		if onReady != nil {
			onReady() // systray already calls this on its own goroutine
		}
	}, nil)
}

// menuText prepares a label for the platform menu. Linux menus travel over
// com.canonical.dbusmenu, which hides a single "_" and underlines the next
// character as an access key, so a folder called "work_docs" would read
// "workdocs"; "__" shows one literal underscore. The unescaped text stays in
// node.shown for diffing.
func menuText(s string) string {
	if runtime.GOOS == "linux" {
		return strings.ReplaceAll(s, "_", "__")
	}
	return s
}

// quantise rounds pct down to a 5% step in 0..100, so the ring only closes
// at 100%.
func quantise(pct int) int {
	return max(0, min(pct, 100)) / 5 * 5
}

func (t *fyneTray) SetIcon(s model.State, pct int) {
	k := iconKeyOther{state: s}
	if s == model.StateSyncing {
		k.pct = quantise(pct)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.iconSet && k == t.icon {
		return
	}
	t.iconSet, t.icon = true, k
	if t.ready {
		systray.SetIcon(icon.PNG(icon.Ring(k.state, k.pct, iconPixels())))
	}
}

func (t *fyneTray) SetTooltip(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if text == t.tip {
		return
	}
	t.tip = text
	if t.ready {
		systray.SetTooltip(text)
	}
}

func (t *fyneTray) SetMenu(items []MenuItem) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.menu = items
	if t.ready {
		t.applyMenuLocked()
	}
}

// Balloon is Windows-only; notifications on macOS and Linux go through the
// notify package.
func (t *fyneTray) Balloon(string, string) bool { return false }

func (t *fyneTray) Quit() {
	t.mu.Lock()
	t.quitting = true
	ready := t.ready
	t.mu.Unlock()
	if ready {
		systray.Quit()
	}
}

// shapeOf describes the menu structure: IDs, nesting and check boxes. Text,
// enabled, checked and visible changes keep the shape and are applied in
// place; a new shape rebuilds the menu.
func shapeOf(items []MenuItem) string {
	var b strings.Builder
	var walk func([]MenuItem)
	walk = func(items []MenuItem) {
		for _, it := range items {
			b.WriteString(it.ID)
			if checkable[it.ID] || it.Checked {
				b.WriteString("[x]")
			}
			if len(it.Children) > 0 {
				b.WriteString("{")
				walk(it.Children)
				b.WriteString("}")
			}
			b.WriteString("\x00")
		}
	}
	walk(items)
	return b.String()
}

// applyMenuLocked brings the systray menu in line with t.menu. The caller
// holds mu and the tray is ready.
func (t *fyneTray) applyMenuLocked() {
	shape := shapeOf(t.menu)
	if shape == t.shape && len(t.nodes) == len(t.menu) {
		update(t.nodes, t.menu)
		return
	}
	systray.ResetMenu()
	t.nodes = t.build(nil, t.menu)
	t.shape = shape
}

func (t *fyneTray) build(parent *systray.MenuItem, items []MenuItem) []node {
	nodes := make([]node, 0, len(items))
	for _, it := range items {
		if it.ID == Separator {
			if parent == nil {
				systray.AddSeparator()
			} else {
				parent.AddSeparator()
			}
			nodes = append(nodes, node{shown: it})
			continue
		}
		box := checkable[it.ID] || it.Checked
		var mi *systray.MenuItem
		switch {
		case parent == nil && box:
			mi = systray.AddMenuItemCheckbox(menuText(it.Text), "", it.Checked)
		case parent == nil:
			mi = systray.AddMenuItem(menuText(it.Text), "")
		case box:
			mi = parent.AddSubMenuItemCheckbox(menuText(it.Text), "", it.Checked)
		default:
			mi = parent.AddSubMenuItem(menuText(it.Text), "")
		}
		if !it.Enabled {
			mi.Disable()
		}
		if !it.Visible {
			mi.Hide()
		}
		n := node{it: mi, shown: it}
		if len(it.Children) > 0 {
			n.children = t.build(mi, it.Children)
		} else {
			go t.listen(mi, it.ID)
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// listen forwards clicks until the item is removed (which closes ClickedCh).
func (t *fyneTray) listen(mi *systray.MenuItem, id string) {
	for range mi.ClickedCh {
		t.mu.Lock()
		f := t.onMenu
		t.mu.Unlock()
		if f != nil {
			f(id)
		}
	}
}

// update applies text, enabled, checked and visible changes to a menu of the
// same shape.
func update(nodes []node, items []MenuItem) {
	for i := range nodes {
		n, it := &nodes[i], items[i]
		if n.it == nil {
			continue
		}
		if it.Text != n.shown.Text {
			n.it.SetTitle(menuText(it.Text))
		}
		if it.Enabled != n.shown.Enabled {
			if it.Enabled {
				n.it.Enable()
			} else {
				n.it.Disable()
			}
		}
		if it.Checked != n.shown.Checked {
			if it.Checked {
				n.it.Check()
			} else {
				n.it.Uncheck()
			}
		}
		if it.Visible != n.shown.Visible {
			if it.Visible {
				n.it.Show()
			} else {
				n.it.Hide()
			}
		}
		update(n.children, it.Children)
		n.shown = it
	}
}
