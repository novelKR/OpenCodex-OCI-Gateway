package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func standaloneRemovalRequest(mode OpenCodexRemovalMode) OpenCodexRemovalRequest {
	request := testRemovalRequest(mode)
	request.Context = RemovalContextStandaloneNative
	request.ExpectedBoundaryRevision = strings.Repeat("b", 64)
	request.ExpectedNativeState = NativeStateOpenCodex
	request.NativeRestoreFingerprint = strings.Repeat("c", 64)
	if mode == RemovalModeTrashSelected {
		request.NativeInventoryRevision = strings.Repeat("d", 64)
	}
	return request
}

func TestStandaloneRemovalJournalRequiresNativeRestoreBeforePackageMutation(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if record.SchemaVersion != 6 || record.Context != RemovalContextStandaloneNative ||
		record.NativeOriginState != NativeStateOpenCodex || record.NativeState != NativeStateOpenCodex {
		t.Fatalf("native intent = %#v", record)
	}
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginExecution(anchor, RemovalExecutionNativeRestore); err == nil {
		t.Fatal("native restore began before teardown completion")
	}
	if _, err := MarkStandaloneTeardownComplete(anchor); err != nil {
		t.Fatal(err)
	}
	oldBootProvider := removalBootSessionProvider
	removalBootSessionProvider = func() (string, bool, error) { return strings.Repeat("d", 64), true, nil }
	t.Cleanup(func() { removalBootSessionProvider = oldBootProvider })
	if _, err := BeginExecution(anchor, RemovalExecutionNativeRestore); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginExecution(anchor, RemovalExecutionPackage); err == nil {
		t.Fatal("package began while native restore was active")
	}
	if _, err := FinishExecution(anchor, RemovalExecutionNativeRestore, RemovalExecutionResult{Started: true, CleanupVerified: true}); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("non-atomic Native completion error=%v", err)
	}
	current, exists, err := ReadRemovalCleanup(anchor)
	if err != nil || !exists || current.ActiveExecution == nil || current.NativeVerifiedBoundaryRevision != "" || current.NativeState != NativeStateOpenCodex {
		t.Fatalf("unverified restore state = %#v exists=%t err=%v", current, exists, err)
	}
	if _, err := PrepareRemovalPackageCleanup(anchor, candidate, request, 0); err == nil {
		t.Fatal("package cleanup advanced before post-restore Native verification")
	}
	verified := strings.Repeat("e", 64)
	completed, err := CompleteStandaloneNativeRestore(
		anchor, RemovalExecutionResult{Started: true, CleanupVerified: true}, verified,
	)
	if err != nil || completed.ActiveExecution != nil || completed.NativeState != NativeStateNative ||
		completed.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	current, exists, err = ReadRemovalCleanup(anchor)
	if err != nil || !exists || current.ActiveExecution != nil || current.NativeState != NativeStateNative ||
		current.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("atomic Native state=%#v exists=%t err=%v", current, exists, err)
	}
	if _, err := CompleteStandaloneNativeRestore(
		anchor, RemovalExecutionResult{Started: true, CleanupVerified: true}, verified,
	); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("duplicate Native completion error=%v", err)
	}
	prepared, err := PrepareRemovalPackageCleanup(anchor, candidate, request, 0)
	if err != nil || prepared.Phase != RemovalCleanupPhasePackagePending || prepared.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("prepared = %#v err=%v", prepared, err)
	}
}

