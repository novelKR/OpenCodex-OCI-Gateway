package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypedRemovalExecutionLifecycleAndAdmission(t *testing.T) {
	boot := useAttestedRemovalBootSession(t)
	for _, kind := range []RemovalExecutionKind{
		RemovalExecutionTeardown,
		RemovalExecutionTrash,
		RemovalExecutionPackage,
	} {
		t.Run(string(kind), func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			mode := RemovalModePreserveData
			if kind == RemovalExecutionTrash {
				mode = RemovalModeTrashSelected
			}
			request := testRemovalRequest(mode)
			intent, err := NewRemovalIntentRecord(candidate, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, intent); err != nil {
				t.Fatal(err)
			}
			if kind == RemovalExecutionPackage {
				pending, err := NewRemovalCleanupRecord(candidate, request, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteRemovalCleanup(configPath, pending); err != nil {
					t.Fatal(err)
				}
			}
			active, err := BeginExecution(configPath, kind)
			if err != nil {
				t.Fatal(err)
			}
			if active.ActiveExecution == nil || active.ActiveExecution.Kind != kind ||
				active.ActiveExecution.Attempt != active.ExecutionAttempt ||
				active.ActiveExecution.BootSession != boot || !active.ActiveExecution.BootAttested {
				t.Fatalf("active=%#v", active)
			}
			if err := RemovalExecutionAdmission(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
				t.Fatalf("active admission=%v", err)
			}
			cleared, err := FinishExecution(configPath, kind, RemovalExecutionResult{})
			if err != nil {
				t.Fatal(err)
			}
			if cleared.ActiveExecution != nil {
				t.Fatalf("not-started execution remained active: %#v", cleared)
			}
			if kind == RemovalExecutionPackage && cleared.Phase != RemovalCleanupPhasePackagePending {
				t.Fatalf("package not-started phase=%s", cleared.Phase)
			}
		})
	}
}

func TestTypedRemovalExecutionRejectsUnattestedBoot(t *testing.T) {
	previous := removalBootSessionProvider
	t.Cleanup(func() { removalBootSessionProvider = previous })
	removalBootSessionProvider = func() (string, bool, error) {
		return strings.Repeat("a", 64), false, nil
	}
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginExecution(configPath, RemovalExecutionTeardown); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("unattested begin error=%v", err)
	}
}

func TestTypedRemovalExecutionRejectsForgedUnattestedActiveRecord(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.ExecutionAttempt = 1
	record.ActiveExecution = &RemovalActiveExecution{
		Kind:        RemovalExecutionTeardown,
		Attempt:     1,
		BootSession: strings.Repeat("a", 64),
	}
	if err := WriteRemovalCleanup(configPath, record); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("forged active record error=%v", err)
	}
}

func TestTypedRemovalExecutionChangedBootReconcilesTrashToRefresh(t *testing.T) {
	boot := useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModeTrashSelected)
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginExecution(configPath, RemovalExecutionTrash); err != nil {
		t.Fatal(err)
	}
	removalBootSessionProvider = func() (string, bool, error) {
		return strings.Repeat("b", 64), true, nil
	}
	reconciled, _, err := ReconcileActiveExecutionAfterBoot(configPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.ActiveExecution != nil || reconciled.Phase != RemovalCleanupPhaseDataRefresh ||
		reconciled.DataOutcome != removalDataOutcomeUnknown ||
		len(reconciled.RetiredItemIDs) == 0 ||
		!selectionContainedBy(reconciled.SelectedItemIDs, map[string]struct{}{
			reconciled.RetiredItemIDs[0]: {},
		}) {
		t.Fatalf("reconciled=%#v boot=%s", reconciled, boot)
	}
}

