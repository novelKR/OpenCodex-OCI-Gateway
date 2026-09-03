package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type maintenanceRuntimeRecorder struct {
	prepareCalls int
	verifyCalls  []ControlOperation
	resumeCalls  int
	prepareErr   error
	verifyErr    error
}

func (r *maintenanceRuntimeRecorder) Prepare(context.Context) error {
	r.prepareCalls++
	return r.prepareErr
}

func (r *maintenanceRuntimeRecorder) Verify(_ context.Context, operation ControlOperation) error {
	r.verifyCalls = append(r.verifyCalls, operation)
	return r.verifyErr
}

func (r *maintenanceRuntimeRecorder) Resume() { r.resumeCalls++ }

func TestMaintenanceCoordinatorPrepareAndCommit(t *testing.T) {
	coordinator, store, watcher, runtime, state := maintenanceFixture(t)
	intent := maintenanceIntentForTest(1)

	witness, err := coordinator.Prepare(context.Background(), state.Generation, intent)
	if err != nil {
		t.Fatal(err)
	}
	if witness.OriginRoutingGeneration != state.Generation || witness.PreparedRoutingGeneration != state.Generation+1 ||
		witness.FinalRoutingGeneration != state.Generation+2 || witness.Intent != intent {
		t.Fatalf("maintenance witness = %#v", witness)
	}
	prepared, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Phase != PhaseApplying || prepared.Generation != witness.PreparedRoutingGeneration ||
		prepared.DesiredBackend != BackendLocalAppleContainer || prepared.AppliedBackend != BackendLocalAppleContainer {
		t.Fatalf("prepared routing state = %#v", prepared)
	}
	if snapshot := watcher.Snapshot(); snapshot.Invalid || snapshot.AllowsDataPlane() || snapshot.State != prepared {
		t.Fatalf("prepared watcher snapshot = %#v", snapshot)
	}
	if runtime.prepareCalls != 1 {
		t.Fatalf("runtime prepare calls = %d", runtime.prepareCalls)
	}

	if err := coordinator.Commit(context.Background(), witness); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !stableAppleState(committed) || committed.Generation != witness.FinalRoutingGeneration {
		t.Fatalf("committed routing state = %#v", committed)
	}
	if pending, err := store.HasPendingMaintenance(); err != nil || pending {
		t.Fatalf("maintenance journal after commit: pending=%t err=%v", pending, err)
	}
	if runtime.prepareCalls != 2 || len(runtime.verifyCalls) != 1 || runtime.verifyCalls[0] != ControlOperationMaintenanceCommit || runtime.resumeCalls != 1 {
		t.Fatalf("runtime calls = prepare:%d verify:%#v resume:%d", runtime.prepareCalls, runtime.verifyCalls, runtime.resumeCalls)
	}
	if snapshot := watcher.Snapshot(); snapshot.Invalid || !snapshot.AllowsDataPlane() || snapshot.State != committed {
		t.Fatalf("committed watcher snapshot = %#v", snapshot)
	}
}