func TestStandaloneTeardownCompletionAtomicallyPublishesNativeProof(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	useAttestedRemovalBootSession(t)
	if _, err := BeginExecution(anchor, RemovalExecutionTeardown); err != nil {
		t.Fatal(err)
	}
	result := RemovalExecutionResult{Started: true, CleanupVerified: true}
	if _, err := CompleteStandaloneTeardown(anchor, result, "not-a-fingerprint"); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("invalid Native proof error=%v", err)
	}
	active, exists, err := ReadRemovalCleanup(anchor)
	if err != nil || !exists || active.ActiveExecution == nil || active.TeardownCompleted || active.NativeVerifiedBoundaryRevision != "" {
		t.Fatalf("invalid proof changed active journal: record=%#v exists=%t err=%v", active, exists, err)
	}
	verified := strings.Repeat("e", 64)
	completed, err := CompleteStandaloneTeardown(anchor, result, verified)
	if err != nil || completed.ActiveExecution != nil || !completed.TeardownCompleted ||
		completed.NativeState != NativeStateNative || completed.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("atomic teardown completion=%#v err=%v", completed, err)
	}
	if _, err := CompleteStandaloneTeardown(anchor, result, verified); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("duplicate teardown completion error=%v", err)
	}
	prepared, err := PrepareRemovalPackageCleanup(anchor, candidate, request, 0)
	if err != nil || prepared.Phase != RemovalCleanupPhasePackagePending || prepared.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
}

func TestStandaloneTeardownChangedBootReconciliationKeepsRetryAndNativeProof(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	oldBootProvider := removalBootSessionProvider
	removalBootSessionProvider = func() (string, bool, error) { return strings.Repeat("1", 64), true, nil }
	t.Cleanup(func() { removalBootSessionProvider = oldBootProvider })
	active, err := BeginExecution(anchor, RemovalExecutionTeardown)
	if err != nil {
		t.Fatal(err)
	}
	verified := strings.Repeat("2", 64)
	if _, err := ReconcileStandaloneTeardownAfterBoot(anchor, active, verified, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("same-boot reconciliation error=%v", err)
	}
	removalBootSessionProvider = func() (string, bool, error) { return strings.Repeat("3", 64), true, nil }
	reconciled, err := ReconcileStandaloneTeardownAfterBoot(anchor, active, verified, true)
	if err != nil || reconciled.ActiveExecution != nil || reconciled.TeardownCompleted ||
		!reconciled.OperationRetryPending || reconciled.NativeState != NativeStateNative ||
		reconciled.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("reconciled=%#v err=%v", reconciled, err)
	}
}