func TestTypedRemovalExecutionChangedBootReconcilesPackageAbsence(t *testing.T) {
	previous := removalBootSessionProvider
	t.Cleanup(func() { removalBootSessionProvider = previous })
	firstBoot := strings.Repeat("a", 64)
	secondBoot := strings.Repeat("b", 64)
	removalBootSessionProvider = func() (string, bool, error) {
		return firstBoot, true, nil
	}
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := NewRemovalCleanupRecord(candidate, request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginExecution(configPath, RemovalExecutionPackage); err != nil {
		t.Fatal(err)
	}
	removalBootSessionProvider = func() (string, bool, error) {
		return secondBoot, true, nil
	}
	reconciled, absent, err := ReconcileActiveExecutionAfterBoot(configPath, true)
	if err != nil || !absent || reconciled.Phase != RemovalCleanupPhasePackageVerified ||
		reconciled.ActiveExecution != nil {
		t.Fatalf("reconciled=%#v absent=%t err=%v", reconciled, absent, err)
	}
}

func TestV4PackageInFlightMigratesWithoutReplayAuthority(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalCleanupRecord(candidate, request, 0)
	if err != nil {
		t.Fatal(err)
	}
	record.SchemaVersion = legacyRemovalCleanupVersion
	record.Phase = RemovalCleanupPhasePackageInFlight
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("c", 64)
	recordJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RemovalCleanupPath(configPath), append(recordJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists {
		t.Fatalf("migration read=%#v exists=%t err=%v", migrated, exists, err)
	}
	if migrated.SchemaVersion != removalCleanupSchemaVersion || migrated.ActiveExecution == nil ||
		migrated.ActiveExecution.Kind != RemovalExecutionPackage || migrated.ActiveExecution.BootAttested ||
		!migrated.ActiveExecution.LegacyUnattestedBoot {
		t.Fatalf("migrated=%#v", migrated)
	}
	var onDisk struct {
		SchemaVersion int `json:"schema_version"`
	}
	diskPayload, err := os.ReadFile(RemovalCleanupPath(configPath))
	if err != nil || json.Unmarshal(diskPayload, &onDisk) != nil || onDisk.SchemaVersion != legacyRemovalCleanupVersion {
		t.Fatalf("read-only v4 migration changed disk schema: schema=%d err=%v", onDisk.SchemaVersion, err)
	}
	if err := RemovalExecutionAdmission(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("migrated active admission=%v", err)
	}
}

func TestFinalizationGateReleaseRequiresVerifiedPackageAbsence(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	record, err := NewRemovalCleanupRecord(candidate, testRemovalRequest(RemovalModePreserveData), 0)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = RemovalCleanupPhasePackageVerified
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	if err := RemovalFinalizationAdmission(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("unreleased finalization admission=%v", err)
	}
	released, err := ReleaseRemovalRoutingGateForFinalization(configPath)
	if err != nil || !released.RoutingRecoveryReleased || !released.FinalizationActive {
		t.Fatalf("released=%#v err=%v", released, err)
	}
	if err := RemovalFinalizationAdmission(configPath); err != nil {
		t.Fatalf("released finalization admission=%v", err)
	}
	if err := RemovalRoutingGate(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("terminal finalization gate=%v", err)
	}
}

func TestFinalizationGateReleaseRejectsResidualPackage(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	if err := os.MkdirAll(candidate.PackageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := NewRemovalCleanupRecord(candidate, testRemovalRequest(RemovalModePreserveData), 0)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = RemovalCleanupPhasePackageVerified
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseRemovalRoutingGateForFinalization(configPath); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("residual package release error=%v", err)
	}
	after, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || after.RoutingRecoveryReleased || after.FinalizationActive {
		t.Fatalf("after=%#v exists=%t err=%v", after, exists, err)
	}
}

func TestRemovalCoordinatorUsesTypedHooksBeforeAndAfterEachChild(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	var events []string
	coordinator := RemovalCoordinator{
		Resolver:      resolver,
		Runner:        runner,
		VerifyRouting: func(context.Context) error { return nil },
		PrepareOperation: func(context.Context, NPMInstallation, OpenCodexRemovalRequest) error {
			return nil
		},
		RecordDataOutcome:     noopRecordDataOutcome,
		PreparePackageRemoval: noopPreparePackageRemoval,
		BeginExecution: func(_ context.Context, kind RemovalExecutionKind) error {
			events = append(events, "begin:"+string(kind))
			return nil
		},
		FinishExecution: func(_ context.Context, kind RemovalExecutionKind, _ RemovalExecutionResult) error {
			events = append(events, "finish:"+string(kind))
			return nil
		},
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	wantEvents := []string{
		"begin:teardown", "finish:teardown",
		"begin:package", "finish:package",
	}
	if receipt.Status != RemovalStatusCompleted || receipt.RoutingRecoveryRequired ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!sameOrderedStrings(events, wantEvents) ||
		!sameOrderedStrings(runner.calls, []string{"teardown", "uninstall"}) {
		t.Fatalf("receipt=%#v events=%#v calls=%#v", receipt, events, runner.calls)
	}
}

func TestRemovalCoordinatorTypedHooksCommitDurableJournal(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	coordinator := RemovalCoordinator{
		Resolver:      &fakeRemovalResolver{candidate: candidate},
		Runner:        runner,
		VerifyRouting: func(context.Context) error { return nil },
		CheckAdmission: func(context.Context) error {
			return RemovalExecutionAdmission(configPath)
		},
		PrepareOperation: func(_ context.Context, c NPMInstallation, r OpenCodexRemovalRequest) error {
			_, err := EnsureRemovalIntent(configPath, c, r)
			return err
		},
		RecordDataOutcome: func(_ context.Context, c NPMInstallation, r OpenCodexRemovalRequest, moved int, status string) error {
			_, err := RecordRemovalDataOutcome(configPath, c, r, moved, status)
			return err
		},
		PreparePackageRemoval: func(_ context.Context, c NPMInstallation, r OpenCodexRemovalRequest, moved int) error {
			_, err := PrepareRemovalPackageCleanup(configPath, c, r, moved)
			return err
		},
		BeginExecution: func(_ context.Context, kind RemovalExecutionKind) error {
			_, err := BeginExecution(configPath, kind)
			return err
		},
		FinishExecution: func(_ context.Context, kind RemovalExecutionKind, result RemovalExecutionResult) error {
			_, err := FinishExecution(configPath, kind, result)
			return err
		},
	}
	_, directErr := NewRemovalIntentRecord(candidate, testRemovalRequest(RemovalModePreserveData))
	if directErr != nil {
		t.Fatalf("new intent=%v candidate=%#v", directErr, candidate)
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	record, exists, err := ReadRemovalCleanup(configPath)
	if receipt.Status != RemovalStatusCompleted || receipt.RoutingRecoveryRequired ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!exists || err != nil ||
		record.ActiveExecution != nil || record.Phase != RemovalCleanupPhasePackageVerified {
		t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
	}
}

func resolutionTestCoordinator(
	configPath string,
	resolver *fakeRemovalResolver,
	runner *fakeOpenCodexRemovalRunner,
	verifyRouting func(context.Context) error,
	events *[]string,
	parkErr error,
) RemovalCoordinator {
	return RemovalCoordinator{
		Resolver:      resolver,
		Runner:        runner,
		VerifyRouting: verifyRouting,
		CheckAdmission: func(context.Context) error {
			return RemovalExecutionAdmission(configPath)
		},
		CheckResumeAdmission: func(context.Context) error {
			return RemovalPackageResumeAdmission(configPath)
		},
		MarkRoutingRecovery: func() error {
			return parkErr
		},
		PrepareOperation: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest) error {
			_, err := EnsureRemovalIntent(configPath, candidate, request)
			return err
		},
		RecordDataOutcome: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int, status string) error {
			_, err := RecordRemovalDataOutcome(configPath, candidate, request, moved, status)
			return err
		},
		MarkDataRefresh: func(context.Context) error {
			_, err := MarkRemovalDataRefreshRequired(configPath)
			return err
		},
		PreparePackageRemoval: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int) error {
			_, err := PrepareRemovalPackageCleanup(configPath, candidate, request, moved)
			return err
		},
		BeginExecution: func(_ context.Context, kind RemovalExecutionKind) error {
			_, err := BeginExecution(configPath, kind)
			return err
		},
		FinishExecution: func(_ context.Context, kind RemovalExecutionKind, result RemovalExecutionResult) error {
			_, err := FinishExecution(configPath, kind, result)
			return err
		},
		ResolveExecution: func(_ context.Context, kind RemovalExecutionKind, resolution RemovalExecutionResolution, parkRouting bool) (bool, error) {
			if _, err := MarkRemovalExecutionResolution(configPath, kind, resolution, parkRouting); err != nil {
				return false, err
			}
			if events != nil {
				*events = append(*events, "mark")
			}
			var park func() error
			routingParked := false
			if parkRouting {
				park = func() error {
					if events != nil {
						*events = append(*events, "park")
					}
					if parkErr != nil {
						return parkErr
					}
					routingParked = true
					return nil
				}
			}
			_, _, err := ResumeRemovalExecutionResolution(configPath, park)
			if err == nil && events != nil {
				*events = append(*events, "resolve")
			}
			return routingParked, err
		},
	}
}

func TestRemovalCoordinatorResolvesPreStartRoutingChangeWithoutActiveWitness(t *testing.T) {
	useAttestedRemovalBootSession(t)
	for _, testCase := range []struct {
		name                  string
		mode                  OpenCodexRemovalMode
		runner                *fakeOpenCodexRemovalRunner
		expectedCalls         []string
		expectedPhase         string
		operationRetryPending bool
		packageRetryPending   bool
	}{
		{
			name: "teardown", mode: RemovalModePreserveData,
			runner:                &fakeOpenCodexRemovalRunner{teardownErr: ErrRemovalRoutingChanged},
			expectedCalls:         []string{"teardown"},
			expectedPhase:         RemovalCleanupPhaseIntent,
			operationRetryPending: true,
		},
		{
			name: "trash", mode: RemovalModeTrashSelected,
			runner: &fakeOpenCodexRemovalRunner{
				teardown: teardownResult("completed"),
				trashErr: ErrRemovalRoutingChanged,
			},
			expectedCalls:         []string{"teardown", "trash"},
			expectedPhase:         RemovalCleanupPhaseIntent,
			operationRetryPending: true,
		},
		{
			name: "package", mode: RemovalModePreserveData,
			runner: &fakeOpenCodexRemovalRunner{
				teardown:     teardownResult("completed"),
				uninstallErr: ErrRemovalRoutingChanged,
			},
			expectedCalls:       []string{"teardown", "uninstall"},
			expectedPhase:       RemovalCleanupPhasePackagePending,
			packageRetryPending: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			events := []string{}
			coordinator := resolutionTestCoordinator(
				configPath,
				&fakeRemovalResolver{candidate: candidate},
				testCase.runner,
				func(context.Context) error { return nil },
				&events,
				nil,
			)
			receipt := coordinator.Remove(context.Background(), testRemovalRequest(testCase.mode))
			record, exists, err := ReadRemovalCleanup(configPath)
			if err != nil || !exists {
				t.Fatalf("record exists=%t err=%v", exists, err)
			}
			if record.ActiveExecution != nil || record.ExecutionResolution != "" ||
				!record.RecoveryPending || record.Phase != testCase.expectedPhase ||
				record.OperationRetryPending != testCase.operationRetryPending ||
				record.PackageRetryPending != testCase.packageRetryPending {
				t.Fatalf("resolved record=%#v", record)
			}
			if !receipt.RoutingRecoveryRequired ||
				!hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
				!sameOrderedStrings(events, []string{"mark", "park", "resolve"}) ||
				!sameOrderedStrings(testCase.runner.calls, testCase.expectedCalls) {
				t.Fatalf("receipt=%#v events=%#v calls=%#v", receipt, events, testCase.runner.calls)
			}
			if err := RemovalRoutingGate(configPath); !errors.Is(err, ErrRemovalRoutingGate) ||
				!RemovalRoutingGateReleasable(configPath) {
				t.Fatalf("pending recovery gate=%v releasable=%t", err, RemovalRoutingGateReleasable(configPath))
			}
			released, err := ReleaseRemovalRoutingGateForRecovery(configPath)
			if err != nil || released.RecoveryPending || RemovalRoutingGate(configPath) != nil {
				t.Fatalf("released=%#v gate=%v err=%v", released, RemovalRoutingGate(configPath), err)
			}
			if testCase.packageRetryPending {
				if err := RemovalPackageResumeAdmission(configPath); err != nil {
					t.Fatalf("package retry admission=%v", err)
				}
			} else if testCase.operationRetryPending {
				testCase.runner.teardown = teardownResult("completed")
				testCase.runner.teardownErr = nil
				testCase.runner.trash = trashResult("completed", []string{testDataItemID})
				testCase.runner.trashErr = nil
				testCase.runner.uninstall = RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0}
				testCase.runner.uninstallErr = nil
				retryReceipt := coordinator.Remove(context.Background(), testRemovalRequest(testCase.mode))
				retried, exists, err := ReadRemovalCleanup(configPath)
				if err != nil || !exists || retryReceipt.Status != RemovalStatusCompleted ||
					!retryReceipt.PackageRemoved || retried.OperationRetryPending ||
					retried.Phase != RemovalCleanupPhasePackageVerified {
					t.Fatalf("retry receipt=%#v record=%#v exists=%t err=%v", retryReceipt, retried, exists, err)
				}
			}
		})
	}
}

