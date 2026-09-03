package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestParkHandoffForRecoveryPersistsDurableGate(t *testing.T) {
	relayPath := filepath.Join(t.TempDir(), "relay.json")
	store, err := routing.Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(relayPath), "config.toml"))
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
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	lock, err = store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := parkHandoffForRecovery(lock, state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	parked, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if parked.Phase != routing.PhaseRecoveryRequired || parked.Generation <= state.Generation {
		t.Fatalf("handoff recovery gate = %#v", parked)
	}
}

func TestParkHandoffForRecoverySurfacesClosedLockSaveFailure(t *testing.T) {
	relayPath := filepath.Join(t.TempDir(), "relay.json")
	store, err := routing.Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(relayPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parkHandoffForRecovery(lock, state); err == nil {
		t.Fatal("closed routing lock save failure was reported as persisted recovery")
	}
}

func TestHandoffRoutingPreflightsRejectRuntimeMaintenanceWitness(t *testing.T) {
	relayPath := filepath.Join(t.TempDir(), "relay.json")
	store, err := routing.Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(relayPath), "config.toml"))
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
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	preparation := removalRoutingRecoveryPreparation{
		gateState: &removalRoutingRecoveryGateState{allowedGeneration: state.Generation},
	}
	if !preparation.routingGenerationMatches(relayPath) {
		t.Fatal("removal routing generation rejected a clean stable state")
	}
	if err := os.WriteFile(routing.MaintenancePath(relayPath), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preflightHandoffRoutingState(store, state); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("handoff maintenance preflight error = %v", err)
	}
	if preparation.routingGenerationMatches(relayPath) {
		t.Fatal("removal routing generation accepted a runtime maintenance witness")
	}
}

func removalRoutingRecoveryFixture(
	t *testing.T,
	recoveryPending bool,
) (string, string, *routing.Store, routing.State) {
	t.Helper()
	relayPath := filepath.Join(t.TempDir(), "relay.json")
	codexPath := filepath.Join(filepath.Dir(relayPath), "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := routing.Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, codexPath)
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
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, relayPath)
	if !recoveryPending {
		record.Phase = handoff.RemovalCleanupPhasePackageVerified
		record.PackageAttempt = 1
		record.ExecutionBootSession = strings.Repeat("b", 64)
		if err := handoff.WriteRemovalCleanup(relayPath, record); err != nil {
			t.Fatal(err)
		}
		return relayPath, codexPath, store, state
	}
	record.Phase = handoff.RemovalCleanupPhaseIntent
	if err := handoff.WriteRemovalCleanup(relayPath, record); err != nil {
		t.Fatal(err)
	}
	active := record
	active.ExecutionAttempt = 1
	active.ActiveExecution = &handoff.RemovalActiveExecution{
		Kind:         handoff.RemovalExecutionTeardown,
		Attempt:      1,
		BootSession:  strings.Repeat("b", 64),
		BootAttested: true,
	}
	if err := handoff.WriteRemovalCleanup(relayPath, active); err != nil {
		t.Fatal(err)
	}
	if _, err := handoff.MarkRemovalExecutionResolution(
		relayPath,
		handoff.RemovalExecutionTeardown,
		handoff.RemovalExecutionResolutionPreStartRoutingChanged,
		true,
	); err != nil {
		t.Fatal(err)
	}
	if _, didResolve, err := handoff.ResumeRemovalExecutionResolution(
		relayPath,
		func() error { return nil },
	); err != nil || !didResolve {
		t.Fatalf("resolution didResolve=%t err=%v", didResolve, err)
	}
	return relayPath, codexPath, store, state
}

func removalRoutingRecoverySelectionForTest() handoff.NPMRemovalSelection {
	return handoff.NPMRemovalSelection{
		ID:          "0123456789abcdef01234567",
		Fingerprint: strings.Repeat("a", 64),
	}
}

type removalRoutingTransactionForTest struct {
	generation          uint64
	target              routing.Mode
	origin              routing.Mode
	targetBackend       routing.Backend
	originBackend       routing.Backend
	originAuthoritative bool
	stage               string
}

