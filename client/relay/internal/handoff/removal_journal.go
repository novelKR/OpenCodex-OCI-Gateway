package handoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	removalCleanupSchemaVersion  = 5
	legacyRemovalCleanupVersion  = 4
	maxRemovalCleanupBytes       = 16 << 10
	maxRemovalPackageAttempts    = 32
	maxRemovalExecutionAttempts  = maxRemovalPackageAttempts * 3
	maxRemovalSelectionRevisions = 32
	maxRemovalRetiredDataItems   = maxRemovalDataItems
	removalDataOutcomeUnknown    = "unknown"

	RemovalCleanupPhaseIntent          = "operation_intent"
	RemovalCleanupPhaseDataOutcome     = "data_outcome_recorded"
	RemovalCleanupPhaseDataRefresh     = "data_refresh_required"
	RemovalCleanupPhasePackagePending  = "package_cleanup_pending"
	RemovalCleanupPhasePackageInFlight = "package_cleanup_in_flight"
	RemovalCleanupPhasePackageVerified = "package_cleanup_verified"
)

type RemovalExecutionKind string

const (
	RemovalExecutionTeardown RemovalExecutionKind = "teardown"
	RemovalExecutionTrash    RemovalExecutionKind = "trash"
	RemovalExecutionPackage  RemovalExecutionKind = "package"
)

// RemovalExecutionResolution is a durable, finite marker for an execution
// whose child lifecycle is known to be resolved. It also records whether
// routing must be parked before the active witness can be cleared. The marker
// is written while ActiveExecution is still present so a crash cannot reopen
// an unresolved operation.
type RemovalExecutionResolution string

const (
	RemovalExecutionResolutionPreStartRoutingChanged RemovalExecutionResolution = "pre_start_routing_changed"
	RemovalExecutionResolutionTeardownReceiptInvalid RemovalExecutionResolution = "teardown_receipt_invalid"
	RemovalExecutionResolutionTrashReceiptInvalid    RemovalExecutionResolution = "trash_receipt_invalid"
)

// RemovalActiveExecution is the single durable witness that a mutating child
// may still be running or may have completed without its caller observing the
// result. New executions are armed only with a platform-attested boot session.
// BootAttested can be false only for a migrated v4 package in-flight record;
// that legacy record remains fail-closed and cannot authorize same-boot replay.
type RemovalActiveExecution struct {
	Kind                 RemovalExecutionKind `json:"kind"`
	Attempt              int                  `json:"attempt"`
	BootSession          string               `json:"boot_session"`
	BootAttested         bool                 `json:"boot_attested"`
	LegacyUnattestedBoot bool                 `json:"legacy_unattested_boot,omitempty"`
}

var (
	ErrRemovalCleanupUnsafe = errors.New("OpenCodex removal cleanup journal is unsafe")
	ErrRemovalRoutingGate   = errors.New("OpenCodex removal routing recovery gate is active")
)

// RemovalCleanupRecord is the durable, path-bounded removal transaction record.
// The intent phase is committed before the first mutating child. Every
// teardown, Trash, and package launch then arms ActiveExecution before the
// child starts. An active record is deliberately not resumable on the same
// boot because a crashed caller cannot prove the old mutation's outcome. A
// data-refresh record is also not replay authority: only an explicitly
// confirmed, freshly inventoried, non-overlapping selection can atomically
// supersede it.
type RemovalCleanupRecord struct {
	SchemaVersion                     int                        `json:"schema_version"`
	Operation                         string                     `json:"operation"`
	Phase                             string                     `json:"phase"`
	InstallationID                    string                     `json:"installation_id"`
	Fingerprint                       string                     `json:"installation_fingerprint"`
	Mode                              OpenCodexRemovalMode       `json:"mode"`
	SelectionDigest                   string                     `json:"selection_digest"`
	SelectedItemIDs                   []string                   `json:"selected_item_ids"`
	RetiredItemIDs                    []string                   `json:"retired_item_ids"`
	SelectedDataItems                 int                        `json:"selected_data_items"`
	SelectionRevision                 int                        `json:"selection_revision"`
	TrashSelectionLocked              bool                       `json:"trash_selection_locked"`
	MovedDataItems                    int                        `json:"moved_data_items"`
	DataOutcome                       string                     `json:"data_outcome,omitempty"`
	ExecutionAttempt                  int                        `json:"execution_attempt"`
	ActiveExecution                   *RemovalActiveExecution    `json:"active_execution,omitempty"`
	ExecutionResolution               RemovalExecutionResolution `json:"execution_resolution,omitempty"`
	ResolutionRequiresRoutingRecovery bool                       `json:"resolution_requires_routing_recovery"`
	RecoveryPending                   bool                       `json:"recovery_pending"`
	OperationRetryPending             bool                       `json:"operation_retry_pending"`
	PackageAttempt                    int                        `json:"package_attempt"`
	ExecutionBootSession              string                     `json:"execution_boot_session,omitempty"`
	ProcessReconciledAfterReboot      bool                       `json:"process_reconciled_after_reboot"`
	PackageRetryPending               bool                       `json:"package_retry_pending"`
	RoutingRecoveryReleased           bool                       `json:"routing_recovery_released"`
	FinalizationActive                bool                       `json:"finalization_active"`
	PackageRoot                       string                     `json:"package_root"`
	Launchers                         []string                   `json:"launchers"`
}

func RemovalCleanupPath(relayConfigPath string) string {
	return filepath.Clean(relayConfigPath) + ".open-codex-removal.json"
}

