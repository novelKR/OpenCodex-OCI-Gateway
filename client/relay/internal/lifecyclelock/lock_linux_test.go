//go:build linux

package lifecyclelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireIsSideEffectFreeOnLinux(t *testing.T) {
	home := t.TempDir()
	lock, err := Acquire(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("macOS lifecycle directory was created on Linux: %v", err)
	}
}
