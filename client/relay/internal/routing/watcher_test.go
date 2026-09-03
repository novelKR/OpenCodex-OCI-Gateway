package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWatcherGatesNativeApplyingAndRecoveryStates(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   State
		invalid bool
	}{
		{
			name:  "native active",
			state: State{DesiredMode: ModeNative, AppliedMode: ModeNative, Phase: PhaseNativeActive},
		},
		{
			name:  "applying native",
			state: State{DesiredMode: ModeNative, AppliedMode: ModeRelay, Phase: PhaseApplying},
		},
		{
			name:  "recovery required",
			state: State{DesiredMode: ModeUnknown, AppliedMode: ModeUnknown, Phase: PhaseRecoveryRequired},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, state := testStoreAndState(t)
			state.DesiredMode = test.state.DesiredMode
			state.AppliedMode = test.state.AppliedMode
			state.Phase = test.state.Phase
			writeState(t, store, state)

			watcher := NewWatcher(store, 0)
			snapshot := watcher.Snapshot()
			if snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
				t.Fatalf("snapshot admitted parked state: %#v", snapshot)
			}
			if snapshot.Invalid != test.invalid {
				t.Fatalf("snapshot.Invalid = %t, want %t", snapshot.Invalid, test.invalid)
			}
		})
	}
}

func TestWatcherAllowsRelayAndNativePendingRestart(t *testing.T) {
	for _, test := range []struct {
		name  string
		state State
	}{
		{
			name:  "relay active",
			state: State{DesiredMode: ModeRelay, AppliedMode: ModeRelay, Phase: PhaseRelayActive},
		},
		{
			name:  "native pending restart preserves existing traffic",
			state: State{DesiredMode: ModeNative, AppliedMode: ModeRelay, Phase: PhaseNativePendingRestart},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, state := testStoreAndState(t)
			state.DesiredMode = test.state.DesiredMode
			state.AppliedMode = test.state.AppliedMode
			state.Phase = test.state.Phase
			writeState(t, store, state)

			snapshot := NewWatcher(store, 0).Snapshot()
			if !snapshot.AllowsDataPlane() || !snapshot.AllowsCatalog() {
				t.Fatalf("snapshot unexpectedly parked: %#v", snapshot)
			}
		})
	}
}

