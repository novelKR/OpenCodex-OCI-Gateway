//go:build !darwin && !linux

package routing

import (
	"fmt"
	"os"
)

func readStateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateOwnedRegular(info, path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func validateExistingControlFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect routing state: %w", err)
	}
	return validateOwnedRegular(info, path)
}

func validateOwnedRegular(info os.FileInfo, path string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s must be a regular non-symlink file", ErrStateCorrupt, path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: %s must have mode 0600", ErrStateCorrupt, path)
	}
	return nil
}
