//go:build darwin || linux

package routing

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// readStateFile refuses indirection and broad/foreign ownership before
// decoding control state. The state is non-secret, but accepting a symlink or
// another user's file would let a local control-plane caller redirect routing.
func readStateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateOwnedRegular(info, path); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open routing state without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened routing state: %w", err)
	}
	if err := validateOwnedRegular(opened, path); err != nil {
		return nil, err
	}
	reader := io.LimitReader(file, maxStateBytes+1)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read routing state: %w", err)
	}
	return payload, nil
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
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s must not be a symlink", ErrStateCorrupt, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s must be a regular file", ErrStateCorrupt, path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: %s must have mode 0600", ErrStateCorrupt, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s must be owned by the current user", ErrStateCorrupt, path)
	}
	return nil
}
