package routing

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

func TestWatcherParksWhenManagedCodexArtifactsDrift(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "127.0.0.1:18180"
	cfg.Responses.Scheduler.InteractiveListenAddress = "127.0.0.1:18182"
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	store, err := Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	_ = lock.Close()

	watcher := NewWatcher(store, 0, WithDriftCheck(AppliedRoutingDriftCheck(relayPath)))
	if watcher.Snapshot().Invalid {
		t.Fatalf("matching managed routing was parked: %#v", watcher.Snapshot())
	}
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	watcher.Refresh()
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
		t.Fatalf("drifted managed routing was admitted: %#v", snapshot)
	}
}

func TestWatcherValidatesAppleCatalogBindingAndParksItsDrift(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "127.0.0.1:18180"
	cfg.Responses.Scheduler.InteractiveListenAddress = "127.0.0.1:18182"
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	cfg.LocalAppleContainer, err = config.NewLocalAppleContainerProfileForCodexConfig(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.LocalAppleContainer.CatalogPath); err != nil {
		t.Fatal(err)
	}
	store, err := Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredBackend = BackendLocalAppleContainer
	state.AppliedBackend = BackendLocalAppleContainer
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	_ = lock.Close()

	watcher := NewWatcher(store, 0, WithDriftCheck(AppliedRoutingDriftCheck(relayPath)))
	if snapshot := watcher.Snapshot(); snapshot.Invalid || !snapshot.AllowsDataPlane() {
		t.Fatalf("matching Apple routing was parked: %#v", snapshot)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	watcher.Refresh()
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
		t.Fatalf("drifted Apple routing was admitted: %#v", snapshot)
	}
}