func RemovalDataSelectionDigest(itemIDs []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("pw-open-codex-removal-selection-v1\x00"))
	for _, itemID := range itemIDs {
		_, _ = hash.Write([]byte(itemID))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validRemovalExecutionResolution(value RemovalExecutionResolution) bool {
	switch value {
	case RemovalExecutionResolutionPreStartRoutingChanged,
		RemovalExecutionResolutionTeardownReceiptInvalid,
		RemovalExecutionResolutionTrashReceiptInvalid:
		return true
	default:
		return false
	}
}

func validRemovalExecutionResolutionForKind(kind RemovalExecutionKind, resolution RemovalExecutionResolution) bool {
	if !validRemovalExecutionResolution(resolution) {
		return false
	}
	switch resolution {
	case RemovalExecutionResolutionPreStartRoutingChanged:
		return validRemovalExecutionKind(kind)
	case RemovalExecutionResolutionTeardownReceiptInvalid:
		return kind == RemovalExecutionTeardown
	case RemovalExecutionResolutionTrashReceiptInvalid:
		return kind == RemovalExecutionTrash
	default:
		return false
	}
}

func validRemovalExecutionResolutionRoutingRequirement(
	resolution RemovalExecutionResolution,
	requiresRoutingRecovery bool,
) bool {
	switch resolution {
	case RemovalExecutionResolutionPreStartRoutingChanged,
		RemovalExecutionResolutionTeardownReceiptInvalid:
		return requiresRoutingRecovery
	case RemovalExecutionResolutionTrashReceiptInvalid:
		return true
	default:
		return false
	}
}

func NewRemovalIntentRecord(candidate NPMInstallation, request OpenCodexRemovalRequest) (RemovalCleanupRecord, error) {
	return newRemovalCleanupRecord(candidate, request, RemovalCleanupPhaseIntent, 0, "")
}

func NewRemovalDataOutcomeRecord(candidate NPMInstallation, request OpenCodexRemovalRequest, moved int, status string) (RemovalCleanupRecord, error) {
	return newRemovalCleanupRecord(candidate, request, RemovalCleanupPhaseDataOutcome, moved, status)
}

func NewRemovalCleanupRecord(candidate NPMInstallation, request OpenCodexRemovalRequest, moved int) (RemovalCleanupRecord, error) {
	dataOutcome := ""
	if request.Mode == RemovalModeTrashSelected {
		dataOutcome = "completed"
	}
	return newRemovalCleanupRecord(candidate, request, RemovalCleanupPhasePackagePending, moved, dataOutcome)
}

func newRemovalCleanupRecord(candidate NPMInstallation, request OpenCodexRemovalRequest, phase string, moved int, dataOutcome string) (RemovalCleanupRecord, error) {
	if err := validateRemovalRequest(request); err != nil {
		return RemovalCleanupRecord{}, err
	}
	if candidate.ID != request.Selection.ID || candidate.Fingerprint != request.Selection.Fingerprint {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record := RemovalCleanupRecord{
		SchemaVersion:     removalCleanupSchemaVersion,
		Operation:         "remove-open-codex",
		Phase:             phase,
		InstallationID:    candidate.ID,
		Fingerprint:       candidate.Fingerprint,
		Mode:              request.Mode,
		SelectionDigest:   RemovalDataSelectionDigest(request.DataItemIDs),
		SelectedItemIDs:   append([]string{}, request.DataItemIDs...),
		SelectedDataItems: len(request.DataItemIDs),
		SelectionRevision: 1,
		MovedDataItems:    moved,
		DataOutcome:       dataOutcome,
		PackageRoot:       candidate.PackageRoot,
		Launchers:         uniqueSortedStrings(append([]string(nil), candidate.Launchers...)),
	}
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// EnsureRemovalIntent creates the initial revision or durably recommits an
// already-superseded intent. It never replaces a different transaction.
func EnsureRemovalIntent(relayConfigPath string, candidate NPMInstallation, request OpenCodexRemovalRequest) (RemovalCleanupRecord, error) {
	desired, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		return RemovalCleanupRecord{}, err
	}
	existing, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil {
		return RemovalCleanupRecord{}, err
	}
	if !exists {
		if err := WriteRemovalCleanup(relayConfigPath, desired); err != nil {
			return RemovalCleanupRecord{}, err
		}
		return desired, nil
	}
	if existing.Phase != RemovalCleanupPhaseIntent || existing.ActiveExecution != nil ||
		existing.ExecutionResolution != "" || existing.RecoveryPending ||
		!removalCleanupMatchesCandidateRequest(existing, candidate, request) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if err := WriteRemovalCleanup(relayConfigPath, existing); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return existing, nil
}

// RecordRemovalDataOutcome advances the exact durable intent while preserving
// its selection revision, including one created by refresh supersession.
func RecordRemovalDataOutcome(relayConfigPath string, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int, status string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.Phase != RemovalCleanupPhaseIntent ||
		record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending ||
		record.OperationRetryPending ||
		!removalCleanupMatchesCandidateRequest(record, candidate, request) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.Phase = RemovalCleanupPhaseDataOutcome
	record.MovedDataItems = moved
	record.DataOutcome = status
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// PrepareRemovalPackageCleanup advances only a preserve-data intent or a
// completed exact Trash outcome. It cannot skip ambiguous data state.
func PrepareRemovalPackageCleanup(relayConfigPath string, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil ||
		record.ExecutionResolution != "" || record.RecoveryPending || record.OperationRetryPending ||
		!removalCleanupMatchesCandidateRequest(record, candidate, request) ||
		moved != record.MovedDataItems {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	switch request.Mode {
	case RemovalModePreserveData:
		if record.Phase != RemovalCleanupPhaseIntent || moved != 0 {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
	case RemovalModeTrashSelected:
		if record.Phase != RemovalCleanupPhaseDataOutcome || record.DataOutcome != "completed" ||
			record.MovedDataItems != record.SelectedDataItems {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
	default:
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.Phase = RemovalCleanupPhasePackagePending
	record.PackageRetryPending = false
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// RemovalCleanupMatchesRequest compares the caller's opaque selector and exact
// item-ID sequence with a validated durable record. No pathname is accepted.
func RemovalCleanupMatchesRequest(record RemovalCleanupRecord, request OpenCodexRemovalRequest) bool {
	if validateRemovalCleanupRecord(record) != nil || validateRemovalRequest(request) != nil {
		return false
	}
	return record.InstallationID == request.Selection.ID && record.Fingerprint == request.Selection.Fingerprint &&
		record.Mode == request.Mode && record.SelectionDigest == RemovalDataSelectionDigest(request.DataItemIDs) &&
		record.SelectedDataItems == len(request.DataItemIDs) && sameOrderedStrings(record.SelectedItemIDs, request.DataItemIDs)
}

// MarkRemovalDataRefreshRequired durably converts ambiguous Trash state into a
// non-replayable record. Unknown intent outcomes and known partial outcomes are
// intentionally distinguished for bounded receipts.
func MarkRemovalDataRefreshRequired(relayConfigPath string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil || record.RecoveryPending ||
		record.OperationRetryPending {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	switch record.Phase {
	case RemovalCleanupPhaseIntent:
		if record.Mode != RemovalModeTrashSelected {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		retired, overflow := boundedRetiredSelection(record.RetiredItemIDs, record.SelectedItemIDs)
		if overflow {
			record.TrashSelectionLocked = true
		} else {
			record.RetiredItemIDs = retired
		}
		record.Phase = RemovalCleanupPhaseDataRefresh
		record.DataOutcome = removalDataOutcomeUnknown
	case RemovalCleanupPhaseDataOutcome:
		if record.Mode != RemovalModeTrashSelected || record.DataOutcome == "completed" {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		retired, overflow := boundedRetiredSelection(record.RetiredItemIDs, record.SelectedItemIDs)
		if overflow {
			record.TrashSelectionLocked = true
		} else {
			record.RetiredItemIDs = retired
		}
		record.Phase = RemovalCleanupPhaseDataRefresh
	case RemovalCleanupPhaseDataRefresh:
		// Recommit the exact record to restore rename + directory-fsync proof.
	default:
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// SupersedeRemovalDataRefreshRequired atomically replaces refresh-required
// evidence with a new intent. The caller must explicitly confirm the refresh,
// provide a freshly verified inventory for the same installation, and choose
// no previously selected item ID. Switching to preserve_data is allowed.
func SupersedeRemovalDataRefreshRequired(
	relayConfigPath string,
	request OpenCodexRemovalRequest,
	inventory OpenCodexDataInventoryReceipt,
	confirmed bool,
) (RemovalCleanupRecord, error) {
	if !confirmed {
		return RemovalCleanupRecord{}, ErrRemovalConfirmationNeeded
	}
	if err := validateRemovalRequest(request); err != nil {
		return RemovalCleanupRecord{}, err
	}
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.Phase != RemovalCleanupPhaseDataRefresh ||
		record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending ||
		record.InstallationID != request.Selection.ID || record.Fingerprint != request.Selection.Fingerprint ||
		!validateRefreshInventoryForRequest(inventory, request) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if request.Mode == RemovalModeTrashSelected &&
		(record.TrashSelectionLocked || record.SelectionRevision >= maxRemovalSelectionRevisions ||
			selectionsOverlap(record.RetiredItemIDs, request.DataItemIDs)) {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.Phase = RemovalCleanupPhaseIntent
	record.Mode = request.Mode
	record.SelectionDigest = RemovalDataSelectionDigest(request.DataItemIDs)
	record.SelectedItemIDs = append([]string{}, request.DataItemIDs...)
	record.SelectedDataItems = len(request.DataItemIDs)
	record.SelectionRevision++
	record.MovedDataItems = 0
	record.DataOutcome = ""
	record.PackageAttempt = 0
	record.OperationRetryPending = false
	record.PackageRetryPending = false
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

// RemovalRoutingGate fails closed while any mutating execution is ambiguous or
// a verified package-removal journal has not yet been explicitly released from
// an already-durable routing recovery state.
func RemovalRoutingGate(relayConfigPath string) error {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil {
		return ErrRemovalRoutingGate
	}
	if !exists {
		return nil
	}
	if record.ActiveExecution != nil || record.ExecutionResolution != "" || record.RecoveryPending ||
		record.Phase == RemovalCleanupPhasePackageInFlight ||
		record.Phase == RemovalCleanupPhasePackagePending && record.ProcessReconciledAfterReboot ||
		record.Phase == RemovalCleanupPhasePackageVerified && !record.RoutingRecoveryReleased ||
		record.FinalizationActive {
		return ErrRemovalRoutingGate
	}
	return nil
}

func RemovalRoutingGateReleasable(relayConfigPath string) bool {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	return err == nil && exists && record.ActiveExecution == nil && record.ExecutionResolution == "" &&
		(record.RecoveryPending ||
			record.Phase == RemovalCleanupPhasePackageVerified && !record.RoutingRecoveryReleased)
}

// ReleaseRemovalRoutingGateForRecovery is valid only after the caller has
// independently proved that the routing state is durably recovery_required.
// That state denies admission while Recover re-establishes an exact route.
func ReleaseRemovalRoutingGateForRecovery(relayConfigPath string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.ActiveExecution != nil || record.ExecutionResolution != "" {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if record.FinalizationActive {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if record.RecoveryPending {
		record.RecoveryPending = false
		if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
			return RemovalCleanupRecord{}, err
		}
		return record, nil
	}
	if record.Phase != RemovalCleanupPhasePackageVerified {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	if record.RoutingRecoveryReleased {
		if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
			return RemovalCleanupRecord{}, err
		}
		return record, nil
	}
	record.RoutingRecoveryReleased = true
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

func PreflightRemovalCleanup(relayConfigPath string) error {
	path := RemovalCleanupPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
		return ErrRemovalCleanupUnsafe
	}
	if _, _, err := ReadRemovalCleanup(relayConfigPath); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	return nil
}

func WriteRemovalCleanup(relayConfigPath string, record RemovalCleanupRecord) error {
	if err := validateRemovalCleanupRecord(record); err != nil {
		return err
	}
	if existing, exists, err := ReadRemovalCleanup(relayConfigPath); err != nil {
		return err
	} else if exists && !sameRemovalCleanupRecord(existing, record) && !validRemovalCleanupTransition(existing, record) {
		return ErrRemovalCleanupUnsafe
	}
	path := RemovalCleanupPath(relayConfigPath)
	return writeRemovalCleanupFile(path, record)
}

func writeRemovalCleanupFile(path string, record RemovalCleanupRecord) error {
	payload, err := json.Marshal(record)
	if err != nil || len(payload) > maxRemovalCleanupBytes-1 {
		return ErrRemovalCleanupUnsafe
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".open-codex-removal.")
	if err != nil {
		return ErrRemovalCleanupUnsafe
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return ErrRemovalCleanupUnsafe
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return ErrRemovalCleanupUnsafe
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ErrRemovalCleanupUnsafe
	}
	if err := temporary.Close(); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	if err := syncControlDirectory(filepath.Dir(path)); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	return nil
}

func ReadRemovalCleanup(relayConfigPath string) (RemovalCleanupRecord, bool, error) {
	path := RemovalCleanupPath(relayConfigPath)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return RemovalCleanupRecord{}, false, nil
	}
	if err := validateExisting(path); err != nil {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	payload, err := os.ReadFile(path)
	if err != nil || len(payload) == 0 || len(payload) > maxRemovalCleanupBytes {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record RemovalCleanupRecord
	if err := decoder.Decode(&record); err != nil {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	if record.SchemaVersion == legacyRemovalCleanupVersion {
		migrated, err := migrateLegacyRemovalCleanupRecord(record)
		if err != nil {
			return RemovalCleanupRecord{}, false, err
		}
		if err := writeRemovalCleanupFile(path, migrated); err != nil {
			return RemovalCleanupRecord{}, false, err
		}
		record = migrated
	}
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalCleanupRecord{}, false, err
	}
	return record, true, nil
}

func migrateLegacyRemovalCleanupRecord(record RemovalCleanupRecord) (RemovalCleanupRecord, error) {
	if record.SchemaVersion != legacyRemovalCleanupVersion || record.ActiveExecution != nil || record.ExecutionAttempt != 0 {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.SchemaVersion = removalCleanupSchemaVersion
	record.ExecutionAttempt = record.PackageAttempt
	if record.Phase == RemovalCleanupPhasePackageInFlight {
		if record.PackageAttempt < 1 || !isFingerprint(record.ExecutionBootSession) {
			return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
		}
		record.ActiveExecution = &RemovalActiveExecution{
			Kind:                 RemovalExecutionPackage,
			Attempt:              record.ExecutionAttempt,
			BootSession:          record.ExecutionBootSession,
			BootAttested:         false,
			LegacyUnattestedBoot: true,
		}
	}
	if err := validateRemovalCleanupRecord(record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

func BeginRemovalPackageExecution(relayConfigPath string) (RemovalCleanupRecord, error) {
	return BeginExecution(relayConfigPath, RemovalExecutionPackage)
}

func FinishRemovalPackageExecution(relayConfigPath string, result RemovalExecutionResult) (RemovalCleanupRecord, error) {
	return FinishExecution(relayConfigPath, RemovalExecutionPackage, result)
}

// ReconcileRemovalPackageAfterReboot is the only crash-recovery path for an
// in-flight package process. A changed, platform-attested boot session proves
// that the old process group cannot still exist. Exact path absence then
// selects verified; any residual path returns the transaction to pending for
// ordinary candidate revalidation and retry. PID-only evidence is never used.
func ReconcileRemovalPackageAfterReboot(relayConfigPath string, confirmed bool) (RemovalCleanupRecord, bool, error) {
	record, absent, err := ReconcileActiveExecutionAfterBoot(relayConfigPath, confirmed)
	if err != nil {
		return RemovalCleanupRecord{}, false, err
	}
	if record.Phase != RemovalCleanupPhasePackagePending && record.Phase != RemovalCleanupPhasePackageVerified {
		return RemovalCleanupRecord{}, false, ErrRemovalCleanupUnsafe
	}
	return record, absent, nil
}

func AdvanceRemovalDataOutcomeToPackagePending(relayConfigPath string) (RemovalCleanupRecord, error) {
	record, exists, err := ReadRemovalCleanup(relayConfigPath)
	if err != nil || !exists || record.Phase != RemovalCleanupPhaseDataOutcome || record.DataOutcome != "completed" ||
		record.MovedDataItems != record.SelectedDataItems {
		return RemovalCleanupRecord{}, ErrRemovalCleanupUnsafe
	}
	record.Phase = RemovalCleanupPhasePackagePending
	if err := WriteRemovalCleanup(relayConfigPath, record); err != nil {
		return RemovalCleanupRecord{}, err
	}
	return record, nil
}

func RemoveRemovalCleanup(relayConfigPath string) error {
	path := RemovalCleanupPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrRemovalCleanupUnsafe
	}
	if err := syncControlDirectory(filepath.Dir(path)); err != nil {
		return ErrRemovalCleanupUnsafe
	}
	return nil
}

func VerifyRemovalCleanupAbsent(record RemovalCleanupRecord) error {
	if err := validateRemovalCleanupRecord(record); err != nil {
		return err
	}
	if record.Phase != RemovalCleanupPhasePackageVerified {
		return ErrRemovalOutcomeUnknown
	}
	paths := uniqueSortedStrings(append([]string{record.PackageRoot}, record.Launchers...))
	prefix, ok := prefixFromPackageRoot(record.PackageRoot)
	if !ok {
		return ErrRemovalCleanupUnsafe
	}
	paths = uniqueSortedStrings(append(paths, filepath.Join(prefix, "bin", "ocx"), filepath.Join(prefix, "bin", "opencodex")))
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrRemovalOutcomeUnknown
		}
	}
	return nil
}

func validateRemovalCleanupRecord(record RemovalCleanupRecord) error {
	if record.SchemaVersion != removalCleanupSchemaVersion || record.Operation != "remove-open-codex" ||
		(record.Phase != RemovalCleanupPhaseIntent && record.Phase != RemovalCleanupPhaseDataOutcome &&
			record.Phase != RemovalCleanupPhaseDataRefresh && record.Phase != RemovalCleanupPhasePackagePending &&
			record.Phase != RemovalCleanupPhasePackageInFlight && record.Phase != RemovalCleanupPhasePackageVerified) ||
		!validRemovalSelection(NPMRemovalSelection{ID: record.InstallationID, Fingerprint: record.Fingerprint}) ||
		(record.Mode != RemovalModePreserveData && record.Mode != RemovalModeTrashSelected) || !isFingerprint(record.SelectionDigest) ||
		record.SelectedDataItems < 0 || record.SelectedDataItems > maxRemovalDataItems ||
		len(record.SelectedItemIDs) != record.SelectedDataItems || len(record.RetiredItemIDs) > maxRemovalRetiredDataItems ||
		record.SelectionRevision < 1 || record.SelectionRevision > maxRemovalSelectionRevisions+1 ||
		record.Mode == RemovalModeTrashSelected && record.SelectionRevision > maxRemovalSelectionRevisions ||
		record.MovedDataItems < 0 || record.MovedDataItems > record.SelectedDataItems ||
		record.ExecutionAttempt < 0 || record.ExecutionAttempt > maxRemovalExecutionAttempts ||
		record.PackageAttempt < 0 || record.PackageAttempt > maxRemovalPackageAttempts ||
		record.ExecutionBootSession != "" && !isFingerprint(record.ExecutionBootSession) ||
		!filepath.IsAbs(record.PackageRoot) || filepath.Clean(record.PackageRoot) != record.PackageRoot || !hasOpenCodexPackageSuffix(record.PackageRoot) ||
		len(record.Launchers) > 4 {
		return ErrRemovalCleanupUnsafe
	}
	if record.ActiveExecution != nil {
		active := record.ActiveExecution
		if !validRemovalExecutionKind(active.Kind) || active.Attempt < 1 || active.Attempt > maxRemovalExecutionAttempts ||
			!isFingerprint(active.BootSession) || record.ExecutionAttempt != active.Attempt ||
			record.RoutingRecoveryReleased ||
			(!active.BootAttested && !active.LegacyUnattestedBoot) ||
			(active.BootAttested && active.LegacyUnattestedBoot) ||
			(active.LegacyUnattestedBoot && active.Kind != RemovalExecutionPackage) {
			return ErrRemovalCleanupUnsafe
		}
		switch active.Kind {
		case RemovalExecutionPackage:
			if record.Phase != RemovalCleanupPhasePackageInFlight || record.PackageAttempt < 1 ||
				record.PackageAttempt > maxRemovalPackageAttempts || record.ExecutionBootSession != active.BootSession {
				return ErrRemovalCleanupUnsafe
			}
		case RemovalExecutionTeardown:
			if record.Phase != RemovalCleanupPhaseIntent || record.PackageAttempt != 0 ||
				record.ExecutionBootSession != "" || record.ProcessReconciledAfterReboot {
				return ErrRemovalCleanupUnsafe
			}
		case RemovalExecutionTrash:
			if record.Phase != RemovalCleanupPhaseIntent || record.Mode != RemovalModeTrashSelected ||
				record.PackageAttempt != 0 || record.ExecutionBootSession != "" || record.ProcessReconciledAfterReboot {
				return ErrRemovalCleanupUnsafe
			}
		}
	}
	if record.ExecutionResolution == "" && record.ResolutionRequiresRoutingRecovery ||
		record.ExecutionResolution != "" && (!validRemovalExecutionResolution(record.ExecutionResolution) ||
			record.RecoveryPending || record.ActiveExecution == nil ||
			!validRemovalExecutionResolutionForKind(record.ActiveExecution.Kind, record.ExecutionResolution) ||
			!validRemovalExecutionResolutionRoutingRequirement(
				record.ExecutionResolution, record.ResolutionRequiresRoutingRecovery,
			) ||
			record.RoutingRecoveryReleased || record.FinalizationActive) ||
		record.RecoveryPending && (record.ActiveExecution != nil || record.ExecutionResolution != "" ||
			record.ResolutionRequiresRoutingRecovery || record.RoutingRecoveryReleased || record.FinalizationActive ||
			record.Phase != RemovalCleanupPhaseIntent &&
				record.Phase != RemovalCleanupPhaseDataRefresh &&
				record.Phase != RemovalCleanupPhasePackagePending) {
		return ErrRemovalCleanupUnsafe
	}
	if record.ActiveExecution == nil && record.Phase == RemovalCleanupPhasePackageInFlight {
		return ErrRemovalCleanupUnsafe
	}
	seenItems := make(map[string]struct{}, len(record.SelectedItemIDs))
	for _, itemID := range record.SelectedItemIDs {
		if !validOpenCodexDataItemID(itemID) {
			return ErrRemovalCleanupUnsafe
		}
		if _, duplicate := seenItems[itemID]; duplicate {
			return ErrRemovalCleanupUnsafe
		}
		seenItems[itemID] = struct{}{}
	}
	seenRetired := make(map[string]struct{}, len(record.RetiredItemIDs))
	for _, itemID := range record.RetiredItemIDs {
		if !validOpenCodexDataItemID(itemID) {
			return ErrRemovalCleanupUnsafe
		}
		if _, duplicate := seenRetired[itemID]; duplicate {
			return ErrRemovalCleanupUnsafe
		}
		seenRetired[itemID] = struct{}{}
	}
	if !sameOrderedStrings(record.RetiredItemIDs, uniqueSortedStrings(append([]string(nil), record.RetiredItemIDs...))) ||
		record.SelectionDigest != RemovalDataSelectionDigest(record.SelectedItemIDs) ||
		record.RoutingRecoveryReleased && record.Phase != RemovalCleanupPhasePackageVerified ||
		record.RecoveryPending && record.RoutingRecoveryReleased ||
		record.FinalizationActive && (record.Phase != RemovalCleanupPhasePackageVerified ||
			!record.RoutingRecoveryReleased || record.ActiveExecution != nil || record.RecoveryPending) ||
		record.ProcessReconciledAfterReboot && record.Phase != RemovalCleanupPhasePackagePending ||
		record.OperationRetryPending && (record.Phase != RemovalCleanupPhaseIntent ||
			record.ActiveExecution != nil || record.ExecutionResolution != "" ||
			record.PackageAttempt != 0 || record.ExecutionBootSession != "") ||
		record.PackageRetryPending && (record.Phase != RemovalCleanupPhasePackagePending ||
			record.ActiveExecution != nil || record.ProcessReconciledAfterReboot ||
			record.ExecutionBootSession != "") ||
		record.TrashSelectionLocked && record.Mode == RemovalModeTrashSelected && record.Phase != RemovalCleanupPhaseDataRefresh {
		return ErrRemovalCleanupUnsafe
	}
	if record.Phase == RemovalCleanupPhaseDataRefresh {
		if !record.TrashSelectionLocked && !selectionContainedBy(record.SelectedItemIDs, seenRetired) {
			return ErrRemovalCleanupUnsafe
		}
	} else if selectionsOverlap(record.SelectedItemIDs, record.RetiredItemIDs) {
		return ErrRemovalCleanupUnsafe
	}
	if record.Mode == RemovalModePreserveData {
		if record.SelectedDataItems != 0 || record.MovedDataItems != 0 || record.DataOutcome != "" ||
			record.Phase == RemovalCleanupPhaseDataOutcome || record.Phase == RemovalCleanupPhaseDataRefresh {
			return ErrRemovalCleanupUnsafe
		}
	} else if record.SelectedDataItems == 0 {
		return ErrRemovalCleanupUnsafe
	}
	switch record.Phase {
	case RemovalCleanupPhaseIntent:
		if record.MovedDataItems != 0 || record.DataOutcome != "" || record.PackageAttempt != 0 ||
			record.ExecutionBootSession != "" || record.PackageRetryPending {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalCleanupPhaseDataOutcome:
		if record.Mode != RemovalModeTrashSelected || !validRemovalDataOutcome(record.DataOutcome) || record.PackageAttempt != 0 ||
			record.ExecutionBootSession != "" || record.DataOutcome == "completed" && record.MovedDataItems != record.SelectedDataItems {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalCleanupPhaseDataRefresh:
		if record.Mode != RemovalModeTrashSelected || record.PackageAttempt != 0 || record.ExecutionBootSession != "" ||
			record.PackageRetryPending ||
			(record.DataOutcome != removalDataOutcomeUnknown && (!validRemovalDataOutcome(record.DataOutcome) || record.DataOutcome == "completed")) ||
			record.DataOutcome == removalDataOutcomeUnknown && record.MovedDataItems != 0 {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalCleanupPhasePackagePending:
		if !validPackageReadyCleanupRecord(record) ||
			record.ProcessReconciledAfterReboot && !isFingerprint(record.ExecutionBootSession) ||
			!record.ProcessReconciledAfterReboot && record.ExecutionBootSession != "" ||
			record.PackageRetryPending && record.ProcessReconciledAfterReboot {
			return ErrRemovalCleanupUnsafe
		}
	case RemovalCleanupPhasePackageInFlight, RemovalCleanupPhasePackageVerified:
		if !validPackageReadyCleanupRecord(record) || record.PackageAttempt < 1 || !isFingerprint(record.ExecutionBootSession) ||
			record.PackageRetryPending {
			return ErrRemovalCleanupUnsafe
		}
	}
	prefix, ok := prefixFromPackageRoot(record.PackageRoot)
	if !ok {
		return ErrRemovalCleanupUnsafe
	}
	allowed := map[string]struct{}{
		filepath.Join(prefix, "bin", "ocx"):       {},
		filepath.Join(prefix, "bin", "opencodex"): {},
	}
	seen := make(map[string]struct{}, len(record.Launchers))
	for _, launcher := range record.Launchers {
		if !filepath.IsAbs(launcher) || filepath.Clean(launcher) != launcher {
			return ErrRemovalCleanupUnsafe
		}
		if _, ok := allowed[launcher]; !ok {
			return ErrRemovalCleanupUnsafe
		}
		if _, duplicate := seen[launcher]; duplicate {
			return ErrRemovalCleanupUnsafe
		}
		seen[launcher] = struct{}{}
	}
	return nil
}

func validPackageReadyCleanupRecord(record RemovalCleanupRecord) bool {
	if record.Mode == RemovalModePreserveData {
		return record.DataOutcome == "" && record.MovedDataItems == 0
	}
	return record.DataOutcome == "completed" && record.MovedDataItems == record.SelectedDataItems
}

func validRemovalDataOutcome(status string) bool {
	return status == "completed" || status == "partial" || status == "failed" || status == "unsupported"
}

func validRemovalCleanupTransition(previous, next RemovalCleanupRecord) bool {
	if validRemovalExecutionTransition(previous, next) {
		return true
	}
	if previous.RecoveryPending && !next.RecoveryPending {
		normalized := next
		normalized.RecoveryPending = true
		return sameRemovalCleanupRecord(previous, normalized)
	}
	if previous.Phase == RemovalCleanupPhasePackageVerified && next.Phase == RemovalCleanupPhasePackageVerified &&
		!previous.RoutingRecoveryReleased && next.RoutingRecoveryReleased {
		normalized := next
		normalized.RoutingRecoveryReleased = false
		normalized.FinalizationActive = false
		return sameRemovalCleanupRecord(previous, normalized)
	}
	if previous.Phase == RemovalCleanupPhasePackageVerified && next.Phase == RemovalCleanupPhasePackageVerified &&
		previous.RoutingRecoveryReleased && next.RoutingRecoveryReleased &&
		!previous.FinalizationActive && next.FinalizationActive {
		normalized := next
		normalized.FinalizationActive = false
		return sameRemovalCleanupRecord(previous, normalized)
	}
	if previous.Phase == RemovalCleanupPhasePackageVerified && next.Phase == RemovalCleanupPhasePackageInFlight &&
		previous.RoutingRecoveryReleased && !next.RoutingRecoveryReleased {
		previous.RoutingRecoveryReleased = false
	}
	if next.Phase == RemovalCleanupPhasePackageInFlight &&
		(previous.Phase == RemovalCleanupPhasePackagePending || previous.Phase == RemovalCleanupPhasePackageVerified) {
		previous.ExecutionBootSession = next.ExecutionBootSession
		previous.ProcessReconciledAfterReboot = next.ProcessReconciledAfterReboot
	}
	if previous.Phase == RemovalCleanupPhasePackageInFlight && next.Phase == RemovalCleanupPhasePackagePending {
		previous.ExecutionBootSession = next.ExecutionBootSession
		previous.ProcessReconciledAfterReboot = next.ProcessReconciledAfterReboot
	}
	if next.Phase == RemovalCleanupPhaseDataRefresh &&
		(previous.Phase == RemovalCleanupPhaseIntent || previous.Phase == RemovalCleanupPhaseDataOutcome) {
		retired, overflow := boundedRetiredSelection(previous.RetiredItemIDs, previous.SelectedItemIDs)
		if overflow {
			previous.TrashSelectionLocked = true
		} else {
			previous.RetiredItemIDs = retired
		}
	}
	if previous.Phase == RemovalCleanupPhaseDataRefresh && next.Phase == RemovalCleanupPhaseIntent {
		return validRemovalRefreshSupersession(previous, next)
	}
	if !sameRemovalCleanupAuthority(previous, next) {
		return false
	}
	switch {
	case previous.Phase == RemovalCleanupPhaseIntent && next.Phase == RemovalCleanupPhaseDataOutcome:
		return previous.Mode == RemovalModeTrashSelected && next.PackageAttempt == 0
	case previous.Phase == RemovalCleanupPhaseIntent && next.Phase == RemovalCleanupPhaseDataRefresh:
		return previous.Mode == RemovalModeTrashSelected && next.DataOutcome == removalDataOutcomeUnknown && next.PackageAttempt == 0
	case previous.Phase == RemovalCleanupPhaseIntent && next.Phase == RemovalCleanupPhasePackagePending:
		return previous.Mode == RemovalModePreserveData && next.PackageAttempt == 0
	case previous.Phase == RemovalCleanupPhaseDataOutcome && next.Phase == RemovalCleanupPhaseDataRefresh:
		return previous.DataOutcome != "completed" && next.DataOutcome == previous.DataOutcome &&
			next.MovedDataItems == previous.MovedDataItems && next.PackageAttempt == 0
	case previous.Phase == RemovalCleanupPhaseDataOutcome && next.Phase == RemovalCleanupPhasePackagePending:
		return previous.DataOutcome == "completed" && next.DataOutcome == "completed" &&
			next.MovedDataItems == next.SelectedDataItems && next.PackageAttempt == 0
	case (previous.Phase == RemovalCleanupPhasePackagePending || previous.Phase == RemovalCleanupPhasePackageVerified) &&
		next.Phase == RemovalCleanupPhasePackageInFlight:
		return next.PackageAttempt == previous.PackageAttempt+1
	case previous.Phase == RemovalCleanupPhasePackageInFlight && next.Phase == RemovalCleanupPhasePackageVerified:
		return next.PackageAttempt == previous.PackageAttempt
	case previous.Phase == RemovalCleanupPhasePackageInFlight && next.Phase == RemovalCleanupPhasePackagePending:
		return next.PackageAttempt == previous.PackageAttempt
	default:
		return false
	}
}

func validRemovalExecutionTransition(previous, next RemovalCleanupRecord) bool {
	// Mark a known-safe resolution while retaining the active witness. This
	// write is the crash boundary before routing is parked.
	if previous.ActiveExecution != nil && next.ActiveExecution != nil &&
		previous.ExecutionResolution == "" && !previous.RecoveryPending &&
		validRemovalExecutionResolutionForKind(next.ActiveExecution.Kind, next.ExecutionResolution) {
		normalized := previous
		normalized.ExecutionResolution = next.ExecutionResolution
		normalized.ResolutionRequiresRoutingRecovery = next.ResolutionRequiresRoutingRecovery
		return sameRemovalCleanupRecord(normalized, next)
	}

	// Complete a previously marked resolution after any required routing park.
	// The resolution helper supplies the exact expected phase/selection
	// transition; this guard prevents callers from clearing the witness with an
	// unrelated record.
	if previous.ActiveExecution != nil && previous.ExecutionResolution != "" &&
		next.ActiveExecution == nil && next.ExecutionResolution == "" &&
		!next.ResolutionRequiresRoutingRecovery {
		expected, err := resolvedRemovalExecutionRecord(previous)
		return err == nil && sameRemovalCleanupRecord(expected, next)
	}

	previousCore := previous
	nextCore := next
	previousCore.ActiveExecution = nil
	nextCore.ActiveExecution = nil
	previousCore.ExecutionAttempt = nextCore.ExecutionAttempt

	// Durable begin.
	if previous.ActiveExecution == nil && next.ActiveExecution != nil {
		active := next.ActiveExecution
		if !active.BootAttested || active.Attempt != previous.ExecutionAttempt+1 ||
			next.ExecutionAttempt != active.Attempt || !isFingerprint(active.BootSession) ||
			next.ExecutionResolution != "" || next.ResolutionRequiresRoutingRecovery ||
			next.RecoveryPending || next.PackageRetryPending {
			return false
		}
		switch active.Kind {
		case RemovalExecutionTeardown, RemovalExecutionTrash:
			if active.Kind == RemovalExecutionTeardown {
				previousCore.OperationRetryPending = nextCore.OperationRetryPending
			}
			return previous.Phase == RemovalCleanupPhaseIntent && next.Phase == previous.Phase &&
				previous.PackageAttempt == next.PackageAttempt &&
				sameRemovalCleanupRecord(previousCore, nextCore)
		case RemovalExecutionPackage:
			previousCore.Phase = nextCore.Phase
			previousCore.PackageAttempt = nextCore.PackageAttempt
			previousCore.ExecutionBootSession = nextCore.ExecutionBootSession
			previousCore.ProcessReconciledAfterReboot = nextCore.ProcessReconciledAfterReboot
			previousCore.PackageRetryPending = nextCore.PackageRetryPending
			previousCore.RoutingRecoveryReleased = nextCore.RoutingRecoveryReleased
			return (previous.Phase == RemovalCleanupPhasePackagePending || previous.Phase == RemovalCleanupPhasePackageVerified) &&
				next.Phase == RemovalCleanupPhasePackageInFlight &&
				next.PackageAttempt == previous.PackageAttempt+1 &&
				next.ExecutionBootSession == active.BootSession &&
				!next.ProcessReconciledAfterReboot && !next.RoutingRecoveryReleased &&
				sameRemovalCleanupRecord(previousCore, nextCore)
		default:
			return false
		}
	}

	// Durable finish or changed-boot reconciliation.
	if previous.ActiveExecution != nil && next.ActiveExecution == nil {
		active := previous.ActiveExecution
		if next.ExecutionAttempt != previous.ExecutionAttempt {
			return false
		}
		switch active.Kind {
		case RemovalExecutionTeardown:
			return previous.Phase == RemovalCleanupPhaseIntent && next.Phase == previous.Phase &&
				sameRemovalCleanupRecord(previousCore, nextCore)
		case RemovalExecutionTrash:
			if next.Phase == previous.Phase {
				return sameRemovalCleanupRecord(previousCore, nextCore)
			}
			if previous.Phase != RemovalCleanupPhaseIntent || next.Phase != RemovalCleanupPhaseDataRefresh ||
				next.DataOutcome != removalDataOutcomeUnknown || next.MovedDataItems != 0 {
				return false
			}
			retired, overflow := boundedRetiredSelection(previous.RetiredItemIDs, previous.SelectedItemIDs)
			if overflow {
				previousCore.TrashSelectionLocked = true
			} else {
				previousCore.RetiredItemIDs = retired
			}
			previousCore.Phase = nextCore.Phase
			previousCore.DataOutcome = nextCore.DataOutcome
			previousCore.MovedDataItems = nextCore.MovedDataItems
			return sameRemovalCleanupRecord(previousCore, nextCore)
		case RemovalExecutionPackage:
			if previous.Phase != RemovalCleanupPhasePackageInFlight ||
				(next.Phase != RemovalCleanupPhasePackagePending && next.Phase != RemovalCleanupPhasePackageVerified) ||
				next.PackageAttempt != previous.PackageAttempt {
				return false
			}
			previousCore.Phase = nextCore.Phase
			previousCore.ExecutionBootSession = nextCore.ExecutionBootSession
			previousCore.ProcessReconciledAfterReboot = nextCore.ProcessReconciledAfterReboot
			previousCore.PackageRetryPending = nextCore.PackageRetryPending
			return sameRemovalCleanupRecord(previousCore, nextCore)
		default:
			return false
		}
	}
	return false
}

func validRemovalRefreshSupersession(previous, next RemovalCleanupRecord) bool {
	if previous.SchemaVersion != next.SchemaVersion || previous.Operation != next.Operation ||
		previous.InstallationID != next.InstallationID || previous.Fingerprint != next.Fingerprint ||
		previous.PackageRoot != next.PackageRoot || !sameOrderedStrings(previous.Launchers, next.Launchers) ||
		!sameOrderedStrings(previous.RetiredItemIDs, next.RetiredItemIDs) ||
		previous.TrashSelectionLocked != next.TrashSelectionLocked || previous.RoutingRecoveryReleased != next.RoutingRecoveryReleased ||
		previous.FinalizationActive != next.FinalizationActive || previous.ExecutionResolution != next.ExecutionResolution ||
		previous.ResolutionRequiresRoutingRecovery != next.ResolutionRequiresRoutingRecovery ||
		previous.RecoveryPending != next.RecoveryPending || previous.OperationRetryPending != next.OperationRetryPending ||
		previous.PackageRetryPending != next.PackageRetryPending ||
		next.SelectionRevision != previous.SelectionRevision+1 || next.MovedDataItems != 0 || next.DataOutcome != "" ||
		next.PackageAttempt != 0 || selectionsOverlap(previous.RetiredItemIDs, next.SelectedItemIDs) {
		return false
	}
	if next.Mode == RemovalModeTrashSelected &&
		(previous.TrashSelectionLocked || previous.SelectionRevision >= maxRemovalSelectionRevisions) {
		return false
	}
	return true
}

func sameRemovalCleanupAuthority(left, right RemovalCleanupRecord) bool {
	left.Phase = right.Phase
	left.MovedDataItems = right.MovedDataItems
	left.DataOutcome = right.DataOutcome
	left.PackageAttempt = right.PackageAttempt
	return sameRemovalCleanupRecord(left, right)
}

func removalCleanupMatchesCandidateRequest(record RemovalCleanupRecord, candidate NPMInstallation, request OpenCodexRemovalRequest) bool {
	if !RemovalCleanupMatchesRequest(record, request) {
		return false
	}
	expected, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		return false
	}
	expected.SelectionRevision = record.SelectionRevision
	expected.RetiredItemIDs = append([]string(nil), record.RetiredItemIDs...)
	expected.TrashSelectionLocked = record.TrashSelectionLocked
	expected.ExecutionAttempt = record.ExecutionAttempt
	actual := record
	actual.Phase = RemovalCleanupPhaseIntent
	actual.MovedDataItems = 0
	actual.DataOutcome = ""
	actual.PackageAttempt = 0
	actual.ActiveExecution = nil
	actual.ExecutionBootSession = ""
	actual.ProcessReconciledAfterReboot = false
	actual.OperationRetryPending = false
	actual.PackageRetryPending = false
	return sameRemovalCleanupRecord(actual, expected)
}

func validRefreshInventory(inventory OpenCodexDataInventoryReceipt, request OpenCodexRemovalRequest) bool {
	if inventory.SchemaVersion != OpenCodexInventorySchemaVersion || inventory.Operation != "open-codex-data-inventory" ||
		inventory.InstallationID != request.Selection.ID || len(inventory.Items) > maxRemovalInventoryItems {
		return false
	}
	if request.Mode == RemovalModePreserveData {
		if inventory.Status != "verified" && inventory.Status != "absent" || inventory.Status == "absent" && len(inventory.Items) != 0 {
			return false
		}
	} else if inventory.Status != "verified" {
		return false
	}
	items := make(map[string]OpenCodexDataInventoryItem, len(inventory.Items))
	for _, item := range inventory.Items {
		if !validOpenCodexDataItemID(item.ID) {
			return false
		}
		if _, duplicate := items[item.ID]; duplicate {
			return false
		}
		items[item.ID] = item
	}
	if request.Mode == RemovalModePreserveData {
		return true
	}
	for _, itemID := range request.DataItemIDs {
		item, ok := items[itemID]
		if !ok || !item.Exists || !item.Trashable {
			return false
		}
	}
	return true
}

func validateRefreshInventoryForRequest(inventory OpenCodexDataInventoryReceipt, request OpenCodexRemovalRequest) bool {
	if request.ExpectedRoutingGeneration == 0 ||
		inventory.RoutingGeneration != request.ExpectedRoutingGeneration ||
		inventory.InstallationFingerprint != request.Selection.Fingerprint ||
		!validRefreshInventory(inventory, request) {
		return false
	}
	return request.Mode != RemovalModeTrashSelected ||
		(request.ExpectedInventoryRevision != "" && inventory.InventoryRevision == request.ExpectedInventoryRevision)
}

func boundedRetiredSelection(existing, selected []string) ([]string, bool) {
	combined := uniqueSortedStrings(append(append([]string(nil), existing...), selected...))
	if len(combined) > maxRemovalRetiredDataItems {
		return append([]string(nil), existing...), true
	}
	return combined, false
}

func selectionContainedBy(selected []string, allowed map[string]struct{}) bool {
	for _, itemID := range selected {
		if _, ok := allowed[itemID]; !ok {
			return false
		}
	}
	return true
}

func selectionsOverlap(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, exists := seen[value]; exists {
			return true
		}
	}
	return false
}

func sameOrderedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameRemovalCleanupRecord(left, right RemovalCleanupRecord) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