func TestRemovalCoordinatorClearsDefinitePreStartTeardownRefusalWithoutRecovery(t *testing.T) {
	useAttestedRemovalBootSession(t)
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{name: "candidate changed", err: ErrRemovalCandidateChanged, code: "candidate_changed"},
		{name: "manual only", err: ErrRemovalManualOnly, code: "manual_removal_required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			events := []string{}
			runner := &fakeOpenCodexRemovalRunner{teardownErr: testCase.err}
			coordinator := resolutionTestCoordinator(
				configPath,
				&fakeRemovalResolver{candidate: candidate},
				runner,
				func(context.Context) error { return nil },
				&events,
				nil,
			)

			receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
			record, exists, err := ReadRemovalCleanup(configPath)
			if err != nil || !exists {
				t.Fatalf("record exists=%t err=%v", exists, err)
			}
			if record.Phase != RemovalCleanupPhaseIntent ||
				record.ActiveExecution != nil ||
				record.ExecutionResolution != "" ||
				record.RecoveryPending ||
				record.OperationRetryPending {
				t.Fatalf("record=%#v", record)
			}
			if err := RemovalRoutingGate(configPath); err != nil {
				t.Fatalf("routing gate=%v", err)
			}
			if !sameOrderedStrings(runner.calls, []string{"teardown"}) || len(events) != 0 {
				t.Fatalf("calls=%#v events=%#v", runner.calls, events)
			}
			foundRefusal := false
			for _, stage := range receipt.Stages {
				if stage.Stage == "teardown" && stage.Status == RemovalStageRefused && stage.Code == testCase.code {
					foundRefusal = true
				}
			}
			if !foundRefusal ||
				receipt.RoutingRecoveryRequired ||
				hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
				hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persist_failed") ||
				hasRemovalStage(receipt.Stages, "teardown", "process_cleanup_unverified") {
				t.Fatalf("receipt=%#v", receipt)
			}
		})
	}
}

