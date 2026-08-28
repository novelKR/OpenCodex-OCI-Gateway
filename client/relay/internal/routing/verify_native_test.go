package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

func TestVerifyNativeSucceedsWithUnavailableRelayWithoutCredentialOrRuntimeCalls(t *testing.T) {
	credentialCalls := 0
	runtimeCalls := 0
	controller, store, codexPath := localDevelopmentNativeVerifyFixture(t,
		withCredentialLoader(func(config.CredentialsConfig) (credentials.Values, error) {
			credentialCalls++
			return credentials.Values{}, errors.New("credential lookup must not run")
		}),
		WithRuntimeControl(runtimeControlFunc(func(context.Context, uint64, Backend) error {
			runtimeCalls++
			return errors.New("runtime control must not run")
		})),
	)
	stateBefore, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	codexBefore, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := controller.VerifyNative(context.Background())
	if err != nil {
		t.Fatalf("VerifyNative() error = %v", err)
	}
	if status.Phase != PhaseNativeActive || status.DesiredBackend != BackendNone || status.AppliedBackend != BackendNone {
		t.Fatalf("verified status = %#v", status)
	}
	if status.Connection.LocalRelay != LocalRelayUnreachable || status.RelayRunning {
		t.Fatalf("unavailable parked relay must remain informational: %#v", status)
	}
	if credentialCalls != 0 || runtimeCalls != 0 {
		t.Fatalf("VerifyNative credential/runtime calls = %d/%d, want 0/0", credentialCalls, runtimeCalls)
	}
	stateAfter, err := os.ReadFile(store.StatePath())
	if err != nil || string(stateAfter) != string(stateBefore) {
		t.Fatalf("VerifyNative changed state: %q err=%v", stateAfter, err)
	}
	codexAfter, err := os.ReadFile(codexPath)
	if err != nil || string(codexAfter) != string(codexBefore) {
		t.Fatalf("VerifyNative changed Codex config: %q err=%v", codexAfter, err)
	}
}

func TestVerifyNativeRejectsNonNativeOrInterruptedState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*State)
	}{
		{
			name: "active relay",
			mutate: func(state *State) {
				state.Phase = PhaseRelayActive
				state.DesiredMode = ModeRelay
				state.AppliedMode = ModeRelay
				state.DesiredBackend = BackendExternal
				state.AppliedBackend = BackendExternal
			},
		},
		{
			name: "native pending restart",
			mutate: func(state *State) {
				state.Phase = PhaseNativePendingRestart
				state.DesiredMode = ModeNative
				state.AppliedMode = ModeRelay
				state.DesiredBackend = BackendNone
				state.AppliedBackend = BackendExternal
			},
		},
		{
			name: "recovery required",
			mutate: func(state *State) {
				state.Phase = PhaseRecoveryRequired
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, store, _ := localDevelopmentNativeVerifyFixture(t)
			replaceVerifyNativeState(t, store, test.mutate)
			if _, err := controller.VerifyNative(context.Background()); !errors.Is(err, ErrNativeVerification) {
				t.Fatalf("VerifyNative() error = %v, want ErrNativeVerification", err)
			}
		})
	}
}

func TestVerifyNativeRejectsPresentOrMalformedJournal(t *testing.T) {
	t.Run("valid journal", func(t *testing.T) {
		controller, store, _ := localDevelopmentNativeVerifyFixture(t)
		state, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		journal, err := controller.newJournal(state, BackendNone, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := controller.writeJournal(journal); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.VerifyNative(context.Background()); !errors.Is(err, ErrNativeVerification) {
			t.Fatalf("VerifyNative() error = %v, want ErrNativeVerification", err)
		}
	})

	t.Run("malformed journal", func(t *testing.T) {
		controller, store, _ := localDevelopmentNativeVerifyFixture(t)
		if err := os.WriteFile(store.TransactionPath(), []byte("{not-json}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.VerifyNative(context.Background()); !errors.Is(err, ErrNativeVerification) {
			t.Fatalf("VerifyNative() error = %v, want ErrNativeVerification", err)
		}
	})
}

func TestVerifyNativeRejectsLocalAndProductionMarkerDrift(t *testing.T) {
	tests := []struct {
		name  string
		owner codexconfig.Owner
	}{
		{name: "local development marker", owner: codexconfig.LocalDevelopmentOwner},
		{name: "foreign production marker", owner: codexconfig.ProductionOwner},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, store, codexPath := localDevelopmentNativeVerifyFixture(t)
			cfg, err := config.Load(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := codexconfig.EnableWithInteractiveProfileForOwner(
				codexPath,
				test.owner,
				"http://127.0.0.1:18190/v1",
				"http://127.0.0.1:18192/v1",
				cfg.Catalog.Path,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.VerifyNative(context.Background()); !errors.Is(err, ErrNativeVerification) {
				t.Fatalf("VerifyNative() error = %v, want ErrNativeVerification", err)
			}
		})
	}
}

func localDevelopmentNativeVerifyFixture(t *testing.T, options ...ControllerOption) (*Controller, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
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
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options = append([]ControllerOption{
		WithCodexConfigOwner(codexconfig.LocalDevelopmentOwner),
		WithHealthReader(unavailableHealth{}),
	}, options...)
	controller, err := NewController(configPath, codexPath, options...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SeedNativeParked(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return controller, store, codexPath
}

func replaceVerifyNativeState(t *testing.T, store *Store, mutate func(*State)) {
	t.Helper()
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	mutate(&state)
	state.Generation++
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lock.Replace(state); err != nil {
		t.Fatal(err)
	}
}
