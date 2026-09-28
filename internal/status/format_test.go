package status

import (
	"testing"

	"github.com/Neutx/syncthing-v2/internal/model"
)

func TestGrp(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 12345: "12,345", 123456: "123,456",
		1234567: "1,234,567", -1234: "-1,234", -999: "-999", 1000000000000: "1,000,000,000,000",
	}
	for in, want := range cases {
		if got := Grp(in); got != want {
			t.Errorf("Grp(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRate(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 KB/s"},
		{1023.9, "0 KB/s"},
		{1024, "1.0 KB/s"},
		{1536, "1.5 KB/s"},
		{2304, "2.3 KB/s"}, // 2.25 rounds away from zero, as .NET does
		{10 * 1024, "10 KB/s"},
		{10.5 * 1024, "11 KB/s"},
		{1023 * 1024, "1023 KB/s"},
		{1024 * 1024, "1.0 MB/s"},
		{8.44 * 1024 * 1024, "8.4 MB/s"},
		{250 * 1024 * 1024, "250 MB/s"},
		{3 * 1024 * 1024 * 1024, "3.0 GB/s"},
		{5000 * 1024 * 1024 * 1024, "5000 GB/s"},
	}
	for _, c := range cases {
		if got := Rate(c.in); got != c.want {
			t.Errorf("Rate(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{9, "9 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10 * 1024, "10 KB"},
		{1048575, "1024 KB"},
		{5 << 20, "5.0 MB"},
		{123 << 20, "123 MB"},
		{15 << 30, "15 GB"},
		{2 << 40, "2.0 TB"},
		{3000 << 40, "3000 TB"},
	}
	for _, c := range cases {
		if got := Size(c.in); got != c.want {
			t.Errorf("Size(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestETA(t *testing.T) {
	cases := []struct {
		need int64
		rate float64
		want string
	}{
		{0, 5000, ""},
		{1 << 20, 1024, ""}, // not more than 1 KB/s
		{1 << 20, 0, ""},
		{45 * 2048, 2048, "45s"},
		{122675, 2048, "59s"},
		{60 * 2048, 2048, "1m"},
		{1585152, 2048, "12m"},
		{3600 * 2048, 2048, "1.0h"},
		{9216000, 2048, "1.3h"},
		{195379200, 2048, "26.5h"},
	}
	for _, c := range cases {
		if got := ETA(c.need, c.rate); got != c.want {
			t.Errorf("ETA(%d, %v) = %q, want %q", c.need, c.rate, got, c.want)
		}
	}
}

func TestLabelAndHeadline(t *testing.T) {
	cases := []struct {
		s               model.State
		label, headline string
	}{
		{model.StateDown, "Not running", "Syncthing not running"},
		{model.StateUnauthorized, "API key rejected", "API key rejected"},
		{model.StateError, "Error", "Needs attention"},
		{model.StatePaused, "Paused", "Paused"},
		{model.StateSyncing, "Syncing", "Syncing"},
		{model.StateScanning, "Checking", "Checking for changes"},
		{model.StateNoPeer, "Disconnected", "Disconnected"},
		{model.StateInSync, "In sync", "In sync"},
	}
	for _, c := range cases {
		if got := Label(c.s); got != c.label {
			t.Errorf("Label(%d) = %q, want %q", c.s, got, c.label)
		}
		if got := Headline(c.s); got != c.headline {
			t.Errorf("Headline(%d) = %q, want %q", c.s, got, c.headline)
		}
	}
}
