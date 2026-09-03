package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestFinalizeRelayRemovalClearsOnlyLocalProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		PackageRemoved: true,
	}

	finalizeRelayRemoval(configPath, &cfg, &receipt)
	if receipt.Status != handoff.RemovalStatusCompleted || receipt.Stages[len(receipt.Stages)-1].Code != "relay_cleanup_completed" {
		t.Fatalf("receipt = %#v", receipt)
	}
	after, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.LocalOpenCodex != nil || after.UpstreamBaseURL != "https://gateway.example.test/v1" {
		t.Fatalf("relay config after cleanup = %#v", after)
	}
}

func TestFinalizeRelayRemovalFailureStaysBoundedAndPreservesReceipt(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("owned by test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(blocker, "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		InstallationID: "0123456789abcdef01234567",
		PackageRemoved: true,
	}
	finalizeRelayRemoval(configPath, &cfg, &receipt)
	if receipt.Status != handoff.RemovalStatusPartial || receipt.Stages[len(receipt.Stages)-1].Code != "relay_config_cleanup_failed" {
		t.Fatalf("receipt = %#v", receipt)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), configPath) || len(encoded) > 16<<10 {
		t.Fatalf("unsafe removal receipt = %s", encoded)
	}
}

func TestSafeOperationErrorMapsRemovalSelectionWithoutDetails(t *testing.T) {
	changed := safeOperationError(handoff.ErrRemovalCandidateChanged)
	if changed.Error.Code != "opencodex_candidate_changed" || changed.Error.RecommendedAction != "rediscover_opencodex" {
		t.Fatalf("candidate error = %#v", changed)
	}
	manual := safeOperationError(handoff.ErrRemovalManualOnly)
	if manual.Error.Code != "opencodex_manual_removal_required" || manual.Error.RecommendedAction != "manual_remediation" || manual.Error.Retryable {
		t.Fatalf("manual error = %#v", manual)
	}
}

func TestOpenRemovalBoundaryRejectsRuntimeMaintenanceWitness(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	store, err := routing.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(configPath)
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
	if err := os.WriteFile(routing.MaintenancePath(configPath), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRemovalBoundary(context.Background(), configPath, codexPath, state.Generation); !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("OpenCodex removal maintenance error = %v", err)
	}
}

func TestSafeOperationErrorMapsRelayTeardownFailuresWithoutDetails(t *testing.T) {
	tests := []struct {
		err       error
		code      string
		retryable bool
		action    string
	}{
		{handoff.ErrTeardownUnsupported, "teardown_unsupported", false, "manual_remediation"},
		{handoff.ErrTeardownCandidateChanged, "teardown_candidate_changed", true, "rediscover_opencodex"},
		{handoff.ErrTeardownPreflightFailed, "teardown_preflight_failed", true, "refresh_status"},
		{handoff.ErrTeardownRefused, "teardown_refused", false, "manual_remediation"},
		{handoff.ErrTeardownResultInvalid, "teardown_result_invalid", false, "manual_remediation"},
		{handoff.ErrTeardownVerificationFailed, "teardown_verification_failed", false, "open_recovery"},
	}
	for _, test := range tests {
		envelope := safeOperationError(test.err)
		if envelope.Error.Code != test.code || envelope.Error.MessageKey != test.code ||
			envelope.Error.Retryable != test.retryable || envelope.Error.RecommendedAction != test.action {
			t.Fatalf("%s envelope = %#v", test.code, envelope)
		}
	}
}

func TestFinalizeRelayRemovalRetiresDurableJournalLast(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, configPath)
	if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion: handoff.OpenCodexRemovalSchemaVersion, Operation: "remove-open-codex",
		Status: handoff.RemovalStatusCompleted, PackageRemoved: true,
	}
	finalizeRelayRemoval(configPath, &cfg, &receipt)
	if receipt.Status != handoff.RemovalStatusCompleted || receipt.Stages[len(receipt.Stages)-1].Code != "relay_cleanup_completed" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(configPath); err != nil || exists {
		t.Fatalf("cleanup journal exists=%t err=%v", exists, err)
	}
}