func writeRemovalRoutingTransactionForTest(
	t *testing.T,
	store *routing.Store,
	codexPath string,
	transaction removalRoutingTransactionForTest,
) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema":                   routing.SchemaVersion,
		"generation":               transaction.generation,
		"target":                   transaction.target,
		"origin":                   transaction.origin,
		"target_backend":           transaction.targetBackend,
		"origin_backend":           transaction.originBackend,
		"origin_authoritative":     transaction.originAuthoritative,
		"stage":                    transaction.stage,
		"relay_config_fingerprint": optionalRoutingFileFingerprintForTest(t, store.ConfigPath()),
		"codex_config_fingerprint": optionalRoutingFileFingerprintForTest(t, codexPath),
		"interactive_fingerprint": optionalRoutingFileFingerprintForTest(
			t,
			codexconfig.InteractiveProfilePathForOwner(codexPath, codexconfig.ProductionOwner),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func optionalRoutingFileFingerprintForTest(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type removalRoutingStoreHealth struct {
	store *routing.Store
}

func (health removalRoutingStoreHealth) Read(_ context.Context, _ config.Config) routing.LocalRelay {
	if health.store == nil {
		return routing.LocalRelay{}
	}
	state, err := health.store.Load()
	if err != nil {
		return routing.LocalRelay{}
	}
	active := int64(0)
	admission := "deny"
	refresh := "pause"
	lifecycle := "paused"
	if state.AllowsDataPlane() {
		admission = "allow"
		refresh = "run"
		lifecycle = "running"
	}
	endpoint := routing.LocalRelayEndpoint{
		Valid:            true,
		Generation:       state.Generation,
		DesiredMode:      state.DesiredMode,
		AppliedMode:      state.AppliedMode,
		Phase:            state.Phase,
		RelayAdmission:   admission,
		CatalogRefresh:   refresh,
		CatalogLifecycle: lifecycle,
		RemoteGateway:    string(routing.RemoteGatewayNotApplicable),
		LocalOpenCodex:   string(routing.LocalOpenCodexUnknown),
		ActiveRequests:   &active,
	}
	return routing.LocalRelay{General: endpoint, Interactive: endpoint}
}

func parkRemovalRoutingState(t *testing.T, store *routing.Store, state routing.State) routing.State {
	t.Helper()
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := parkHandoffForRecovery(lock, state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	parked, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return parked
}

func stableRemovalRoutingJournalFixture(
	t *testing.T,
) (string, string, *routing.Store, routing.State, removalRoutingTransactionForTest) {
	t.Helper()
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	stable := parked
	stable.DesiredMode = routing.ModeNative
	stable.AppliedMode = routing.ModeNative
	stable.DesiredBackend = routing.BackendNone
	stable.AppliedBackend = routing.BackendNone
	stable.Phase = routing.PhaseNativeActive
	stable.Generation += 2
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(stable); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	transaction := removalRoutingTransactionForTest{
		generation:          stable.Generation - 1,
		target:              routing.ModeNative,
		origin:              routing.ModeRelay,
		targetBackend:       routing.BackendNone,
		originBackend:       routing.BackendExternal,
		originAuthoritative: false,
		stage:               "config_mutated",
	}
	writeRemovalRoutingTransactionForTest(t, store, codexPath, transaction)
	return relayPath, codexPath, store, stable, transaction
}

func TestRemovalRoutingRecoveryTokenSurvivesFailureAndReleasesOnlyAfterSuccess(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		recoveryPending bool
	}{
		{name: "resolved execution", recoveryPending: true},
		{name: "verified package", recoveryPending: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, testCase.recoveryPending)
			parked := parkRemovalRoutingState(t, store, state)
			preparation, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				parked.Generation,
				removalRoutingRecoverySelectionForTest(),
			)
			if err != nil || !preparation.active() || preparation.alreadyRecovered {
				t.Fatalf("preparation=%#v err=%v", preparation, err)
			}
			if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
				t.Fatalf("ordinary gate was released before Recover: %v", err)
			}
			if err := preparation.recoveryGate(relayPath)(); err != nil {
				t.Fatalf("exact CLI token did not bypass its own gate: %v", err)
			}
			drifted := preparation
			drifted.recordToken += "drift"
			if err := drifted.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
				t.Fatalf("drifted token bypassed gate: %v", err)
			}

			recoverErr := errors.New("forced recovery failure")
			if _, err := executeRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				true,
				preparation,
				func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
					return routing.Status{}, recoverErr
				},
				func(context.Context) routing.Status {
					t.Fatal("failed recovery refreshed status")
					return routing.Status{}
				},
			); !errors.Is(err, recoverErr) {
				t.Fatalf("forced recovery error=%v", err)
			}
			stillPending, exists, err := handoff.ReadRemovalCleanup(relayPath)
			if err != nil || !exists || !removalRoutingRecoveryRecordEligible(stillPending) {
				t.Fatalf("failure cleared gate: record=%#v exists=%t err=%v", stillPending, exists, err)
			}

			recovered := parked
			recovered.Generation += 2
			recovered.DesiredMode = routing.ModeNative
			recovered.AppliedMode = routing.ModeNative
			recovered.DesiredBackend = routing.BackendNone
			recovered.AppliedBackend = routing.BackendNone
			recovered.Phase = routing.PhaseNativeActive
			fresh := routing.Status{
				Generation:     recovered.Generation,
				DesiredMode:    routing.ModeNative,
				AppliedMode:    routing.ModeNative,
				DesiredBackend: routing.BackendNone,
				AppliedBackend: routing.BackendNone,
				Phase:          routing.PhaseNativeActive,
				RelayRunning:   true,
				Connection: routing.Connection{
					LocalRelay:  routing.LocalRelayHealthy,
					RoutingSync: routing.RoutingSyncAcknowledged,
				},
			}
			result, err := executeRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				true,
				preparation,
				func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
					lock, err := store.Lock(context.Background())
					if err != nil {
						return routing.Status{}, err
					}
					if err := lock.Replace(recovered); err != nil {
						_ = lock.Close()
						return routing.Status{}, err
					}
					if err := preparation.routingWitness.RebindStable(lock); err != nil {
						_ = lock.Close()
						return routing.Status{}, err
					}
					if err := lock.Close(); err != nil {
						return routing.Status{}, err
					}
					return routing.Status{Generation: recovered.Generation, Phase: routing.PhaseRecoveryRequired}, nil
				},
				func(context.Context) routing.Status { return fresh },
			)
			if err != nil || result.Generation != fresh.Generation || result.Phase != fresh.Phase {
				t.Fatalf("success result=%#v err=%v", result, err)
			}
			released, exists, err := handoff.ReadRemovalCleanup(relayPath)
			if err != nil || !exists || removalRoutingRecoveryRecordEligible(released) ||
				handoff.RemovalRoutingGate(relayPath) != nil {
				t.Fatalf("success record=%#v exists=%t err=%v gate=%v", released, exists, err, handoff.RemovalRoutingGate(relayPath))
			}
			if testCase.recoveryPending {
				if released.RecoveryPending || !released.OperationRetryPending {
					t.Fatalf("resolved execution release=%#v", released)
				}
			} else if !released.RoutingRecoveryReleased {
				t.Fatalf("verified package release=%#v", released)
			}
		})
	}
}