func TestRemovalCoordinatorResolvesMalformedReceiptsByLifecycleProof(t *testing.T) {
	useAttestedRemovalBootSession(t)
	t.Run("teardown parks and records operation retry", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "relay.json")
		candidate := removalCleanupCandidate(t)
		events := []string{}
		runner := &fakeOpenCodexRemovalRunner{
			teardown: RemovalExecutionResult{
				Output:  []byte(`{"schemaVersion":1,"operation":"teardown","unknown":true}`),
				Started: true, CleanupVerified: true,
			},
		}
		coordinator := resolutionTestCoordinator(
			configPath, &fakeRemovalResolver{candidate: candidate}, runner,
			func(context.Context) error { return nil }, &events, nil,
		)
		receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
		record, exists, err := ReadRemovalCleanup(configPath)
		if err != nil || !exists || record.ActiveExecution != nil ||
			record.ExecutionResolution != "" || !record.RecoveryPending ||
			!record.OperationRetryPending || record.Phase != RemovalCleanupPhaseIntent {
			t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
		}
		if !receipt.RoutingRecoveryRequired ||
			!hasRemovalStage(receipt.Stages, "teardown", "teardown_result_invalid") ||
			!sameOrderedStrings(events, []string{"mark", "park", "resolve"}) {
			t.Fatalf("receipt=%#v events=%#v", receipt, events)
		}
	})

	t.Run("trash retires selection without stable-route park", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "relay.json")
		candidate := removalCleanupCandidate(t)
		events := []string{}
		runner := &fakeOpenCodexRemovalRunner{
			teardown: teardownResult("completed"),
			trash: RemovalExecutionResult{
				Output:  []byte(`{"schemaVersion":1,"operation":"data-trash","unknown":true}`),
				Started: true, CleanupVerified: true,
			},
		}
		coordinator := resolutionTestCoordinator(
			configPath, &fakeRemovalResolver{candidate: candidate}, runner,
			func(context.Context) error { return nil }, &events, nil,
		)
		receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
		record, exists, err := ReadRemovalCleanup(configPath)
		if err != nil || !exists || record.ActiveExecution != nil ||
			record.ExecutionResolution != "" || record.RecoveryPending ||
			record.Phase != RemovalCleanupPhaseDataRefresh ||
			record.DataOutcome != removalDataOutcomeUnknown ||
			len(record.RetiredItemIDs) != 1 {
			t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
		}
		if !receipt.DataMovementUnknown || receipt.RoutingRecoveryRequired ||
			!hasRemovalStage(receipt.Stages, "data_trash", "trash_receipt_invalid") ||
			!sameOrderedStrings(events, []string{"mark", "resolve"}) {
			t.Fatalf("receipt=%#v events=%#v", receipt, events)
		}
	})
}