func TestResolvedStandaloneTeardownCanPublishNativeProofWithoutClaimingCompletion(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.OperationRetryPending = true
	record.NativeRecoveryRequired = true
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	verified := strings.Repeat("4", 64)
	recovered, err := RecoverStandaloneTeardownNativeBoundary(anchor, record, verified)
	if err != nil || recovered.ActiveExecution != nil || recovered.TeardownCompleted ||
		recovered.NativeRecoveryRequired || !recovered.OperationRetryPending ||
		recovered.NativeState != NativeStateNative || recovered.NativeVerifiedBoundaryRevision != verified {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
}

func TestStandaloneNativeRestoreChangedBootRequiresVerifiedBoundary(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.TeardownCompleted = true
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	oldBootProvider := removalBootSessionProvider
	removalBootSessionProvider = func() (string, bool, error) { return strings.Repeat("1", 64), true, nil }
	t.Cleanup(func() { removalBootSessionProvider = oldBootProvider })
	if _, err := BeginExecution(anchor, RemovalExecutionNativeRestore); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileStandaloneNativeRestoreAfterBoot(anchor, strings.Repeat("2", 64), true); err == nil {
		t.Fatal("same-boot native restore reconciliation succeeded")
	}
	removalBootSessionProvider = func() (string, bool, error) { return strings.Repeat("3", 64), true, nil }
	if _, err := ReconcileStandaloneNativeRestoreAfterBoot(anchor, "not-a-fingerprint", true); err == nil {
		t.Fatal("unverified Native boundary was accepted")
	}
	reconciled, err := ReconcileStandaloneNativeRestoreAfterBoot(anchor, strings.Repeat("2", 64), true)
	if err != nil || reconciled.ActiveExecution != nil || reconciled.NativeState != NativeStateNative ||
		reconciled.NativeVerifiedBoundaryRevision != strings.Repeat("2", 64) {
		t.Fatalf("reconciled=%#v err=%v", reconciled, err)
	}
}

func TestStandaloneTerminalRemovalIsReplayableAndRetiresOnlyExactRecord(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "standalone-native")
	candidate := removalCleanupCandidate(t)
	request := standaloneRemovalRequest(RemovalModePreserveData)
	record, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = RemovalCleanupPhasePackageVerified
	record.TeardownCompleted = true
	record.NativeState = NativeStateNative
	record.NativeVerifiedBoundaryRevision = strings.Repeat("e", 64)
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("f", 64)
	record.RoutingRecoveryReleased = true
	record.FinalizationActive = true
	if err := WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	retained, exists, err := ReadRemovalCleanup(anchor)
	if err != nil || !exists || !StandaloneTerminalRemovalReplayReady(retained) {
		t.Fatalf("retained=%#v exists=%t err=%v", retained, exists, err)
	}
	digest, err := StandaloneTerminalReceiptDigest(retained)
	if err != nil || !isFingerprint(digest) {
		t.Fatalf("terminal digest=%q err=%v", digest, err)
	}
	if repeated, err := StandaloneTerminalReceiptDigest(retained); err != nil || repeated != digest {
		t.Fatalf("terminal digest was not stable: repeated=%q err=%v", repeated, err)
	}

	stale := retained
	stale.ExecutionAttempt++
	staleDigest, err := StandaloneTerminalReceiptDigest(stale)
	if err != nil || staleDigest == digest {
		t.Fatalf("stale digest=%q original=%q err=%v", staleDigest, digest, err)
	}
	if err := RetireStandaloneTerminalRemoval(anchor, stale); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("stale terminal retirement error=%v", err)
	}
	if _, exists, err := ReadRemovalCleanup(anchor); err != nil || !exists {
		t.Fatalf("stale retirement consumed journal: exists=%t err=%v", exists, err)
	}
	if acknowledged, err := AcknowledgeStandaloneTerminalRemoval(anchor, staleDigest); err != nil || acknowledged {
		t.Fatalf("different receipt acknowledged=%t err=%v", acknowledged, err)
	}
	if _, exists, err := ReadRemovalCleanup(anchor); err != nil || !exists {
		t.Fatalf("different digest consumed journal: exists=%t err=%v", exists, err)
	}
	if acknowledged, err := AcknowledgeStandaloneTerminalRemoval(anchor, "not-a-digest"); !errors.Is(err, ErrRemovalCleanupUnsafe) || acknowledged {
		t.Fatalf("invalid receipt acknowledged=%t err=%v", acknowledged, err)
	}
	if acknowledged, err := AcknowledgeStandaloneTerminalRemoval(anchor, digest); err != nil || !acknowledged {
		t.Fatalf("exact receipt acknowledged=%t err=%v", acknowledged, err)
	}
	if _, exists, err := ReadRemovalCleanup(anchor); err != nil || exists {
		t.Fatalf("exact terminal retirement exists=%t err=%v", exists, err)
	}
	if acknowledged, err := AcknowledgeStandaloneTerminalRemoval(anchor, digest); err != nil || acknowledged {
		t.Fatalf("idempotent missing receipt acknowledged=%t err=%v", acknowledged, err)
	}
}