func TestRemovalRoutingRecoveryRejectsRollbackAndUnsafeOrDriftedState(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	if _, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryRollback,
		parked.Generation,
		removalRoutingRecoverySelectionForTest(),
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("removal rollback error=%v", err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("rollback released gate: %v", err)
	}

	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	applying := parked
	applying.DesiredBackend = routing.BackendExternal
	applying.DesiredMode = routing.ModeRelay
	applying.AppliedBackend = routing.BackendNone
	applying.AppliedMode = routing.ModeNative
	applying.Phase = routing.PhaseApplying
	applying.Generation++
	if err := lock.Replace(applying); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		applying.Generation,
		removalRoutingRecoverySelectionForTest(),
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("applying state recovery error=%v", err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("unsafe state released gate: %v", err)
	}
}

func TestRemovalRoutingRecoveryRequiresExactExpectedGeneration(t *testing.T) {
	t.Run("missing or mismatched", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		for _, generation := range []uint64{0, parked.Generation + 1} {
			if _, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				generation,
				removalRoutingRecoverySelectionForTest(),
			); !errors.Is(err, routing.ErrRecoveryRequired) {
				t.Fatalf("generation=%d error=%v", generation, err)
			}
		}
		if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("generation refusal released gate: %v", err)
		}
	})

	t.Run("inactive flag is rejected", func(t *testing.T) {
		relayPath := filepath.Join(t.TempDir(), "relay.json")
		codexPath := filepath.Join(filepath.Dir(relayPath), "config.toml")
		if _, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			7,
			handoff.NPMRemovalSelection{},
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("inactive expected generation error=%v", err)
		}
		if _, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			0,
			removalRoutingRecoverySelectionForTest(),
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("inactive installation selector error=%v", err)
		}
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			0,
			handoff.NPMRemovalSelection{},
		)
		if err != nil || preparation.active() {
			t.Fatalf("ordinary recovery preparation=%#v err=%v", preparation, err)
		}
	})

	t.Run("state drift after prepare revokes bypass", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			parked.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := store.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		drifted := parked
		drifted.Generation++
		if err := lock.Replace(drifted); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		if err := preparation.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("generation drift retained bypass: %v", err)
		}
	})
}

func TestRemovalRoutingRecoveryRequiresExactInstallationSelector(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	exact := removalRoutingRecoverySelectionForTest()
	for _, testCase := range []struct {
		name      string
		selection handoff.NPMRemovalSelection
	}{
		{name: "missing"},
		{name: "missing fingerprint", selection: handoff.NPMRemovalSelection{ID: exact.ID}},
		{name: "missing id", selection: handoff.NPMRemovalSelection{Fingerprint: exact.Fingerprint}},
		{name: "invalid id", selection: handoff.NPMRemovalSelection{ID: "not-an-opaque-id", Fingerprint: exact.Fingerprint}},
		{name: "invalid fingerprint", selection: handoff.NPMRemovalSelection{ID: exact.ID, Fingerprint: "not-a-fingerprint"}},
		{name: "mismatched id", selection: handoff.NPMRemovalSelection{ID: strings.Repeat("f", 24), Fingerprint: exact.Fingerprint}},
		{name: "mismatched fingerprint", selection: handoff.NPMRemovalSelection{ID: exact.ID, Fingerprint: strings.Repeat("b", 64)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				parked.Generation,
				testCase.selection,
			); !errors.Is(err, routing.ErrRecoveryRequired) {
				t.Fatalf("selection=%#v error=%v", testCase.selection, err)
			}
		})
	}

	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		parked.Generation,
		exact,
	)
	if err != nil || !preparation.active() || preparation.selection != exact {
		t.Fatalf("exact preparation=%#v err=%v", preparation, err)
	}
	drifted := preparation
	drifted.selection.Fingerprint = strings.Repeat("b", 64)
	if err := drifted.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("selector drift retained mutation bypass: %v", err)
	}
	if err := drifted.release(relayPath); !errors.Is(err, handoff.ErrRemovalCleanupUnsafe) {
		t.Fatalf("selector drift released cleanup token: %v", err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("selector refusals released journal gate: %v", err)
	}
}