func TestRemovalExecutionResolutionParkFailureRemainsCrashResumable(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	parkErr := errors.New("routing park failed")
	events := []string{}
	runner := &fakeOpenCodexRemovalRunner{teardownErr: ErrRemovalRoutingChanged}
	coordinator := resolutionTestCoordinator(
		configPath, &fakeRemovalResolver{candidate: candidate}, runner,
		func(context.Context) error { return nil }, &events, parkErr,
	)
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	record, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || record.ActiveExecution == nil ||
		record.ExecutionResolution != RemovalExecutionResolutionPreStartRoutingChanged ||
		!record.ResolutionRequiresRoutingRecovery || record.RecoveryPending {
		t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
	}
	if !receipt.RoutingRecoveryRequired ||
		!hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persist_failed") ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!sameOrderedStrings(events, []string{"mark", "park"}) {
		t.Fatalf("receipt=%#v events=%#v", receipt, events)
	}
	pending, found, err := PendingRemovalExecutionResolution(record)
	if err != nil || !found || !pending.RequiresRoutingRecovery ||
		pending.Kind != RemovalExecutionTeardown {
		t.Fatalf("pending=%#v found=%t err=%v", pending, found, err)
	}
	resolved, didResolve, err := ResumeRemovalExecutionResolution(configPath, func() error { return nil })
	if err != nil || !didResolve || resolved.ActiveExecution != nil ||
		!resolved.RecoveryPending || !resolved.OperationRetryPending {
		t.Fatalf("resolved=%#v didResolve=%t err=%v", resolved, didResolve, err)
	}
	reconstructed, err := ResolvedRemovalExecutionReceipt(record, resolved, true)
	if err != nil || !reconstructed.RoutingRecoveryRequired ||
		!hasRemovalStage(reconstructed.Stages, "teardown", "routing_ownership_changed") ||
		!hasRemovalStage(reconstructed.Stages, "routing_recovery", "routing_recovery_persisted") ||
		hasRemovalStage(reconstructed.Stages, "teardown", "process_cleanup_unverified") {
		t.Fatalf("reconstructed=%#v err=%v", reconstructed, err)
	}
}

