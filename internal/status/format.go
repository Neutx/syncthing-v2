package status

import (
	"math"
	"strconv"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// Grp formats n with comma thousands separators, like .NET's "N0" with the
// invariant culture (F18): 1234567 → "1,234,567".
func Grp(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := false
	if n < 0 {
		neg, s = true, s[1:]
	}
	out := make([]byte, 0, len(s)+len(s)/3+1)
	if neg {
		out = append(out, '-')
	}
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// Rate formats a transfer rate in bytes per second (F18). Anything below
// 1 KB/s is "0 KB/s"; values below 10 get one decimal: "1.5 KB/s", "12 MB/s".
func Rate(bps float64) string {
	if bps < 1024 {
		return "0 KB/s"
	}
	units := [...]string{"B", "KB", "MB", "GB"}
	v, i := bps, 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return num(v, v < 10) + " " + units[i] + "/s"
}

// Size formats a byte count from B to TB (F18): "512 B", "1.5 KB", "20 GB".
// Values below 10 in KB and above get one decimal.
func Size(b int64) string {
	units := [...]string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return num(v, v < 10 && i > 0) + " " + units[i]
}

// ETA estimates the time left for needBytes at downRate bytes per second
// (F16). It is "" unless bytes are needed and more than 1 KB/s is arriving.
// Seconds and minutes are truncated; hours have one decimal: "45s", "12m", "1.5h".
func ETA(needBytes int64, downRate float64) string {
	if needBytes <= 0 || downRate <= 1024 {
		return ""
	}
	eta := float64(needBytes) / downRate
	switch {
	case eta < 60:
		return strconv.Itoa(int(eta)) + "s"
	case eta < 3600:
		return strconv.Itoa(int(eta/60)) + "m"
	default:
		return num(eta/3600, true) + "h"
	}
}

// Label is the short state name used in the tray tooltip and menu (F18).
func Label(s model.State) string {
	switch s {
	case model.StateDown:
		return "Not running"
	case model.StateUnauthorized:
		return "API key rejected"
	case model.StateNoPeer:
		return "Disconnected"
	case model.StateScanning:
		return "Checking"
	case model.StateSyncing:
		return "Syncing"
	case model.StateError:
		return "Error"
	case model.StatePaused:
		return "Paused"
	default:
		return "In sync"
	}
}

// Headline is the dashboard header title for a state (D5).
func Headline(s model.State) string {
	switch s {
	case model.StateDown:
		return "Syncthing not running"
	case model.StateUnauthorized:
		return "API key rejected"
	case model.StateNoPeer:
		return "Disconnected"
	case model.StateScanning:
		return "Checking for changes"
	case model.StateSyncing:
		return "Syncing"
	case model.StateError:
		return "Needs attention"
	case model.StatePaused:
		return "Paused"
	default:
		return "In sync"
	}
}

// num formats v like .NET's "0.0" (oneDecimal) or "0" custom formats, which
// round halves away from zero.
func num(v float64, oneDecimal bool) string {
	if oneDecimal {
		return strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64)
	}
	return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
}