func TestMaintenanceCoordinatorRollbackUsesSamePinnedRoutingBackend(t *testing.T) {
	coordinator, store, _, runtime, state := maintenanceFixture(t)
	witness, err := coordinator.Prepare(context.Background(), state.Generation, maintenanceIntentForTest(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Rollback(context.Background(), witness); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !stableAppleState(rolledBack) || rolledBack.Generation != witness.FinalRoutingGeneration {
		t.Fatalf("rolled-back routing state = %#v", rolledBack)
	}
	if len(runtime.verifyCalls) != 1 || runtime.verifyCalls[0] != ControlOperationMaintenanceRollback || runtime.resumeCalls != 1 {
		t.Fatalf("rollback runtime calls = verify:%#v resume:%d", runtime.verifyCalls, runtime.resumeCalls)
	}
}

func TestMaintenanceFailureLeavesDurableRecoveryWitness(t *testing.T) {
	coordinator, store, watcher, runtime, state := maintenanceFixture(t)
	runtime.prepareErr = errors.New("drain failed")
	intent := maintenanceIntentForTest(3)
	witness, err := coordinator.Prepare(context.Background(), state.Generation, intent)
	if !errors.Is(err, ErrMaintenanceRecoveryRequired) {
		t.Fatalf("prepare error = %v", err)
	}
	if witness.PreparedRoutingGeneration != state.Generation+1 {
		t.Fatalf("failure witness = %#v", witness)
	}
	pending, pendingErr := store.HasPendingMaintenance()
	prepared, stateErr := store.Load()
	if pendingErr != nil || stateErr != nil || !pending || prepared.Phase != PhaseApplying || prepared.Generation != witness.PreparedRoutingGeneration {
		t.Fatalf("failed prepare durability: pending=%t pendingErr=%v state=%#v stateErr=%v", pending, pendingErr, prepared, stateErr)
	}
	if snapshot := watcher.Snapshot(); snapshot.Invalid || snapshot.AllowsDataPlane() {
		t.Fatalf("failed prepare reopened watcher: %#v", snapshot)
	}
	runtime.prepareErr = nil
	retried, err := coordinator.Prepare(context.Background(), state.Generation, intent)
	if err != nil || retried != witness {
		t.Fatalf("idempotent prepare retry = %#v err=%v, want %#v", retried, err, witness)
	}
}

func TestMaintenanceRecoveryStateAcceptsEachCrashPosition(t *testing.T) {
	for _, position := range []string{"origin", "prepared", "final"} {
		t.Run(position, func(t *testing.T) {
			coordinator, store, _, _, state := maintenanceFixture(t)
			witness, err := newMaintenanceWitness(state.Generation, maintenanceIntentForTest(4))
			if err != nil {
				t.Fatal(err)
			}
			journal := maintenanceJournal{Schema: maintenanceSchemaVersion, BoundConfigPath: store.ConfigPath(), Witness: witness}
			if err := store.writeMaintenance(journal); err != nil {
				t.Fatal(err)
			}
			_ = coordinator
			if position != "origin" {
				lock, err := store.Lock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				next := state
				next.Generation = witness.PreparedRoutingGeneration
				next.Phase = PhaseApplying
				if err := lock.Save(next); err != nil {
					_ = lock.Close()
					t.Fatal(err)
				}
				state = next
				if position == "final" {
					next.Generation = witness.FinalRoutingGeneration
					next.Phase = PhaseRelayActive
					if err := lock.Save(next); err != nil {
						_ = lock.Close()
						t.Fatal(err)
					}
					state = next
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			}
			recovered, found, err := store.MaintenanceRecoveryState()
			if err != nil || !found || recovered != state {
				t.Fatalf("%s recovery state = %#v found=%t err=%v", position, recovered, found, err)
			}
		})
	}
}

func TestMaintenanceRecoveryStateRejectsMismatchedJournal(t *testing.T) {
	_, store, _, _, state := maintenanceFixture(t)
	witness, err := newMaintenanceWitness(state.Generation+10, maintenanceIntentForTest(5))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeMaintenance(maintenanceJournal{Schema: maintenanceSchemaVersion, BoundConfigPath: store.ConfigPath(), Witness: witness}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.MaintenanceRecoveryState(); !errors.Is(err, ErrMaintenanceRecoveryRequired) || found {
		t.Fatalf("mismatched maintenance state: found=%t err=%v", found, err)
	}
}

func TestMaintenanceRecoveryStateRejectsCompetingRoutingJournal(t *testing.T) {
	_, store, watcher, _, state := maintenanceFixture(t)
	witness, err := newMaintenanceWitness(state.Generation, maintenanceIntentForTest(9))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeMaintenance(maintenanceJournal{Schema: maintenanceSchemaVersion, BoundConfigPath: store.ConfigPath(), Witness: witness}); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		Schema:                 SchemaVersion,
		Kind:                   transactionKindRoutingSwitch,
		Generation:             state.Generation,
		Target:                 ModeRelay,
		Origin:                 ModeRelay,
		TargetBackend:          BackendLocalAppleContainer,
		OriginBackend:          BackendExternal,
		OriginAuthoritative:    true,
		Stage:                  transactionPrepared,
		RelayConfigFingerprint: "absent",
		CodexConfigFingerprint: "absent",
		InteractiveFingerprint: "absent",
	}
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.MaintenanceRecoveryState(); !errors.Is(err, ErrMaintenanceRecoveryRequired) || found {
		t.Fatalf("competing routing journal: found=%t err=%v", found, err)
	}
	watcher.Refresh()
	if snapshot := watcher.Snapshot(); !snapshot.Invalid || snapshot.AllowsDataPlane() {
		t.Fatalf("competing journals were admitted: %#v", snapshot)
	}
}

func TestMaintenanceJournalRejectsDuplicateKeys(t *testing.T) {
	_, store, _, _, state := maintenanceFixture(t)
	witness, err := newMaintenanceWitness(state.Generation, maintenanceIntentForTest(8))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(maintenanceJournal{Schema: maintenanceSchemaVersion, BoundConfigPath: store.ConfigPath(), Witness: witness})
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(payload, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1)
	if err := os.WriteFile(store.MaintenancePath(), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.MaintenanceRecoveryState(); !errors.Is(err, ErrMaintenanceRecoveryRequired) || found {
		t.Fatalf("duplicate-key maintenance journal: found=%t err=%v", found, err)
	}
}

func TestSocketRuntimeMaintenanceRoundTrip(t *testing.T) {
	coordinator, store, _, _, state := maintenanceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := StartControlServer(ctx, store.ConfigPath(), coordinator.Handle)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("Unix-domain sockets are restricted in this test sandbox: %v", err)
		}
		t.Fatal(err)
	}
	defer server.Close()
	client := NewSocketRuntimeMaintenance(store.ConfigPath())
	status, err := client.Status(context.Background())
	if err != nil || status.Pending || status.Backend != BackendLocalAppleContainer || status.RoutingGeneration != state.Generation {
		t.Fatalf("maintenance status = %#v err=%v", status, err)
	}
	witness, err := client.Prepare(context.Background(), state.Generation, maintenanceIntentForTest(6))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Commit(context.Background(), witness); err != nil {
		t.Fatal(err)
	}
}

func maintenanceFixture(t *testing.T) (*MaintenanceCoordinator, *Store, *Watcher, *maintenanceRuntimeRecorder, State) {
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
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
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
	watcher := NewWatcher(store, 0, WithWatcherStateRecoveryGate(func(State) error { return nil }))
	runtime := &maintenanceRuntimeRecorder{}
	coordinator, err := NewMaintenanceCoordinator(configPath, watcher, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, store, watcher, runtime, state
}

func maintenanceIntentForTest(seed byte) MaintenanceIntent {
	oldHash := repeatedHex(seed)
	newHash := repeatedHex(seed + 1)
	return MaintenanceIntent{
		OperationID:        "operation-0000001",
		InstallationID:     "installation-0001",
		OldManifestSHA256:  oldHash,
		NewManifestSHA256:  newHash,
		OldImageDigest:     "sha256:" + oldHash,
		NewImageDigest:     "sha256:" + newHash,
		OldStateGeneration: 1,
		NewStateGeneration: 2,
	}
}

func repeatedHex(seed byte) string {
	digits := "0123456789abcdef"
	value := digits[int(seed)%len(digits)]
	payload := make([]byte, 64)
	for index := range payload {
		payload[index] = value
	}
	return string(payload)
}

func TestMaintenanceJournalIsOwnerOnlyRegularFile(t *testing.T) {
	coordinator, store, _, _, state := maintenanceFixture(t)
	if _, err := coordinator.Prepare(context.Background(), state.Generation, maintenanceIntentForTest(7)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(store.MaintenancePath())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("maintenance journal mode = %v", info.Mode())
	}
}