func TestResolvedPreStartRoutingReceiptKeepsRefusedStageContract(t *testing.T) {
	useAttestedRemovalBootSession(t)
	for _, testCase := range []struct {
		name  string
		kind  RemovalExecutionKind
		mode  OpenCodexRemovalMode
		stage string
	}{
		{name: "teardown", kind: RemovalExecutionTeardown, mode: RemovalModePreserveData, stage: "teardown"},
		{name: "trash", kind: RemovalExecutionTrash, mode: RemovalModeTrashSelected, stage: "data_trash"},
		{name: "package", kind: RemovalExecutionPackage, mode: RemovalModePreserveData, stage: "npm_uninstall"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			request := testRemovalRequest(testCase.mode)
			record, err := NewRemovalIntentRecord(candidate, request)
			if testCase.kind == RemovalExecutionPackage {
				record, err = NewRemovalCleanupRecord(candidate, request, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, record); err != nil {
				t.Fatal(err)
			}
			if _, err := BeginExecution(configPath, testCase.kind); err != nil {
				t.Fatal(err)
			}
			before, err := MarkRemovalExecutionResolution(
				configPath,
				testCase.kind,
				RemovalExecutionResolutionPreStartRoutingChanged,
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			after, resolved, err := ResumeRemovalExecutionResolution(configPath, func() error { return nil })
			if err != nil || !resolved {
				t.Fatalf("after=%#v resolved=%t err=%v", after, resolved, err)
			}
			receipt, err := ResolvedRemovalExecutionReceipt(before, after, true)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, stage := range receipt.Stages {
				if stage.Stage == testCase.stage && stage.Code == "routing_ownership_changed" {
					found = true
					if stage.Status != RemovalStageRefused {
						t.Fatalf("stage=%#v", stage)
					}
				}
			}
			if !found {
				t.Fatalf("receipt=%#v", receipt)
			}
		})
	}
}

func TestResolvedTrashReceiptDefersRefreshOnlyForRoutingRecovery(t *testing.T) {
	useAttestedRemovalBootSession(t)
	for _, testCase := range []struct {
		name                    string
		requiresRoutingRecovery bool
	}{
		{name: "stable", requiresRoutingRecovery: false},
		{name: "parked", requiresRoutingRecovery: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			request := testRemovalRequest(RemovalModeTrashSelected)
			intent, err := NewRemovalIntentRecord(candidate, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, intent); err != nil {
				t.Fatal(err)
			}
			if _, err := BeginExecution(configPath, RemovalExecutionTrash); err != nil {
				t.Fatal(err)
			}
			before, err := MarkRemovalExecutionResolution(
				configPath,
				RemovalExecutionTrash,
				RemovalExecutionResolutionTrashReceiptInvalid,
				testCase.requiresRoutingRecovery,
			)
			if err != nil {
				t.Fatal(err)
			}
			var park func() error
			if testCase.requiresRoutingRecovery {
				park = func() error { return nil }
			}
			after, didResolve, err := ResumeRemovalExecutionResolution(configPath, park)
			if err != nil || !didResolve {
				t.Fatalf("after=%#v didResolve=%t err=%v", after, didResolve, err)
			}
			receipt, err := ResolvedRemovalExecutionReceipt(before, after, testCase.requiresRoutingRecovery)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.requiresRoutingRecovery {
				if !receipt.RoutingRecoveryRequired || receipt.DataMovementUnknown ||
					hasRemovalStage(receipt.Stages, "data_trash", "data_selection_refresh_required") ||
					!hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") {
					t.Fatalf("parked receipt=%#v", receipt)
				}
				if _, err := InterruptedRemovalDataRefreshReceipt(after); !errors.Is(err, ErrRemovalCleanupUnsafe) {
					t.Fatalf("pending refresh receipt error=%v", err)
				}
				released, err := ReleaseRemovalRoutingGateForRecovery(configPath)
				if err != nil {
					t.Fatal(err)
				}
				refreshReceipt, err := InterruptedRemovalDataRefreshReceipt(released)
				if err != nil || !refreshReceipt.DataMovementUnknown ||
					!hasRemovalStage(refreshReceipt.Stages, "data_trash", "data_selection_refresh_required") {
					t.Fatalf("released refresh receipt=%#v err=%v", refreshReceipt, err)
				}
			} else if receipt.RoutingRecoveryRequired || !receipt.DataMovementUnknown ||
				!hasRemovalStage(receipt.Stages, "data_trash", "data_selection_refresh_required") {
				t.Fatalf("stable receipt=%#v", receipt)
			}
		})
	}
}

