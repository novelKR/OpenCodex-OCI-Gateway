//go:build !darwin

package containerruntime

func hasExtendedACL(string) (bool, error) {
	return false, nil
}