func TestRemovalJournalV5MigratesOnlyAsIntegrated(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	record, err := NewRemovalIntentRecord(removalCleanupCandidate(t), testRemovalRequest(RemovalModePreserveData))
	if err != nil {
		t.Fatal(err)
	}
	record.SchemaVersion = previousRemovalCleanupVersion
	record.Context = ""
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RemovalCleanupPath(configPath), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || migrated.SchemaVersion != removalCleanupSchemaVersion || migrated.Context != RemovalContextIntegrated {
		t.Fatalf("migrated=%#v exists=%t err=%v", migrated, exists, err)
	}
	var onDisk struct {
		SchemaVersion int `json:"schema_version"`
	}
	diskPayload, err := os.ReadFile(RemovalCleanupPath(configPath))
	if err != nil || json.Unmarshal(diskPayload, &onDisk) != nil || onDisk.SchemaVersion != previousRemovalCleanupVersion {
		t.Fatalf("read-only migration changed disk schema: schema=%d err=%v", onDisk.SchemaVersion, err)
	}
	if err := WriteRemovalCleanup(configPath, migrated); err != nil {
		t.Fatalf("explicit mutating migration failed: %v", err)
	}
	diskPayload, err = os.ReadFile(RemovalCleanupPath(configPath))
	if err != nil || json.Unmarshal(diskPayload, &onDisk) != nil || onDisk.SchemaVersion != removalCleanupSchemaVersion {
		t.Fatalf("explicit migration did not persist v6: schema=%d err=%v", onDisk.SchemaVersion, err)
	}

	standaloneDirectory := filepath.Join(t.TempDir(), "OpenCodexRelayLifecycle")
	if err := os.MkdirAll(standaloneDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	standalone := filepath.Join(standaloneDirectory, "standalone-native")
	if err := os.WriteFile(RemovalCleanupPath(standalone), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRemovalCleanup(standalone); err == nil {
		t.Fatal("legacy journal migrated into standalone context")
	}
}

func TestConcurrentLegacyReadsCannotOverwriteNewerV6Journal(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	legacy, err := NewRemovalIntentRecord(removalCleanupCandidate(t), testRemovalRequest(RemovalModePreserveData))
	if err != nil {
		t.Fatal(err)
	}
	legacy.SchemaVersion = previousRemovalCleanupVersion
	legacy.Context = ""
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RemovalCleanupPath(configPath), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var readers sync.WaitGroup
	for index := 0; index < 32; index++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for iteration := 0; iteration < 64; iteration++ {
				_, _, _ = ReadRemovalCleanup(configPath)
			}
		}()
	}
	close(start)
	normalized, _, err := ReadRemovalCleanup(configPath)
	if err != nil {
		t.Fatal(err)
	}
	normalized.OperationRetryPending = true
	if err := writeRemovalCleanupFile(RemovalCleanupPath(configPath), normalized); err != nil {
		t.Fatal(err)
	}
	readers.Wait()

	current, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || current.SchemaVersion != removalCleanupSchemaVersion || !current.OperationRetryPending {
		t.Fatalf("newer v6 journal was overwritten: current=%#v exists=%t err=%v", current, exists, err)
	}
}

func TestNativeCandidateProjectionExposesPathsOnlyForHomebrewGuard(t *testing.T) {
	candidate := NPMInstallation{
		ID: testRemovalID, Fingerprint: testRemovalFingerprint, NativeRestoreFingerprint: strings.Repeat("c", 64),
		Version: "1.2.3", Manager: DiscoveryManagerNPM, RemovalCapability: RemovalCapabilityExactNPM,
		RemovalAuthority: RemovalAuthorityAutomatic, DataCapability: DataCapabilitySelectiveTrashV1,
	}
	ordinary := ProjectNativeRemovalCandidate(candidate)
	encoded, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.HomebrewGuard != nil || strings.Contains(string(encoded), "package_root") || strings.Contains(string(encoded), "executable") {
		t.Fatalf("ordinary projection leaked execution paths: %s", encoded)
	}
	candidate.Manager = DiscoveryManagerHomebrew
	candidate.RemovalCapability = RemovalCapabilityHomebrewGuardedNPM
	candidate.HomebrewGuardRequired = true
	candidate.Prefix = "/opt/homebrew"
	candidate.PackageRoot = packageRootForPrefix(candidate.Prefix)
	candidate.Executable = filepath.Join(candidate.PackageRoot, "bin", "ocx.mjs")
	candidate.ExecutableSHA256 = strings.Repeat("1", 64)
	candidate.CLIEntry = filepath.Join(candidate.PackageRoot, "src", "cli", "index.ts")
	candidate.CLIEntrySHA256 = strings.Repeat("2", 64)
	candidate.BunExecutable = filepath.Join(candidate.PackageRoot, "node_modules", "bun", "bin", "bun")
	candidate.BunSHA256 = strings.Repeat("3", 64)
	candidate.NodeExecutable = "/opt/homebrew/bin/node"
	candidate.NodeSHA256 = strings.Repeat("4", 64)
	candidate.NPMCLI = "/opt/homebrew/lib/node_modules/npm/bin/npm-cli.js"
	candidate.NPMCLISHA256 = strings.Repeat("5", 64)
	candidate.Launchers = []string{"/opt/homebrew/bin/ocx"}
	guarded := ProjectNativeRemovalCandidate(candidate)
	if guarded.HomebrewGuard == nil || guarded.HomebrewGuard.PackageRoot != candidate.PackageRoot {
		t.Fatalf("guarded projection = %#v", guarded)
	}
}

