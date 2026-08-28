//go:build windows

package handoff

// Windows does not expose the directory fsync primitive used by the macOS
// removal transaction. Automatic OpenCodex removal is unsupported on Windows.
func syncControlDirectory(string) error { return nil }