func TestRelayctlUsageDocumentsExactRemovalRecoveryBinding(t *testing.T) {
	var output strings.Builder
	writeUsage(&output)
	usage := output.String()
	for _, flag := range []string{
		"--expected-routing-generation N",
		"--installation-id ID",
		"--installation-fingerprint SHA256",
	} {
		if !strings.Contains(usage, flag) {
			t.Fatalf("usage omits %s: %q", flag, usage)
		}
	}
}

func TestRemovalRoutingRecoveryReconcilesCrashAfterStableCommit(t *testing.T) {
	for _, recoveryPending := range []bool{true, false} {
		name := "verified package"
		if recoveryPending {
			name = "resolved execution"
		}
		t.Run(name, func(t *testing.T) {
			relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, recoveryPending)
			parked := parkRemovalRoutingState(t, store, state)
			preparation, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				parked.Generation,
				removalRoutingRecoverySelectionForTest(),
			)
			if err != nil || !preparation.active() {
				t.Fatalf("initial preparation=%#v err=%v", preparation, err)
			}

			lock, err := store.Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			recovered := parked
			recovered.DesiredMode = routing.ModeNative
			recovered.AppliedMode = routing.ModeNative
			recovered.DesiredBackend = routing.BackendNone
			recovered.AppliedBackend = routing.BackendNone
			recovered.Phase = routing.PhaseNativeActive
			recovered.Generation++
			if err := lock.Replace(recovered); err != nil {
				_ = lock.Close()
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}

			resumed, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				recovered.Generation,
				removalRoutingRecoverySelectionForTest(),
			)
			if err != nil || !resumed.active() || !resumed.alreadyRecovered {
				t.Fatalf("resumed preparation=%#v err=%v", resumed, err)
			}
			statusCalls := 0
			result, err := executeRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				true,
				resumed,
				func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
					t.Fatal("already-recovered routing state was replayed")
					return routing.Status{}, nil
				},
				func(context.Context) routing.Status {
					statusCalls++
					return routing.Status{
						Generation:     recovered.Generation,
						DesiredMode:    recovered.DesiredMode,
						AppliedMode:    recovered.AppliedMode,
						DesiredBackend: recovered.DesiredBackend,
						AppliedBackend: recovered.AppliedBackend,
						Phase:          recovered.Phase,
						RelayRunning:   true,
						Connection: routing.Connection{
							LocalRelay:  routing.LocalRelayHealthy,
							RoutingSync: routing.RoutingSyncAcknowledged,
						},
					}
				},
			)
			if err != nil || statusCalls != 1 || result.Generation != recovered.Generation ||
				handoff.RemovalRoutingGate(relayPath) != nil {
				t.Fatalf("result=%#v statusCalls=%d err=%v gate=%v", result, statusCalls, err, handoff.RemovalRoutingGate(relayPath))
			}
		})
	}
}

func TestRemovalRoutingRecoveryActiveTokenFailsClosedAfterJournalDrift(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			parked.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || !preparation.active() {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		if err := handoff.RemoveRemovalCleanup(relayPath); err != nil {
			t.Fatal(err)
		}
		if err := handoff.RemovalRoutingGate(relayPath); err != nil {
			t.Fatalf("ordinary missing-journal gate=%v", err)
		}
		if err := preparation.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("deleted exact token gained bypass: %v", err)
		}
	})

	t.Run("replaced by non-gating record", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			parked.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || !preparation.active() {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		if err := handoff.RemoveRemovalCleanup(relayPath); err != nil {
			t.Fatal(err)
		}
		replacement := removalCleanupRecordForTest(t, relayPath)
		replacement.Phase = handoff.RemovalCleanupPhaseIntent
		if err := handoff.WriteRemovalCleanup(relayPath, replacement); err != nil {
			t.Fatal(err)
		}
		if err := handoff.RemovalRoutingGate(relayPath); err != nil {
			t.Fatalf("ordinary replacement gate=%v", err)
		}
		if err := preparation.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("replacement exact token gained bypass: %v", err)
		}
	})
}