func TestNativeCandidateProjectionUsesBoundedAutomaticRemovalReasons(t *testing.T) {
	if NativeRemovalSchemaVersion != 1 || NativeRemovalReadSchemaVersion != 2 {
		t.Fatalf("native removal schema versions = receipt %d read %d", NativeRemovalSchemaVersion, NativeRemovalReadSchemaVersion)
	}
	base := NPMInstallation{
		ID: testRemovalID, Fingerprint: testRemovalFingerprint, NativeRestoreFingerprint: strings.Repeat("c", 64),
		Version: "2.22.0", Manager: DiscoveryManagerNPM, RemovalCapability: RemovalCapabilityExactNPM,
		RemovalAuthority: RemovalAuthorityManual, TeardownCompatibility: teardownCompatibilityClosureChanged,
	}
	if got := automaticRemovalReason(base, true); got != AutomaticRemovalReasonEligible {
		t.Fatalf("eligible reason = %q", got)
	}
	tests := []struct {
		name   string
		mutate func(*NPMInstallation)
		want   string
	}{
		{name: "closure", want: AutomaticRemovalReasonUnreviewedPackageClosure},
		{name: "version", mutate: func(candidate *NPMInstallation) {
			candidate.TeardownCompatibility = teardownCompatibilityUnsupportedVersion
		}, want: AutomaticRemovalReasonUnsupportedPackageVersion},
		{name: "module", mutate: func(candidate *NPMInstallation) {
			candidate.TeardownCompatibility = teardownCompatibilityModuleChanged
		}, want: AutomaticRemovalReasonPackageModuleChanged},
		{name: "execution", mutate: func(candidate *NPMInstallation) {
			candidate.TeardownCompatibility = teardownCompatibilityCompatible
		}, want: AutomaticRemovalReasonExecutionEvidenceIncomplete},
		{name: "manager", mutate: func(candidate *NPMInstallation) {
			candidate.Manager = DiscoveryManagerVolta
			candidate.TeardownCompatibility = teardownCompatibilityIdentityUnverified
		}, want: AutomaticRemovalReasonManualPackageManager},
		{name: "identity", mutate: func(candidate *NPMInstallation) {
			candidate.TeardownCompatibility = teardownCompatibilityIdentityUnverified
		}, want: AutomaticRemovalReasonIdentityUnverified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			projected := ProjectNativeRemovalCandidate(candidate)
			if projected.AutomaticRemovalEligible || projected.AutomaticRemovalReason != test.want {
				t.Fatalf("projection = %#v, want reason %q", projected, test.want)
			}
			encoded, err := json.Marshal(projected)
			if err != nil || strings.Contains(string(encoded), "package_root") || strings.Contains(string(encoded), "teardown_compatibility") {
				t.Fatalf("bounded projection = %s, err=%v", encoded, err)
			}
		})
	}
}

