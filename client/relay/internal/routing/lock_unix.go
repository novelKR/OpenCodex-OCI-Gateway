//go:build darwin || linux

package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lockFile(ctx context.Context, path string) (*os.File, error) {
	if err := validateExistingControlFile(path); err != nil {
		return nil, fmt.Errorf("inspect routing lock: %w", err)
	}
	return lockFileWithFlags(ctx, path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, true)
}

// lockExistingFile shares the writer advisory-lock protocol without changing
// the filesystem. It is used only by proofs such as local-dev uninstall
// verification, where creating or chmod-ing a lock file would itself violate
// the read-only contract.
func lockExistingFile(ctx context.Context, path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect existing routing lock: %w", err)
	}
	if err := validateOwnedRegular(info, path); err != nil {
		return nil, fmt.Errorf("inspect existing routing lock: %w", err)
	}
	return lockFileWithFlags(ctx, path, syscall.O_RDWR|syscall.O_NOFOLLOW, false)
}

func lockFileWithFlags(ctx context.Context, path string, flags int, protect bool) (*os.File, error) {
	fd, err := syscall.Open(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open routing lock without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if protect {
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("protect routing lock: %w", err)
		}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat routing lock: %w", err)
	}
	if err := validateOwnedRegular(info, path); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect routing lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire routing lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("acquire routing lock: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func unlockFile(file *os.File) error {
	if file == nil {
		return nil
	}
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("release routing lock: %w", err)
	}
	return closeErr
}