func TestRemovalRoutingRecoveryStableCommitRequiresNoRoutingTransaction(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered := parked
	recovered.DesiredMode = routing.ModeNative
	recovered.AppliedMode = routing.ModeNative
	recovered.DesiredBackend = routing.BackendNone
	recovered.AppliedBackend = routing.BackendNone
	recovered.Phase = routing.PhaseNativeActive
	recovered.Generation++
	if err := lock.Replace(recovered); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		recovered.Generation,
		removalRoutingRecoverySelectionForTest(),
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("stable state with transaction error=%v", err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("transactional stable state released gate: %v", err)
	}
}

func TestRemovalRoutingRecoveryParkedStateRejectsMalformedRoutingTransaction(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		parked.Generation,
		removalRoutingRecoverySelectionForTest(),
	)
	if err != nil || !preparation.active() {
		t.Fatalf("initial preparation=%#v err=%v", preparation, err)
	}
	if err := os.WriteFile(store.TransactionPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		parked.Generation,
		removalRoutingRecoverySelectionForTest(),
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("malformed parked transaction preparation error=%v", err)
	}
	if err := preparation.recoveryGate(relayPath)(); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("malformed transaction retained token bypass: %v", err)
	}
	controller, err := routingControllerWithRecoveryGate(
		relayPath,
		codexPath,
		preparation.recoveryGate(relayPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Recover(
		context.Background(),
		routing.RecoveryComplete,
		true,
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("malformed transaction reached observed recovery: %v", err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after != parked {
		t.Fatalf("malformed transaction mutated routing state: before=%#v after=%#v", parked, after)
	}
	record, exists, err := handoff.ReadRemovalCleanup(relayPath)
	if err != nil || !exists || !removalRoutingRecoveryRecordEligible(record) ||
		!errors.Is(handoff.RemovalRoutingGate(relayPath), handoff.ErrRemovalRoutingGate) {
		t.Fatalf("malformed transaction released token: record=%#v exists=%t err=%v gate=%v", record, exists, err, handoff.RemovalRoutingGate(relayPath))
	}
}

func TestRemovalRoutingRecoveryJournalFreeParkRejectsObservedLocalTarget(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	cfg, err := config.Load(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfigWithCatalogName(
		codexPath,
		"opencodex-relay-local-catalog.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	local, err := cfg.LocalOpenCodexRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(
		codexPath,
		"http://"+local.ListenAddress+"/v1",
		"http://"+local.Responses.Scheduler.InteractiveListenAddress+"/v1",
		local.Catalog.Path,
	); err != nil {
		t.Fatal(err)
	}
	parked := parkRemovalRoutingState(t, store, state)
	if _, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		parked.Generation,
		removalRoutingRecoverySelectionForTest(),
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("observed Local target preparation error=%v", err)
	}
	after, err := store.Load()
	if err != nil || after != parked {
		t.Fatalf("observed Local target mutated routing state: before=%#v after=%#v err=%v", parked, after, err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("observed Local target released cleanup token: %v", err)
	}
}

func TestRemovalRoutingRecoveryPendingStateRetainsTokenWithRoutingTransaction(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parked.AppliedMode = routing.ModeNative
	parked.AppliedBackend = routing.BackendNone
	if err := lock.Replace(parked); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	relayPayload, err := os.ReadFile(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	relayDigest := sha256.Sum256(relayPayload)
	payload, err := json.Marshal(map[string]any{
		"schema":                   routing.SchemaVersion,
		"generation":               parked.Generation - 1,
		"target":                   parked.DesiredMode,
		"origin":                   parked.AppliedMode,
		"target_backend":           parked.DesiredBackend,
		"origin_backend":           parked.AppliedBackend,
		"origin_authoritative":     false,
		"stage":                    "prepared",
		"relay_config_fingerprint": hex.EncodeToString(relayDigest[:]),
		"codex_config_fingerprint": "absent",
		"interactive_fingerprint":  "absent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingTransaction(); err != nil || !pending {
		t.Fatalf("pending transaction=%t err=%v", pending, err)
	}
	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		parked.Generation,
		removalRoutingRecoverySelectionForTest(),
	)
	if err != nil || !preparation.active() || preparation.alreadyRecovered {
		t.Fatalf("preparation=%#v err=%v", preparation, err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("pending transaction cleared removal token: %v", err)
	}
}

func TestRemovalRoutingRecoveryStableStateWithValidTransactionNeedsRecover(t *testing.T) {
	relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
	parked := parkRemovalRoutingState(t, store, state)
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered := parked
	recovered.DesiredMode = routing.ModeNative
	recovered.AppliedMode = routing.ModeNative
	recovered.DesiredBackend = routing.BackendNone
	recovered.AppliedBackend = routing.BackendNone
	recovered.Phase = routing.PhaseNativeActive
	recovered.Generation += 2
	if err := lock.Replace(recovered); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	relayPayload, err := os.ReadFile(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	relayDigest := sha256.Sum256(relayPayload)
	payload, err := json.Marshal(map[string]any{
		"schema":                   routing.SchemaVersion,
		"generation":               recovered.Generation - 1,
		"target":                   routing.ModeNative,
		"origin":                   routing.ModeRelay,
		"target_backend":           routing.BackendNone,
		"origin_backend":           routing.BackendExternal,
		"origin_authoritative":     false,
		"stage":                    "config_mutated",
		"relay_config_fingerprint": hex.EncodeToString(relayDigest[:]),
		"codex_config_fingerprint": "absent",
		"interactive_fingerprint":  "absent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingTransaction(); err != nil || !pending {
		t.Fatalf("pending transaction=%t err=%v", pending, err)
	}
	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		recovered.Generation,
		removalRoutingRecoverySelectionForTest(),
	)
	if err != nil || !preparation.active() || preparation.alreadyRecovered {
		t.Fatalf("preparation=%#v err=%v", preparation, err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("stable transaction cleared removal token: %v", err)
	}
	controller, err := routing.NewController(
		relayPath,
		codexPath,
		routing.WithCodexConfigOwner(codexconfig.ProductionOwner),
		routing.WithControllerRecoveryGate(preparation.recoveryGate(relayPath)),
		routing.WithControllerRemovalRecoveryWitness(preparation.routingWitness),
		routing.WithHealthReader(removalRoutingStoreHealth{store: store}),
		routing.WithTransitionTiming(time.Second, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		true,
		preparation,
		controller.Recover,
		controller.Status,
	)
	if err != nil || result.Phase != routing.PhaseNativeActive ||
		result.DesiredBackend != routing.BackendNone ||
		result.AppliedBackend != routing.BackendNone {
		t.Fatalf("stable final-save recovery result=%#v err=%v", result, err)
	}
	if _, err := os.Lstat(store.TransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stable final-save journal remained: %v", err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); err != nil {
		t.Fatalf("stable final-save cleanup token remained: %v", err)
	}
}

func TestRemovalRoutingRecoveryRejectsUnrelatedStableRoutingJournal(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*removalRoutingTransactionForTest)
	}{
		{
			name: "stale journal generation",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.generation--
			},
		},
		{
			name: "Local target",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.target = routing.ModeRelay
				transaction.targetBackend = routing.BackendLocalOpenCodex
				transaction.origin = routing.ModeNative
				transaction.originBackend = routing.BackendNone
			},
		},
		{
			name: "target mismatch",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.target = routing.ModeRelay
				transaction.targetBackend = routing.BackendExternal
				transaction.origin = routing.ModeNative
				transaction.originBackend = routing.BackendNone
			},
		},
		{
			name: "prepared journal beside stable state",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.stage = "prepared"
			},
		},
		{
			name: "authoritative ordinary transaction",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.originAuthoritative = true
			},
		},
		{
			name: "Local origin",
			mutate: func(transaction *removalRoutingTransactionForTest) {
				transaction.origin = routing.ModeRelay
				transaction.originBackend = routing.BackendLocalOpenCodex
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			relayPath, codexPath, store, stable, transaction := stableRemovalRoutingJournalFixture(t)
			testCase.mutate(&transaction)
			writeRemovalRoutingTransactionForTest(t, store, codexPath, transaction)
			if pending, err := store.HasPendingTransaction(); err != nil || !pending {
				t.Fatalf("mutated journal was not structurally valid: pending=%t err=%v", pending, err)
			}
			if _, err := prepareRemovalRoutingRecovery(
				context.Background(),
				relayPath,
				codexPath,
				routing.RecoveryComplete,
				stable.Generation,
				removalRoutingRecoverySelectionForTest(),
			); !errors.Is(err, routing.ErrRecoveryRequired) {
				t.Fatalf("unrelated journal preparation error=%v", err)
			}
			after, err := store.Load()
			if err != nil || after != stable {
				t.Fatalf("unrelated journal mutated state: before=%#v after=%#v err=%v", stable, after, err)
			}
			if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
				t.Fatalf("unrelated journal released cleanup token: %v", err)
			}
		})
	}
}

func TestRemovalRoutingRecoveryWitnessRejectsJournalSwapBeforeRecover(t *testing.T) {
	relayPath, codexPath, store, stable, transaction := stableRemovalRoutingJournalFixture(t)
	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		relayPath,
		codexPath,
		routing.RecoveryComplete,
		stable.Generation,
		removalRoutingRecoverySelectionForTest(),
	)
	if err != nil || !preparation.active() || preparation.alreadyRecovered {
		t.Fatalf("preparation=%#v err=%v", preparation, err)
	}
	transaction.target = routing.ModeRelay
	transaction.targetBackend = routing.BackendExternal
	transaction.origin = routing.ModeNative
	transaction.originBackend = routing.BackendNone
	writeRemovalRoutingTransactionForTest(t, store, codexPath, transaction)
	if pending, err := store.HasPendingTransaction(); err != nil || !pending {
		t.Fatalf("swapped journal was not structurally valid: pending=%t err=%v", pending, err)
	}
	controller, err := routingControllerWithRecoveryGate(
		relayPath,
		codexPath,
		preparation.recoveryGate(relayPath),
		routing.WithControllerRemovalRecoveryWitness(preparation.routingWitness),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Recover(
		context.Background(),
		routing.RecoveryComplete,
		true,
	); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("swapped journal reached recovery mutation: %v", err)
	}
	after, err := store.Load()
	if err != nil || after != stable {
		t.Fatalf("swapped journal mutated state: before=%#v after=%#v err=%v", stable, after, err)
	}
	if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("swapped journal released cleanup token: %v", err)
	}
}

