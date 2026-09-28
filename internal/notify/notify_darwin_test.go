//go:build darwin

package notify

import (
	"reflect"
	"testing"
)

func TestOsascriptArgs(t *testing.T) {
	title := `-e "quoted" & (item 1)`
	body := "line one\nline two \" ' \\"
	got := osascriptArgs(title, body)
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 3 of argv) with title (item 2 of argv)",
		"-e", "end run",
		"stv2", title, body,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("osascriptArgs =\n%q\nwant\n%q", got, want)
	}
	for _, a := range got[:6] {
		if a == title || a == body {
			t.Fatal("user text leaked into the script")
		}
	}
}
