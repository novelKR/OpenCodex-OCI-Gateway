//go:build darwin || linux

package lifecyclelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func validateOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: foreign owner", ErrUnsafe)
	}
	return nil
}

func lockFile(ctx context.Context, path string, shared bool) (*os.File, error) {
	fd := -1
	for {
		flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		info, inspectErr := os.Lstat(path)
		if errors.Is(inspectErr, os.ErrNotExist) {
			flags |= syscall.O_CREAT | syscall.O_EXCL
		} else if inspectErr != nil || validateLockFileInfo(info) != nil {
			return nil, fmt.Errorf("%w: inspect lock", ErrUnsafe)
		}
		opened, err := syscall.Open(path, flags, 0o600)
		if err == nil {
			fd = opened
			break
		}
		// Another lifecycle writer may have created the persistent lock after
		// our lstat. Re-inspect that winner instead of treating an ordinary
		// first-use race as an unsafe filesystem boundary.
		if flags&syscall.O_EXCL != 0 && errors.Is(err, syscall.EEXIST) {
			continue
		}
		return nil, fmt.Errorf("%w: open lock", ErrUnsafe)
	}
	file := os.NewFile(uintptr(fd), path)
	closeUnsafe := func() (*os.File, error) {
		_ = file.Close()
		return nil, ErrUnsafe
	}
	openedInfo, err := file.Stat()
	if err != nil || validateLockFileInfo(openedInfo) != nil {
		return closeUnsafe()
	}
	operation := syscall.LOCK_EX | syscall.LOCK_NB
	if shared {
		operation = syscall.LOCK_SH | syscall.LOCK_NB
	}
	for {
		if err := syscall.Flock(int(file.Fd()), operation); err == nil {
			// A waiter may have opened the former path inode before another
			// current-user process replaced the directory entry. Match the Swift
			// participant and prove the locked descriptor is still the inode named
			// by the canonical lifecycle path before admitting a writer.
			pathInfo, pathErr := os.Lstat(path)
			lockedInfo, lockedErr := file.Stat()
			if pathErr != nil || lockedErr != nil || validateLockFileInfo(pathInfo) != nil ||
				!sameFileIdentity(pathInfo, lockedInfo) {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				return closeUnsafe()
			}
			return file, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("%w: acquire lock", ErrUnsafe)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func sameFileIdentity(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino
}

func validateLockFileInfo(info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return ErrUnsafe
	}
	return validateOwner(info)
}

func unlockFile(file *os.File) error {
	if file == nil {
		return nil
	}
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
