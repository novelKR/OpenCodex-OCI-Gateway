//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
)

func nativeBoundaryTestHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("OPENCODEX_HOME", "")
	return home
}

func TestStandaloneNativeBoundaryAllowsOnlyFixedCleanNativeState(t *testing.T) {
	home := nativeBoundaryTestHome(t)
	boundary, err := openStandaloneNativeBoundary(context.Background(), "")
	if err != nil || boundary.state != handoff.NativeStateNative || len(boundary.revision) != 64 || boundary.recovery != nil {
		t.Fatalf("boundary=%#v err=%v", boundary, err)
	}
	if boundary.codexPath != filepath.Join(home, ".codex", "config.toml") {
		t.Fatalf("codex path=%q", boundary.codexPath)
	}
	if _, err := openStandaloneNativeBoundary(context.Background(), strings.Repeat("f", 64)); !errors.Is(err, handoff.ErrNativeRemovalBoundaryChanged) {
		t.Fatalf("stale revision error=%v", err)
	}

	custom := filepath.Join(home, "custom-codex")
	if err := os.Mkdir(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", custom)
	if _, err := openStandaloneNativeBoundary(context.Background(), ""); !errors.Is(err, handoff.ErrNativeRemovalCustomCodexHome) {
		t.Fatalf("custom CODEX_HOME error=%v", err)
	}
}

func TestStandaloneNativeBoundaryRejectsPartialRelayAsset(t *testing.T) {
	home := nativeBoundaryTestHome(t)
	binding := filepath.Join(home, "Library", "Application Support", "OpenCodexRelay", "routing-binding.json")
	if err := os.MkdirAll(filepath.Dir(binding), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binding, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStandaloneNativeBoundary(context.Background(), ""); !errors.Is(err, handoff.ErrNativeRemovalBoundaryUnsafe) {
		t.Fatalf("partial binding error=%v", err)
	}
}

func TestStandaloneNativeBoundaryRejectsAmbientConfigurationRoots(t *testing.T) {
	for _, name := range []string{"XDG_CONFIG_HOME", "OPENCODEX_HOME"} {
		t.Run(name, func(t *testing.T) {
			_ = nativeBoundaryTestHome(t)
			t.Setenv(name, t.TempDir())
			if _, err := openStandaloneNativeBoundary(context.Background(), ""); !errors.Is(err, handoff.ErrNativeRemovalBoundaryUnsafe) {
				t.Fatalf("ambient %s error=%v", name, err)
			}
		})
	}
}

func TestConfigInitGateRejectsValidAndMalformedStandaloneJournal(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed bool
	}{
		{name: "valid"},
		{name: "malformed", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := nativeBoundaryTestHome(t)
			anchor, err := handoff.StandaloneRemovalAnchorPath(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(anchor), 0o700); err != nil {
				t.Fatal(err)
			}
			if test.malformed {
				if err := os.WriteFile(handoff.RemovalCleanupPath(anchor), []byte("{\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				candidate := handoff.NPMInstallation{
					ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
					PackageRoot: filepath.Join(home, "node", "lib", "node_modules", "@bitkyc08", "opencodex"), Launchers: []string{},
				}
				request := handoff.OpenCodexRemovalRequest{
					Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
					Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
					Context:                  handoff.RemovalContextStandaloneNative,
					ExpectedBoundaryRevision: strings.Repeat("b", 64), ExpectedNativeState: handoff.NativeStateOpenCodex,
					NativeRestoreFingerprint: strings.Repeat("c", 64),
				}
				record, err := handoff.NewRemovalIntentRecord(candidate, request)
				if err != nil {
					t.Fatal(err)
				}
				if err := handoff.WriteRemovalCleanup(anchor, record); err != nil {
					t.Fatal(err)
				}
			}
			if err := requireStandaloneRemovalInactiveForInit(home); !errors.Is(err, handoff.ErrNativeRemovalRecoveryRequired) {
				t.Fatalf("init gate error=%v", err)
			}
		})
	}
	home := nativeBoundaryTestHome(t)
	if err := requireStandaloneRemovalInactiveForInit(home); err != nil {
		t.Fatalf("absent init gate error=%v", err)
	}
}

func TestStandaloneRelayAssetGateRejectsPartialCanonicalAssets(t *testing.T) {
	for _, relative := range []string{
		".codex/opencodex-relay-interactive.config.toml",
		".codex/opencodex-relay-local-catalog.json",
		".codex/opencodex-relay-local-catalog.json.restart-pending",
		".codex/opencodex-relay-dev-local-catalog.json",
		".codex/opencodex-relay-dev-local-catalog.json.restart-pending",
		".config/opencodex-relay/credentials.env",
	} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("partial\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ensureStandaloneRelayAssetsAbsent(home); !errors.Is(err, handoff.ErrNativeRemovalBoundaryUnsafe) {
				t.Fatalf("partial asset error=%v", err)
			}
		})
	}
}

