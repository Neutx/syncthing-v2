package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDescribeFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Event json.RawMessage `json:"event"`
		Text  string          `json:"text"`
		Tint  string          `json:"tint"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		var h struct {
			Type string `json:"type"`
			Time string `json:"time"`
		}
		_ = json.Unmarshal(c.Event, &h)
		seen[h.Type] = true

		item, ok := Describe(c.Event)
		if c.Text == "" {
			if ok {
				t.Errorf("%s: got %+v, want no line", c.Event, item)
			}
			continue
		}
		if !ok || item.Text != c.Text || item.Tint != c.Tint {
			t.Errorf("%s: got %q/%q ok=%v, want %q/%q", c.Event, item.Text, item.Tint, ok, c.Text, c.Tint)
		}
		want, _ := time.Parse(time.RFC3339Nano, h.Time)
		if !item.At.Equal(want) {
			t.Errorf("%s: At = %v, want %v", h.Type, item.At, want)
		}
	}
	// Every event type of the inventory map (§2.4) is covered.
	for _, typ := range []string{"DeviceConnected", "DeviceDisconnected", "ItemFinished", "StateChanged",
		"LocalIndexUpdated", "RemoteChangeDetected", "FolderErrors", "ConfigSaved"} {
		if !seen[typ] {
			t.Errorf("fixture lacks %s", typ)
		}
	}
}

func TestDescribeBadInput(t *testing.T) {
	for _, in := range []string{``, `nope`, `[]`, `{"type":"ItemFinished","data":"x"}`, `{"type":"StateChanged","data":[1]}`} {
		if item, ok := Describe(json.RawMessage(in)); ok {
			t.Errorf("Describe(%q) = %+v, want no line", in, item)
		}
	}
	before := time.Now()
	item, ok := Describe(json.RawMessage(`{"id":1,"type":"ConfigSaved","time":"yesterday"}`))
	if !ok || item.At.Before(before) {
		t.Errorf("unparsable time: %+v %v, want now", item, ok)
	}
}

func TestFileName(t *testing.T) {
	cases := map[string]string{
		"a/b/c.txt": "c.txt", `a\b\c.txt`: "c.txt", "c.txt": "c.txt", "dir/": "", "": "",
	}
	for in, want := range cases {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}
