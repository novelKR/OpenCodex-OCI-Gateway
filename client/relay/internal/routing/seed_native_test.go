package routing

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
)

func TestSeedNativeParkedWritesNoCodexRouting(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	controller, err := NewController(configPath, codexPath, WithCodexConfigOwner(codexconfig.LocalDevelopmentOwner))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SeedNativeParked(context.Background()); err != nil {
		t.Fatalf("SeedNativeParked() error = %v", err)
	}
	state, legacy, err := controller.Store().Read()
	if err != nil {
		t.Fatal(err)
	}
	if legacy || state.Phase != PhaseNativeActive || state.DesiredBackend != BackendNone || state.AppliedBackend != BackendNone {
		t.Fatalf("seeded state = %+v, legacy=%t", state, legacy)
	}
	if err := codexconfig.ValidateNativeRoutingForOwner(codexPath, codexconfig.LocalDevelopmentOwner); err != nil {
		t.Fatalf("seed changed Codex routing: %v", err)
	}
}
