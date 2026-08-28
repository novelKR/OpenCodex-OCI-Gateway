package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
)

func TestRepairNativeReplacesOnlyVerifiedLocalDevelopmentRecovery(t *testing.T) {
	controller, store, codexPath := localDevelopmentNativeVerifyFixture(t)
	replaceVerifyNativeState(t, store, func(state *State) {
		state.Phase = PhaseRecoveryRequired
		state.DesiredMode = ModeRelay
		state.AppliedMode = ModeNative
		state.DesiredBackend = BackendExternal
		state.AppliedBackend = BackendNone
	})
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	status, err := controller.RepairNative(context.Background(), before.Generation, true)
	if err != nil {
		t.Fatalf("RepairNative() error = %v", err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation+1 ||
		after.Phase != PhaseNativeActive ||
		after.DesiredMode != ModeNative || after.AppliedMode != ModeNative ||
		after.DesiredBackend != BackendNone || after.AppliedBackend != BackendNone {
		t.Fatalf("repaired state = %#v, before = %#v", after, before)
	}
	if status.Generation != after.Generation ||
		status.Phase != PhaseNativeActive ||
		status.DesiredBackend != BackendNone ||
		status.AppliedBackend != BackendNone {
		t.Fatalf("repaired status = %#v", status)
	}
	if err := codexconfig.ValidateNativeRoutingForOwner(codexPath, codexconfig.LocalDevelopmentOwner); err != nil {
		t.Fatalf("repair changed native Codex routing: %v", err)
	}
}

func TestRepairNativeRejectsWithoutChangingState(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, *Controller, *Store, string, uint64) (uint64, bool)
		want      error
	}{
		{
			name: "confirmation missing",
			configure: func(_ *testing.T, _ *Controller, _ *Store, _ string, generation uint64) (uint64, bool) {
				return generation, false
			},
			want: ErrNativeRepairUnavailable,
		},
		{
			name: "generation changed",
			configure: func(_ *testing.T, _ *Controller, _ *Store, _ string, generation uint64) (uint64, bool) {
				return generation - 1, true
			},
			want: ErrNativeRepairGenerationStale,
		},
		{
			name: "unmanaged base URL",
			configure: func(t *testing.T, _ *Controller, _ *Store, codexPath string, generation uint64) (uint64, bool) {
				if err := os.WriteFile(codexPath, []byte("openai_base_url = \"http://127.0.0.1:10100/v1\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeVerification,
		},
		{
			name: "unmanaged OpenCodex provider",
			configure: func(t *testing.T, _ *Controller, _ *Store, codexPath string, generation uint64) (uint64, bool) {
				if err := os.WriteFile(codexPath, []byte("model_provider = \"pw_opencodex\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeVerification,
		},
		{
			name: "local development Relay artifacts",
			configure: func(t *testing.T, _ *Controller, _ *Store, codexPath string, generation uint64) (uint64, bool) {
				if err := codexconfig.EnableWithInteractiveProfileForOwner(
					codexPath, codexconfig.LocalDevelopmentOwner,
					"http://127.0.0.1:18190/v1", "http://127.0.0.1:18192/v1",
					filepath.Join(filepath.Dir(codexPath), "development.json"),
				); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeVerification,
		},
		{
			name: "foreign production Relay artifacts",
			configure: func(t *testing.T, _ *Controller, _ *Store, codexPath string, generation uint64) (uint64, bool) {
				if err := codexconfig.EnableWithInteractiveProfileForOwner(
					codexPath, codexconfig.ProductionOwner,
					"http://127.0.0.1:18190/v1", "http://127.0.0.1:18192/v1",
					filepath.Join(filepath.Dir(codexPath), "production.json"),
				); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeVerification,
		},
		{
			name: "mismatched Codex path binding",
			configure: func(t *testing.T, _ *Controller, store *Store, _ string, generation uint64) (uint64, bool) {
				state, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				state.BoundCodexConfigPath = filepath.Join(filepath.Dir(store.ConfigPath()), "other-config.toml")
				payload, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.StatePath(), append(payload, 10), 0o600); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeRepairUnavailable,
		},
		{
			name: "removal recovery gate",
			configure: func(_ *testing.T, controller *Controller, _ *Store, _ string, generation uint64) (uint64, bool) {
				controller.recoveryGate = func() error { return errors.New("bounded removal recovery") }
				return generation, true
			},
			want: ErrNativeRepairUnavailable,
		},
		{
			name: "pending transaction",
			configure: func(t *testing.T, controller *Controller, store *Store, _ string, generation uint64) (uint64, bool) {
				state, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				journal, err := controller.newJournal(state, BackendExternal, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := controller.writeJournal(journal); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeRepairUnavailable,
		},
		{
			name: "malformed transaction",
			configure: func(t *testing.T, _ *Controller, store *Store, _ string, generation uint64) (uint64, bool) {
				if err := os.WriteFile(store.TransactionPath(), []byte("{not-json}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return generation, true
			},
			want: ErrNativeRepairUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, store, codexPath := localDevelopmentNativeVerifyFixture(t)
			replaceVerifyNativeState(t, store, func(state *State) {
				state.Phase = PhaseRecoveryRequired
				state.DesiredMode = ModeRelay
				state.AppliedMode = ModeNative
				state.DesiredBackend = BackendExternal
				state.AppliedBackend = BackendNone
			})
			state, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			expected, confirmed := test.configure(t, controller, store, codexPath, state.Generation)
			before, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}

			if _, err := controller.RepairNative(context.Background(), expected, confirmed); !errors.Is(err, test.want) {
				t.Fatalf("RepairNative() error = %v, want %v", err, test.want)
			}
			after, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("rejected repair changed state\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestRepairNativeRejectsProductionOwnerAndNonRecoveryState(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	controller, err := NewController(configPath, codexPath, WithCodexConfigOwner(codexconfig.ProductionOwner))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SeedNativeParked(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RepairNative(context.Background(), state.Generation, true); !errors.Is(err, ErrNativeRepairUnavailable) {
		t.Fatalf("production RepairNative() error = %v", err)
	}

	localController, localStore, _ := localDevelopmentNativeVerifyFixture(t)
	localState, err := localStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := localController.RepairNative(context.Background(), localState.Generation, true); !errors.Is(err, ErrNativeRepairUnavailable) {
		t.Fatalf("stable RepairNative() error = %v", err)
	}
}
