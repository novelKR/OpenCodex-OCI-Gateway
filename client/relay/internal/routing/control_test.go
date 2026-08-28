package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSocketRuntimeControlUsesOwnerOnlySameUserSocket(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan ControlRequest, 1)
	server, err := StartControlServer(ctx, configPath, func(_ context.Context, request ControlRequest) (ControlResponse, error) {
		requests <- request
		return ControlResponse{OK: true, Generation: request.Generation, Backend: request.Backend}, nil
	})
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("Unix-domain sockets are restricted in this test sandbox: %v", err)
		}
		t.Fatal(err)
	}
	defer server.Close()
	info, err := os.Lstat(ControlPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("control socket mode = %v", info.Mode())
	}
	applyCtx, applyCancel := context.WithTimeout(context.Background(), time.Second)
	defer applyCancel()
	if err := NewSocketRuntimeControl(configPath).Apply(applyCtx, 7, BackendLocalOpenCodex); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		if request.Generation != 7 || request.Backend != BackendLocalOpenCodex {
			t.Fatalf("control request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("control handler did not run")
	}
}

func TestSocketRuntimeControlRejectsMissingOrUnsafeSocket(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	if err := NewSocketRuntimeControl(configPath).Apply(context.Background(), 1, BackendExternal); err != ErrControlUnavailable {
		t.Fatalf("missing control error = %v", err)
	}
	path := ControlPath(configPath)
	if err := os.WriteFile(path, []byte("not-a-socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewSocketRuntimeControl(configPath).Apply(context.Background(), 1, BackendExternal); err != ErrControlUnavailable {
		t.Fatalf("unsafe control error = %v", err)
	}
}
