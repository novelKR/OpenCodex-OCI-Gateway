package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

type standaloneNativeBoundary struct {
	home       string
	codexPath  string
	anchorPath string
	revision   string
	state      string
	inspection codexconfig.NativeRepairInspection
	recovery   *handoff.RemovalCleanupRecord
}

func modeDiscoverOpenCodexNative(args []string) {
	requireNativeRemovalPlatform()
	flags := newModeFlagSet("mode discover-open-codex-native")
	terminalReceiptDigest := flags.String("acknowledge-terminal-receipt-digest", "", "acknowledge an exact consumed terminal receipt")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || !*jsonOutput || (*terminalReceiptDigest != "" && !routingDigest(*terminalReceiptDigest)) {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	boundary, err := openStandaloneNativeBoundary(ctx, "")
	if err != nil {
		fatal(err)
	}
	// A bare discovery never consumes a retained terminal v6 journal. Only the
	// digest returned in the already validated success receipt can acknowledge
	// that exact witness after the app has durably stored its local pending-ack
	// checkpoint. Local cleanup happens only after this acknowledgement.
	boundary, _, err = acknowledgeStandaloneNativeTerminal(ctx, boundary, *terminalReceiptDigest)
	if err != nil {
		fatal(err)
	}
	if boundary.recovery != nil {
		emitNativeJSON(handoff.NativeRemovalDiscovery{
			SchemaVersion: handoff.NativeRemovalReadSchemaVersion, Operation: "discover-open-codex-native",
			Context: handoff.RemovalContextStandaloneNative, Status: handoff.NativeRemovalStatusRecoveryRequired,
			BoundaryRevision: boundary.revision, NativeState: handoff.NativeStateUnavailable,
			NativeRecoveryRequired: true, Candidates: []handoff.NativeRemovalCandidate{},
		})
		return
	}
	options, err := standaloneNativeDiscoveryOptions(boundary)
	if err != nil {
		fatal(err)
	}
	result, err := handoff.DiscoverNPMInstallationsWithAuthority(ctx, options, options)
	if err != nil {
		fatal(err)
	}
	candidates := make([]handoff.NativeRemovalCandidate, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		candidates = append(candidates, handoff.ProjectNativeRemovalCandidate(candidate))
	}
	emitNativeJSON(handoff.NativeRemovalDiscovery{
		SchemaVersion: handoff.NativeRemovalReadSchemaVersion, Operation: "discover-open-codex-native",
		Context: handoff.RemovalContextStandaloneNative, Status: handoff.NativeRemovalStatusReady,
		BoundaryRevision: boundary.revision, NativeState: boundary.state,
		Candidates: candidates, Rejected: result.Rejected, Truncated: result.Truncated,
	})
}

func modeInspectOpenCodexNativeRemoval(args []string) {
	requireNativeRemovalPlatform()
	selection, expectedBoundary, jsonOutput := parseNativeRemovalSelection("mode inspect-open-codex-native-removal", args)
	if !jsonOutput {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	boundary, err := openStandaloneNativeBoundary(ctx, expectedBoundary)
	if err != nil {
		fatal(err)
	}
	if boundary.recovery != nil {
		emitNativeJSON(handoff.NativeRemovalInspection{
			SchemaVersion: handoff.NativeRemovalReadSchemaVersion, Operation: "inspect-open-codex-native-removal",
			Context: handoff.RemovalContextStandaloneNative, Status: handoff.NativeRemovalStatusRecoveryRequired,
			BoundaryRevision: boundary.revision, NativeState: handoff.NativeStateUnavailable,
			NativeRecoveryRequired: true,
		})
		return
	}
	resolver, _, err := standaloneNativeRemovalRuntime(boundary)
	if err != nil {
		fatal(err)
	}
	candidate, err := resolver.Resolve(ctx, selection)
	if err != nil {
		fatal(err)
	}
	projected := handoff.ProjectNativeRemovalCandidate(candidate)
	if !projected.AutomaticRemovalEligible {
		fatal(handoff.ErrRemovalManualOnly)
	}
	emitNativeJSON(handoff.NativeRemovalInspection{
		SchemaVersion: handoff.NativeRemovalReadSchemaVersion, Operation: "inspect-open-codex-native-removal",
		Context: handoff.RemovalContextStandaloneNative, Status: handoff.NativeRemovalStatusReady,
		BoundaryRevision: boundary.revision, NativeState: boundary.state, Candidate: &projected,
	})
}

func modeInspectOpenCodexNativeData(args []string) {
	requireNativeRemovalPlatform()
	selection, expectedBoundary, jsonOutput := parseNativeRemovalSelection("mode inspect-open-codex-native-data", args)
	if !jsonOutput {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	boundary, err := openStandaloneNativeBoundary(ctx, expectedBoundary)
	if err != nil {
		fatal(err)
	}
	lifecycle, err := acquireStandaloneNativeLifecycle(ctx, boundary)
	if err != nil {
		fatal(err)
	}
	defer lifecycle.Close()
	boundary, err = openStandaloneNativeBoundary(ctx, expectedBoundary)
	if err != nil {
		fatal(err)
	}
	dataRefresh := boundary.recovery != nil && standaloneNativeDataRefreshReady(*boundary.recovery, selection)
	if boundary.recovery != nil && !dataRefresh {
		base := handoff.OpenCodexDataInventoryReceipt{
			Status: "refused", InstallationID: selection.InstallationID,
			InstallationFingerprint: selection.InstallationFingerprint,
			Items:                   []handoff.OpenCodexDataInventoryItem{},
		}
		receipt := handoff.NewNativeDataInventoryReceipt(base, boundary.revision, handoff.NativeStateUnavailable, selection.NativeRestoreFingerprint)
		receipt.NativeRecoveryRequired = true
		emitNativeJSON(receipt)
		return
	}
	resolver, runner, err := standaloneNativeRemovalRuntime(boundary)
	if err != nil {
		fatal(err)
	}
	adapter := handoff.NativeRemovalResolverAdapter{Resolver: resolver, Fingerprint: selection.NativeRestoreFingerprint}
	coordinator := handoff.RemovalCoordinator{Resolver: adapter, Runner: runner}
	base, err := coordinator.Inventory(ctx, handoff.NPMRemovalSelection{
		ID: selection.InstallationID, Fingerprint: selection.InstallationFingerprint,
	}, 1)
	if err != nil {
		fatal(err)
	}
	nativeState := boundary.state
	if dataRefresh {
		nativeState = handoff.NativeStateNative
	}
	receipt := handoff.NewNativeDataInventoryReceipt(base, boundary.revision, nativeState, selection.NativeRestoreFingerprint)
	emitNativeJSON(receipt)
}

func modeRemoveOpenCodexNative(args []string) {
	requireNativeRemovalPlatform()
	flags := newModeFlagSet("mode remove-open-codex-native")
	id := flags.String("installation-id", "", "opaque OpenCodex installation ID")
	fingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex installation fingerprint")
	restoreFingerprint := flags.String("native-restore-fingerprint", "", "opaque Native restore fingerprint")
	expectedBoundary := flags.String("expected-boundary-revision", "", "reviewed standalone Native boundary")
	mode := flags.String("removal-mode", "", "preserve_data or trash_selected")
	expectedInventory := flags.String("expected-inventory-revision", "", "reviewed Native inventory revision")
	confirmedRemoval := flags.Bool("confirm-opencodex-native-removal", false, "confirm Native return and package removal")
	confirmedTrash := flags.Bool("confirm-data-trash", false, "confirm the exact selected data IDs")
	confirmedDataRefresh := flags.Bool("confirm-interrupted-data-refresh", false, "confirm refreshed data selection")
	confirmedProcessRecovery := flags.Bool("confirm-rebooted-process-recovery", false, "confirm changed-boot process recovery")
	confirmedDesktop := flags.Bool("confirm-desktop-exited", false, "confirm Codex Desktop exited")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	var dataItems stringListFlag
	flags.Var(&dataItems, "data-item", "explicit opaque inventory item ID; repeatable")
	flags.Parse(args)
	if flags.NArg() != 0 || !*jsonOutput || *id == "" || *fingerprint == "" || *restoreFingerprint == "" ||
		*expectedBoundary == "" || *mode == "" || !*confirmedRemoval || !*confirmedDesktop ||
		*confirmedDataRefresh && *confirmedProcessRecovery {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	if (*mode == string(handoff.RemovalModePreserveData) && *expectedInventory != "") ||
		(*mode == string(handoff.RemovalModeTrashSelected) && !routingDigest(*expectedInventory)) {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	items, ok := orderedUniqueNativeDataItems(dataItems)
	if !ok {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	selection := handoff.NativeRemovalSelection{
		InstallationID: *id, InstallationFingerprint: *fingerprint, NativeRestoreFingerprint: *restoreFingerprint,
	}
	ctx, stop := signalNotifyRemovalContext()
	defer stop()
	preflight, err := openStandaloneNativeBoundary(ctx, *expectedBoundary)
	if err != nil {
		fatal(err)
	}
	lifecycle, err := acquireStandaloneNativeLifecycle(ctx, preflight)
	if err != nil {
		fatal(err)
	}
	defer lifecycle.Close()
	boundary, err := openStandaloneNativeBoundary(ctx, *expectedBoundary)
	if err != nil {
		fatal(err)
	}
	resolver, runner, err := standaloneNativeRemovalRuntime(boundary)
	if err != nil {
		fatal(err)
	}
	adapter := handoff.NativeRemovalResolverAdapter{Resolver: resolver, Fingerprint: selection.NativeRestoreFingerprint}

	originState := boundary.state
	if boundary.recovery != nil {
		originState = boundary.recovery.NativeOriginState
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection: handoff.NPMRemovalSelection{ID: selection.InstallationID, Fingerprint: selection.InstallationFingerprint},
		Mode:      handoff.OpenCodexRemovalMode(*mode), DataItemIDs: append([]string(nil), items...),
		ConfirmedRemoval: *confirmedRemoval, ConfirmedTrash: *confirmedTrash,
		Context: handoff.RemovalContextStandaloneNative, ExpectedBoundaryRevision: *expectedBoundary,
		ExpectedNativeState: originState, NativeRestoreFingerprint: selection.NativeRestoreFingerprint,
		NativeInventoryRevision: *expectedInventory,
	}
	if err := handoff.ValidateOpenCodexRemovalRequest(request); err != nil {
		fatal(err)
	}

	verifiedRevision := ""
	teardownAlreadyCompleted := false
	nativeBoundaryVerified := false
	nativeAlreadyVerified := false
	if boundary.recovery != nil {
		pending, proceed, recoveryErr := reconcileStandaloneNativeRemoval(
			ctx, boundary, resolver, request, *confirmedProcessRecovery,
		)
		if recoveryErr != nil {
			fatal(recoveryErr)
		}
		if !proceed {
			emitNativeJSON(nativeRecoveryReceipt(pending, *expectedBoundary))
			return
		}
		boundary.recovery = &pending
		verifiedRevision = pending.NativeVerifiedBoundaryRevision
		teardownAlreadyCompleted = pending.TeardownCompleted
		nativeBoundaryVerified = verifiedRevision != ""
		nativeAlreadyVerified = teardownAlreadyCompleted && nativeBoundaryVerified
	}

	coordinator, closeCoordinator := standaloneNativeCoordinator(
		boundary, resolver, adapter, runner, request, &verifiedRevision,
		teardownAlreadyCompleted, nativeBoundaryVerified, nativeAlreadyVerified,
	)
	defer closeCoordinator()
	validatedInventory := func(nativeState string) handoff.OpenCodexDataInventoryReceipt {
		baseInventory, inventoryErr := coordinator.Inventory(ctx, request.Selection, 1)
		if inventoryErr != nil {
			fatal(inventoryErr)
		}
		nativeInventory := handoff.NewNativeDataInventoryReceipt(
			baseInventory, *expectedBoundary, nativeState, selection.NativeRestoreFingerprint,
		)
		if !constantHexEqual(nativeInventory.InventoryRevision, *expectedInventory) {
			fatal(handoff.ErrNativeRemovalBoundaryChanged)
		}
		request.ExpectedRoutingGeneration = 1
		request.ExpectedInventoryRevision = baseInventory.InventoryRevision
		if !handoff.ValidateOpenCodexInventoryForRequest(baseInventory, request) {
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
		return baseInventory
	}

	if boundary.recovery != nil && boundary.recovery.Phase == handoff.RemovalCleanupPhaseDataRefresh &&
		!*confirmedDataRefresh {
		if !handoff.RemovalCleanupMatchesRequest(*boundary.recovery, request) {
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
		emitStandaloneNativeDataRefreshReceipt(*boundary.recovery, *expectedBoundary)
		return
	}

	needsInventory := request.Mode == handoff.RemovalModeTrashSelected &&
		(boundary.recovery == nil || boundary.recovery.Phase == handoff.RemovalCleanupPhaseIntent)
	if needsInventory {
		_ = validatedInventory(originState)
	}

	if boundary.recovery != nil {
		pending := *boundary.recovery
		if !standaloneNativeJournalAuthorityMatches(pending, request) {
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
		if err := handoff.WriteRemovalCleanup(boundary.anchorPath, pending); err != nil {
			fatal(err)
		}
		switch pending.Phase {
		case handoff.RemovalCleanupPhaseDataOutcome:
			if pending.DataOutcome != "completed" {
				marked, err := handoff.MarkRemovalDataRefreshRequired(boundary.anchorPath)
				if err != nil {
					fatal(err)
				}
				pending = marked
				if !*confirmedDataRefresh {
					emitStandaloneNativeDataRefreshReceipt(pending, *expectedBoundary)
					return
				}
				baseInventory := validatedInventory(handoff.NativeStateNative)
				if _, err := handoff.SupersedeRemovalDataRefreshRequired(
					boundary.anchorPath, request, baseInventory, true,
				); err != nil {
					fatal(err)
				}
				coordinator.NativeAlreadyVerified = true
				base := coordinator.Remove(ctx, request)
				emitFinalStandaloneNativeReceipt(boundary, base, *expectedBoundary)
				return
			} else {
				if !handoff.RemovalCleanupMatchesRequest(pending, request) {
					fatal(handoff.ErrRemovalCleanupUnsafe)
				}
				advanced, err := handoff.AdvanceRemovalDataOutcomeToPackagePending(boundary.anchorPath)
				if err != nil {
					fatal(err)
				}
				pending = advanced
			}
		case handoff.RemovalCleanupPhaseDataRefresh:
			if !*confirmedDataRefresh {
				emitStandaloneNativeDataRefreshReceipt(pending, *expectedBoundary)
				return
			}
			baseInventory := validatedInventory(handoff.NativeStateNative)
			if _, err := handoff.SupersedeRemovalDataRefreshRequired(
				boundary.anchorPath, request, baseInventory, true,
			); err != nil {
				fatal(err)
			}
			coordinator.NativeAlreadyVerified = true
			base := coordinator.Remove(ctx, request)
			emitFinalStandaloneNativeReceipt(boundary, base, *expectedBoundary)
			return
		case handoff.RemovalCleanupPhasePackageInFlight:
			fatal(handoff.ErrNativeRemovalRecoveryRequired)
		case handoff.RemovalCleanupPhasePackagePending, handoff.RemovalCleanupPhasePackageVerified:
			if !handoff.RemovalCleanupMatchesRequest(pending, request) {
				fatal(handoff.ErrRemovalCleanupUnsafe)
			}
			if pending.Phase == handoff.RemovalCleanupPhasePackageVerified && handoff.VerifyRemovalCleanupAbsent(pending) == nil {
				base, err := handoff.RemovedPackageCleanupReceipt(pending)
				if err != nil {
					fatal(err)
				}
				emitFinalStandaloneNativeReceipt(boundary, base, *expectedBoundary)
				return
			}
			base := coordinator.ResumePackageRemoval(ctx, pending)
			emitFinalStandaloneNativeReceipt(boundary, base, *expectedBoundary)
			return
		case handoff.RemovalCleanupPhaseIntent:
			if !handoff.RemovalCleanupMatchesRequest(pending, request) {
				fatal(handoff.ErrRemovalCleanupUnsafe)
			}
			coordinator.NativeAlreadyVerified = pending.NativeVerifiedBoundaryRevision != ""
		default:
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
	}

	base := coordinator.Remove(ctx, request)
	emitFinalStandaloneNativeReceipt(boundary, base, *expectedBoundary)
}

func standaloneNativeCoordinator(
	boundary *standaloneNativeBoundary,
	resolver handoff.DiscoveryNativeRemovalResolver,
	adapter handoff.NativeRemovalResolverAdapter,
	runner handoff.ExactNPMRunner,
	request handoff.OpenCodexRemovalRequest,
	verifiedRevision *string,
	teardownAlreadyCompleted bool,
	nativeBoundaryVerified bool,
	nativeAlreadyVerified bool,
) (handoff.RemovalCoordinator, func()) {
	var ownerSession *handoff.NativeRestoreSession
	closeOwnerSession := func() {
		if ownerSession != nil {
			ownerSession.Close()
			ownerSession = nil
		}
	}
	ownerExecutor := handoff.NativeRestoreExecutor{HomeDir: boundary.home}
	ownerSelection := handoff.NativeRemovalSelection{
		InstallationID: request.Selection.ID, InstallationFingerprint: request.Selection.Fingerprint,
		NativeRestoreFingerprint: request.NativeRestoreFingerprint,
	}
	verifyOwnerIntegration := func(ctx context.Context) error {
		if ownerSession == nil {
			candidate, err := resolver.Resolve(ctx, ownerSelection)
			if err != nil {
				return handoff.ErrNativeRemovalBoundaryChanged
			}
			session, err := ownerExecutor.Open(ctx, candidate, boundary.codexPath)
			if err != nil {
				return handoff.ErrNativeRemovalBoundaryChanged
			}
			ownerSession = session
		}
		if err := ownerSession.VerifyIntegrationDisabled(ctx); err != nil {
			return handoff.ErrNativeRemovalBoundaryChanged
		}
		return nil
	}
	verifyCurrent := func(ctx context.Context) error {
		state, revision, _, err := inspectCurrentStandaloneNative(boundary)
		if err != nil {
			return err
		}
		if verifiedRevision != nil && *verifiedRevision != "" {
			if state != handoff.NativeStateNative || !constantHexEqual(revision, *verifiedRevision) {
				return handoff.ErrNativeRemovalBoundaryChanged
			}
			return verifyOwnerIntegration(ctx)
		}
		if state != request.ExpectedNativeState || !constantHexEqual(revision, request.ExpectedBoundaryRevision) {
			return handoff.ErrNativeRemovalBoundaryChanged
		}
		return nil
	}
	verifyPostTeardown := func(ctx context.Context) error {
		if err := verifyOwnerIntegration(ctx); err != nil {
			return err
		}
		state, _, _, err := inspectCurrentStandaloneNative(boundary)
		if err != nil || state != handoff.NativeStateNative {
			return handoff.ErrNativeRemovalBoundaryChanged
		}
		return nil
	}
	runner.BeforeOCXMutation = verifyCurrent
	runner.BeforeUninstallCandidate = adapter.Revalidate
	runner.BeforeUninstall = verifyCurrent
	coordinator := handoff.RemovalCoordinator{
		Resolver: adapter, Runner: runner, VerifyRouting: verifyCurrent,
		VerifyPostTeardown:       verifyPostTeardown,
		TeardownAlreadyCompleted: teardownAlreadyCompleted,
		NativeBoundaryVerified:   nativeBoundaryVerified,
		NativeAlreadyVerified:    nativeAlreadyVerified,
		CheckAdmission:           func(context.Context) error { return nil },
		CheckResumeAdmission: func(context.Context) error {
			return handoff.RemovalPackageResumeAdmission(boundary.anchorPath)
		},
		MarkRoutingRecovery: func() error { return handoff.MarkStandaloneNativeRecovery(boundary.anchorPath) },
		PrepareOperation: func(_ context.Context, candidate handoff.NPMInstallation, value handoff.OpenCodexRemovalRequest) error {
			_, err := handoff.EnsureRemovalIntent(boundary.anchorPath, candidate, value)
			return err
		},
		RecordDataOutcome: func(_ context.Context, candidate handoff.NPMInstallation, value handoff.OpenCodexRemovalRequest, moved int, status string) error {
			_, err := handoff.RecordRemovalDataOutcome(boundary.anchorPath, candidate, value, moved, status)
			return err
		},
		MarkDataRefresh: func(context.Context) error {
			_, err := handoff.MarkRemovalDataRefreshRequired(boundary.anchorPath)
			return err
		},
		PreparePackageRemoval: func(_ context.Context, candidate handoff.NPMInstallation, value handoff.OpenCodexRemovalRequest, moved int) error {
			_, err := handoff.PrepareRemovalPackageCleanup(boundary.anchorPath, candidate, value, moved)
			return err
		},
		BeginExecution: func(ctx context.Context, kind handoff.RemovalExecutionKind) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := handoff.BeginExecution(boundary.anchorPath, kind)
			return err
		},
		FinishExecution: func(_ context.Context, kind handoff.RemovalExecutionKind, result handoff.RemovalExecutionResult) error {
			_, err := handoff.FinishExecution(boundary.anchorPath, kind, result)
			return err
		},
		ResolveExecution: func(ctx context.Context, kind handoff.RemovalExecutionKind, resolution handoff.RemovalExecutionResolution, recovery bool) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if _, err := handoff.MarkRemovalExecutionResolution(boundary.anchorPath, kind, resolution, recovery); err != nil {
				return false, err
			}
			var mark func() error
			if recovery {
				mark = func() error { return handoff.MarkStandaloneNativeRecovery(boundary.anchorPath) }
			}
			_, resolved, err := handoff.ResumeRemovalExecutionResolution(boundary.anchorPath, mark)
			return recovery && resolved && err == nil, err
		},
		CompleteTeardown: func(ctx context.Context, candidate handoff.NPMInstallation, result handoff.RemovalExecutionResult) error {
			if err := resolver.Revalidate(ctx, candidate); err != nil {
				return err
			}
			if err := verifyPostTeardown(ctx); err != nil {
				return err
			}
			state, revision, _, err := inspectCurrentStandaloneNative(boundary)
			if err != nil || state != handoff.NativeStateNative {
				return handoff.ErrNativeRemovalBoundaryChanged
			}
			if _, err := handoff.CompleteStandaloneTeardown(boundary.anchorPath, result, revision); err != nil {
				return err
			}
			*verifiedRevision = revision
			return nil
		},
		RestoreNative: func(ctx context.Context, candidate handoff.NPMInstallation) (handoff.NativeRestoreResult, error) {
			if err := resolver.Revalidate(ctx, candidate); err != nil {
				return handoff.NativeRestoreResult{}, err
			}
			closeOwnerSession()
			session, err := ownerExecutor.Open(ctx, candidate, boundary.codexPath)
			if err != nil {
				return handoff.NativeRestoreResult{}, err
			}
			result, err := session.Execute(ctx)
			if err != nil {
				session.Close()
				return handoff.NativeRestoreResult{}, err
			}
			ownerSession = session
			return result, nil
		},
		CompleteNativeRestore: func(ctx context.Context, _ handoff.NPMInstallation, _ handoff.NativeRestoreResult) error {
			if err := verifyOwnerIntegration(ctx); err != nil {
				return err
			}
			state, revision, _, err := inspectCurrentStandaloneNative(boundary)
			if err != nil || state != handoff.NativeStateNative {
				return handoff.ErrNativeRemovalBoundaryChanged
			}
			completed := handoff.RemovalExecutionResult{Started: true, CleanupVerified: true}
			if _, err := handoff.CompleteStandaloneNativeRestore(boundary.anchorPath, completed, revision); err != nil {
				return err
			}
			*verifiedRevision = revision
			return nil
		},
	}
	return coordinator, closeOwnerSession
}

func inspectCurrentStandaloneNative(boundary *standaloneNativeBoundary) (string, string, codexconfig.NativeRepairInspection, error) {
	if boundary == nil || ensureStandaloneRelayAssetsAbsent(boundary.home) != nil {
		return "", "", codexconfig.NativeRepairInspection{}, handoff.ErrNativeRemovalBoundaryChanged
	}
	inspection, err := codexconfig.InspectNativeRepairForOwner(boundary.codexPath, codexconfig.ProductionOwner)
	if err != nil {
		return "", "", codexconfig.NativeRepairInspection{}, handoff.ErrNativeRemovalBoundaryChanged
	}
	state := ""
	switch inspection.Kind {
	case codexconfig.NativeRepairStateOnly:
		state = handoff.NativeStateNative
	case codexconfig.NativeRepairOpenCodex:
		state = handoff.NativeStateOpenCodex
	default:
		return "", "", inspection, handoff.ErrNativeRemovalBoundaryChanged
	}
	revision, err := codexconfig.NativeRepairBoundaryRevision(boundary.codexPath, codexconfig.ProductionOwner, inspection)
	if err != nil {
		return "", "", inspection, handoff.ErrNativeRemovalBoundaryChanged
	}
	ownerRevision, err := handoff.OpenCodexOwnerConfigurationRevision(boundary.home)
	if err != nil {
		return "", "", inspection, handoff.ErrNativeRemovalBoundaryChanged
	}
	return state, standaloneNativeBoundaryRevision(revision, ownerRevision), inspection, nil
}

func reconcileStandaloneNativeRemoval(
	ctx context.Context,
	boundary *standaloneNativeBoundary,
	resolver handoff.DiscoveryNativeRemovalResolver,
	request handoff.OpenCodexRemovalRequest,
	confirmedReboot bool,
) (handoff.RemovalCleanupRecord, bool, error) {
	if boundary == nil || boundary.recovery == nil {
		return handoff.RemovalCleanupRecord{}, true, nil
	}
	record := *boundary.recovery
	if !standaloneNativeJournalAuthorityMatches(record, request) {
		return record, false, handoff.ErrRemovalCleanupUnsafe
	}
	if (record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending ||
		record.NativeRecoveryRequired) && !handoff.RemovalCleanupMatchesRequest(record, request) {
		return record, false, handoff.ErrRemovalCleanupUnsafe
	}
	verifyNativeBoundary := func() (string, error) {
		candidate, err := resolver.Resolve(ctx, handoff.NativeRemovalSelection{
			InstallationID: request.Selection.ID, InstallationFingerprint: request.Selection.Fingerprint,
			NativeRestoreFingerprint: request.NativeRestoreFingerprint,
		})
		if err != nil {
			return "", err
		}
		session, err := (handoff.NativeRestoreExecutor{HomeDir: boundary.home}).Open(ctx, candidate, boundary.codexPath)
		if err != nil {
			return "", err
		}
		defer session.Close()
		if err := session.VerifyIntegrationDisabled(ctx); err != nil {
			return "", err
		}
		state, revision, _, err := inspectCurrentStandaloneNative(boundary)
		if err != nil || state != handoff.NativeStateNative {
			return "", handoff.ErrNativeRemovalBoundaryChanged
		}
		return revision, nil
	}
	if record.ExecutionResolution != "" {
		if _, resolved, err := handoff.ResumeRemovalExecutionResolution(boundary.anchorPath, func() error {
			return handoff.MarkStandaloneNativeRecovery(boundary.anchorPath)
		}); err != nil || !resolved {
			return record, false, handoff.ErrNativeRemovalRecoveryRequired
		}
		updated, _, err := handoff.ReadRemovalCleanup(boundary.anchorPath)
		return updated, false, err
	}
	if record.ActiveExecution != nil {
		if !confirmedReboot {
			return record, false, nil
		}
		switch record.ActiveExecution.Kind {
		case handoff.RemovalExecutionNativeRestore:
			revision, err := verifyNativeBoundary()
			if err != nil {
				return record, false, handoff.ErrNativeRemovalRecoveryRequired
			}
			updated, err := handoff.ReconcileStandaloneNativeRestoreAfterBoot(boundary.anchorPath, revision, true)
			if err != nil {
				return record, false, err
			}
			record = updated
		case handoff.RemovalExecutionTeardown:
			state, revision, _, inspectErr := inspectCurrentStandaloneNative(boundary)
			if inspectErr == nil && state == record.NativeOriginState &&
				constantHexEqual(revision, record.NativeOriginBoundaryRevision) {
				updated, _, err := handoff.ReconcileActiveExecutionAfterBoot(boundary.anchorPath, true)
				if err != nil {
					return record, false, err
				}
				record = updated
				break
			}
			verified, err := verifyNativeBoundary()
			if err != nil {
				return record, false, handoff.ErrNativeRemovalRecoveryRequired
			}
			updated, err := handoff.ReconcileStandaloneTeardownAfterBoot(
				boundary.anchorPath, record, verified, true,
			)
			if err != nil {
				return record, false, err
			}
			record = updated
		default:
			updated, _, err := handoff.ReconcileActiveExecutionAfterBoot(boundary.anchorPath, true)
			if err != nil {
				return record, false, err
			}
			record = updated
		}
	}
	if record.RecoveryPending {
		updated, err := handoff.ReleaseRemovalRoutingGateForRecovery(boundary.anchorPath)
		if err != nil {
			return record, false, err
		}
		record = updated
	}
	if record.NativeRecoveryRequired {
		state, revision, _, err := inspectCurrentStandaloneNative(boundary)
		if err != nil {
			return record, false, nil
		}
		if record.NativeVerifiedBoundaryRevision != "" {
			if state != handoff.NativeStateNative || !constantHexEqual(revision, record.NativeVerifiedBoundaryRevision) {
				return record, false, nil
			}
			verified, verifyErr := verifyNativeBoundary()
			if verifyErr != nil || !constantHexEqual(verified, record.NativeVerifiedBoundaryRevision) {
				return record, false, nil
			}
		} else if state == record.NativeOriginState && constantHexEqual(revision, record.NativeOriginBoundaryRevision) {
			// A pre-start refusal left the exact reviewed boundary unchanged.
		} else {
			verified, verifyErr := verifyNativeBoundary()
			if verifyErr != nil {
				return record, false, nil
			}
			updated, recoverErr := handoff.RecoverStandaloneTeardownNativeBoundary(
				boundary.anchorPath, record, verified,
			)
			if recoverErr != nil {
				return record, false, recoverErr
			}
			record = updated
			return record, true, nil
		}
		updated, err := handoff.ClearStandaloneNativeRecovery(boundary.anchorPath)
		if err != nil {
			return record, false, err
		}
		record = updated
	}
	if record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending || record.NativeRecoveryRequired {
		return record, false, nil
	}
	return record, true, nil
}

func nativeRecoveryReceipt(record handoff.RemovalCleanupRecord, boundaryRevision string) handoff.NativeRemovalReceipt {
	dataScope := "preserved"
	if record.Mode == handoff.RemovalModeTrashSelected {
		dataScope = "explicit_items_only"
	}
	base := handoff.OpenCodexRemovalReceipt{
		SchemaVersion: handoff.OpenCodexRemovalSchemaVersion, Operation: "remove-open-codex",
		Status: handoff.RemovalStatusPartial, Mode: record.Mode, InstallationID: record.InstallationID,
		DataScope: dataScope, SelectedDataItems: record.SelectedDataItems, MovedDataItems: record.MovedDataItems,
		DataMovementUnknown:     record.Phase == handoff.RemovalCleanupPhaseDataRefresh,
		RoutingRecoveryRequired: true,
		Stages: []handoff.OpenCodexRemovalStage{{
			Stage: "routing_recovery", Status: handoff.RemovalStageFailed, Code: "native_recovery_required",
		}},
	}
	return handoff.NewNativeRemovalReceipt(base, boundaryRevision, handoff.NativeStateUnavailable)
}

func emitStandaloneNativeDataRefreshReceipt(record handoff.RemovalCleanupRecord, boundaryRevision string) {
	base, err := handoff.InterruptedRemovalDataRefreshReceipt(record)
	if err != nil {
		base, err = handoff.RecordedRemovalDataRefreshReceipt(record)
	}
	if err != nil {
		fatal(err)
	}
	emitNativeJSON(handoff.NewNativeRemovalReceipt(base, boundaryRevision, handoff.NativeStateNative))
}

func standaloneNativeJournalAuthorityMatches(record handoff.RemovalCleanupRecord, request handoff.OpenCodexRemovalRequest) bool {
	return record.Context == handoff.RemovalContextStandaloneNative &&
		record.InstallationID == request.Selection.ID &&
		record.Fingerprint == request.Selection.Fingerprint &&
		record.NativeOriginBoundaryRevision == request.ExpectedBoundaryRevision &&
		record.NativeOriginState == request.ExpectedNativeState &&
		record.NativeRestoreFingerprint == request.NativeRestoreFingerprint
}

func standaloneNativeDataRefreshReady(record handoff.RemovalCleanupRecord, selection handoff.NativeRemovalSelection) bool {
	return record.Context == handoff.RemovalContextStandaloneNative &&
		record.Phase == handoff.RemovalCleanupPhaseDataRefresh &&
		record.ActiveExecution == nil && record.ExecutionResolution == "" &&
		!record.RecoveryPending && !record.NativeRecoveryRequired &&
		record.NativeState == handoff.NativeStateNative && record.NativeVerifiedBoundaryRevision != "" &&
		record.InstallationID == selection.InstallationID &&
		record.Fingerprint == selection.InstallationFingerprint &&
		record.NativeRestoreFingerprint == selection.NativeRestoreFingerprint
}

func emitFinalStandaloneNativeReceipt(boundary *standaloneNativeBoundary, base handoff.OpenCodexRemovalReceipt, boundaryRevision string) {
	nativeState := handoff.NativeStateUnavailable
	currentRevision := ""
	var terminalRecord *handoff.RemovalCleanupRecord
	if state, revision, _, err := inspectCurrentStandaloneNative(boundary); err == nil {
		nativeState = state
		currentRevision = revision
	}
	if base.Status == handoff.RemovalStatusCompleted && base.PackageRemoved && !base.RoutingRecoveryRequired {
		record, exists, err := handoff.ReadRemovalCleanup(boundary.anchorPath)
		if err != nil || !exists || record.NativeVerifiedBoundaryRevision == "" ||
			nativeState != handoff.NativeStateNative ||
			!constantHexEqual(currentRevision, record.NativeVerifiedBoundaryRevision) {
			base.Status = handoff.RemovalStatusPartial
			base.Stages = append(base.Stages, handoff.OpenCodexRemovalStage{
				Stage: "routing_final_verification", Status: handoff.RemovalStageFailed, Code: "routing_ownership_changed",
			})
			appendStandaloneNativeRecovery(&base, boundary)
		} else if record.InstallationID != base.InstallationID ||
			handoff.VerifyRemovalCleanupAbsent(record) != nil {
			base.Status = handoff.RemovalStatusPartial
			appendStandaloneNativeRecovery(&base, boundary)
		} else if _, err := handoff.ReleaseRemovalRoutingGateForFinalization(boundary.anchorPath); err != nil {
			base.Status = handoff.RemovalStatusPartial
			appendStandaloneNativeRecovery(&base, boundary)
		} else if terminal, exists, err := handoff.ReadRemovalCleanup(boundary.anchorPath); err != nil || !exists ||
			!handoff.StandaloneTerminalRemovalReplayReady(terminal) ||
			terminal.InstallationID != base.InstallationID ||
			!standaloneNativeTerminalBoundaryMatches(boundary, terminal) {
			base.Status = handoff.RemovalStatusPartial
			appendStandaloneNativeRecovery(&base, boundary)
		} else if !ensureStandaloneNativeFinalProof(&base) {
			base.Status = handoff.RemovalStatusPartial
			appendStandaloneNativeRecovery(&base, boundary)
		} else {
			retained := terminal
			terminalRecord = &retained
			base.Stages = append(base.Stages, handoff.OpenCodexRemovalStage{
				Stage: "cleanup_journal_retained", Status: handoff.RemovalStageCompleted, Code: "terminal_receipt_replayable",
			})
		}
	}
	if terminalRecord != nil {
		receipt, err := handoff.NewTerminalNativeRemovalReceipt(base, boundaryRevision, *terminalRecord)
		if err != nil {
			fatal(err)
		}
		emitNativeJSON(receipt)
		return
	}
	emitNativeJSON(handoff.NewNativeRemovalReceipt(base, boundaryRevision, nativeState))
}

func ensureStandaloneNativeFinalProof(base *handoff.OpenCodexRemovalReceipt) bool {
	if base == nil {
		return false
	}
	proofs := 0
	for _, stage := range base.Stages {
		if stage.Stage != "routing_final_verification" {
			continue
		}
		proofs++
		if stage.Status != handoff.RemovalStageCompleted || stage.Code != "routing_ownership_reverified" || stage.SubjectID != "" {
			return false
		}
	}
	if proofs > 1 {
		return false
	}
	if proofs == 0 {
		base.Stages = append(base.Stages, handoff.OpenCodexRemovalStage{
			Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified",
		})
	}
	return true
}

func standaloneNativeTerminalBoundaryMatches(boundary *standaloneNativeBoundary, record handoff.RemovalCleanupRecord) bool {
	if boundary == nil || !handoff.StandaloneTerminalRemovalReplayReady(record) {
		return false
	}
	state, revision, _, err := inspectCurrentStandaloneNative(boundary)
	return err == nil && state == handoff.NativeStateNative &&
		constantHexEqual(revision, record.NativeVerifiedBoundaryRevision) &&
		handoff.VerifyRemovalCleanupAbsent(record) == nil
}

func acknowledgeStandaloneNativeTerminal(
	ctx context.Context,
	boundary *standaloneNativeBoundary,
	receiptDigest string,
) (*standaloneNativeBoundary, bool, error) {
	if boundary == nil {
		return boundary, false, handoff.ErrInvalidRemovalRequest
	}
	if receiptDigest == "" || boundary.recovery == nil || !handoff.StandaloneTerminalRemovalReplayReady(*boundary.recovery) {
		return boundary, false, nil
	}
	if !routingDigest(receiptDigest) {
		return boundary, false, handoff.ErrInvalidRemovalRequest
	}
	lifecycle, err := acquireStandaloneNativeLifecycle(ctx, boundary)
	if err != nil {
		return boundary, false, err
	}
	defer lifecycle.Close()

	current, err := openStandaloneNativeBoundary(ctx, "")
	if err != nil {
		return boundary, false, err
	}
	if current.recovery == nil || !handoff.StandaloneTerminalRemovalReplayReady(*current.recovery) ||
		!standaloneNativeTerminalBoundaryMatches(current, *current.recovery) {
		return current, false, nil
	}
	acknowledged, err := handoff.AcknowledgeStandaloneTerminalRemoval(current.anchorPath, receiptDigest)
	if err != nil || !acknowledged {
		return current, false, err
	}
	ready, err := openStandaloneNativeBoundary(ctx, "")
	if err != nil {
		return current, false, err
	}
	return ready, true, nil
}

func appendStandaloneNativeRecovery(base *handoff.OpenCodexRemovalReceipt, boundary *standaloneNativeBoundary) {
	if base == nil || base.RoutingRecoveryRequired {
		return
	}
	base.RoutingRecoveryRequired = true
	status := handoff.RemovalStageFailed
	code := "routing_recovery_persist_failed"
	if boundary != nil && handoff.MarkStandaloneNativeRecovery(boundary.anchorPath) == nil {
		status = handoff.RemovalStageCompleted
		code = "routing_recovery_persisted"
	}
	base.Stages = append(base.Stages, handoff.OpenCodexRemovalStage{
		Stage: "routing_recovery", Status: status, Code: code,
	})
}

func signalNotifyRemovalContext() (context.Context, context.CancelFunc) {
	signaled, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signaled, openCodexRemovalTimeout)
	return ctx, func() {
		cancel()
		stop()
	}
}

func orderedUniqueNativeDataItems(values []string) ([]string, bool) {
	seen := make(map[string]struct{}, len(values))
	result := append([]string(nil), values...)
	for _, value := range result {
		if _, exists := seen[value]; exists {
			return nil, false
		}
		seen[value] = struct{}{}
	}
	return result, true
}

func parseNativeRemovalSelection(name string, args []string) (handoff.NativeRemovalSelection, string, bool) {
	flags := newModeFlagSet(name)
	id := flags.String("installation-id", "", "opaque OpenCodex installation ID")
	fingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex installation fingerprint")
	restore := flags.String("native-restore-fingerprint", "", "opaque Native restore fingerprint")
	boundary := flags.String("expected-boundary-revision", "", "reviewed standalone Native boundary")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	selection := handoff.NativeRemovalSelection{
		InstallationID: *id, InstallationFingerprint: *fingerprint, NativeRestoreFingerprint: *restore,
	}
	if flags.NArg() != 0 || *id == "" || *fingerprint == "" || *restore == "" || *boundary == "" {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	return selection, *boundary, *jsonOutput
}

func requireNativeRemovalPlatform() {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(errors.New("standalone Native OpenCodex removal is supported only on macOS Apple Silicon"))
	}
}

func openStandaloneNativeBoundary(ctx context.Context, expectedRevision string) (*standaloneNativeBoundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	home, err = filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil || !filepath.IsAbs(home) {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	// The standalone contract owns only the canonical ~/.config Relay roots.
	// An ambient XDG override could hide a live production transaction from the
	// absence check, so it is not eligible for automatic Native removal.
	if os.Getenv("XDG_CONFIG_HOME") != "" || os.Getenv("OPENCODEX_HOME") != "" {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	codexHome := filepath.Join(home, ".codex")
	if value := os.Getenv("CODEX_HOME"); value != "" {
		value = filepath.Clean(value)
		resolved, resolveErr := filepath.EvalSymlinks(value)
		if resolveErr != nil || resolved != codexHome {
			return nil, handoff.ErrNativeRemovalCustomCodexHome
		}
	}
	if err := validateStandaloneCodexHome(codexHome); err != nil {
		return nil, err
	}
	codexPath := filepath.Join(codexHome, "config.toml")
	if err := ensureStandaloneRelayAssetsAbsent(home); err != nil {
		return nil, err
	}
	anchor, err := handoff.StandaloneRemovalAnchorPath(home)
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	record, exists, err := handoff.ReadRemovalCleanup(anchor)
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	if exists {
		boundary := &standaloneNativeBoundary{
			home: home, codexPath: codexPath, anchorPath: anchor,
			revision: record.NativeOriginBoundaryRevision, state: handoff.NativeStateUnavailable,
			recovery: &record,
		}
		if expectedRevision != "" && !constantHexEqual(expectedRevision, boundary.revision) {
			return nil, handoff.ErrNativeRemovalBoundaryChanged
		}
		return boundary, nil
	}
	inspection, err := codexconfig.InspectNativeRepairForOwner(codexPath, codexconfig.ProductionOwner)
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	state := ""
	switch inspection.Kind {
	case codexconfig.NativeRepairStateOnly:
		state = handoff.NativeStateNative
	case codexconfig.NativeRepairOpenCodex:
		state = handoff.NativeStateOpenCodex
	default:
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	revision, err := codexconfig.NativeRepairBoundaryRevision(codexPath, codexconfig.ProductionOwner, inspection)
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	ownerRevision, err := handoff.OpenCodexOwnerConfigurationRevision(home)
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	revision = standaloneNativeBoundaryRevision(revision, ownerRevision)
	boundary := &standaloneNativeBoundary{
		home: home, codexPath: codexPath, anchorPath: anchor,
		revision: revision, state: state, inspection: inspection,
	}
	if expectedRevision != "" && !constantHexEqual(expectedRevision, revision) {
		return nil, handoff.ErrNativeRemovalBoundaryChanged
	}
	return boundary, nil
}

func validateStandaloneCodexHome(path string) error {
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return handoff.ErrNativeRemovalBoundaryUnsafe
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return handoff.ErrNativeRemovalBoundaryUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return handoff.ErrNativeRemovalBoundaryUnsafe
	}
	return nil
}

func ensureStandaloneRelayAssetsAbsent(home string) error {
	productionConfig := filepath.Join(home, ".config", "opencodex-relay", "relay.json")
	developmentConfig := filepath.Join(home, ".config", "opencodex-relay", "relay-dev", "relay.json")
	paths := []string{
		productionConfig, routing.StatePath(productionConfig), routing.InitializedPath(productionConfig),
		routing.TransactionPath(productionConfig), routing.MaintenancePath(productionConfig), handoff.EnrollmentPath(productionConfig), handoff.RemovalCleanupPath(productionConfig),
		developmentConfig, routing.StatePath(developmentConfig), routing.InitializedPath(developmentConfig),
		routing.TransactionPath(developmentConfig), routing.MaintenancePath(developmentConfig), handoff.EnrollmentPath(developmentConfig), handoff.RemovalCleanupPath(developmentConfig),
		filepath.Join(home, ".local", "lib", "opencodex-relay", "relay"),
		filepath.Join(home, ".local", "lib", "opencodex-relay", "relay-dev"),
		filepath.Join(home, "Library", "LaunchAgents", "io.github.novelkr.opencodex-relay.plist"),
		filepath.Join(home, "Library", "LaunchAgents", "io.github.novelkr.opencodex-relay.dev.plist"),
		filepath.Join(home, "Library", "Application Support", "OpenCodexRelay", "routing-binding.json"),
		filepath.Join(home, "Library", "Application Support", "OpenCodexRelay", "integration-journal.json"),
		filepath.Join(home, "Library", "Application Support", "OpenCodexRelayDev", "routing-binding.json"),
		filepath.Join(home, "Library", "Application Support", "OpenCodexRelayDev", "integration-journal.json"),
		filepath.Join(home, ".codex", "opencodex-relay-interactive.config.toml"),
		filepath.Join(home, ".codex", "opencodex-relay-local-catalog.json"),
		filepath.Join(home, ".codex", "opencodex-relay-local-catalog.json.restart-pending"),
		filepath.Join(home, ".codex", config.LocalDevelopmentLocalCatalog),
		filepath.Join(home, ".codex", config.LocalDevelopmentLocalCatalog+config.CatalogRestartPendingSuffix),
		filepath.Join(home, ".config", "opencodex-relay", "credentials.env"),
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return handoff.ErrNativeRemovalBoundaryUnsafe
		}
	}
	return nil
}

func standaloneNativeDiscoveryOptions(boundary *standaloneNativeBoundary) (handoff.DiscoveryOptions, error) {
	if boundary == nil {
		return handoff.DiscoveryOptions{}, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	options, err := handoff.ProductionDiscoveryOptions(filepath.Join(boundary.home, ".config", "opencodex-relay", "relay.json"))
	if err != nil {
		return handoff.DiscoveryOptions{}, err
	}
	options.Tier = handoff.DiscoveryTierB
	options.GOOS = runtime.GOOS
	options.GOARCH = runtime.GOARCH
	options.HomeDir = boundary.home
	options.PathEnv = ""
	options.Getenv = func(string) string { return "" }
	return options, nil
}

func standaloneNativeRemovalRuntime(boundary *standaloneNativeBoundary) (handoff.DiscoveryNativeRemovalResolver, handoff.ExactNPMRunner, error) {
	options, err := standaloneNativeDiscoveryOptions(boundary)
	if err != nil {
		return handoff.DiscoveryNativeRemovalResolver{}, handoff.ExactNPMRunner{}, err
	}
	return handoff.NewDiscoveryNativeRemovalResolver(options), handoff.ExactNPMRunner{
		HomeDir: boundary.home, CodexConfigPath: boundary.codexPath,
	}, nil
}

func acquireStandaloneNativeLifecycle(ctx context.Context, boundary *standaloneNativeBoundary) (*lifecyclelock.Lock, error) {
	if boundary == nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	lock, err := lifecyclelock.AcquireWriter(ctx, boundary.home, "")
	if err != nil {
		return nil, handoff.ErrNativeRemovalBoundaryUnsafe
	}
	return lock, nil
}

func emitNativeJSON(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fatal(err)
	}
}

func constantHexEqual(left, right string) bool {
	if !routingDigest(left) || !routingDigest(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func standaloneNativeBoundaryRevision(codexRevision, ownerRevision string) string {
	if !routingDigest(codexRevision) || !routingDigest(ownerRevision) {
		return ""
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("opencodex-standalone-native-composite-boundary-v1\x00"))
	_, _ = hash.Write([]byte(codexRevision))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(ownerRevision))
	return hex.EncodeToString(hash.Sum(nil))
}
