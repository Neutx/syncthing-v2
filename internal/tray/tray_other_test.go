//go:build darwin || linux

package tray

import (
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

func TestQuantise(t *testing.T) {
	cases := map[int]int{-5: 0, 0: 0, 4: 0, 5: 5, 42: 40, 97: 95, 99: 95, 100: 100, 180: 100}
	for in, want := range cases {
		if got := quantise(in); got != want {
			t.Errorf("quantise(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestShapeOf(t *testing.T) {
	a := buildMenu(model.Snapshot{State: model.StateInSync, Folders: []model.Folder{{ID: "x"}}}, "on login")
	b := buildMenu(model.Snapshot{State: model.StateDown, Label: "Not running", FolderLine: "", Folders: []model.Folder{{ID: "x"}},
		Startup: model.Startup{Tray: true, Syncthing: true}}, "on login")
	if shapeOf(a) != shapeOf(b) {
		t.Error("text, enabled, checked and visible changes must keep the shape")
	}
	c := buildMenu(model.Snapshot{State: model.StateInSync, Folders: []model.Folder{{ID: "x"}, {ID: "y"}}}, "on login")
	if shapeOf(a) == shapeOf(c) {
		t.Error("a folder submenu must change the shape")
	}
	d := buildMenu(model.Snapshot{State: model.StatePaused, Folders: []model.Folder{{ID: "x"}}}, "on login")
	if shapeOf(a) == shapeOf(d) {
		t.Error("pause and resume have different IDs and must change the shape")
	}
}

func TestInitialTooltip(t *testing.T) {
	if got := newTray().(*fyneTray).tip; got != startingTooltip {
		t.Errorf("initial tooltip %q, want %q", got, startingTooltip)
	}
}
