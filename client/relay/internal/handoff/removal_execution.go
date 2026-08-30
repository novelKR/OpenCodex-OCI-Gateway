package handoff

import (
	"errors"
	"fmt"
)

var removalBootSessionProvider = currentBootSessionID

// RemovalExecutionAdmission is the common resolver/inventory/mutation gate. It
// delegates to the full routing gate so an active child, a reboot-reconciled
// pending package, or an unreleased verified package all block new work.
func RemovalExecutionAdmission(relayConfigPath string) error {
	if err := RemovalRoutingGate(relayConfigPath); err != nil {
		return err
	}
	return nil
}

// ReleaseRemovalRoutingGateForFinalization durably records that an exact,
// verified-absent package is ready for terminal relay cleanup. It releases the
// recovery gate while arming a separate finalization witness, so routing
// remains fail-closed until the journal is deleted last. The caller must still
// hold the routing writer lock and independently prove the reviewed generation;
// this helper owns only the cleanup-journal and absence boundary.
func ReleaseRemovalRoutingGateForFinalization(relayConfigPath string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.ExecutionResolution != "" || record.RecoveryPending ||
		record.Phase != RemovalCleanupPhasePackageVerified ||
		VerifyRemovalCleanupAbsent(record) != nil {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if !record.RoutingRecoveryReleased || !record.FinalizationActive {
		record.RoutingRecoveryReleased = true
		record.FinalizationActive = true
		if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
			return RemovalCleanupRecord{}, err
		}
	}
	if err := RemovalFinalizationAdmission(relayConfigPath); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// RemovalFinalizationAdmission is the independent terminal gate. Finalization
// may consume the journal only after its active witness is clear, the package
// phase is verified, the durable recovery gate has been released, and the
// separate finalization witness is armed. Ordinary routing admission remains
// denied by that witness until terminal cleanup deletes the journal.
func RemovalFinalizationAdmission(relayConfigPath string) error {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.ExecutionResolution != "" || record.RecoveryPending ||
		record.Phase != RemovalCleanupPhasePackageVerified ||
		!record.RoutingRecoveryReleased || !record.FinalizationActive {
		return ErrRemovalRoutingGate
	}
	return nil
}

// RemovalPackageResumeAdmission is the deliberately narrow exception for a
// package that was reconciled after an attested reboot or whose launch was
// rejected before the child could start. It still rejects any active child,
// ordinary package-pending records, and an unreleased verified gate.
func RemovalPackageResumeAdmission(relayConfigPath string) error {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil {
		return ErrRemovalRoutingGate
	}
	if !exists || record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending {
		return ErrRemovalRoutingGate
	}
	if record.Phase == RemovalCleanupPhasePackagePending &&
		(record.ProcessReconciledAfterReboot || record.PackageRetryPending) {
		return nil
	}
	if record.Phase == RemovalCleanupPhasePackageVerified && record.RoutingRecoveryReleased && !record.FinalizationActive {
		return nil
	}
	return ErrRemovalRoutingGate
}

// BeginExecution durably arms one of the three fixed mutating children before
// the child is launched. The journal write is fsync-backed by WriteRemovalCleanup.
func BeginExecution(relayConfigPath string, kind RemovalExecutionKind) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.ExecutionResolution != "" || record.RecoveryPending {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if !validRemovalExecutionKind(kind) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if err := validateExecutionSource(record, kind); err != nil {
		return RemovalCleanupRecord{}, err
	}
	bootSession, attested, bootErr := removalBootSessionProvider()
	if bootErr != nil || !attested || !isFingerprint(bootSession) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	attempt := record.ExecutionAttempt + 1
	if attempt < 1 || attempt > maxRemovalExecutionAttempts {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	active := &RemovalActiveExecution{
		Kind:         kind,
		Attempt:      attempt,
		BootSession:  bootSession,
		BootAttested: attested,
	}
	next := record
	next.ExecutionAttempt = attempt
	next.ActiveExecution = active
	if kind == RemovalExecutionTeardown {
		next.OperationRetryPending = false
	}
	if kind == RemovalExecutionPackage {
		if next.PackageAttempt >= maxRemovalPackageAttempts {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		next.PackageAttempt++
		next.Phase = RemovalCleanupPhasePackageInFlight
		next.ExecutionBootSession = bootSession
		next.ProcessReconciledAfterReboot = false
		next.PackageRetryPending = false
		next.RoutingRecoveryReleased = false
		next.FinalizationActive = false
	}
	if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// FinishExecution records a definitive child outcome. An ambiguous started
// child deliberately leaves ActiveExecution intact, so no later invocation can
// replay it on the same boot.
func FinishExecution(relayConfigPath string, kind RemovalExecutionKind, result RemovalExecutionResult) (RemovalCleanupRecord, error) {
	// A standalone Native restore must publish its verified Native boundary in
	// the same durable transition that clears the active execution witness.
	// Clearing it here first would leave a crash window that cannot be safely
	// distinguished from an unverified restore.
	if kind == RemovalExecutionNativeRestore {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution == nil ||
		record.ActiveExecution.Kind != kind || record.ExecutionResolution != "" {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if !result.Started && result.CleanupVerified {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if result.Started && !result.CleanupVerified {
		return record, nil
	}
	next := record
	next.ActiveExecution = nil
	if kind == RemovalExecutionPackage {
		if result.Started {
			next.Phase = RemovalCleanupPhasePackageVerified
		} else {
			next.Phase = RemovalCleanupPhasePackagePending
			next.ExecutionBootSession = ""
			next.ProcessReconciledAfterReboot = false
			next.PackageRetryPending = false
		}
	}
	if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// CompleteStandaloneTeardown clears the teardown child witness and records
// teardown completion plus the independently verified Native boundary in one
// fsync-backed replacement. Splitting these writes would leave a crash window
// where the owner adapter may already have restored Native Codex but the journal
// still looks like a retryable pre-teardown intent.
func CompleteStandaloneTeardown(
	anchorPath string,
	result RemovalExecutionResult,
	verifiedRevision string,
) (RemovalCleanupRecord, error) {
	if !result.Started || !result.CleanupVerified || !isFingerprint(verifiedRevision) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || record.Context != RemovalContextStandaloneNative ||
		record.Phase != RemovalCleanupPhaseIntent || record.ActiveExecution == nil ||
		record.ActiveExecution.Kind != RemovalExecutionTeardown || record.ExecutionResolution != "" ||
		record.TeardownCompleted || record.NativeRecoveryRequired {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next := record
	next.ActiveExecution = nil
	next.TeardownCompleted = true
	next.NativeState = NativeStateNative
	next.NativeVerifiedBoundaryRevision = verifiedRevision
	if err := validateRemovalCleanupRecord(next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// CompleteStandaloneNativeRestore atomically clears the completed child
// witness and records the independently verified Native boundary. The caller
// must hold the shared user lifecycle lock and must already have proved both
// the fixed Codex configuration and clientIntegrations.codex=false through the
// immutable OpenCodex owner snapshot.
func CompleteStandaloneNativeRestore(
	anchorPath string,
	result RemovalExecutionResult,
	verifiedRevision string,
) (RemovalCleanupRecord, error) {
	if !result.Started || !result.CleanupVerified || !isFingerprint(verifiedRevision) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || record.Context != RemovalContextStandaloneNative ||
		record.Phase != RemovalCleanupPhaseIntent || record.ActiveExecution == nil ||
		record.ActiveExecution.Kind != RemovalExecutionNativeRestore || record.ExecutionResolution != "" ||
		!record.TeardownCompleted || record.NativeVerifiedBoundaryRevision != "" || record.NativeRecoveryRequired {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next := record
	next.ActiveExecution = nil
	next.NativeState = NativeStateNative
	next.NativeVerifiedBoundaryRevision = verifiedRevision
	if err := validateRemovalCleanupRecord(next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// MarkRemovalExecutionResolution records that a child has a known-safe
// resolution path, while retaining the active witness through any required
// routing park. It is intentionally idempotent for the same marker so a caller
// can resume after a crash between the marker and the routing write.
func MarkRemovalExecutionResolution(
	relayConfigPath string,
	kind RemovalExecutionKind,
	resolution RemovalExecutionResolution,
	requiresRoutingRecovery bool,
) (RemovalCleanupRecord, error) {
	if !validRemovalExecutionKind(kind) || !validRemovalExecutionResolutionForKind(kind, resolution) ||
		!validRemovalExecutionResolutionRoutingRequirement(resolution, requiresRoutingRecovery) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution == nil ||
		record.ActiveExecution.Kind != kind {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if record.RecoveryPending {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if record.ExecutionResolution != "" {
		if record.ExecutionResolution != resolution ||
			record.ResolutionRequiresRoutingRecovery != requiresRoutingRecovery {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		return record, nil
	}
	next := record
	next.ExecutionResolution = resolution
	next.ResolutionRequiresRoutingRecovery = requiresRoutingRecovery
	if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

type RemovalExecutionResolutionState struct {
	Kind                    RemovalExecutionKind
	Resolution              RemovalExecutionResolution
	RequiresRoutingRecovery bool
}

// PendingRemovalExecutionResolution returns the exact crash-resumable
// resolution marker from a validated cleanup record.
func PendingRemovalExecutionResolution(record RemovalCleanupRecord) (RemovalExecutionResolutionState, bool, error) {
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalExecutionResolutionState{}, false, err
	}
	if record.ExecutionResolution == "" {
		return RemovalExecutionResolutionState{}, false, nil
	}
	if record.ActiveExecution == nil {
		return RemovalExecutionResolutionState{}, false, ErrRemovalCleanupUnsafe
	}
	return RemovalExecutionResolutionState{
		Kind:                    record.ActiveExecution.Kind,
		Resolution:              record.ExecutionResolution,
		RequiresRoutingRecovery: record.ResolutionRequiresRoutingRecovery,
	}, true, nil
}

// ResumeRemovalExecutionResolution completes a marked resolution. When the
// marker requires routing recovery, parkRouting must durably park it before the
// active witness is cleared. A callback failure leaves the exact marker intact
// for a later retry.
func ResumeRemovalExecutionResolution(
	relayConfigPath string,
	parkRouting func() error,
) (RemovalCleanupRecord, bool, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	pending, found, err := PendingRemovalExecutionResolution(record)
	if err != nil || !found {
		return record, false, err
	}
	if pending.RequiresRoutingRecovery {
		if parkRouting == nil {
			return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
		}
		if err := parkRouting(); err != nil {
			return RemovalCleanupRecord{}, false, err
		}
	}
	resolved, err := resolveRemovalExecution(relayConfigPath, pending.Kind, pending.Resolution)
	if err != nil {
		return RemovalCleanupRecord{}, false, err
	}
	return resolved, true, nil
}

// resolveRemovalExecution clears a previously marked active witness and
// applies its exact durable phase transition. It is intentionally private so
// callers cannot bypass ResumeRemovalExecutionResolution's routing-park proof.
func resolveRemovalExecution(
	relayConfigPath string,
	kind RemovalExecutionKind,
	resolution RemovalExecutionResolution,
) (RemovalCleanupRecord, error) {
	if !validRemovalExecutionKind(kind) || !validRemovalExecutionResolutionForKind(kind, resolution) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution == nil ||
		record.ActiveExecution.Kind != kind || record.ExecutionResolution == "" ||
		record.ExecutionResolution != resolution {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next, err := resolvedRemovalExecutionRecord(record)
	if err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// resolvedRemovalExecutionRecord computes the only legal post-recovery record
// for a marked execution. It is shared by the transition validator so direct
// journal writes cannot clear a witness into an unrelated phase.
func resolvedRemovalExecutionRecord(record RemovalCleanupRecord) (RemovalCleanupRecord, error) {
	if record.ActiveExecution == nil || record.ExecutionResolution == "" ||
		!validRemovalExecutionResolutionForKind(record.ActiveExecution.Kind, record.ExecutionResolution) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next := record
	kind := record.ActiveExecution.Kind
	resolution := record.ExecutionResolution
	next.ActiveExecution = nil
	next.ExecutionResolution = ""
	next.ResolutionRequiresRoutingRecovery = false
	next.RecoveryPending = record.ResolutionRequiresRoutingRecovery
	switch resolution {
	case RemovalExecutionResolutionPreStartRoutingChanged:
		switch kind {
		case RemovalExecutionTeardown, RemovalExecutionTrash:
			next.Phase = RemovalCleanupPhaseIntent
			next.OperationRetryPending = true
			next.PackageRetryPending = false
		case RemovalExecutionPackage:
			next.Phase = RemovalCleanupPhasePackagePending
			next.ExecutionBootSession = ""
			next.ProcessReconciledAfterReboot = false
			next.PackageRetryPending = true
			next.RoutingRecoveryReleased = false
			next.FinalizationActive = false
		}
	case RemovalExecutionResolutionTeardownReceiptInvalid:
		if kind != RemovalExecutionTeardown {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		next.Phase = RemovalCleanupPhaseIntent
		next.OperationRetryPending = true
		next.PackageRetryPending = false
	case RemovalExecutionResolutionTrashReceiptInvalid:
		if kind != RemovalExecutionTrash || next.Mode != RemovalModeTrashSelected ||
			next.Phase != RemovalCleanupPhaseIntent {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		retired, overflow := boundedRetiredSelection(next.RetiredItemIDs, next.SelectedItemIDs)
		if overflow {
			next.TrashSelectionLocked = true
		} else {
			next.RetiredItemIDs = retired
		}
		next.Phase = RemovalCleanupPhaseDataRefresh
		next.DataOutcome = removalDataOutcomeUnknown
		next.MovedDataItems = 0
		next.OperationRetryPending = false
		next.PackageRetryPending = false
	default:
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	return next, nil
}

// ReconcileActiveExecutionAfterBoot is the only generic crash-recovery path.
// It requires explicit confirmation and a currently attested boot session that
// differs from the recorded session. No PID/PGID scan or timeout is accepted
// as evidence.
func ReconcileActiveExecutionAfterBoot(relayConfigPath string, confirmed bool) (RemovalCleanupRecord, bool, error) {
	if !confirmed {
		return RemovalCleanupRecord{}, false, ErrRemovalConfirmationNeeded
	}
	currentBoot, currentAttested, err := removalBootSessionProvider()
	if err != nil || !currentAttested || !isFingerprint(currentBoot) {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution == nil || record.ExecutionResolution != "" {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	active := *record.ActiveExecution
	if !isFingerprint(active.BootSession) || active.BootSession == currentBoot {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}

	switch active.Kind {
	case RemovalExecutionTeardown:
		next := record
		next.ActiveExecution = nil
		if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
			return RemovalCleanupRecord{}, false, err
		}
		return next, false, nil
	case RemovalExecutionTrash:
		next := record
		retired, overflow := boundedRetiredSelection(record.RetiredItemIDs, record.SelectedItemIDs)
		if overflow {
			next.TrashSelectionLocked = true
		} else {
			next.RetiredItemIDs = retired
		}
		next.ActiveExecution = nil
		next.Phase = RemovalCleanupPhaseDataRefresh
		next.DataOutcome = removalDataOutcomeUnknown
		next.MovedDataItems = 0
		if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
			return RemovalCleanupRecord{}, false, err
		}
		return next, false, nil
	case RemovalExecutionPackage:
		// Keep the established package absence/residual proof semantics. The
		// migrated v4 record may have an unattested old witness; a changed
		// current attested boot still prevents same-boot replay.
		probe := record
		probe.ActiveExecution = nil
		probe.Phase = RemovalCleanupPhasePackageVerified
		if err := VerifyRemovalCleanupAbsent(probe); err == nil {
			next := record
			next.ActiveExecution = nil
			next.Phase = RemovalCleanupPhasePackageVerified
			next.ProcessReconciledAfterReboot = false
			next.PackageRetryPending = false
			if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
				return RemovalCleanupRecord{}, false, err
			}
			return next, true, nil
		} else if !errors.Is(err, ErrRemovalOutcomeUnknown) {
			return RemovalCleanupRecord{}, false, err
		}
		next := record
		next.ActiveExecution = nil
		next.Phase = RemovalCleanupPhasePackagePending
		next.ProcessReconciledAfterReboot = true
		next.ExecutionBootSession = active.BootSession
		next.PackageRetryPending = false
		if err := WriteRemovalCleanup(relayConfigPath, next); err != nil {
			return RemovalCleanupRecord{}, false, err
		}
		return next, false, nil
	default:
		return RemovalCleanupRecord{}, false, fmt.Errorf("%w: unknown execution kind", ErrRemovalCleanupUnsafe)
	}
}

// ReconcileStandaloneNativeRestoreAfterBoot clears an ambiguous native restore
// only after a different attested boot and an independently verified Native
// boundary revision. Same-boot PID or timeout evidence is never accepted.
func ReconcileStandaloneNativeRestoreAfterBoot(anchorPath, verifiedRevision string, confirmed bool) (RemovalCleanupRecord, error) {
	if !confirmed || !isFingerprint(verifiedRevision) {
		return RemovalCleanupRecord{}, ErrRemovalConfirmationNeeded
	}
	currentBoot, currentAttested, err := removalBootSessionProvider()
	if err != nil || !currentAttested || !isFingerprint(currentBoot) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || record.Context != RemovalContextStandaloneNative ||
		record.ActiveExecution == nil || record.ActiveExecution.Kind != RemovalExecutionNativeRestore ||
		record.ExecutionResolution != "" || record.ActiveExecution.BootSession == currentBoot {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.ActiveExecution = nil
	record.NativeState = "native"
	record.NativeVerifiedBoundaryRevision = verifiedRevision
	record.NativeRecoveryRequired = false
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// ReconcileStandaloneTeardownAfterBoot resolves only a teardown child from a
// different attested boot after the caller independently proves the current
// Native boundary. Teardown remains incomplete, so the adapter must run again
// and return a valid complete receipt before data or package mutation.
func ReconcileStandaloneTeardownAfterBoot(
	anchorPath string,
	expected RemovalCleanupRecord,
	verifiedRevision string,
	confirmed bool,
) (RemovalCleanupRecord, error) {
	if !confirmed || !isFingerprint(verifiedRevision) {
		return RemovalCleanupRecord{}, ErrRemovalConfirmationNeeded
	}
	currentBoot, currentAttested, err := removalBootSessionProvider()
	if err != nil || !currentAttested || !isFingerprint(currentBoot) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || !sameRemovalCleanupRecord(record, expected) ||
		record.Context != RemovalContextStandaloneNative || record.Phase != RemovalCleanupPhaseIntent ||
		record.ActiveExecution == nil || record.ActiveExecution.Kind != RemovalExecutionTeardown ||
		record.ExecutionResolution != "" || record.ActiveExecution.BootSession == currentBoot ||
		record.TeardownCompleted || record.NativeVerifiedBoundaryRevision != "" {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next := record
	next.ActiveExecution = nil
	next.NativeState = NativeStateNative
	next.NativeVerifiedBoundaryRevision = verifiedRevision
	next.NativeRecoveryRequired = false
	next.OperationRetryPending = true
	if err := validateRemovalCleanupRecord(next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

// RecoverStandaloneTeardownNativeBoundary records an independently re-proved
// Native boundary after a resolved teardown failure. It is exact-record bound
// so a replacement journal cannot be accepted between proof and persistence.
// Teardown remains incomplete and must be retried before later mutation.
func RecoverStandaloneTeardownNativeBoundary(
	anchorPath string,
	expected RemovalCleanupRecord,
	verifiedRevision string,
) (RemovalCleanupRecord, error) {
	if !isFingerprint(verifiedRevision) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || !sameRemovalCleanupRecord(record, expected) ||
		record.Context != RemovalContextStandaloneNative || record.Phase != RemovalCleanupPhaseIntent ||
		record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending ||
		!record.NativeRecoveryRequired || record.TeardownCompleted || record.NativeVerifiedBoundaryRevision != "" {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	next := record
	next.NativeState = NativeStateNative
	next.NativeVerifiedBoundaryRevision = verifiedRevision
	next.NativeRecoveryRequired = false
	next.OperationRetryPending = true
	if err := validateRemovalCleanupRecord(next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), next); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return next, nil
}

func ClearStandaloneNativeRecovery(anchorPath string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(anchorPath)
	if err != nil || !exists || record.Context != RemovalContextStandaloneNative || record.ActiveExecution != nil {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.NativeRecoveryRequired = false
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if err := writeRemovalCleanupFile(RemovalCleanupPath(anchorPath), record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

func validRemovalExecutionKind(kind RemovalExecutionKind) bool {
	return kind == RemovalExecutionTeardown || kind == RemovalExecutionNativeRestore ||
		kind == RemovalExecutionTrash || kind == RemovalExecutionPackage
}

func validateExecutionSource(record RemovalCleanupRecord, kind RemovalExecutionKind) error {
	switch kind {
	case RemovalExecutionTeardown:
		if record.Phase != RemovalCleanupPhaseIntent || record.Mode != RemovalModePreserveData && record.Mode != RemovalModeTrashSelected {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalExecutionNativeRestore:
		if record.Context != RemovalContextStandaloneNative || record.Phase != RemovalCleanupPhaseIntent ||
			!record.TeardownCompleted || record.NativeVerifiedBoundaryRevision != "" || record.NativeRecoveryRequired {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalExecutionTrash:
		if record.Phase != RemovalCleanupPhaseIntent || record.Mode != RemovalModeTrashSelected ||
			record.SelectedDataItems == 0 || record.OperationRetryPending ||
			record.Context == RemovalContextStandaloneNative &&
				(!record.TeardownCompleted || record.NativeVerifiedBoundaryRevision == "") {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalExecutionPackage:
		if (record.Phase != RemovalCleanupPhasePackagePending && record.Phase != RemovalCleanupPhasePackageVerified) ||
			record.PackageAttempt >= maxRemovalPackageAttempts || record.FinalizationActive ||
			record.Context == RemovalContextStandaloneNative &&
				(!record.TeardownCompleted || record.NativeVerifiedBoundaryRevision == "") {
			return ErrRemovalCleanupUnsafe
		}
	default:
		return ErrRemovalCleanupUnsafe
	}
	return nil
}
