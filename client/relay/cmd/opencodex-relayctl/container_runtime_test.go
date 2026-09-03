package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
		"container-runtime recover --expected-state-digest SHA256\n      --confirm-desktop-exited --json",
	} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omits the required Desktop exit contract %q: %q", required, usage)
		}
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
