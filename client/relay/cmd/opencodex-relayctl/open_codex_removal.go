package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

const openCodexRemovalTimeout = 3 * time.Minute

func modeInspectOpenCodexData(args []string) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(errorsNew("OpenCodex data inventory is supported only on macOS Apple Silicon"))
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode inspect-open-codex-data")
	installationID := flags.String("installation-id", "", "opaque OpenCodex discovery candidate ID")
	installationFingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex discovery candidate fingerprint")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path used only for exact enrollment rediscovery")
	flags.Parse(args)
	if flags.NArg() != 0 || *installationID == "" || *installationFingerprint == "" {
		fatal(handoff.ErrInvalidRemovalRequest)
	}

	resolver, runner, err := removalRuntime(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, err := routing.Open(configPath)
	if err != nil {
		fatal(err)
	}
	lock, err := store.Lock(ctx)
	if err != nil {
		fatal(err)
	}
	defer lock.Close()
	if err := handoff.RemovalExecutionAdmission(configPath); err != nil {
		fatal(err)
	}
	state, legacy, err := store.Read()
	if err != nil || legacy || state.Generation == 0 || state.Phase == routing.PhaseApplying || state.Phase == routing.PhaseRecoveryRequired {
		fatal(handoff.ErrRemovalRoutingChanged)
	}
	coordinator := handoff.RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		CheckAdmission: func(context.Context) error {
			return handoff.RemovalExecutionAdmission(configPath)
		},
	}
	receipt, err := coordinator.Inventory(ctx, handoff.NPMRemovalSelection{
		ID: *installationID, Fingerprint: *installationFingerprint,
	}, state.Generation)
	if err != nil {
		fatal(err)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Printf("opencodex_data_inventory=%s items=%d\n", receipt.Status, len(receipt.Items))
}