func TestFinalizeRelayRemovalKeepsJournalWhenEnrollmentCleanupFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, configPath)
	if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff.EnrollmentPath(configPath), []byte("unsafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion: handoff.OpenCodexRemovalSchemaVersion, Operation: "remove-open-codex",
		Status: handoff.RemovalStatusCompleted, PackageRemoved: true,
	}
	finalizeRelayRemoval(configPath, &cfg, &receipt)
	if receipt.Status != handoff.RemovalStatusPartial || receipt.Stages[len(receipt.Stages)-1].Code != "enrollment_cleanup_failed" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(configPath); err != nil || !exists {
		t.Fatalf("cleanup journal exists=%t err=%v", exists, err)
	}
}

func TestRemovalReceiptClaimsFinalizationOnlyWithoutRoutingRecovery(t *testing.T) {
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		InstallationID: "0123456789abcdef01234567",
		PackageRemoved: true,
		Stages: []handoff.OpenCodexRemovalStage{
			{Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
			{Stage: "package_verification", Status: handoff.RemovalStageCompleted, Code: "package_absent"},
		},
	}
	if !removalReceiptClaimsFinalization(receipt) {
		t.Fatal("verified package absence should allow relay cleanup")
	}
	receipt.RoutingRecoveryRequired = true
	if removalReceiptClaimsFinalization(receipt) {
		t.Fatal("routing recovery requirement must retain relay cleanup journal")
	}
	receipt.RoutingRecoveryRequired = false
	receipt.Stages = receipt.Stages[:1]
	if removalReceiptClaimsFinalization(receipt) {
		t.Fatal("receipt without package absence proof allowed relay cleanup")
	}
}

func TestVerifiedFinalizationDurablyReleasesGateBeforeRelayCleanup(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, configPath)
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{
			ID:          record.InstallationID,
			Fingerprint: record.Fingerprint,
		},
		Mode:                      handoff.RemovalModePreserveData,
		ExpectedRoutingGeneration: 7,
		ConfirmedRemoval:          true,
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		InstallationID: record.InstallationID,
		PackageRemoved: true,
		Stages: []handoff.OpenCodexRemovalStage{
			{Stage: "package_verification", Status: handoff.RemovalStageCompleted, Code: "package_absent"},
			{Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
		},
	}
	boundary := &removalBoundary{state: routing.State{Generation: 7}, cfg: cfg}
	finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
	if receipt.Status != handoff.RemovalStatusCompleted ||
		receipt.Stages[len(receipt.Stages)-1].Code != "relay_cleanup_completed" {
		t.Fatalf("receipt=%#v", receipt)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(configPath); err != nil || exists {
		t.Fatalf("journal exists=%t err=%v", exists, err)
	}
	after, err := config.Load(configPath)
	if err != nil || after.LocalOpenCodex != nil {
		t.Fatalf("relay config after finalization=%#v err=%v", after, err)
	}
}

func TestVerifiedFinalizationFailureKeepsTerminalGateClosed(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, configPath)
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	// An invalid enrollment record forces terminal cleanup to fail after the
	// finalization witness has been durably armed.
	if err := os.WriteFile(handoff.EnrollmentPath(configPath), []byte("unsafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{
			ID:          record.InstallationID,
			Fingerprint: record.Fingerprint,
		},
		Mode:                      handoff.RemovalModePreserveData,
		ExpectedRoutingGeneration: 7,
		ConfirmedRemoval:          true,
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		InstallationID: record.InstallationID,
		PackageRemoved: true,
		Stages: []handoff.OpenCodexRemovalStage{
			{Stage: "package_verification", Status: handoff.RemovalStageCompleted, Code: "package_absent"},
			{Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
		},
	}
	boundary := &removalBoundary{state: routing.State{Generation: 7}, cfg: cfg}
	finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
	if receipt.Status != handoff.RemovalStatusPartial ||
		receipt.Stages[len(receipt.Stages)-1].Code != "enrollment_cleanup_failed" {
		t.Fatalf("receipt=%#v", receipt)
	}
	after, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil || !exists || !after.RoutingRecoveryReleased || !after.FinalizationActive {
		t.Fatalf("journal=%#v exists=%t err=%v", after, exists, err)
	}
	if err := handoff.RemovalRoutingGate(configPath); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("terminal gate after cleanup failure=%v", err)
	}
}