func TestWatcherParksOnStaleOrInvalidTransactionJournal(t *testing.T) {
	store, state := testStoreAndState(t)
	writeState(t, store, state)
	if err := os.WriteFile(store.TransactionPath(), []byte("non-secret transaction\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := NewWatcher(store, 0).Snapshot()
	if !snapshot.Invalid || snapshot.State.Phase != PhaseRecoveryRequired || snapshot.AllowsDataPlane() {
		t.Fatalf("stale journal did not fail closed: %#v", snapshot)
	}

	state.Generation++
	state.DesiredMode = ModeNative
	state.AppliedMode = ModeRelay
	state.Phase = PhaseNativePendingRestart
	writeState(t, store, state)
	state.Generation++
	state.Phase = PhaseApplying
	writeState(t, store, state)
	// writeState receives a value, so keep the matching explicit v2 labels in
	// the journal fixture as well.
	state.DesiredBackend = backendForLegacyMode(state.DesiredMode)
	state.AppliedBackend = backendForLegacyMode(state.AppliedMode)
	if err := os.WriteFile(store.TransactionPath(), validTransactionJournal(t, state), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher := NewWatcher(store, 0)
	snapshot = watcher.Snapshot()
	if snapshot.Invalid || snapshot.State.Phase != PhaseApplying || snapshot.AllowsDataPlane() {
		t.Fatalf("active applying journal did not remain safely observable: %#v", snapshot)
	}

	if err := os.Chmod(store.TransactionPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	watcher.Refresh()
	snapshot = watcher.Snapshot()
	if !snapshot.Invalid || snapshot.AllowsCatalog() {
		t.Fatalf("invalid journal did not fail closed: %#v", snapshot)
	}
}

func TestWatcherParksWhenInitializedStateIsDeleted(t *testing.T) {
	store, state := testStoreAndState(t)
	writeState(t, store, state)
	if _, err := os.Stat(store.InitializedPath()); err != nil {
		t.Fatalf("initialization marker was not written: %v", err)
	}
	if err := os.Remove(store.StatePath()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("deleted initialized state error = %v, want ErrStateCorrupt", err)
	}

	snapshot := NewWatcher(store, 0).Snapshot()
	if !snapshot.Invalid || snapshot.State.Phase != PhaseRecoveryRequired || snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
		t.Fatalf("deleted initialized state reopened admission: %#v", snapshot)
	}
}

func validTransactionJournal(t *testing.T, state State) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema":                   SchemaVersion,
		"generation":               state.Generation,
		"target":                   state.DesiredMode,
		"origin":                   state.AppliedMode,
		"target_backend":           state.DesiredBackend,
		"origin_backend":           state.AppliedBackend,
		"stage":                    "prepared",
		"relay_config_fingerprint": "absent",
		"codex_config_fingerprint": "absent",
		"interactive_fingerprint":  "absent",
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(payload, '\n')
}

func testStoreAndState(t *testing.T) (*Store, State) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	store, err := Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "codex-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return store, state
}

func writeState(t *testing.T, store *Store, state State) {
	t.Helper()
	// These legacy state-machine fixtures set mode/phase directly. Persist the
	// explicit v2 backend counterparts so they exercise the same durable
	// representation used by the production controller.
	state.DesiredBackend = backendForLegacyMode(state.DesiredMode)
	state.AppliedBackend = backendForLegacyMode(state.AppliedMode)
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lock.Save(state); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherParksOnExternalRecoveryGate(t *testing.T) {
	store, state := testStoreAndState(t)
	writeState(t, store, state)
	gateActive := true
	watcher := NewWatcher(store, 0, WithWatcherRecoveryGate(func() error {
		if gateActive {
			return errors.New("bounded external recovery witness")
		}
		return nil
	}))
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.State.Phase != PhaseRecoveryRequired ||
		snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
		t.Fatalf("external recovery gate snapshot=%#v", snapshot)
	}
	gateActive = false
	watcher.Refresh()
	if snapshot := watcher.Snapshot(); snapshot.Invalid || !snapshot.AllowsDataPlane() || !snapshot.AllowsCatalog() {
		t.Fatalf("released external recovery gate snapshot=%#v", snapshot)
	}
}

func TestWatcherParksStableAppleRouteWithoutLifecycleAuthority(t *testing.T) {
	store, state := testStoreAndState(t)
	state.DesiredBackend = BackendLocalAppleContainer
	state.AppliedBackend = BackendLocalAppleContainer
	state.DesiredMode = ModeRelay
	state.AppliedMode = ModeRelay
	state.Phase = PhaseRelayActive
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	calls := 0
	watcher := NewWatcher(store, 0)
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.State.Phase != PhaseRecoveryRequired || snapshot.AllowsDataPlane() {
		t.Fatalf("Apple snapshot without an authority provider = %#v", snapshot)
	}

	watcher = NewWatcher(store, 0, WithWatcherStateRecoveryGate(func(observed State) error {
		calls++
		if observed.Generation != state.Generation || observed.AppliedBackend != BackendLocalAppleContainer {
			t.Fatalf("authority state = %#v", observed)
		}
		return errors.New("lifecycle state missing")
	}))
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.State.Phase != PhaseRecoveryRequired || snapshot.AllowsDataPlane() {
		t.Fatalf("unwitnessed Apple snapshot = %#v", snapshot)
	}
	if calls != 1 {
		t.Fatalf("authority calls = %d", calls)
	}

	watcher = NewWatcher(store, 0, WithWatcherStateRecoveryGate(func(State) error { return nil }))
	if snapshot := watcher.Snapshot(); snapshot.Invalid || snapshot.State.AppliedBackend != BackendLocalAppleContainer || !snapshot.AllowsDataPlane() {
		t.Fatalf("committed Apple snapshot = %#v", snapshot)
	}
}