func TestRemovalResolutionReceiptKeepsMarkerUnresolvedWhenJournalClearFails(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{teardownErr: ErrRemovalRoutingChanged}
	coordinator := RemovalCoordinator{
		Resolver:              resolver,
		Runner:                runner,
		VerifyRouting:         func(context.Context) error { return nil },
		PrepareOperation:      noopPrepareOperation,
		RecordDataOutcome:     noopRecordDataOutcome,
		PreparePackageRemoval: noopPreparePackageRemoval,
		BeginExecution:        noopBeginExecution,
		FinishExecution:       noopFinishExecution,
		ResolveExecution: func(
			context.Context,
			RemovalExecutionKind,
			RemovalExecutionResolution,
			bool,
		) (bool, error) {
			return true, errors.New("cleanup journal resolution failed")
		},
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if !receipt.RoutingRecoveryRequired ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persist_failed") ||
		!hasRemovalStage(receipt.Stages, "teardown", "process_cleanup_unverified") ||
		!hasRemovalStage(receipt.Stages, "cleanup_journal", "teardown_execution_result_unavailable") {
		t.Fatalf("receipt=%#v", receipt)
	}
}

func TestRemovalCoordinatorNormalizesUnsupportedTrashToRefresh(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	runner := &fakeOpenCodexRemovalRunner{
		teardown: teardownResult("completed"),
		trash:    trashResult("unsupported", nil),
	}
	coordinator := resolutionTestCoordinator(
		configPath, &fakeRemovalResolver{candidate: candidate}, runner,
		func(context.Context) error { return nil }, nil, nil,
	)
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
	record, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.Phase != RemovalCleanupPhaseDataRefresh || record.DataOutcome != "unsupported" ||
		len(record.RetiredItemIDs) != 1 {
		t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
	}
	if receipt.Status != RemovalStatusPartial || !receipt.DataMovementUnknown ||
		receipt.RoutingRecoveryRequired ||
		!hasRemovalStage(receipt.Stages, "data_trash", "data_selection_refresh_required") ||
		hasRemovalStage(receipt.Stages, "data_trash", "trash_unsupported") ||
		!sameOrderedStrings(runner.calls, []string{"teardown", "trash"}) {
		t.Fatalf("receipt=%#v calls=%#v", receipt, runner.calls)
	}
}

func TestUnsupportedTrashDefersRefreshReceiptUntilRoutingRecovery(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	runner := &fakeOpenCodexRemovalRunner{
		teardown: teardownResult("completed"),
		trash:    trashResult("unsupported", nil),
	}
	verifyCalls := 0
	coordinator := resolutionTestCoordinator(
		configPath, &fakeRemovalResolver{candidate: candidate}, runner,
		func(context.Context) error {
			verifyCalls++
			if verifyCalls == 4 {
				return errors.New("routing changed after Trash")
			}
			return nil
		},
		nil,
		nil,
	)
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
	record, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.Phase != RemovalCleanupPhaseDataRefresh || record.DataOutcome != "unsupported" ||
		len(record.RetiredItemIDs) != 1 {
		t.Fatalf("receipt=%#v record=%#v exists=%t err=%v", receipt, record, exists, err)
	}
	if !receipt.RoutingRecoveryRequired || receipt.DataMovementUnknown ||
		hasRemovalStage(receipt.Stages, "data_trash", "data_selection_refresh_required") ||
		!hasRemovalStage(receipt.Stages, "routing_post_trash", "routing_ownership_changed") ||
		!hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!hasRemovalStage(receipt.Stages, "cleanup_journal", "data_outcome_persisted") {
		t.Fatalf("routing-first receipt=%#v", receipt)
	}
	refreshReceipt, err := RecordedRemovalDataRefreshReceipt(record)
	if err != nil || refreshReceipt.RoutingRecoveryRequired ||
		!hasRemovalStage(refreshReceipt.Stages, "data_trash", "data_selection_refresh_required") {
		t.Fatalf("post-recovery refresh receipt=%#v err=%v", refreshReceipt, err)
	}
}

func TestRemovalCoordinatorAdmissionStopsBeforeDiscoveryOrRunner(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{}
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		CheckAdmission: func(context.Context) error {
			return ErrRemovalRoutingGate
		},
		VerifyRouting:            func(context.Context) error { return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if resolver.revalidateCalls != 0 || len(runner.calls) != 0 ||
		!hasRemovalStage(receipt.Stages, "cleanup_journal", "removal_in_flight") {
		t.Fatalf("receipt=%#v resolver=%#v calls=%#v", receipt, resolver, runner.calls)
	}
	candidate := testRemovalCandidate()
	if _, err := coordinator.Inventory(context.Background(), NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint}, 1); err == nil {
		t.Fatal("inventory bypassed active execution admission")
	}
}

func TestPackageResumeUsesNarrowReconciledAdmission(t *testing.T) {
	record := RemovalCleanupRecord{
		SchemaVersion:                removalCleanupSchemaVersion,
		Operation:                    "remove-open-codex",
		Context:                      RemovalContextIntegrated,
		Phase:                        RemovalCleanupPhasePackagePending,
		InstallationID:               "0123456789abcdef01234567",
		Fingerprint:                  strings.Repeat("a", 64),
		Mode:                         RemovalModePreserveData,
		SelectionDigest:              RemovalDataSelectionDigest(nil),
		SelectedItemIDs:              []string{},
		RetiredItemIDs:               []string{},
		SelectionRevision:            1,
		ExecutionBootSession:         strings.Repeat("b", 64),
		PackageAttempt:               1,
		ExecutionAttempt:             1,
		ProcessReconciledAfterReboot: true,
		PackageRoot:                  "/tmp/node/lib/node_modules/@bitkyc08/opencodex",
		Launchers:                    []string{"/tmp/node/bin/ocx", "/tmp/node/bin/opencodex"},
	}
	resolver := &fakeRemovalResolver{candidate: NPMInstallation{
		ID: record.InstallationID, Fingerprint: record.Fingerprint,
		PackageRoot: record.PackageRoot, Launchers: record.Launchers,
	}}
	runner := &fakeOpenCodexRemovalRunner{
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		CheckAdmission: func(context.Context) error {
			return ErrRemovalRoutingGate
		},
		CheckResumeAdmission: func(context.Context) error {
			return nil
		},
		VerifyRouting:            func(context.Context) error { return nil },
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.ResumePackageRemoval(context.Background(), record)
	if !receipt.PackageRemoved || !hasRemovalStage(receipt.Stages, "package_verification", "package_absent") ||
		!reflectRemovalCalls(runner.calls, []string{"uninstall"}) {
		t.Fatalf("receipt=%#v calls=%#v", receipt, runner.calls)
	}
}

func TestPackageResumeAdmissionRejectsOrdinaryPending(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalCleanupRecord(candidate, request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	if err := RemovalPackageResumeAdmission(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("ordinary pending admission=%v", err)
	}
}
