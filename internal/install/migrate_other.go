//go:build !windows

package install

// DetectLegacy finds nothing: the legacy prototype tray existed on Windows
// only.
func DetectLegacy(Roots) Legacy { return Legacy{} }

// MigrateLegacy has nothing to do off Windows.
func MigrateLegacy(Roots, Legacy) error { return nil }