func TestRemovalRoutingRecoveryStableCommitReprovesHealthAndOwnership(t *testing.T) {
	t.Run("verified package without a proven park keeps the gate", func(t *testing.T) {
		relayPath, codexPath, _, state := removalRoutingRecoveryFixture(t, false)
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			state.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || !preparation.alreadyRecovered {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		foreign := routing.Status{
			Generation:     state.Generation,
			DesiredMode:    routing.ModeRelay,
			AppliedMode:    routing.ModeRelay,
			DesiredBackend: routing.BackendExternal,
			AppliedBackend: routing.BackendExternal,
			Phase:          routing.PhaseRelayActive,
			RelayRunning:   true,
			Connection: routing.Connection{
				LocalRelay:  routing.LocalRelayHealthy,
				RoutingSync: routing.RoutingSyncAcknowledged,
			},
		}
		if _, err := executeRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			true,
			preparation,
			func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
				t.Fatal("stable package record replayed recovery")
				return routing.Status{}, nil
			},
			func(context.Context) routing.Status { return foreign },
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("foreign ownership error=%v", err)
		}
		record, exists, err := handoff.ReadRemovalCleanup(relayPath)
		if err != nil || !exists || record.RoutingRecoveryReleased ||
			!errors.Is(handoff.RemovalRoutingGate(relayPath), handoff.ErrRemovalRoutingGate) {
			t.Fatalf("foreign ownership released record=%#v exists=%t err=%v", record, exists, err)
		}
	})

	t.Run("unhealthy stable commit keeps exact token", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		lock, err := store.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		recovered := parked
		recovered.DesiredMode = routing.ModeNative
		recovered.AppliedMode = routing.ModeNative
		recovered.DesiredBackend = routing.BackendNone
		recovered.AppliedBackend = routing.BackendNone
		recovered.Phase = routing.PhaseNativeActive
		recovered.Generation++
		if err := lock.Replace(recovered); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			recovered.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || !preparation.alreadyRecovered {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		unhealthy := routing.Status{
			Generation:     recovered.Generation,
			DesiredMode:    routing.ModeNative,
			AppliedMode:    routing.ModeNative,
			DesiredBackend: routing.BackendNone,
			AppliedBackend: routing.BackendNone,
			Phase:          routing.PhaseNativeActive,
			Connection: routing.Connection{
				LocalRelay:  routing.LocalRelayUnreachable,
				RoutingSync: routing.RoutingSyncUnreachable,
			},
		}
		if _, err := executeRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			true,
			preparation,
			func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
				t.Fatal("already-recovered state replayed recovery")
				return routing.Status{}, nil
			},
			func(context.Context) routing.Status { return unhealthy },
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("unhealthy recovery error=%v", err)
		}
		if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("unhealthy status released gate: %v", err)
		}
	})

	t.Run("already-recovered invalid sync keeps exact token", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		lock, err := store.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		recovered := parked
		recovered.DesiredMode = routing.ModeNative
		recovered.AppliedMode = routing.ModeNative
		recovered.DesiredBackend = routing.BackendNone
		recovered.AppliedBackend = routing.BackendNone
		recovered.Phase = routing.PhaseNativeActive
		recovered.Generation++
		if err := lock.Replace(recovered); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			recovered.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || !preparation.alreadyRecovered {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		invalidSync := routing.Status{
			Generation:     recovered.Generation,
			DesiredMode:    recovered.DesiredMode,
			AppliedMode:    recovered.AppliedMode,
			DesiredBackend: recovered.DesiredBackend,
			AppliedBackend: recovered.AppliedBackend,
			Phase:          recovered.Phase,
			RelayRunning:   true,
			Connection: routing.Connection{
				LocalRelay:  routing.LocalRelayHealthy,
				RoutingSync: routing.RoutingSyncInvalid,
			},
		}
		if _, err := executeRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			true,
			preparation,
			func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
				t.Fatal("already-recovered state replayed recovery")
				return routing.Status{}, nil
			},
			func(context.Context) routing.Status { return invalidSync },
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("invalid-sync recovery error=%v", err)
		}
		if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("invalid-sync status released gate: %v", err)
		}
	})

	t.Run("post-Recover invalid sync keeps exact token", func(t *testing.T) {
		relayPath, codexPath, store, state := removalRoutingRecoveryFixture(t, true)
		parked := parkRemovalRoutingState(t, store, state)
		preparation, err := prepareRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			parked.Generation,
			removalRoutingRecoverySelectionForTest(),
		)
		if err != nil || preparation.alreadyRecovered {
			t.Fatalf("preparation=%#v err=%v", preparation, err)
		}
		recovered := parked
		recovered.DesiredMode = routing.ModeNative
		recovered.AppliedMode = routing.ModeNative
		recovered.DesiredBackend = routing.BackendNone
		recovered.AppliedBackend = routing.BackendNone
		recovered.Phase = routing.PhaseNativeActive
		recovered.Generation += 2
		invalidSync := routing.Status{
			Generation:     recovered.Generation,
			DesiredMode:    recovered.DesiredMode,
			AppliedMode:    recovered.AppliedMode,
			DesiredBackend: recovered.DesiredBackend,
			AppliedBackend: recovered.AppliedBackend,
			Phase:          recovered.Phase,
			RelayRunning:   true,
			Connection: routing.Connection{
				LocalRelay:  routing.LocalRelayHealthy,
				RoutingSync: routing.RoutingSyncInvalid,
			},
		}
		if _, err := executeRemovalRoutingRecovery(
			context.Background(),
			relayPath,
			codexPath,
			routing.RecoveryComplete,
			true,
			preparation,
			func(context.Context, routing.RecoveryAction, bool) (routing.Status, error) {
				lock, err := store.Lock(context.Background())
				if err != nil {
					return routing.Status{}, err
				}
				if err := lock.Replace(recovered); err != nil {
					_ = lock.Close()
					return routing.Status{}, err
				}
				if err := preparation.routingWitness.RebindStable(lock); err != nil {
					_ = lock.Close()
					return routing.Status{}, err
				}
				if err := lock.Close(); err != nil {
					return routing.Status{}, err
				}
				return routing.Status{Generation: recovered.Generation}, nil
			},
			func(context.Context) routing.Status { return invalidSync },
		); !errors.Is(err, routing.ErrRecoveryRequired) {
			t.Fatalf("post-Recover invalid-sync error=%v", err)
		}
		if err := handoff.RemovalRoutingGate(relayPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
			t.Fatalf("post-Recover invalid-sync released gate: %v", err)
		}
	})
}

