//go:build !darwin

package handoff

func hasExtendedACL(string) (bool, error) {
	return false, nil
}