func TestNativeInventoryRevisionBindsBoundaryAndRestoreWitness(t *testing.T) {
	base := OpenCodexDataInventoryReceipt{
		Status: "verified", InstallationID: testRemovalID, InstallationFingerprint: testRemovalFingerprint,
		Items: []OpenCodexDataInventoryItem{},
	}
	one := NewNativeDataInventoryReceipt(base, strings.Repeat("a", 64), NativeStateOpenCodex, strings.Repeat("b", 64))
	two := NewNativeDataInventoryReceipt(base, strings.Repeat("c", 64), NativeStateOpenCodex, strings.Repeat("b", 64))
	three := NewNativeDataInventoryReceipt(base, strings.Repeat("a", 64), NativeStateOpenCodex, strings.Repeat("d", 64))
	if !isFingerprint(one.InventoryRevision) || one.InventoryRevision == two.InventoryRevision || one.InventoryRevision == three.InventoryRevision {
		t.Fatalf("inventory revisions one=%q two=%q three=%q", one.InventoryRevision, two.InventoryRevision, three.InventoryRevision)
	}
}

func TestNativeRemovalResolverRejectsMismatchedRestoreWitnessBeforeRediscovery(t *testing.T) {
	resolver := DiscoveryNativeRemovalResolver{}
	_, err := resolver.Resolve(context.Background(), NativeRemovalSelection{
		InstallationID: testRemovalID, InstallationFingerprint: testRemovalFingerprint,
		NativeRestoreFingerprint: "invalid",
	})
	if err == nil {
		t.Fatal("invalid third witness was accepted")
	}
}

func TestStandaloneRemovalCoordinatorRequiresNativeLifecycleHooks(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	coordinator := RemovalCoordinator{
		Resolver: resolver, Runner: &fakeOpenCodexRemovalRunner{},
		VerifyRouting:         func(context.Context) error { return nil },
		PrepareOperation:      noopPrepareOperation,
		RecordDataOutcome:     noopRecordDataOutcome,
		PreparePackageRemoval: noopPreparePackageRemoval,
		BeginExecution:        noopBeginExecution,
		FinishExecution:       noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), standaloneRemovalRequest(RemovalModePreserveData))
	if resolver.resolveCalls != 0 || !hasRemovalStage(receipt.Stages, "request_validation", "coordinator_unavailable") {
		t.Fatalf("standalone removal admitted without Native lifecycle hooks: %#v", receipt)
	}
}

func TestStandaloneRemovalCoordinatorUsesAtomicTeardownNativeProof(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{ExitCode: 0, Started: true, CleanupVerified: true},
	}
	completeTeardownCalls := 0
	restoreNativeCalls := 0
	coordinator := RemovalCoordinator{
		Resolver:                 resolver,
		Runner:                   runner,
		VerifyRouting:            func(context.Context) error { return nil },
		VerifyPostTeardown:       func(context.Context) error { return nil },
		MarkRoutingRecovery:      func() error { return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
		CompleteTeardown: func(context.Context, NPMInstallation, RemovalExecutionResult) error {
			completeTeardownCalls++
			return nil
		},
		RestoreNative: func(context.Context, NPMInstallation) (NativeRestoreResult, error) {
			restoreNativeCalls++
			return NativeRestoreResult{}, nil
		},
		CompleteNativeRestore: func(context.Context, NPMInstallation, NativeRestoreResult) error {
			return nil
		},
	}
	receipt := coordinator.Remove(context.Background(), standaloneRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusCompleted || !receipt.PackageRemoved || completeTeardownCalls != 1 || restoreNativeCalls != 0 {
		t.Fatalf("receipt=%#v teardown completions=%d native restores=%d", receipt, completeTeardownCalls, restoreNativeCalls)
	}
	if !hasRemovalStage(receipt.Stages, "native_restore", "native_already_active") ||
		!reflect.DeepEqual(runner.calls, []string{"teardown", "uninstall"}) {
		t.Fatalf("stages=%#v runner=%#v", receipt.Stages, runner.calls)
	}
}
