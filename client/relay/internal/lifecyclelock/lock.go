// Package lifecyclelock serializes user-owned Relay integration, routing, and
// OpenCodex removal writers that otherwise use independent per-install locks.
package lifecyclelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const directoryName = "OpenCodexRelayLifecycle"

var ErrUnsafe = errors.New("Relay lifecycle lock is unsafe")

type Lock struct {
	file   *os.File
	closed bool
}

func Path(home string) (string, error) {
	if home == "" {
		return "", ErrUnsafe
	}
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) {
		return "", ErrUnsafe
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", ErrUnsafe
	}
	return filepath.Join(filepath.Clean(resolved), "Library", "Application Support", directoryName, "lifecycle.lock"), nil
}

func Acquire(ctx context.Context, home string) (*Lock, error) {
	return acquire(ctx, home, false)
}

// AcquireReader holds a shared lifecycle lease from request admission through
// one authenticated Apple connection's post-dial running proof and credential
// header write. The resident RuntimeManager gate separately drains the full
// response before a lifecycle writer can mutate routing or the container.
func AcquireReader(ctx context.Context, home, reservationToken string) (*Lock, error) {
	lock, err := acquire(ctx, home, true)
	if err != nil {
		return nil, err
	}
	if err := ValidateSourceInstallReservation(home, reservationToken); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func acquire(ctx context.Context, home string, shared bool) (*Lock, error) {
	// Linux has no macOS app-level integration/removal lifecycle to serialize.
	// Keep existing Linux commands and tests side-effect free instead of
	// creating a macOS-shaped directory below HOME.
	if runtime.GOOS == "linux" {
		return &Lock{}, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsafe
	}
	path, err := Path(home)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return nil, fmt.Errorf("%w: create lock parent", ErrUnsafe)
	}
	if err := secureDirectory(directory); err != nil {
		return nil, err
	}
	file, err := lockFile(ctx, path, shared)
	if err != nil {
		return nil, err
	}
	return &Lock{file: file}, nil
}

// AcquireWriter takes the shared lifecycle lock and then applies the durable
// source-install admission marker while the lock is still held. Lifecycle
// writers must use this entry point; raw Acquire is reserved for creating or
// releasing the marker itself.
func AcquireWriter(ctx context.Context, home, reservationToken string) (*Lock, error) {
	lock, err := Acquire(ctx, home)
	if err != nil {
		return nil, err
	}
	if err := ValidateSourceInstallReservation(home, reservationToken); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: create lock directory", ErrUnsafe)
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: inspect lock directory", ErrUnsafe)
	}
	if err := validateOwner(info); err != nil {
		return err
	}
	return nil
}

func (lock *Lock) Close() error {
	if lock == nil || lock.closed {
		return nil
	}
	lock.closed = true
	return unlockFile(lock.file)
}