func TestHandoffPreflightBlocksEveryRemovalRoutingGatePhase(t *testing.T) {
	if err := preflightHandoffRemovalGate(filepath.Join(t.TempDir(), "relay.json")); err != nil {
		t.Fatalf("handoff without removal journal error=%v", err)
	}
	for _, test := range []struct {
		name    string
		mutate  func(*handoff.RemovalCleanupRecord)
		blocked bool
	}{
		{name: "ordinary pending", blocked: true},
		{name: "in flight", blocked: true, mutate: func(record *handoff.RemovalCleanupRecord) {
			record.Phase = handoff.RemovalCleanupPhasePackageInFlight
			record.PackageAttempt = 1
			record.ExecutionAttempt = 1
			record.ExecutionBootSession = strings.Repeat("b", 64)
			record.ActiveExecution = &handoff.RemovalActiveExecution{
				Kind:         handoff.RemovalExecutionPackage,
				Attempt:      1,
				BootSession:  strings.Repeat("b", 64),
				BootAttested: true,
			}
		}},
		{name: "reboot reconciled pending", blocked: true, mutate: func(record *handoff.RemovalCleanupRecord) {
			record.PackageAttempt = 1
			record.ExecutionBootSession = strings.Repeat("b", 64)
			record.ProcessReconciledAfterReboot = true
		}},
		{name: "verified", blocked: true, mutate: func(record *handoff.RemovalCleanupRecord) {
			record.Phase = handoff.RemovalCleanupPhasePackageVerified
			record.PackageAttempt = 1
			record.ExecutionBootSession = strings.Repeat("b", 64)
		}},
		{name: "released verified", blocked: true, mutate: func(record *handoff.RemovalCleanupRecord) {
			record.Phase = handoff.RemovalCleanupPhasePackageVerified
			record.PackageAttempt = 1
			record.ExecutionBootSession = strings.Repeat("b", 64)
			record.RoutingRecoveryReleased = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			record := removalCleanupRecordForTest(t, configPath)
			if test.mutate != nil {
				test.mutate(&record)
			}
			if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
				t.Fatal(err)
			}
			err := preflightHandoffRemovalGate(configPath)
			if test.blocked && err == nil {
				t.Fatal("gated removal phase reached handoff")
			}
			if !test.blocked && err != nil {
				t.Fatalf("ungated removal phase error=%v", err)
			}
		})
	}
}