func modeRemoveOpenCodex(args []string) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(errorsNew("automatic OpenCodex removal is supported only on macOS Apple Silicon"))
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode remove-open-codex")
	installationID := flags.String("installation-id", "", "opaque OpenCodex discovery candidate ID")
	installationFingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex discovery candidate fingerprint")
	mode := flags.String("removal-mode", "", "preserve_data or trash_selected")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "routing generation shown during confirmation")
	expectedInventoryRevision := flags.String("expected-inventory-revision", "", "opaque inventory revision shown during confirmation")
	confirmedRemoval := flags.Bool("confirm-opencodex-removal", false, "confirm package removal and integration teardown")
	confirmedTrash := flags.Bool("confirm-data-trash", false, "second confirmation for the exact selected data IDs")
	confirmedDataRefresh := flags.Bool("confirm-interrupted-data-refresh", false, "confirm a fresh non-overlapping selection after interrupted data movement")
	confirmedProcessRecovery := flags.Bool("confirm-rebooted-process-recovery", false, "confirm recovery after reboot proves the old package process cannot remain")
	confirmedDesktop := flags.Bool("confirm-desktop-exited", false, "confirm the verified Codex Desktop app has exited")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	var dataItems stringListFlag
	flags.Var(&dataItems, "data-item", "explicit opaque inventory item ID; repeatable")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	flags.Parse(args)
	if flags.NArg() != 0 || *installationID == "" || *installationFingerprint == "" || *mode == "" ||
		*expectedGeneration == 0 || !*confirmedRemoval || !*confirmedDesktop {
		fatal(handoff.ErrRemovalConfirmationNeeded)
	}
	if *confirmedDataRefresh && *confirmedProcessRecovery {
		fatal(handoff.ErrInvalidRemovalRequest)
	}
	if err := preflightHandoffCodexConfig(codexPath); err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, openCodexRemovalTimeout)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	lifecycle, err := lifecyclelock.AcquireWriter(ctx, home, "")
	if err != nil {
		fatal(err)
	}
	defer lifecycle.Close()
	boundary, err := openRemovalBoundary(ctx, configPath, codexPath, *expectedGeneration)
	if err != nil {
		fatal(err)
	}
	defer boundary.lock.Close()
	resolver, runner, err := removalRuntime(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	verifyRouting := func(context.Context) error {
		ownership, ownershipErr := validateHandoffResultOwnership(codexPath, boundary.owner, boundary.cfg)
		if ownershipErr != nil || !handoffOwnershipMatchesAppliedBackend(ownership, boundary.state.AppliedBackend) {
			return errorsNew("post-teardown Codex routing ownership is not proven")
		}
		return nil
	}
	runner.BeforeOCXMutation = verifyRouting
	runner.BeforeUninstallCandidate = resolver.Revalidate
	runner.BeforeUninstall = verifyRouting
	coordinator := handoff.RemovalCoordinator{
		Resolver:      resolver,
		Runner:        runner,
		VerifyRouting: verifyRouting,
		CheckAdmission: func(context.Context) error {
			return handoff.RemovalExecutionAdmission(configPath)
		},
		CheckResumeAdmission: func(context.Context) error {
			return handoff.RemovalPackageResumeAdmission(configPath)
		},
		MarkRoutingRecovery: func() error {
			return persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state)
		},
		PrepareOperation: func(_ context.Context, candidate handoff.NPMInstallation, request handoff.OpenCodexRemovalRequest) error {
			_, prepareErr := handoff.EnsureRemovalIntent(configPath, candidate, request)
			return prepareErr
		},
		RecordDataOutcome: func(_ context.Context, candidate handoff.NPMInstallation, request handoff.OpenCodexRemovalRequest, moved int, status string) error {
			_, recordErr := handoff.RecordRemovalDataOutcome(configPath, candidate, request, moved, status)
			return recordErr
		},
		MarkDataRefresh: func(context.Context) error {
			_, markErr := handoff.MarkRemovalDataRefreshRequired(configPath)
			return markErr
		},
		PreparePackageRemoval: func(_ context.Context, candidate handoff.NPMInstallation, request handoff.OpenCodexRemovalRequest, moved int) error {
			_, prepareErr := handoff.PrepareRemovalPackageCleanup(configPath, candidate, request, moved)
			return prepareErr
		},
		BeginExecution: func(ctx context.Context, kind handoff.RemovalExecutionKind) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, beginErr := handoff.BeginExecution(configPath, kind)
			return beginErr
		},
		FinishExecution: func(_ context.Context, kind handoff.RemovalExecutionKind, result handoff.RemovalExecutionResult) error {
			_, finishErr := handoff.FinishExecution(configPath, kind, result)
			return finishErr
		},
		ResolveExecution: func(ctx context.Context, kind handoff.RemovalExecutionKind, resolution handoff.RemovalExecutionResolution, parkRouting bool) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if _, markErr := handoff.MarkRemovalExecutionResolution(configPath, kind, resolution, parkRouting); markErr != nil {
				return false, markErr
			}
			routingParked := false
			var park func() error
			if parkRouting {
				park = func() error {
					if err := persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state); err != nil {
						return err
					}
					routingParked = true
					return nil
				}
			}
			_, _, resolveErr := handoff.ResumeRemovalExecutionResolution(configPath, park)
			return routingParked, resolveErr
		},
	}
	request := handoff.OpenCodexRemovalRequest{
		Selection:                 handoff.NPMRemovalSelection{ID: *installationID, Fingerprint: *installationFingerprint},
		Mode:                      handoff.OpenCodexRemovalMode(*mode),
		DataItemIDs:               append([]string(nil), dataItems...),
		ExpectedRoutingGeneration: *expectedGeneration,
		ExpectedInventoryRevision: *expectedInventoryRevision,
		ConfirmedRemoval:          *confirmedRemoval,
		ConfirmedTrash:            *confirmedTrash,
	}
	if err := handoff.ValidateOpenCodexRemovalRequest(request); err != nil {
		fatal(err)
	}
	pending, exists, readErr := handoff.ReadRemovalCleanup(configPath)
	if readErr != nil {
		fatal(readErr)
	}
	if !exists && request.Mode == handoff.RemovalModeTrashSelected {
		inventory, inventoryErr := coordinator.Inventory(
			ctx, request.Selection, boundary.state.Generation,
		)
		if inventoryErr != nil || !handoff.ValidateOpenCodexInventoryForRequest(inventory, request) {
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
	}
	if exists {
		if err := validatePendingRemovalRequest(pending, request); err != nil {
			fatal(err)
		}
		// Recommit the exact readable record before it authorizes absence
		// verification or resume. This restores rename + directory-fsync proof
		// after a crash that may have exposed bytes before durable metadata.
		if err := handoff.WriteRemovalCleanup(configPath, pending); err != nil {
			fatal(err)
		}
		if resolution, found, resolutionErr := handoff.PendingRemovalExecutionResolution(pending); resolutionErr != nil {
			fatal(resolutionErr)
		} else if found {
			beforeResolution := pending
			var park func() error
			if resolution.RequiresRoutingRecovery {
				park = func() error {
					return persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state)
				}
			}
			resolved, didResolve, resolveErr := handoff.ResumeRemovalExecutionResolution(configPath, park)
			if resolveErr != nil || !didResolve {
				receipt, receiptErr := handoff.InterruptedRemovalExecutionReceipt(beforeResolution, false)
				if receiptErr != nil {
					fatal(receiptErr)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
			receipt, receiptErr := handoff.ResolvedRemovalExecutionReceipt(
				beforeResolution,
				resolved,
				resolution.RequiresRoutingRecovery,
			)
			if receiptErr != nil {
				fatal(receiptErr)
			}
			emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
			return
		}
		if pending.RecoveryPending {
			receipt, receiptErr := handoff.PendingRemovalRoutingRecoveryReceipt(pending)
			if receiptErr != nil {
				fatal(receiptErr)
			}
			emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
			return
		}
		reconciledExecutionKind := handoff.RemovalExecutionKind("")
		if pending.ActiveExecution != nil {
			reconciledExecutionKind = pending.ActiveExecution.Kind
			if !*confirmedProcessRecovery {
				recoveryPersisted := persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state) == nil
				receipt, receiptErr := handoff.InterruptedRemovalExecutionReceipt(pending, recoveryPersisted)
				if receiptErr != nil {
					fatal(receiptErr)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
			reconciled, _, reconcileErr := handoff.ReconcileActiveExecutionAfterBoot(configPath, true)
			if reconcileErr != nil {
				fatal(reconcileErr)
			}
			pending = reconciled
			// A changed boot proves that the old teardown child cannot still
			// mutate, but it does not prove which teardown components changed.
			// Return to the durable intent and park routing; do not replay the
			// teardown in this invocation. Trash and package have their own
			// changed-boot transitions below (refresh or absence/residual).
			if reconciledExecutionKind == handoff.RemovalExecutionTeardown {
				recoveryPersisted := persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state) == nil
				receipt, receiptErr := handoff.InterruptedRemovalReceipt(pending, recoveryPersisted)
				if receiptErr != nil {
					fatal(receiptErr)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
		}
		switch pending.Phase {
		case handoff.RemovalCleanupPhaseIntent:
			if pending.OperationRetryPending {
				if !handoff.RemovalCleanupMatchesRequest(pending, request) {
					fatal(handoff.ErrRemovalCleanupUnsafe)
				}
				receipt := coordinator.Remove(ctx, request)
				if removalReceiptClaimsFinalization(receipt) {
					finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
			if pending.Mode == handoff.RemovalModeTrashSelected {
				marked, markErr := handoff.MarkRemovalDataRefreshRequired(configPath)
				if markErr != nil {
					fatal(markErr)
				}
				pending = marked
			} else {
				if !handoff.RemovalCleanupMatchesRequest(pending, request) {
					fatal(handoff.ErrRemovalCleanupUnsafe)
				}
				receipt := coordinator.Remove(ctx, request)
				if removalReceiptClaimsFinalization(receipt) {
					finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
		case handoff.RemovalCleanupPhaseDataOutcome:
			if pending.DataOutcome != "completed" {
				marked, markErr := handoff.MarkRemovalDataRefreshRequired(configPath)
				if markErr != nil {
					fatal(markErr)
				}
				pending = marked
			} else {
				if !handoff.RemovalCleanupMatchesRequest(pending, request) {
					fatal(handoff.ErrRemovalCleanupUnsafe)
				}
				advanced, advanceErr := handoff.AdvanceRemovalDataOutcomeToPackagePending(configPath)
				if advanceErr != nil {
					fatal(advanceErr)
				}
				pending = advanced
			}
		case handoff.RemovalCleanupPhaseDataRefresh:
			// A durable refresh record is handled below and is never replayed.
		case handoff.RemovalCleanupPhasePackageInFlight:
			if !handoff.RemovalCleanupMatchesRequest(pending, request) {
				fatal(handoff.ErrRemovalCleanupUnsafe)
			}
			if *confirmedProcessRecovery {
				reconciled, _, reconcileErr := handoff.ReconcileRemovalPackageAfterReboot(configPath, true)
				if reconcileErr != nil {
					fatal(reconcileErr)
				}
				pending = reconciled
				break
			}
			recoveryPersisted := persistRemovalRoutingRecovery(configPath, boundary.lock, boundary.state) == nil
			receipt, receiptErr := handoff.InterruptedRemovalProcessReceipt(pending, recoveryPersisted)
			if receiptErr != nil {
				fatal(receiptErr)
			}
			emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
			return
		case handoff.RemovalCleanupPhasePackagePending, handoff.RemovalCleanupPhasePackageVerified:
			if !handoff.RemovalCleanupMatchesRequest(pending, request) {
				fatal(handoff.ErrRemovalCleanupUnsafe)
			}
		default:
			fatal(handoff.ErrRemovalCleanupUnsafe)
		}
		if pending.Phase == handoff.RemovalCleanupPhaseDataRefresh {
			if pending.InstallationID != request.Selection.ID || pending.Fingerprint != request.Selection.Fingerprint {
				fatal(handoff.ErrRemovalCleanupUnsafe)
			}
			if !*confirmedDataRefresh {
				receipt, receiptErr := removalDataRefreshReceipt(pending)
				if receiptErr != nil {
					fatal(receiptErr)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			}
			inventory, inventoryErr := coordinator.Inventory(ctx, request.Selection, boundary.state.Generation)
			if inventoryErr != nil {
				fatal(inventoryErr)
			}
			if _, supersedeErr := handoff.SupersedeRemovalDataRefreshRequired(
				configPath, request, inventory, *confirmedDataRefresh,
			); supersedeErr != nil {
				fatal(supersedeErr)
			}
			receipt := coordinator.Remove(ctx, request)
			if removalReceiptClaimsFinalization(receipt) {
				finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
			}
			emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
			return
		}
		if pending.Phase == handoff.RemovalCleanupPhasePackageVerified {
			if absentErr := handoff.VerifyRemovalCleanupAbsent(pending); absentErr == nil {
				receipt, canFinalize, receiptErr := reconcileVerifiedRemovedPackage(ctx, pending, verifyRouting, func() error {
					return parkHandoffForRecovery(boundary.lock, boundary.state)
				})
				if receiptErr != nil {
					fatal(receiptErr)
				}
				if canFinalize && removalReceiptClaimsFinalization(receipt) {
					finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
				}
				emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
				return
			} else if !errors.Is(absentErr, handoff.ErrRemovalOutcomeUnknown) {
				fatal(absentErr)
			}
		}
		receipt := coordinator.ResumePackageRemoval(ctx, pending)
		if removalReceiptClaimsFinalization(receipt) {
			finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
		}
		emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
		return
	}

	receipt := coordinator.Remove(ctx, request)
	if removalReceiptClaimsFinalization(receipt) {
		finalizeRelayRemovalVerified(configPath, boundary, request, &receipt)
	}
	emitOpenCodexRemovalReceipt(receipt, *jsonOutput)
}

func validatePendingRemovalRequest(
	record handoff.RemovalCleanupRecord,
	request handoff.OpenCodexRemovalRequest,
) error {
	if record.InstallationID != request.Selection.ID ||
		record.Fingerprint != request.Selection.Fingerprint {
		return handoff.ErrRemovalCleanupUnsafe
	}
	if (record.ActiveExecution != nil ||
		record.ExecutionResolution != "" ||
		record.RecoveryPending) &&
		!handoff.RemovalCleanupMatchesRequest(record, request) {
		return handoff.ErrRemovalCleanupUnsafe
	}
	return nil
}

type removalBoundary struct {
	lock  *routing.Lock
	state routing.State
	cfg   config.Config
	owner codexconfig.Owner
}

func openRemovalBoundary(ctx context.Context, configPath, codexPath string, expectedGeneration uint64) (*removalBoundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := routing.Open(configPath)
	if err != nil {
		return nil, errorsNew("routing state cannot be opened for OpenCodex removal")
	}
	lock, err := store.Lock(ctx)
	if err != nil {
		return nil, errorsNew("routing state is busy; retry OpenCodex removal after the current routing operation")
	}
	fail := func(err error) (*removalBoundary, error) {
		_ = lock.Close()
		return nil, err
	}
	state, legacy, err := store.Read()
	if err != nil || legacy || state.ValidateForCodexConfig(configPath, codexPath) != nil {
		return fail(errorsNew("initialize and verify the v2 routing state before removing OpenCodex"))
	}
	if state.Generation != expectedGeneration {
		return fail(errorsNew("routing state changed after OpenCodex removal confirmation; refresh status and retry"))
	}
	if pending, transactionErr := store.HasPendingTransaction(); transactionErr != nil || pending {
		return fail(routing.ErrRecoveryRequired)
	}
	if pending, maintenanceErr := store.HasPendingMaintenance(); maintenanceErr != nil || pending {
		return fail(routing.ErrRecoveryRequired)
	}
	cleanup, cleanupExists, cleanupErr := handoff.ReadRemovalCleanup(configPath)
	if cleanupErr != nil {
		return fail(errorsNew("OpenCodex removal cleanup journal is not safe"))
	}
	reconcilingGate := cleanupExists &&
		(cleanup.ActiveExecution != nil ||
			cleanup.ExecutionResolution != "" ||
			cleanup.RecoveryPending ||
			cleanup.Phase == handoff.RemovalCleanupPhasePackageInFlight ||
			cleanup.Phase == handoff.RemovalCleanupPhasePackageVerified ||
			cleanup.Phase == handoff.RemovalCleanupPhasePackagePending && cleanup.ProcessReconciledAfterReboot)
	if state.Phase == routing.PhaseApplying || state.AppliedBackend == routing.BackendUnknown ||
		state.Phase == routing.PhaseRecoveryRequired && !reconcilingGate {
		return fail(routing.ErrRecoveryRequired)
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		return fail(err)
	}
	status := controller.Status(ctx)
	if !reconcilingGate && (status.Connection.LocalRelay != routing.LocalRelayHealthy || !status.RelayRunning) {
		return fail(errorsNew("resident relay health is required before removing OpenCodex"))
	}
	if !reconcilingGate && status.Connection.RoutingSync != routing.RoutingSyncAcknowledged && status.Connection.RoutingSync != routing.RoutingSyncInvalid {
		return fail(errorsNew("resident relay routing synchronization is not stable enough for OpenCodex removal"))
	}
	stateSafeForReconciliation := (state.Phase == routing.PhaseRelayActive || state.Phase == routing.PhaseNativeActive || state.Phase == routing.PhaseRecoveryRequired) &&
		state.DesiredBackend != routing.BackendLocalOpenCodex && state.AppliedBackend != routing.BackendLocalOpenCodex &&
		state.DesiredBackend != routing.BackendUnknown && state.AppliedBackend != routing.BackendUnknown
	if !safeForOpenCodexUninstall(status) && !(reconcilingGate && stateSafeForReconciliation) {
		return fail(errorsNew("apply a verified External gateway or Native Codex state before removing OpenCodex"))
	}
	if err := handoff.PreflightRelayConfig(configPath); err != nil {
		return fail(errorsNew("relay configuration is not safe for OpenCodex removal"))
	}
	cfg, err := config.Load(configPath)
	if err != nil || cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		return fail(errorsNew("External gateway relay configuration is required for OpenCodex removal"))
	}
	owner, err := codexconfig.OwnerForID(cfg.Scope())
	if err != nil {
		return fail(errorsNew("relay configuration has an unsupported routing ownership scope"))
	}
	if err := handoff.PreflightRecordWrite(configPath); err != nil {
		return fail(errorsNew("OpenCodex enrollment receipt cannot be updated safely"))
	}
	if err := handoff.PreflightRemovalCleanup(configPath); err != nil {
		return fail(errorsNew("OpenCodex removal cleanup journal is not safe"))
	}
	return &removalBoundary{lock: lock, state: state, cfg: cfg, owner: owner}, nil
}

func removalRuntime(configPath, codexPath string) (handoff.DiscoveryRemovalResolver, handoff.ExactNPMRunner, error) {
	options, err := handoff.ProductionDiscoveryOptions(configPath)
	if err != nil {
		return handoff.DiscoveryRemovalResolver{}, handoff.ExactNPMRunner{}, err
	}
	options.GOOS = runtime.GOOS
	options.GOARCH = runtime.GOARCH
	options = handoff.SanitizedRemovalDiscoveryOptions(options)
	return handoff.DiscoveryRemovalResolver{Options: options}, handoff.ExactNPMRunner{
		HomeDir: options.HomeDir, CodexConfigPath: codexPath,
	}, nil
}

func persistRemovalRoutingRecovery(configPath string, lock *routing.Lock, state routing.State) error {
	record, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil {
		return err
	}
	if exists && (record.ActiveExecution != nil || record.Phase == handoff.RemovalCleanupPhasePackageInFlight) {
		// A marked execution has a known-safe resolution and may park routing
		// before its witness is cleared. An unmarked active child remains
		// reboot-gated; never claim that routing recovery was persisted merely
		// because the active witness itself blocks admission.
		if record.ExecutionResolution != "" && record.ResolutionRequiresRoutingRecovery {
			return ensureHandoffParkedForRecovery(lock, state)
		}
		return handoff.ErrRemovalRoutingGate
	}
	return ensureHandoffParkedForRecovery(lock, state)
}

func ensureHandoffParkedForRecovery(lock *routing.Lock, reviewed routing.State) error {
	if lock == nil {
		return errorsNew("routing recovery lock is unavailable")
	}
	current, err := lock.Load()
	if err != nil {
		return err
	}
	if current.Phase == routing.PhaseRecoveryRequired {
		return nil
	}
	if current.Generation != reviewed.Generation ||
		current.DesiredBackend != reviewed.DesiredBackend ||
		current.AppliedBackend != reviewed.AppliedBackend ||
		current.Phase != reviewed.Phase {
		return handoff.ErrRemovalRoutingChanged
	}
	return parkHandoffForRecovery(lock, current)
}

func reconcileVerifiedRemovedPackage(
	ctx context.Context,
	record handoff.RemovalCleanupRecord,
	verifyRouting func(context.Context) error,
	markRecovery func() error,
) (handoff.OpenCodexRemovalReceipt, bool, error) {
	if verifyRouting == nil || markRecovery == nil {
		return handoff.OpenCodexRemovalReceipt{}, false, handoff.ErrRemovalCleanupUnsafe
	}
	if err := verifyRouting(ctx); err != nil {
		receipt, receiptErr := handoff.RemovalCleanupRoutingRecoveryReceipt(record, markRecovery() == nil)
		return receipt, false, receiptErr
	}
	receipt, err := handoff.RemovedPackageCleanupReceipt(record)
	if err != nil {
		return receipt, false, err
	}
	receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
		Stage: "routing_final_verification", Status: handoff.RemovalStageCompleted, Code: "routing_ownership_reverified",
	})
	return receipt, true, nil
}

func removalDataRefreshReceipt(record handoff.RemovalCleanupRecord) (handoff.OpenCodexRemovalReceipt, error) {
	receipt, err := handoff.InterruptedRemovalDataRefreshReceipt(record)
	if err == nil {
		return receipt, nil
	}
	return handoff.RecordedRemovalDataRefreshReceipt(record)
}

func removalReceiptClaimsFinalization(receipt handoff.OpenCodexRemovalReceipt) bool {
	if receipt.SchemaVersion != handoff.OpenCodexRemovalSchemaVersion ||
		receipt.Operation != "remove-open-codex" ||
		receipt.Status != handoff.RemovalStatusCompleted ||
		receipt.InstallationID == "" ||
		!receipt.PackageRemoved || receipt.RoutingRecoveryRequired {
		return false
	}
	return hasCompletedRemovalStage(receipt.Stages, "package_verification", "package_absent") &&
		hasCompletedRemovalStage(receipt.Stages, "routing_final_verification", "routing_ownership_reverified")
}

func removalReceiptAllowsFinalization(
	configPath string,
	boundary *removalBoundary,
	request handoff.OpenCodexRemovalRequest,
	receipt handoff.OpenCodexRemovalReceipt,
) bool {
	if boundary == nil || !removalReceiptClaimsFinalization(receipt) ||
		boundary.state.Generation != request.ExpectedRoutingGeneration {
		return false
	}
	record, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.Phase != handoff.RemovalCleanupPhasePackageVerified ||
		!handoff.RemovalCleanupMatchesRequest(record, request) ||
		record.InstallationID != receipt.InstallationID ||
		handoff.VerifyRemovalCleanupAbsent(record) != nil {
		return false
	}
	// The routing writer lock in boundary keeps the reviewed generation
	// stable. Release the cleanup journal's terminal gate only after the exact
	// selector and absence proof have both been re-established, then re-read
	// and independently verify the durable release before consuming it.
	if _, err := handoff.ReleaseRemovalRoutingGateForFinalization(configPath); err != nil {
		return false
	}
	record, exists, err = handoff.ReadRemovalCleanup(configPath)
	return err == nil && exists && record.ActiveExecution == nil &&
		record.Phase == handoff.RemovalCleanupPhasePackageVerified &&
		record.RoutingRecoveryReleased &&
		handoff.RemovalCleanupMatchesRequest(record, request) &&
		record.InstallationID == receipt.InstallationID &&
		handoff.VerifyRemovalCleanupAbsent(record) == nil &&
		handoff.RemovalFinalizationAdmission(configPath) == nil
}

func hasCompletedRemovalStage(stages []handoff.OpenCodexRemovalStage, stage, code string) bool {
	for _, item := range stages {
		if item.Stage == stage && item.Status == handoff.RemovalStageCompleted && item.Code == code {
			return true
		}
	}
	return false
}

func finalizeRelayRemovalVerified(
	configPath string,
	boundary *removalBoundary,
	request handoff.OpenCodexRemovalRequest,
	receipt *handoff.OpenCodexRemovalReceipt,
) {
	if receipt == nil || !removalReceiptAllowsFinalization(configPath, boundary, request, *receipt) {
		appendFinalizationProofFailure(receipt)
		return
	}
	finalizeRelayRemoval(configPath, &boundary.cfg, receipt)
}

func appendFinalizationProofFailure(receipt *handoff.OpenCodexRemovalReceipt) {
	if receipt == nil {
		return
	}
	receipt.Status = handoff.RemovalStatusPartial
	receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
		Stage: "relay_cleanup", Status: handoff.RemovalStageFailed, Code: "finalization_proof_unavailable",
	})
}

func finalizeRelayRemoval(configPath string, cfg *config.Config, receipt *handoff.OpenCodexRemovalReceipt) {
	if cfg == nil || receipt == nil {
		return
	}
	cfg.LocalOpenCodex = nil
	if err := config.Write(configPath, *cfg); err != nil {
		receipt.Status = handoff.RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
			Stage: "relay_cleanup", Status: handoff.RemovalStageFailed, Code: "relay_config_cleanup_failed",
		})
		return
	}
	if err := handoff.RemoveRecord(configPath); err != nil {
		receipt.Status = handoff.RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
			Stage: "relay_cleanup", Status: handoff.RemovalStageFailed, Code: "enrollment_cleanup_failed",
		})
		return
	}
	if err := handoff.RemoveRemovalCleanup(configPath); err != nil {
		receipt.Status = handoff.RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
			Stage: "relay_cleanup", Status: handoff.RemovalStageFailed, Code: "cleanup_journal_remove_failed",
		})
		return
	}
	receipt.Stages = append(receipt.Stages, handoff.OpenCodexRemovalStage{
		Stage: "relay_cleanup", Status: handoff.RemovalStageCompleted, Code: "relay_cleanup_completed",
	})
}

func emitOpenCodexRemovalReceipt(receipt handoff.OpenCodexRemovalReceipt, jsonOutput bool) {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Printf("opencodex_removal=%s package_removed=%t data_scope=%s moved=%d/%d data_movement_unknown=%t recovery_required=%t\n",
		receipt.Status, receipt.PackageRemoved, receipt.DataScope, receipt.MovedDataItems, receipt.SelectedDataItems,
		receipt.DataMovementUnknown, receipt.RoutingRecoveryRequired)
}
