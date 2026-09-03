//go:build darwin

package lifecyclelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAcquireCreatesOwnerOnlyPersistentLockAndSerializes(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := Acquire(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, first.file.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("lifecycle lock is inheritable: flags=%#x errno=%v", flags, errno)
	}
	path, _ := Path(home)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe lock artifact: info=%v err=%v", info, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, home); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("persistent lock removed: %v", err)
	}
}

func TestSharedReadersCoexistAndBlockWriterUntilLastClose(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := acquire(context.Background(), home, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquire(context.Background(), home, true)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	writerContext, cancelWriter := context.WithTimeout(context.Background(), time.Second)
	defer cancelWriter()
	writerResult := make(chan error, 1)
	go func() {
		writer, writerErr := Acquire(writerContext, home)
		if writerErr == nil {
			writerErr = writer.Close()
		}
		writerResult <- writerErr
	}()

	select {
	case err := <-writerResult:
		t.Fatalf("writer passed two shared readers: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerResult:
		t.Fatalf("writer passed the remaining shared reader: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerResult:
		if err != nil {
			t.Fatalf("writer after readers closed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not acquire after the last shared reader closed")
	}
}

func TestAcquireRejectsSymlinkAndLooseDirectory(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "symlink", setup: func(path string) error {
			return os.Symlink(t.TempDir(), path)
		}},
		{name: "loose", setup: func(path string) error { return os.Mkdir(path, 0o755) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			base := filepath.Join(home, "Library", "Application Support")
			if err := os.MkdirAll(base, 0o700); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(base, directoryName)
			if err := test.setup(directory); err != nil {
				t.Fatal(err)
			}
			if _, err := Acquire(context.Background(), home); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("Acquire error = %v", err)
			}
		})
	}
}

func TestAcquireRejectsLooseLockWithoutRepairingMode(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "Library", "Application Support", directoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := Path(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unsafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), home); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Acquire error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("loose lock was repaired: info=%v err=%v", info, err)
	}
}

func TestConcurrentFirstAcquireDoesNotMisclassifyCreatorRace(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support"), 0o700); err != nil {
		t.Fatal(err)
	}
	const writers = 12
	start := make(chan struct{})
	var group sync.WaitGroup
	errorsSeen := make(chan error, writers)
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			lock, err := Acquire(ctx, home)
			if err == nil {
				time.Sleep(time.Millisecond)
				err = lock.Close()
			}
			errorsSeen <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent first acquire failed: %v", err)
		}
	}
}

func TestSameFileIdentityRejectsReplacedPathEntry(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first")
	secondPath := filepath.Join(directory, "second")
	if err := os.WriteFile(firstPath, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := os.Lstat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Lstat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if !sameFileIdentity(first, first) {
		t.Fatal("same inode was rejected")
	}
	if sameFileIdentity(first, second) {
		t.Fatal("replaced path inode was accepted")
	}
}