func TestVerifiedFinalizationRejectsStaleSelectorBeforeRelayCleanup(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := config.NewLocalOpenCodexProfileForCodexConfig(filepath.Join(t.TempDir(), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	record := removalCleanupRecordForTest(t, configPath)
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	if err := handoff.WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{
			ID:          record.InstallationID,
			Fingerprint: strings.Repeat("c", 64),
		},
		Mode:                      handoff.RemovalModePreserveData,
		ExpectedRoutingGeneration: 7,
		ConfirmedRemoval:          true,
	}
	receipt := handoff.OpenCodexRemovalReceipt{
		SchemaVersion:  handoff.OpenCodexRemovalSchemaVersion,
		Operation:      "remove-open-codex",
		Status:         handoff.RemovalStatusCompleted,
		InstallationID: record.InstallationID,
		PackageRemoved: true,
		Stages: []handoff.OpenCodexRemovalStage{
			{Stage: "package_verification", Status: handoff.RemovalStageCompleted, Code: "package_absent"},
			{Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
		},
	}
	boundary := &removalBoundary{state: routing.State{Generation: 7}, cfg: cfg}
	finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
	if receipt.Status != handoff.RemovalStatusPartial ||
		receipt.Stages[len(receipt.Stages)-1].Code != "finalization_proof_unavailable" {
		t.Fatalf("receipt=%#v", receipt)
	}
	after, err := config.Load(configPath)
	if err != nil || after.LocalOpenCodex == nil {
		t.Fatalf("relay config was finalized with stale selector: %#v err=%v", after, err)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(configPath); err != nil || !exists {
		t.Fatalf("journal exists=%t err=%v", exists, err)
	}
}

func TestPendingRemovalRequestAdmissionRejectsMismatchedContinuationWithoutMutation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	base := removalCleanupRecordForTest(t, configPath)
	exact := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{
			ID:          base.InstallationID,
			Fingerprint: base.Fingerprint,
		},
		Mode:             handoff.RemovalModePreserveData,
		ConfirmedRemoval: true,
	}
	active := base
	active.Phase = handoff.RemovalCleanupPhasePackageInFlight
	active.ExecutionAttempt = 1
	active.PackageAttempt = 1
	active.ExecutionBootSession = strings.Repeat("b", 64)
	active.ActiveExecution = &handoff.RemovalActiveExecution{
		Kind:         handoff.RemovalExecutionPackage,
		Attempt:      1,
		BootSession:  active.ExecutionBootSession,
		BootAttested: true,
	}
	resolution := active
	resolutionActive := *active.ActiveExecution
	resolution.ActiveExecution = &resolutionActive
	resolution.ExecutionResolution = handoff.RemovalExecutionResolutionPreStartRoutingChanged
	resolution.ResolutionRequiresRoutingRecovery = true
	recoveryPending := base
	recoveryPending.ExecutionAttempt = 1
	recoveryPending.PackageAttempt = 1
	recoveryPending.PackageRetryPending = true
	recoveryPending.RecoveryPending = true

	mismatchedRequest := exact
	mismatchedRequest.Mode = handoff.RemovalModeTrashSelected
	mismatchedRequest.DataItemIDs = []string{"ocx-data-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	mismatchedRequest.ConfirmedTrash = true
	for name, record := range map[string]handoff.RemovalCleanupRecord{
		"active":           active,
		"resolution":       resolution,
		"recovery pending": recoveryPending,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePendingRemovalRequest(record, exact); err != nil {
				t.Fatalf("exact request rejected: %v", err)
			}
			before, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := validatePendingRemovalRequest(record, mismatchedRequest); !errors.Is(err, handoff.ErrRemovalCleanupUnsafe) {
				t.Fatalf("mismatched continuation error=%v", err)
			}
			after, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("admission mutated record: before=%s after=%s", before, after)
			}
		})
	}

	wrongSelection := exact
	wrongSelection.Selection.ID = "89abcdef0123456789abcdef"
	if err := validatePendingRemovalRequest(base, wrongSelection); !errors.Is(err, handoff.ErrRemovalCleanupUnsafe) {
		t.Fatalf("mismatched installation ID error=%v", err)
	}
	wrongSelection = exact
	wrongSelection.Selection.Fingerprint = strings.Repeat("c", 64)
	if err := validatePendingRemovalRequest(base, wrongSelection); !errors.Is(err, handoff.ErrRemovalCleanupUnsafe) {
		t.Fatalf("mismatched fingerprint error=%v", err)
	}
}