func TestStandaloneNativeBoundaryRejectsSymlinkedOrLooseCodexHome(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "symlink", setup: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(t.TempDir(), path)
		}},
		{name: "group_writable", setup: func(path string) error { return os.Chmod(path, 0o770) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := nativeBoundaryTestHome(t)
			if err := test.setup(filepath.Join(home, ".codex")); err != nil {
				t.Fatal(err)
			}
			if _, err := openStandaloneNativeBoundary(context.Background(), ""); !errors.Is(err, handoff.ErrNativeRemovalBoundaryUnsafe) {
				t.Fatalf("unsafe Codex home error=%v", err)
			}
		})
	}
}

func TestStandaloneNativeBoundarySurfacesJournalBeforeCurrentConfigClassification(t *testing.T) {
	home := nativeBoundaryTestHome(t)
	anchor, err := handoff.StandaloneRemovalAnchorPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := handoff.NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		PackageRoot: filepath.Join(home, "node", "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers:   []string{},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
		Context:                  handoff.RemovalContextStandaloneNative,
		ExpectedBoundaryRevision: strings.Repeat("b", 64), ExpectedNativeState: handoff.NativeStateOpenCodex,
		NativeRestoreFingerprint: strings.Repeat("c", 64),
	}
	record, err := handoff.NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoff.WriteRemovalCleanup(anchor, record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("openai_base_url = \"https://foreign.invalid/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boundary, err := openStandaloneNativeBoundary(context.Background(), request.ExpectedBoundaryRevision)
	if err != nil || boundary.recovery == nil || boundary.state != handoff.NativeStateUnavailable || boundary.revision != request.ExpectedBoundaryRevision {
		t.Fatalf("recovery boundary=%#v err=%v", boundary, err)
	}
}

func TestStandaloneNativeTerminalJournalRequiresCurrentBoundaryBeforeRetirement(t *testing.T) {
	home := nativeBoundaryTestHome(t)
	current, err := openStandaloneNativeBoundary(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	candidate := handoff.NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		PackageRoot: filepath.Join(home, "node", "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers:   []string{},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
		Context:                  handoff.RemovalContextStandaloneNative,
		ExpectedBoundaryRevision: strings.Repeat("b", 64), ExpectedNativeState: handoff.NativeStateOpenCodex,
		NativeRestoreFingerprint: strings.Repeat("c", 64),
	}
	record, err := handoff.NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.TeardownCompleted = true
	record.NativeState = handoff.NativeStateNative
	record.NativeVerifiedBoundaryRevision = current.revision
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("d", 64)
	record.RoutingRecoveryReleased = true
	record.FinalizationActive = true
	if err := os.MkdirAll(filepath.Dir(current.anchorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := handoff.WriteRemovalCleanup(current.anchorPath, record); err != nil {
		t.Fatal(err)
	}
	retained, err := openStandaloneNativeBoundary(context.Background(), request.ExpectedBoundaryRevision)
	if err != nil || retained.recovery == nil || !standaloneNativeTerminalBoundaryMatches(retained, *retained.recovery) {
		t.Fatalf("retained boundary=%#v err=%v", retained, err)
	}

	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"changed\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if standaloneNativeTerminalBoundaryMatches(retained, *retained.recovery) {
		t.Fatal("changed Native boundary authorized terminal retirement")
	}
	if _, exists, err := handoff.ReadRemovalCleanup(current.anchorPath); err != nil || !exists {
		t.Fatalf("changed boundary consumed terminal journal: exists=%t err=%v", exists, err)
	}
}

func TestStandaloneNativeTerminalJournalRequiresReceiptBoundAcknowledgement(t *testing.T) {
	home := nativeBoundaryTestHome(t)
	current, err := openStandaloneNativeBoundary(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	candidate := handoff.NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		PackageRoot: filepath.Join(home, "node", "lib", "node_modules", "@bitkyc08", "opencodex"),
		Launchers:   []string{},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
		Context:                  handoff.RemovalContextStandaloneNative,
		ExpectedBoundaryRevision: strings.Repeat("b", 64), ExpectedNativeState: handoff.NativeStateOpenCodex,
		NativeRestoreFingerprint: strings.Repeat("c", 64),
	}
	record, err := handoff.NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.TeardownCompleted = true
	record.NativeState = handoff.NativeStateNative
	record.NativeVerifiedBoundaryRevision = current.revision
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("d", 64)
	record.RoutingRecoveryReleased = true
	record.FinalizationActive = true
	if err := os.MkdirAll(filepath.Dir(current.anchorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := handoff.WriteRemovalCleanup(current.anchorPath, record); err != nil {
		t.Fatal(err)
	}
	boundary, err := openStandaloneNativeBoundary(context.Background(), request.ExpectedBoundaryRevision)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := handoff.StandaloneTerminalReceiptDigest(record)
	if err != nil {
		t.Fatal(err)
	}

	unchanged, acknowledged, err := acknowledgeStandaloneNativeTerminal(context.Background(), boundary, "")
	if err != nil || acknowledged || unchanged.recovery == nil {
		t.Fatalf("bare discovery boundary=%#v acknowledged=%t err=%v", unchanged, acknowledged, err)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(current.anchorPath); err != nil || !exists {
		t.Fatalf("bare discovery consumed terminal journal: exists=%t err=%v", exists, err)
	}

	wrongDigest := "0" + digest[1:]
	if wrongDigest == digest {
		wrongDigest = "1" + digest[1:]
	}
	wrong, acknowledged, err := acknowledgeStandaloneNativeTerminal(context.Background(), boundary, wrongDigest)
	if err != nil || acknowledged || wrong.recovery == nil {
		t.Fatalf("wrong digest boundary=%#v acknowledged=%t err=%v", wrong, acknowledged, err)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(current.anchorPath); err != nil || !exists {
		t.Fatalf("wrong digest consumed terminal journal: exists=%t err=%v", exists, err)
	}

	ready, acknowledged, err := acknowledgeStandaloneNativeTerminal(context.Background(), boundary, digest)
	if err != nil || !acknowledged || ready.recovery != nil || ready.state != handoff.NativeStateNative {
		t.Fatalf("exact digest boundary=%#v acknowledged=%t err=%v", ready, acknowledged, err)
	}
	if _, exists, err := handoff.ReadRemovalCleanup(current.anchorPath); err != nil || exists {
		t.Fatalf("exact digest retained terminal journal: exists=%t err=%v", exists, err)
	}

	idempotent, acknowledged, err := acknowledgeStandaloneNativeTerminal(context.Background(), ready, digest)
	if err != nil || acknowledged || idempotent.recovery != nil || idempotent.state != handoff.NativeStateNative {
		t.Fatalf("idempotent acknowledgement boundary=%#v acknowledged=%t err=%v", idempotent, acknowledged, err)
	}
}

func TestNativeReceiptProjectionUsesStrictNativeStages(t *testing.T) {
	installationID := "0123456789abcdef01234567"
	base := handoff.OpenCodexRemovalReceipt{
		SchemaVersion: handoff.OpenCodexRemovalSchemaVersion, Operation: "remove-open-codex",
		Status: handoff.RemovalStatusCompleted, Mode: handoff.RemovalModePreserveData,
		InstallationID: installationID, DataScope: "preserved", PackageRemoved: true,
		Stages: []handoff.OpenCodexRemovalStage{
			{Stage: "native_restore", Status: handoff.RemovalStageCompleted, Code: "native_restore_applied", SubjectID: installationID},
			{Stage: "routing_post_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
			{Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified"},
			{Stage: "package_verification", Status: handoff.RemovalStageCompleted, Code: "package_absent", SubjectID: installationID},
			{Stage: "cleanup_journal_retained", Status: handoff.RemovalStageCompleted, Code: "terminal_receipt_replayable"},
		},
	}
	receipt := handoff.NewNativeRemovalReceipt(base, strings.Repeat("a", 64), handoff.NativeStateNative)
	if receipt.Operation != "remove-open-codex-native" || receipt.NativeRecoveryRequired || len(receipt.Stages) != 5 ||
		receipt.Stages[0].SubjectID != "" || receipt.Stages[1].Stage != "native_boundary_final_verification" ||
		receipt.Stages[1].Code != "native_ownership_post_package_verified" ||
		receipt.Stages[2].Code != "native_ownership_reverified" || receipt.Stages[4].Stage != "cleanup_journal_retained" ||
		receipt.Stages[4].Code != "terminal_receipt_replayable" {
		t.Fatalf("native receipt=%#v", receipt)
	}
}

func TestRetainedTerminalReplaySynthesizesExactlyOneFinalNativeProof(t *testing.T) {
	installationID := "0123456789abcdef01234567"
	candidate := handoff.NPMInstallation{
		ID: installationID, Fingerprint: strings.Repeat("a", 64),
		PackageRoot: filepath.Join(t.TempDir(), "lib", "node_modules", "@bitkyc08", "opencodex"), Launchers: []string{},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint},
		Mode:      handoff.RemovalModePreserveData, ConfirmedRemoval: true,
		Context:                  handoff.RemovalContextStandaloneNative,
		ExpectedBoundaryRevision: strings.Repeat("b", 64), ExpectedNativeState: handoff.NativeStateOpenCodex,
		NativeRestoreFingerprint: strings.Repeat("c", 64),
	}
	record, err := handoff.NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = handoff.RemovalCleanupPhasePackageVerified
	record.NativeVerifiedBoundaryRevision = strings.Repeat("d", 64)
	record.NativeState = handoff.NativeStateNative
	record.TeardownCompleted = true
	record.ExecutionAttempt = 1
	record.PackageAttempt = 1
	record.ExecutionBootSession = strings.Repeat("e", 64)
	record.RoutingRecoveryReleased = true
	record.FinalizationActive = true
	base, err := handoff.RemovedPackageCleanupReceipt(record)
	if err != nil {
		t.Fatal(err)
	}
	if !ensureStandaloneNativeFinalProof(&base) || !ensureStandaloneNativeFinalProof(&base) {
		t.Fatal("terminal replay final proof was not synthesized idempotently")
	}
	base.Stages = append(base.Stages, handoff.OpenCodexRemovalStage{
		Stage: "cleanup_journal_retained", Status: handoff.RemovalStageCompleted, Code: "terminal_receipt_replayable",
	})
	receipt, err := handoff.NewTerminalNativeRemovalReceipt(base, record.NativeOriginBoundaryRevision, record)
	if err != nil {
		t.Fatal(err)
	}
	proofs := 0
	for _, stage := range receipt.Stages {
		if stage.Stage == "native_boundary_final_verification" && stage.Status == handoff.RemovalStageCompleted && stage.Code == "native_ownership_reverified" {
			proofs++
		}
	}
	if proofs != 1 || len(receipt.Stages) < 2 || receipt.Stages[len(receipt.Stages)-1].Stage != "cleanup_journal_retained" ||
		len(receipt.TerminalReceiptDigest) != 64 {
		t.Fatalf("terminal receipt=%#v", receipt)
	}
}

func TestSafeOperationErrorMapsStandaloneNativeBoundary(t *testing.T) {
	tests := []struct {
		err    error
		code   string
		action string
	}{
		{handoff.ErrNativeRemovalBoundaryUnsafe, "native_removal_boundary_unsafe", "manual_remediation"},
		{handoff.ErrNativeRemovalBoundaryChanged, "native_removal_boundary_changed", "refresh_native_removal"},
		{handoff.ErrNativeRemovalRecoveryRequired, "native_recovery_required", "open_recovery"},
		{handoff.ErrNativeRemovalCustomCodexHome, "custom_codex_home_unsupported", "review_request"},
	}
	for _, test := range tests {
		envelope := safeOperationError(test.err)
		if envelope.Error.Code != test.code || envelope.Error.RecommendedAction != test.action {
			t.Fatalf("error=%v envelope=%#v", test.err, envelope)
		}
	}
}
