//go:build !darwin

package handoff

func isLocalVolume(string) bool {
	return false
}