func TestPendingRemovalRequestAdmissionPreservesDataRefreshSupersession(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	prefix := filepath.Join(filepath.Dir(configPath), "node")
	candidate := handoff.NPMInstallation{
		ID:          "0123456789abcdef01234567",
		Fingerprint: strings.Repeat("a", 64),
		Prefix:      prefix,
		PackageRoot: filepath.Join(prefix, "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers: []string{
			filepath.Join(prefix, "bin", "ocx"),
			filepath.Join(prefix, "bin", "opencodex"),
		},
	}
	original := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModeTrashSelected,
		DataItemIDs: []string{
			"ocx-data-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		ConfirmedRemoval: true,
		ConfirmedTrash:   true,
	}
	intent, err := handoff.NewRemovalIntentRecord(candidate, original)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoff.WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	refresh, err := handoff.MarkRemovalDataRefreshRequired(configPath)
	if err != nil {
		t.Fatal(err)
	}
	superseding := original
	superseding.DataItemIDs = []string{"ocx-data-v1:cccccccccccccccccccccccccccccccc"}
	if err := validatePendingRemovalRequest(refresh, superseding); err != nil {
		t.Fatalf("same-installation data refresh supersession rejected: %v", err)
	}
}

func removalCleanupRecordForTest(t *testing.T, configPath string) handoff.RemovalCleanupRecord {
	t.Helper()
	prefix := filepath.Join(filepath.Dir(configPath), "node")
	candidate := handoff.NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		Prefix: prefix, PackageRoot: filepath.Join(prefix, "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers: []string{filepath.Join(prefix, "bin", "ocx"), filepath.Join(prefix, "bin", "opencodex")},
	}
	record, err := handoff.NewRemovalCleanupRecord(candidate, handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPersistRemovalRoutingRecoveryRequiresDurableResolutionMarker(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	codexPath := filepath.Join(filepath.Dir(configPath), "config.toml")
	store, err := routing.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(configPath)
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

	intent := removalCleanupRecordForTest(t, configPath)
	intent.Phase = handoff.RemovalCleanupPhaseIntent
	if err := handoff.WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	active := intent
	active.ExecutionAttempt = 1
	active.ActiveExecution = &handoff.RemovalActiveExecution{
		Kind:         handoff.RemovalExecutionTeardown,
		Attempt:      1,
		BootSession:  strings.Repeat("b", 64),
		BootAttested: true,
	}
	if err := handoff.WriteRemovalCleanup(configPath, active); err != nil {
		t.Fatal(err)
	}

	lock, err = store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := persistRemovalRoutingRecovery(configPath, lock, state); !errors.Is(err, handoff.ErrRemovalRoutingGate) {
		t.Fatalf("unmarked active recovery error=%v", err)
	}
	unchanged, err := store.Load()
	if err != nil || unchanged.Phase == routing.PhaseRecoveryRequired {
		t.Fatalf("unmarked active state=%#v err=%v", unchanged, err)
	}

	if _, err := handoff.MarkRemovalExecutionResolution(
		configPath,
		handoff.RemovalExecutionTeardown,
		handoff.RemovalExecutionResolutionPreStartRoutingChanged,
		true,
	); err != nil {
		t.Fatal(err)
	}
	if err := persistRemovalRoutingRecovery(configPath, lock, state); err != nil {
		t.Fatal(err)
	}
	parked, err := store.Load()
	if err != nil || parked.Phase != routing.PhaseRecoveryRequired {
		t.Fatalf("marked recovery state=%#v err=%v", parked, err)
	}
	if err := persistRemovalRoutingRecovery(configPath, lock, parked); err != nil {
		t.Fatalf("idempotent marked recovery error=%v", err)
	}
	reparked, err := store.Load()
	if err != nil || reparked.Generation != parked.Generation {
		t.Fatalf("idempotent recovery changed generation: before=%#v after=%#v err=%v", parked, reparked, err)
	}
	resolved, didResolve, err := handoff.ResumeRemovalExecutionResolution(configPath, func() error { return nil })
	if err != nil || !didResolve || resolved.ActiveExecution != nil ||
		!resolved.RecoveryPending || !resolved.OperationRetryPending {
		t.Fatalf("resolved=%#v didResolve=%t err=%v", resolved, didResolve, err)
	}
}

func TestRemovalDataRefreshReceiptRequiresDurableRefreshPhase(t *testing.T) {
	const itemID = "ocx-data-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	configPath := filepath.Join(t.TempDir(), "relay.json")
	prefix := filepath.Join(filepath.Dir(configPath), "node")
	candidate := handoff.NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		Prefix: prefix, PackageRoot: filepath.Join(prefix, "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers: []string{filepath.Join(prefix, "bin", "ocx"), filepath.Join(prefix, "bin", "opencodex")},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModeTrashSelected, DataItemIDs: []string{itemID}, ConfirmedRemoval: true, ConfirmedTrash: true,
	}
	intent, err := handoff.NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removalDataRefreshReceipt(intent); !errors.Is(err, handoff.ErrRemovalCleanupUnsafe) {
		t.Fatalf("non-durable refresh receipt error=%v", err)
	}
	if err := handoff.WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	refresh, err := handoff.MarkRemovalDataRefreshRequired(configPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := removalDataRefreshReceipt(refresh)
	if err != nil || !receipt.DataMovementUnknown || receipt.PackageRemoved {
		t.Fatalf("unknown refresh receipt=%#v err=%v", receipt, err)
	}
}

func TestVerifiedAbsentPackageStillRequiresExactRoutingOwnership(t *testing.T) {
	record := removalCleanupRecordForTest(t, filepath.Join(t.TempDir(), "relay.json"))
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	parked := 0
	receipt, canFinalize, err := reconcileVerifiedRemovedPackage(
		context.Background(),
		record,
		func(context.Context) error { return errors.New("foreign routing owner") },
		func() error { parked++; return nil },
	)
	if err != nil || canFinalize || receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 {
		t.Fatalf("routing mismatch receipt=%#v finalize=%t parked=%d err=%v", receipt, canFinalize, parked, err)
	}

	receipt, canFinalize, err = reconcileVerifiedRemovedPackage(
		context.Background(),
		record,
		func(context.Context) error { return nil },
		func() error { t.Fatal("stable routing attempted recovery parking"); return nil },
	)
	if err != nil || !canFinalize || !receipt.PackageRemoved || receipt.RoutingRecoveryRequired ||
		len(receipt.Stages) < 2 || receipt.Stages[len(receipt.Stages)-1] != (handoff.OpenCodexRemovalStage{
		Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified",
	}) {
		t.Fatalf("stable routing receipt=%#v finalize=%t err=%v", receipt, canFinalize, err)
	}
}
