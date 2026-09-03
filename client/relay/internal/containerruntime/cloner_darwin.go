//go:build darwin

package containerruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type APFSCloner struct{}

func NewAPFSCloner() *APFSCloner { return &APFSCloner{} }

func (*APFSCloner) Clone(ctx context.Context, source, destination string) error {
	if err := validateClonePaths(source, destination); err != nil {
		return err
	}
	if err := validateGenerationTree(source); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ErrUnavailable
	default:
	}
	if err := unix.Clonefile(source, destination, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
		return ErrUnavailable
	}
	if err := validateGenerationTree(destination); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ErrUnavailable
	default:
		return nil
	}
}

func validateClonePaths(source, destination string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || filepath.Clean(source) != source ||
		filepath.Clean(destination) != destination || source == destination ||
		filepath.Dir(source) != filepath.Dir(destination) {
		return ErrUnsafeState
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeState
	}
	return nil
}

func validateGenerationTree(root string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 ||
		rootInfo.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(rootInfo) {
		return ErrUnsafeState
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrUnsafeState
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
			return ErrUnsafeState
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafeState
		}
		if info.Mode().IsRegular() {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Nlink != 1 {
				return ErrUnsafeState
			}
		}
		return nil
	})
}
