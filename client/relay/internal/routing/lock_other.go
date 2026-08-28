//go:build !darwin && !linux

package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// The supported release hosts use flock in lock_unix.go. This conservative
// fallback keeps tests/cross-compilation usable elsewhere; a stale lock fails
// closed rather than allowing concurrent state writers after a crash.
func lockFile(ctx context.Context, path string) (*os.File, error) {
	for {
		if err := validateExistingControlFile(path); err != nil {
			return nil, fmt.Errorf("inspect routing lock: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("protect routing lock: %w", chmodErr)
			}
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire routing lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire routing lock: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The local-development distribution is macOS-only. On fallback platforms we
// cannot join the create/delete lock protocol without mutating the lock path,
// so a read-only verification deliberately fails closed.
func lockExistingFile(context.Context, string) (*os.File, error) {
	return nil, errors.New("read-only routing lock is unsupported on this platform")
}

func unlockFile(file *os.File) error {
	if file == nil {
		return nil
	}
	path := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
