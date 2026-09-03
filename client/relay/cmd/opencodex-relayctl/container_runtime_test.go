package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/containerruntime"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestContainerRuntimeUsageDocumentsDesktopExitConfirmation(t *testing.T) {
	var output strings.Builder
	writeUsage(&output)
	usage := output.String()
	for _, required := range []string{
		"container-runtime activate --expected-state-digest SHA256\n      --expected-routing-generation N --confirm-desktop-exited --json",
		"container-runtime stop --expected-state-digest SHA256\n      --expected-routing-generation N --confirm-desktop-exited --json",
		"container-runtime park --expected-state-digest SHA256\n      --expected-routing-generation N --json",
		"container-runtime recover --expected-state-digest SHA256\n      --confirm-desktop-exited --json",
	} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omits the required Desktop exit contract %q: %q", required, usage)
		}
	}
}

func TestContainerRuntimeOperationContextHonorsDeadline(t *testing.T) {
	ctx, cancel := containerRuntimeOperationContext(25 * time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("mutation context did not reach its deadline")
	}
}

func TestContainerRuntimeOperationContextHonorsSIGTERM(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestContainerRuntimeMutationSignalHelper$")
	command.Env = append(os.Environ(), "OPENCODEX_MUTATION_SIGNAL_READY="+readyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("signal helper did not become ready")
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("signal helper failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		_ = <-done
		t.Fatal("signal helper did not stop")
	}
}

func TestContainerRuntimeMutationSignalHelper(t *testing.T) {
	readyPath := os.Getenv("OPENCODEX_MUTATION_SIGNAL_READY")
	if readyPath == "" {
		return
	}
	ctx, cancel := containerRuntimeOperationContext(time.Minute)
	defer cancel()
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("signal context error = %v", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not cancel mutation context")
	}
}

func TestContainerRuntimeRejectsLocalDevelopmentScopeAtConstructionAndEnrollment(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "codex.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InstallationScope = config.InstallationScopeLocalDevelopment
	cfg.ListenAddress = config.LocalDevelopmentListenAddress
	cfg.Responses.Scheduler.InteractiveListenAddress = config.LocalDevelopmentInteractiveListen
	cfg.Catalog.Path = filepath.Join(directory, config.LocalDevelopmentExternalCatalog)
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProductionContainerRuntimeConfig(configPath); !errors.Is(err, containerruntime.ErrUnavailable) {
		t.Fatalf("construction scope error = %v", err)
	}
	enroller := &containerProfileEnroller{
		configPath: configPath,
		codexPath:  codexPath,
		account:    "current-user",
	}
	if _, err := enroller.Ensure(context.Background()); !errors.Is(err, containerruntime.ErrUnavailable) {
		t.Fatalf("enrollment scope error = %v", err)
	}
}

func TestRoutingSnapshotUsesStrictMaintenanceStatus(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    routing.MaintenanceRoutingStatus
		want      containerruntime.RoutingSnapshot
		wantError bool
	}{
		{
			name: "prepared maintenance is fail closed and pending",
			status: routing.MaintenanceRoutingStatus{
				Schema: 1, RoutingGeneration: 8, Backend: routing.BackendLocalAppleContainer,
				Phase: routing.PhaseApplying, Pending: true,
			},
			want: containerruntime.RoutingSnapshot{
				Generation: 8, RecoveryRequired: true, MaintenancePending: true,
			},
		},
		{
			name: "finished maintenance is stable Apple",
			status: routing.MaintenanceRoutingStatus{
				Schema: 1, RoutingGeneration: 9, Backend: routing.BackendLocalAppleContainer,
				Phase: routing.PhaseRelayActive,
			},
			want: containerruntime.RoutingSnapshot{Generation: 9, AppleActive: true},
		},
		{
			name: "unknown phase is rejected",
			status: routing.MaintenanceRoutingStatus{
				Schema: 1, RoutingGeneration: 9, Backend: routing.BackendLocalAppleContainer,
				Phase: routing.Phase("future_phase"),
			},
			wantError: true,
		},
		{
			name: "pending maintenance cannot claim an external backend",
			status: routing.MaintenanceRoutingStatus{
				Schema: 1, RoutingGeneration: 8, Backend: routing.BackendExternal,
				Phase: routing.PhaseApplying, Pending: true,
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := routingSnapshotFromMaintenanceStatus(test.status)
			if (err != nil) != test.wantError {
				t.Fatalf("snapshot error = %v", err)
			}
			if !test.wantError && got != test.want {
				t.Fatalf("snapshot = %#v, want %#v", got, test.want)
			}
		})
	}
}
