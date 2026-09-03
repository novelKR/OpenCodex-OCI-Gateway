package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	runtimeRoutingSchemaVersion = 1
	maxRuntimeRoutingBytes      = 16 << 10
)

var (
	ErrRuntimeRoutingConflict = errors.New("container runtime routing conflicts with current routing state")
	ErrRuntimeRoutingWitness  = errors.New("container runtime routing witness is invalid")
)

// RuntimeRoutingDirection is deliberately finite. A lifecycle recovery may
// either finish the route selected by the container operation or restore the
// exact backend that was recorded before that operation; it may not name a new
// route during recovery.
type RuntimeRoutingDirection string

const (
	RuntimeRoutingCompleteTarget RuntimeRoutingDirection = "complete_target"
	RuntimeRoutingRestoreOrigin  RuntimeRoutingDirection = "restore_origin"
)

// RuntimeRoutingIntent is the non-secret identity shared with one ordinary
// container-runtime activation, stopped restart, or stop journal. Container
// IDs remain the lifecycle manager's responsibility; routing binds only the
// immutable artifacts and state generations needed to reject an unrelated
// operation.
type RuntimeRoutingIntent struct {
	OperationID        string `json:"operation_id"`
	InstallationID     string `json:"installation_id"`
	OldManifestSHA256  string `json:"old_manifest_sha256"`
	NewManifestSHA256  string `json:"new_manifest_sha256"`
	OldImageDigest     string `json:"old_image_digest"`
	NewImageDigest     string `json:"new_image_digest"`
	OldStateGeneration uint64 `json:"old_state_generation"`
	NewStateGeneration uint64 `json:"new_state_generation"`
}

func (i RuntimeRoutingIntent) validate(targetAppleActive bool) error {
	if !validMaintenanceID(i.OperationID) || !validMaintenanceID(i.InstallationID) ||
		!validRuntimeArtifact(i.OldManifestSHA256, i.OldImageDigest, i.OldStateGeneration) ||
		!validRuntimeArtifact(i.NewManifestSHA256, i.NewImageDigest, i.NewStateGeneration) {
		return ErrRuntimeRoutingWitness
	}
	oldAbsent := runtimeArtifactAbsent(i.OldManifestSHA256, i.OldImageDigest, i.OldStateGeneration)
	newAbsent := runtimeArtifactAbsent(i.NewManifestSHA256, i.NewImageDigest, i.NewStateGeneration)
	if targetAppleActive {
		// First activation has no old artifact. A stopped restart binds the same
		// artifact and state generation on both sides. Live-image replacement is
		// owned by the separate maintenance protocol.
		if newAbsent {
			return ErrRuntimeRoutingWitness
		}
		return nil
	}
	if oldAbsent || !newAbsent {
		return ErrRuntimeRoutingWitness
	}
	return nil
}

func validRuntimeArtifact(manifest, image string, generation uint64) bool {
	if runtimeArtifactAbsent(manifest, image, generation) {
		return true
	}
	return validSHA256(manifest) && validOCIDigest(image) && generation != 0
}

func runtimeArtifactAbsent(manifest, image string, generation uint64) bool {
	return manifest == "absent" && image == "absent" && generation == 0
}

type RuntimeRoutingRequest struct {
	Intent                          RuntimeRoutingIntent
	ExpectedOriginRoutingGeneration uint64
	TargetAppleActive               bool
	Direction                       RuntimeRoutingDirection
}

func (r RuntimeRoutingRequest) validate() error {
	if r.ExpectedOriginRoutingGeneration == 0 || r.ExpectedOriginRoutingGeneration > ^uint64(0)-3 ||
		r.Intent.validate(r.TargetAppleActive) != nil ||
		(r.Direction != RuntimeRoutingCompleteTarget && r.Direction != RuntimeRoutingRestoreOrigin) {
		return ErrRuntimeRoutingWitness
	}
	return nil
}

type runtimeRoutingPhase string

const (
	runtimeRoutingTransitioning runtimeRoutingPhase = "transitioning"
	runtimeRoutingResolved      runtimeRoutingPhase = "resolved"
)

type runtimeRoutingAttemptKind string

const (
	runtimeRoutingAttemptSwitch         runtimeRoutingAttemptKind = "switch"
	runtimeRoutingAttemptApplyPending   runtimeRoutingAttemptKind = "apply_pending"
	runtimeRoutingAttemptResumeApplying runtimeRoutingAttemptKind = "resume_applying"
	runtimeRoutingAttemptRecover        runtimeRoutingAttemptKind = "recover"
	runtimeRoutingAttemptReplace        runtimeRoutingAttemptKind = "replace"
)

// runtimeRoutingJournal is adjacent to the routing state and survives until
// the container lifecycle manager acknowledges its own durable commit. The
// attempt fields are rewritten before every routing mutation, so a crash can
// be classified from an exact operation, source state, and generation plan.
type runtimeRoutingJournal struct {
	Schema               int                  `json:"schema"`
	BoundConfigPath      string               `json:"bound_config_path"`
	BoundCodexConfigPath string               `json:"bound_codex_config_path"`
	Intent               RuntimeRoutingIntent `json:"intent"`
	OriginBackend        Backend              `json:"origin_backend"`
	TargetBackend        Backend              `json:"target_backend"`
	OriginGeneration     uint64               `json:"origin_routing_generation"`
	TargetAppleActive    bool                 `json:"target_apple_active"`
	Phase                runtimeRoutingPhase  `json:"phase"`

	AttemptKind                runtimeRoutingAttemptKind `json:"attempt_kind"`
	AttemptDirection           RuntimeRoutingDirection   `json:"attempt_direction"`
	AttemptStartGeneration     uint64                    `json:"attempt_start_generation"`
	AttemptStartDesiredBackend Backend                   `json:"attempt_start_desired_backend"`
	AttemptStartAppliedBackend Backend                   `json:"attempt_start_applied_backend"`
	AttemptStartPhase          Phase                     `json:"attempt_start_phase"`
	AttemptOriginBackend       Backend                   `json:"attempt_origin_backend"`
	AttemptTargetBackend       Backend                   `json:"attempt_target_backend"`
	AttemptRequestedGeneration uint64                    `json:"attempt_requested_generation,omitempty"`
	AttemptRecoveryGeneration  uint64                    `json:"attempt_recovery_generation,omitempty"`
	AttemptApplyingGeneration  uint64                    `json:"attempt_applying_generation,omitempty"`
	AttemptFinalGeneration     uint64                    `json:"attempt_final_generation"`
	SourceJournalGeneration    uint64                    `json:"source_journal_generation,omitempty"`
	SourceJournalOriginBackend Backend                   `json:"source_journal_origin_backend,omitempty"`
	SourceJournalTargetBackend Backend                   `json:"source_journal_target_backend,omitempty"`

	ResolvedGeneration  uint64 `json:"resolved_routing_generation,omitempty"`
	ResolvedAppleActive bool   `json:"resolved_apple_active,omitempty"`
}

func RuntimeRoutingPath(configPath string) string {
	return filepath.Clean(configPath) + ".runtime-routing.json"
}

func (s *Store) RuntimeRoutingPath() string {
	if s == nil {
		return ""
	}
	return RuntimeRoutingPath(s.configPath)
}

func (j runtimeRoutingJournal) request() RuntimeRoutingRequest {
	return RuntimeRoutingRequest{
		Intent: j.Intent, ExpectedOriginRoutingGeneration: j.OriginGeneration,
		TargetAppleActive: j.TargetAppleActive, Direction: j.AttemptDirection,
	}
}

func (j runtimeRoutingJournal) validate(configPath, codexConfigPath string) error {
	boundConfig, configErr := canonicalConfigPath(configPath)
	boundCodex, codexErr := canonicalCodexConfigPath(codexConfigPath)
	if configErr != nil || codexErr != nil || j.Schema != runtimeRoutingSchemaVersion ||
		j.BoundConfigPath != boundConfig || j.BoundCodexConfigPath != boundCodex ||
		j.OriginGeneration == 0 || j.OriginGeneration > ^uint64(0)-3 ||
		j.Intent.validate(j.TargetAppleActive) != nil || !validBackend(j.OriginBackend) ||
		!validBackend(j.TargetBackend) || j.OriginBackend == j.TargetBackend ||
		(j.TargetAppleActive && j.TargetBackend != BackendLocalAppleContainer) ||
		(!j.TargetAppleActive && (j.TargetBackend != BackendNone || j.OriginBackend != BackendLocalAppleContainer)) ||
		(j.TargetAppleActive && j.OriginBackend == BackendLocalAppleContainer) ||
		(j.Phase != runtimeRoutingTransitioning && j.Phase != runtimeRoutingResolved) {
		return ErrRuntimeRoutingWitness
	}
	if j.AttemptDirection != RuntimeRoutingCompleteTarget && j.AttemptDirection != RuntimeRoutingRestoreOrigin {
		return ErrRuntimeRoutingWitness
	}
	selected := j.selectedBackend(j.AttemptDirection)
	if j.AttemptStartGeneration == 0 || !validBackend(j.AttemptStartDesiredBackend) ||
		!validBackend(j.AttemptStartAppliedBackend) || j.AttemptTargetBackend != selected ||
		!validBackend(j.AttemptOriginBackend) ||
		(j.AttemptOriginBackend == j.AttemptTargetBackend && j.AttemptKind != runtimeRoutingAttemptReplace) ||
		j.AttemptFinalGeneration == 0 {
		return ErrRuntimeRoutingWitness
	}
	if j.SourceJournalGeneration == 0 {
		if j.SourceJournalOriginBackend != "" || j.SourceJournalTargetBackend != "" {
			return ErrRuntimeRoutingWitness
		}
	} else if !validBackend(j.SourceJournalOriginBackend) || !validBackend(j.SourceJournalTargetBackend) ||
		j.SourceJournalOriginBackend == j.SourceJournalTargetBackend {
		return ErrRuntimeRoutingWitness
	}
	if !j.validAttemptGenerations() {
		return ErrRuntimeRoutingWitness
	}
	if j.Phase == runtimeRoutingResolved {
		if j.ResolvedGeneration == 0 || j.ResolvedGeneration != j.AttemptFinalGeneration ||
			j.ResolvedAppleActive != (selected == BackendLocalAppleContainer) {
			return ErrRuntimeRoutingWitness
		}
	} else if j.ResolvedGeneration != 0 || j.ResolvedAppleActive {
		return ErrRuntimeRoutingWitness
	}
	return nil
}

func (j runtimeRoutingJournal) validAttemptGenerations() bool {
	start := j.AttemptStartGeneration
	switch j.AttemptKind {
	case runtimeRoutingAttemptSwitch:
		return start <= ^uint64(0)-3 && j.AttemptRequestedGeneration == start+1 &&
			j.AttemptRecoveryGeneration == 0 && j.AttemptApplyingGeneration == start+2 &&
			j.AttemptFinalGeneration == start+3
	case runtimeRoutingAttemptApplyPending:
		return start <= ^uint64(0)-2 && j.AttemptRequestedGeneration == start &&
			j.AttemptRecoveryGeneration == 0 && j.AttemptApplyingGeneration == start+1 &&
			j.AttemptFinalGeneration == start+2
	case runtimeRoutingAttemptResumeApplying:
		return start != ^uint64(0) && j.AttemptRequestedGeneration == 0 &&
			j.AttemptRecoveryGeneration == 0 && j.AttemptApplyingGeneration == start &&
			j.AttemptFinalGeneration == start+1
	case runtimeRoutingAttemptRecover:
		return j.AttemptRequestedGeneration == 0 && j.AttemptRecoveryGeneration != 0 &&
			j.AttemptRecoveryGeneration <= ^uint64(0)-2 &&
			j.AttemptApplyingGeneration == j.AttemptRecoveryGeneration+1 &&
			j.AttemptFinalGeneration == j.AttemptRecoveryGeneration+2
	case runtimeRoutingAttemptReplace:
		return start != ^uint64(0) && j.AttemptRequestedGeneration == 0 &&
			j.AttemptRecoveryGeneration == 0 && j.AttemptApplyingGeneration == 0 &&
			j.AttemptFinalGeneration == start+1
	default:
		return false
	}
}

func (j runtimeRoutingJournal) selectedBackend(direction RuntimeRoutingDirection) Backend {
	if direction == RuntimeRoutingRestoreOrigin {
		return j.OriginBackend
	}
	return j.TargetBackend
}

func (j runtimeRoutingJournal) matchesRequest(request RuntimeRoutingRequest) bool {
	return request.validate() == nil && j.Intent == request.Intent &&
		j.OriginGeneration == request.ExpectedOriginRoutingGeneration &&
		j.TargetAppleActive == request.TargetAppleActive
}

func (s *Store) loadRuntimeRouting(codexConfigPath string) (runtimeRoutingJournal, bool, error) {
	if s == nil {
		return runtimeRoutingJournal{}, false, ErrRuntimeRoutingWitness
	}
	payload, err := readStateFile(s.RuntimeRoutingPath())
	if errors.Is(err, os.ErrNotExist) {
		return runtimeRoutingJournal{}, false, nil
	}
	if err != nil {
		return runtimeRoutingJournal{}, false, err
	}
	if len(payload) > maxRuntimeRoutingBytes || rejectDuplicateJSONKeys(payload) != nil {
		return runtimeRoutingJournal{}, false, ErrRuntimeRoutingWitness
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var journal runtimeRoutingJournal
	if err := decoder.Decode(&journal); err != nil {
		return runtimeRoutingJournal{}, false, ErrRuntimeRoutingWitness
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return runtimeRoutingJournal{}, false, ErrRuntimeRoutingWitness
	}
	if err := journal.validate(s.configPath, codexConfigPath); err != nil {
		return runtimeRoutingJournal{}, false, err
	}
	return journal, true, nil
}

func (s *Store) writeRuntimeRouting(codexConfigPath string, journal runtimeRoutingJournal) error {
	if s == nil || journal.validate(s.configPath, codexConfigPath) != nil {
		return ErrRuntimeRoutingWitness
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return writeControlFileAtomically(s.RuntimeRoutingPath(), ".runtime-routing.", append(payload, '\n'))
}

func (s *Store) removeRuntimeRouting() error {
	if s == nil {
		return ErrRuntimeRoutingWitness
	}
	path := s.RuntimeRoutingPath()
	if err := validateExistingControlFile(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove container runtime routing witness: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

// RuntimeRoutingPending validates the strict witness and its relation to the
// current routing state/journal. It is used by the lifecycle bridge and by
// ordinary routing interlocks; malformed or unrelated evidence is an error,
// never an invitation to infer a backend.
func (c *Controller) RuntimeRoutingPending(ctx context.Context) (bool, error) {
	if c == nil || c.store == nil {
		return false, ErrRuntimeRoutingWitness
	}
	lock, err := c.store.ReadLock(ctx)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	journal, found, err := c.store.loadRuntimeRouting(c.codexConfigPath)
	if err != nil || !found {
		return found, err
	}
	state, legacy, stateErr := c.store.Read()
	transaction, transactionFound, transactionErr := c.loadJournal()
	if stateErr != nil || legacy || transactionErr != nil ||
		!journal.matchesState(state, transaction, transactionFound) {
		return true, ErrRuntimeRoutingConflict
	}
	return true, nil
}

// SwitchRuntimeRouting starts or resumes one ordinary lifecycle-owned route.
// The witness is durable before the request generation is saved. It remains
// after successful routing and is released only by AcknowledgeRuntimeRouting.
func (c *Controller) SwitchRuntimeRouting(ctx context.Context, request RuntimeRoutingRequest, desktopExited bool) (uint64, error) {
	if !desktopExited {
		return 0, ErrDesktopExitConfirmation
	}
	if request.validate() != nil || request.Direction != RuntimeRoutingCompleteTarget {
		return 0, ErrRuntimeRoutingWitness
	}
	if err := c.beginRuntimeRouting(ctx, request); err != nil {
		return 0, err
	}
	return c.ReconcileRuntimeRouting(ctx, request, desktopExited)
}

func (c *Controller) beginRuntimeRouting(ctx context.Context, request RuntimeRoutingRequest) error {
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if c.recoveryGateActive() {
		return ErrRuntimeRoutingConflict
	}
	if _, found, err := c.store.loadMaintenance(); err != nil || found {
		return ErrRuntimeRoutingConflict
	}
	existing, found, err := c.store.loadRuntimeRouting(c.codexConfigPath)
	if err != nil {
		return ErrRuntimeRoutingConflict
	}
	if found {
		if !existing.matchesRequest(request) {
			return ErrRuntimeRoutingConflict
		}
		state, legacy, stateErr := c.store.Read()
		transaction, transactionFound, transactionErr := c.loadJournal()
		if stateErr != nil || legacy || transactionErr != nil || !existing.matchesState(state, transaction, transactionFound) {
			return ErrRuntimeRoutingConflict
		}
		return nil
	}
	if _, transactionFound, transactionErr := c.loadJournal(); transactionErr != nil || transactionFound {
		return ErrRuntimeRoutingConflict
	}
	state, legacy, err := c.boundState(lock)
	if err != nil || legacy || state.Generation != request.ExpectedOriginRoutingGeneration || !stableRoutingState(state) {
		return ErrRuntimeRoutingConflict
	}
	target := BackendNone
	if request.TargetAppleActive {
		target = BackendLocalAppleContainer
	}
	if state.AppliedBackend == target || (!request.TargetAppleActive && state.AppliedBackend != BackendLocalAppleContainer) {
		return ErrRuntimeRoutingConflict
	}
	if err := c.preflightRequestedBackend(ctx, target); err != nil {
		return err
	}
	witness := runtimeRoutingJournal{
		Schema: runtimeRoutingSchemaVersion, BoundConfigPath: c.store.ConfigPath(), BoundCodexConfigPath: c.codexConfigPath,
		Intent: request.Intent, OriginBackend: state.AppliedBackend, TargetBackend: target,
		OriginGeneration: state.Generation, TargetAppleActive: request.TargetAppleActive,
		Phase: runtimeRoutingTransitioning,
	}
	if err := witness.setAttempt(runtimeRoutingAttemptSwitch, request.Direction, state, 0, BackendUnknown, BackendUnknown); err != nil {
		return err
	}
	// This write is the cross-journal authority. No routing state, Codex file,
	// or resident runtime has changed when it is made durable.
	if err := c.store.writeRuntimeRouting(c.codexConfigPath, witness); err != nil {
		return err
	}
	next, changed, err := requestBackendTransition(state, target)
	if err != nil || !changed {
		return ErrRuntimeRoutingConflict
	}
	next.KnownLegacyBackupAndMigrate = false
	next.Generation = witness.AttemptRequestedGeneration
	if err := lock.Save(next); err != nil {
		return err
	}
	return nil
}

// ReconcileRuntimeRouting continues only the exact lifecycle operation named
// by request. It never accepts an already-selected backend without the durable
// witness, and every new recovery attempt is itself recorded before mutation.
func (c *Controller) ReconcileRuntimeRouting(ctx context.Context, request RuntimeRoutingRequest, desktopExited bool) (uint64, error) {
	if !desktopExited {
		return 0, ErrDesktopExitConfirmation
	}
	if request.validate() != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if c.recoveryGateActive() {
		return 0, ErrRuntimeRoutingConflict
	}
	if _, found, err := c.store.loadMaintenance(); err != nil || found {
		return 0, ErrRuntimeRoutingConflict
	}
	witness, found, err := c.store.loadRuntimeRouting(c.codexConfigPath)
	if err != nil || !found || !witness.matchesRequest(request) {
		return 0, ErrRuntimeRoutingConflict
	}
	state, legacy, stateErr := c.store.Read()
	transaction, transactionFound, transactionErr := c.loadJournal()
	if stateErr != nil || legacy || transactionErr != nil || !witness.matchesState(state, transaction, transactionFound) {
		return 0, ErrRuntimeRoutingConflict
	}
	selected := witness.selectedBackend(request.Direction)
	if stableStateForBackend(state, selected) && !transactionFound {
		if err := c.verifyStableRuntimeRoute(ctx, state, selected); err != nil {
			return 0, err
		}
		if err := c.markRuntimeRoutingResolved(&witness, request.Direction, state); err != nil {
			return 0, err
		}
		return state.Generation, nil
	}

	if transactionFound {
		return c.reconcileRuntimeRoutingTransaction(ctx, lock, &witness, state, transaction, request.Direction, selected)
	}
	if state.Phase == PhaseRecoveryRequired {
		return c.reconcileRuntimeRoutingWithoutTransaction(ctx, lock, &witness, state, request.Direction, selected)
	}
	if stableRoutingState(state) {
		return c.switchStableRuntimeRouting(ctx, lock, &witness, state, request.Direction, selected)
	}
	if pendingRoutingState(state) {
		if state.DesiredBackend == selected {
			return c.applyPendingRuntimeRouting(ctx, lock, &witness, state, request.Direction, selected)
		}
		if state.AppliedBackend == selected {
			return c.replaceRuntimeRoutingState(ctx, lock, &witness, state, request.Direction, selected)
		}
		return 0, ErrRuntimeRoutingConflict
	}
	if state.Phase == PhaseApplying {
		if state.DesiredBackend == selected {
			return c.resumeApplyingRuntimeRouting(ctx, lock, &witness, state, request.Direction, selected)
		}
		if state.AppliedBackend == selected {
			return c.replaceRuntimeRoutingState(ctx, lock, &witness, state, request.Direction, selected)
		}
	}
	return 0, ErrRuntimeRoutingConflict
}

func (c *Controller) switchStableRuntimeRouting(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if state.AppliedBackend == selected {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := c.preflightRequestedBackend(ctx, selected); err != nil {
		return 0, err
	}
	if err := witness.setAttempt(runtimeRoutingAttemptSwitch, direction, state, 0, BackendUnknown, BackendUnknown); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	next, changed, err := requestBackendTransition(state, selected)
	if err != nil || !changed {
		return 0, ErrRuntimeRoutingConflict
	}
	next.Generation = witness.AttemptRequestedGeneration
	if err := lock.Save(next); err != nil {
		return 0, err
	}
	return c.applyPendingRuntimeRouting(ctx, lock, witness, next, direction, selected)
}

func (c *Controller) applyPendingRuntimeRouting(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if state.DesiredBackend != selected || state.AppliedBackend == selected {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := witness.setAttempt(runtimeRoutingAttemptApplyPending, direction, state, 0, BackendUnknown, BackendUnknown); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	status, err := c.applyLocked(ctx, lock, state)
	if err != nil {
		return 0, err
	}
	if status.Generation != witness.AttemptFinalGeneration || status.AppliedBackend != selected {
		return 0, ErrRuntimeRoutingConflict
	}
	final, _, readErr := c.store.Read()
	if readErr != nil || !stableStateForBackend(final, selected) {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
		return 0, err
	}
	return final.Generation, nil
}

func (c *Controller) resumeApplyingRuntimeRouting(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if state.DesiredBackend != selected || state.AppliedBackend == selected {
		return 0, ErrRuntimeRoutingConflict
	}
	preflight, err := c.preflightBackendWithLegacyIntent(selected, state.KnownLegacyBackupAndMigrate)
	if err != nil {
		return 0, err
	}
	if err := witness.setAttempt(runtimeRoutingAttemptResumeApplying, direction, state, 0, BackendUnknown, BackendUnknown); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	status, err := c.finishApplyingLockedWithOriginAuthority(ctx, lock, state, preflight.Config, selected, true, preflight.LegacyMigrationRequired)
	if err != nil {
		return 0, err
	}
	if status.Generation != witness.AttemptFinalGeneration || status.AppliedBackend != selected {
		return 0, ErrRuntimeRoutingConflict
	}
	final, _, readErr := c.store.Read()
	if readErr != nil || !stableStateForBackend(final, selected) {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
		return 0, err
	}
	return final.Generation, nil
}

func (c *Controller) reconcileRuntimeRoutingTransaction(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, transaction transactionJournal, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if transaction.Kind != transactionKindRoutingSwitch ||
		(selected != transaction.TargetBackend && selected != transaction.OriginBackend) ||
		c.journalMatchesCurrentFiles(transaction) != nil {
		return 0, ErrRuntimeRoutingConflict
	}
	if stableStateForBackend(state, selected) && transaction.TargetBackend == selected && transaction.Stage == transactionConfigMutated {
		// The final state is already durable. Retiring the routing journal is safe
		// only after its exact file fingerprints and lifecycle witness match.
		if err := c.removeJournal(); err != nil {
			return 0, err
		}
		if err := c.verifyStableRuntimeRoute(ctx, state, selected); err != nil {
			return 0, err
		}
		if err := c.markRuntimeRoutingResolved(witness, direction, state); err != nil {
			return 0, err
		}
		return state.Generation, nil
	}
	recovery, err := c.recoveryStateFromJournal(transaction)
	if err != nil {
		return 0, ErrRuntimeRoutingConflict
	}
	// A nested recovery can fail after it has durably advanced beyond the
	// source routing journal (for example E+4 applying while the E+2 source
	// journal is still present, followed by E+5 recovery_required when writing
	// the replacement journal fails). The cross witness proves those positions.
	// Never replace them with the older journal-derived E+3 state: retain an
	// existing recovery generation, or allocate one fresh generation when the
	// exact applying position still needs to be parked.
	if state.Generation > recovery.Generation {
		if state.Generation > ^uint64(0)-2 {
			return 0, ErrRuntimeRoutingConflict
		}
		recovery.Generation = state.Generation
		if state.Phase != PhaseRecoveryRequired {
			recovery.Generation++
		}
	}
	if err := witness.setRecoveryAttempt(direction, state, recovery.Generation, transaction); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	if state != recovery {
		if err := lock.Replace(recovery); err != nil {
			return 0, err
		}
	}
	status, err := c.applyRecoveryLockedWithOriginAuthority(
		ctx, lock, recovery, selected, transaction.OriginAuthoritative,
		transaction.KnownLegacyBackupAndMigrate,
	)
	if err != nil {
		return 0, err
	}
	if status.Generation != witness.AttemptFinalGeneration || status.AppliedBackend != selected {
		return 0, ErrRuntimeRoutingConflict
	}
	final, _, readErr := c.store.Read()
	if readErr != nil || !stableStateForBackend(final, selected) {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
		return 0, err
	}
	return final.Generation, nil
}

func (c *Controller) reconcileRuntimeRoutingWithoutTransaction(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if state.DesiredBackend == state.AppliedBackend {
		if state.AppliedBackend == selected {
			return c.replaceRuntimeRoutingState(ctx, lock, witness, state, direction, selected)
		}
		if state.AppliedBackend != witness.selectedBackend(oppositeRuntimeDirection(direction)) {
			return 0, ErrRuntimeRoutingConflict
		}
		recovery := state
		recovery.DesiredBackend = selected
		recovery.DesiredMode = modeForBackend(selected)
		if err := witness.setRecoveryAttempt(direction, state, state.Generation, transactionJournal{}); err != nil ||
			c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
			return 0, ErrRuntimeRoutingWitness
		}
		status, err := c.applyRecoveryLockedWithOriginAuthority(ctx, lock, recovery, selected, true, false)
		if err != nil {
			return 0, err
		}
		final, _, readErr := c.store.Read()
		if readErr != nil || status.Generation != witness.AttemptFinalGeneration || !stableStateForBackend(final, selected) {
			return 0, ErrRuntimeRoutingConflict
		}
		if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
			return 0, err
		}
		return final.Generation, nil
	}
	if state.DesiredBackend != witness.TargetBackend || state.AppliedBackend != witness.OriginBackend {
		return 0, ErrRuntimeRoutingConflict
	}
	if selected == state.AppliedBackend {
		return c.replaceRuntimeRoutingState(ctx, lock, witness, state, direction, selected)
	}
	if selected != state.DesiredBackend {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := witness.setRecoveryAttempt(direction, state, state.Generation, transactionJournal{}); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	status, err := c.applyRecoveryLockedWithOriginAuthority(ctx, lock, state, selected, true, state.KnownLegacyBackupAndMigrate)
	if err != nil {
		return 0, err
	}
	final, _, readErr := c.store.Read()
	if readErr != nil || status.Generation != witness.AttemptFinalGeneration || !stableStateForBackend(final, selected) {
		return 0, ErrRuntimeRoutingConflict
	}
	if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
		return 0, err
	}
	return final.Generation, nil
}

func (c *Controller) replaceRuntimeRoutingState(ctx context.Context, lock *Lock, witness *runtimeRoutingJournal, state State, direction RuntimeRoutingDirection, selected Backend) (uint64, error) {
	if err := witness.setAttempt(runtimeRoutingAttemptReplace, direction, state, 0, BackendUnknown, BackendUnknown); err != nil ||
		c.store.writeRuntimeRouting(c.codexConfigPath, *witness) != nil {
		return 0, ErrRuntimeRoutingWitness
	}
	final := state
	final.Generation = witness.AttemptFinalGeneration
	final.DesiredBackend = selected
	final.AppliedBackend = selected
	final.DesiredMode = modeForBackend(selected)
	final.AppliedMode = modeForBackend(selected)
	final.KnownLegacyBackupAndMigrate = false
	if selected == BackendNone {
		final.Phase = PhaseNativeActive
	} else {
		final.Phase = PhaseRelayActive
	}
	if err := lock.Replace(final); err != nil {
		return 0, err
	}
	if err := c.verifyStableRuntimeRoute(ctx, final, selected); err != nil {
		return 0, err
	}
	if err := c.markRuntimeRoutingResolved(witness, direction, final); err != nil {
		return 0, err
	}
	return final.Generation, nil
}

func (c *Controller) verifyStableRuntimeRoute(ctx context.Context, state State, selected Backend) error {
	if !stableStateForBackend(state, selected) {
		return ErrRuntimeRoutingConflict
	}
	if check := AppliedRoutingDriftCheck(c.store.ConfigPath()); check != nil && check(state) != nil {
		return ErrRuntimeRoutingConflict
	}
	preflight, err := c.preflightBackendWithLegacyIntent(selected, false)
	if err != nil {
		return err
	}
	if err := c.awaitFinalized(ctx, preflight.Config, state); err != nil {
		return err
	}
	return nil
}

func (c *Controller) markRuntimeRoutingResolved(witness *runtimeRoutingJournal, direction RuntimeRoutingDirection, state State) error {
	if witness == nil || state.Generation != witness.AttemptFinalGeneration ||
		!stableStateForBackend(state, witness.selectedBackend(direction)) {
		return ErrRuntimeRoutingConflict
	}
	witness.Phase = runtimeRoutingResolved
	witness.AttemptDirection = direction
	witness.ResolvedGeneration = state.Generation
	witness.ResolvedAppleActive = state.AppliedBackend == BackendLocalAppleContainer
	return c.store.writeRuntimeRouting(c.codexConfigPath, *witness)
}

// AcknowledgeRuntimeRouting is called only after the lifecycle manager has
// durably saved the corresponding container state. It is the sole operation
// that removes the cross-journal witness.
func (c *Controller) AcknowledgeRuntimeRouting(ctx context.Context, request RuntimeRoutingRequest, resolvedGeneration uint64) error {
	if request.validate() != nil || resolvedGeneration == 0 {
		return ErrRuntimeRoutingWitness
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	witness, found, err := c.store.loadRuntimeRouting(c.codexConfigPath)
	if err != nil || !found || !witness.matchesRequest(request) || witness.Phase != runtimeRoutingResolved ||
		witness.ResolvedGeneration != resolvedGeneration || witness.ResolvedAppleActive != (witness.selectedBackend(request.Direction) == BackendLocalAppleContainer) {
		return ErrRuntimeRoutingConflict
	}
	state, legacy, stateErr := c.store.Read()
	transaction, transactionFound, transactionErr := c.loadJournal()
	if stateErr != nil || legacy || transactionErr != nil || transactionFound ||
		!witness.matchesState(state, transaction, false) || state.Generation != resolvedGeneration {
		return ErrRuntimeRoutingConflict
	}
	if _, maintenanceFound, maintenanceErr := c.store.loadMaintenance(); maintenanceErr != nil || maintenanceFound {
		return ErrRuntimeRoutingConflict
	}
	return c.store.removeRuntimeRouting()
}

func (j *runtimeRoutingJournal) setAttempt(kind runtimeRoutingAttemptKind, direction RuntimeRoutingDirection, state State, sourceJournalGeneration uint64, sourceOrigin, sourceTarget Backend) error {
	if j == nil || (direction != RuntimeRoutingCompleteTarget && direction != RuntimeRoutingRestoreOrigin) || state.Generation == 0 {
		return ErrRuntimeRoutingWitness
	}
	selected := j.selectedBackend(direction)
	j.Phase = runtimeRoutingTransitioning
	j.AttemptKind = kind
	j.AttemptDirection = direction
	j.AttemptStartGeneration = state.Generation
	j.AttemptStartDesiredBackend = state.DesiredBackend
	j.AttemptStartAppliedBackend = state.AppliedBackend
	j.AttemptStartPhase = state.Phase
	j.AttemptOriginBackend = state.AppliedBackend
	j.AttemptTargetBackend = selected
	j.AttemptRequestedGeneration = 0
	j.AttemptRecoveryGeneration = 0
	j.AttemptApplyingGeneration = 0
	j.AttemptFinalGeneration = 0
	j.SourceJournalGeneration = sourceJournalGeneration
	j.SourceJournalOriginBackend = ""
	j.SourceJournalTargetBackend = ""
	j.ResolvedGeneration = 0
	j.ResolvedAppleActive = false
	if sourceJournalGeneration != 0 {
		j.SourceJournalOriginBackend = sourceOrigin
		j.SourceJournalTargetBackend = sourceTarget
	}
	switch kind {
	case runtimeRoutingAttemptSwitch:
		if state.Generation > ^uint64(0)-3 || !stableRoutingState(state) {
			return ErrRuntimeRoutingWitness
		}
		j.AttemptRequestedGeneration = state.Generation + 1
		j.AttemptApplyingGeneration = state.Generation + 2
		j.AttemptFinalGeneration = state.Generation + 3
	case runtimeRoutingAttemptApplyPending:
		if state.Generation > ^uint64(0)-2 || !pendingRoutingState(state) {
			return ErrRuntimeRoutingWitness
		}
		j.AttemptRequestedGeneration = state.Generation
		j.AttemptApplyingGeneration = state.Generation + 1
		j.AttemptFinalGeneration = state.Generation + 2
	case runtimeRoutingAttemptResumeApplying:
		if state.Generation == ^uint64(0) || state.Phase != PhaseApplying {
			return ErrRuntimeRoutingWitness
		}
		j.AttemptApplyingGeneration = state.Generation
		j.AttemptFinalGeneration = state.Generation + 1
	case runtimeRoutingAttemptReplace:
		if state.Generation == ^uint64(0) {
			return ErrRuntimeRoutingWitness
		}
		j.AttemptFinalGeneration = state.Generation + 1
	default:
		return ErrRuntimeRoutingWitness
	}
	return nil
}

func (j *runtimeRoutingJournal) setRecoveryAttempt(direction RuntimeRoutingDirection, state State, recoveryGeneration uint64, source transactionJournal) error {
	if j == nil || recoveryGeneration == 0 || recoveryGeneration > ^uint64(0)-2 {
		return ErrRuntimeRoutingWitness
	}
	sourceGeneration := uint64(0)
	sourceOrigin, sourceTarget := BackendUnknown, BackendUnknown
	if source.Schema != 0 {
		sourceGeneration = source.Generation
		sourceOrigin = source.OriginBackend
		sourceTarget = source.TargetBackend
	}
	if err := j.setAttempt(runtimeRoutingAttemptReplace, direction, state, sourceGeneration, sourceOrigin, sourceTarget); err != nil {
		return err
	}
	j.AttemptKind = runtimeRoutingAttemptRecover
	j.AttemptOriginBackend = j.selectedBackend(oppositeRuntimeDirection(direction))
	j.AttemptTargetBackend = j.selectedBackend(direction)
	j.AttemptRequestedGeneration = 0
	j.AttemptRecoveryGeneration = recoveryGeneration
	j.AttemptApplyingGeneration = recoveryGeneration + 1
	j.AttemptFinalGeneration = recoveryGeneration + 2
	return j.validate(j.BoundConfigPath, j.BoundCodexConfigPath)
}

func (j runtimeRoutingJournal) matchesState(state State, transaction transactionJournal, transactionFound bool) bool {
	if state.ValidateForCodexConfig(j.BoundConfigPath, j.BoundCodexConfigPath) != nil {
		return false
	}
	if j.Phase == runtimeRoutingResolved {
		return !transactionFound && state.Generation == j.ResolvedGeneration &&
			stableStateForBackend(state, j.selectedBackend(j.AttemptDirection))
	}
	if exactRuntimeAttemptStart(j, state) {
		if !transactionFound {
			return true
		}
		return j.sourceTransactionMatches(transaction)
	}
	if transactionFound && !j.attemptTransactionMatches(transaction) && !j.sourceTransactionMatches(transaction) {
		return false
	}
	if j.AttemptRequestedGeneration != 0 && state.Generation == j.AttemptRequestedGeneration &&
		pendingStateForBackends(state, j.AttemptOriginBackend, j.AttemptTargetBackend) {
		return !transactionFound
	}
	if j.AttemptApplyingGeneration != 0 && state.Generation == j.AttemptApplyingGeneration &&
		applyingStateForBackends(state, j.AttemptOriginBackend, j.AttemptTargetBackend) {
		return !transactionFound || j.attemptTransactionMatches(transaction) || j.sourceTransactionMatches(transaction)
	}
	if j.AttemptRecoveryGeneration != 0 && state.Generation == j.AttemptRecoveryGeneration &&
		recoveryStateForBackends(state, j.AttemptOriginBackend, j.AttemptTargetBackend) {
		return !transactionFound || j.sourceTransactionMatches(transaction)
	}
	if j.AttemptApplyingGeneration != 0 && j.AttemptApplyingGeneration != ^uint64(0) &&
		state.Generation == j.AttemptApplyingGeneration+1 &&
		recoveryStateForBackends(state, j.AttemptOriginBackend, j.AttemptTargetBackend) {
		return !transactionFound || j.attemptTransactionMatches(transaction) || j.sourceTransactionMatches(transaction)
	}
	if state.Generation == j.AttemptFinalGeneration && stableStateForBackend(state, j.AttemptTargetBackend) {
		return !transactionFound || j.attemptTransactionMatches(transaction)
	}
	if j.AttemptFinalGeneration != ^uint64(0) && state.Generation == j.AttemptFinalGeneration+1 &&
		state.Phase == PhaseRecoveryRequired && state.DesiredBackend == j.AttemptTargetBackend &&
		state.AppliedBackend == j.AttemptTargetBackend {
		return !transactionFound
	}
	return false
}

func (j runtimeRoutingJournal) attemptTransactionMatches(transaction transactionJournal) bool {
	return transaction.Kind == transactionKindRoutingSwitch && transaction.Generation == j.AttemptApplyingGeneration &&
		transaction.OriginBackend == j.AttemptOriginBackend && transaction.TargetBackend == j.AttemptTargetBackend
}

func (j runtimeRoutingJournal) sourceTransactionMatches(transaction transactionJournal) bool {
	return j.SourceJournalGeneration != 0 && transaction.Kind == transactionKindRoutingSwitch &&
		transaction.Generation == j.SourceJournalGeneration && transaction.OriginBackend == j.SourceJournalOriginBackend &&
		transaction.TargetBackend == j.SourceJournalTargetBackend
}

func exactRuntimeAttemptStart(j runtimeRoutingJournal, state State) bool {
	return state.Generation == j.AttemptStartGeneration && state.DesiredBackend == j.AttemptStartDesiredBackend &&
		state.AppliedBackend == j.AttemptStartAppliedBackend && state.Phase == j.AttemptStartPhase
}

func stableRoutingState(state State) bool {
	return state.DesiredBackend == state.AppliedBackend && stableStateForBackend(state, state.AppliedBackend)
}

func stableStateForBackend(state State, backend Backend) bool {
	phase := PhaseRelayActive
	if backend == BackendNone {
		phase = PhaseNativeActive
	}
	return state.Generation != 0 && state.Phase == phase && state.DesiredBackend == backend &&
		state.AppliedBackend == backend && state.DesiredMode == modeForBackend(backend) && state.AppliedMode == modeForBackend(backend)
}

func pendingRoutingState(state State) bool {
	return state.Phase == PhaseNativePendingRestart || state.Phase == PhaseRelayPendingRestart || state.Phase == PhaseBackendPendingRestart
}

func pendingStateForBackends(state State, origin, target Backend) bool {
	return pendingRoutingState(state) && state.DesiredBackend == target && state.AppliedBackend == origin
}

func applyingStateForBackends(state State, origin, target Backend) bool {
	return state.Phase == PhaseApplying && state.DesiredBackend == target && state.AppliedBackend == origin
}

func recoveryStateForBackends(state State, origin, target Backend) bool {
	return state.Phase == PhaseRecoveryRequired && state.DesiredBackend == target && state.AppliedBackend == origin
}

func oppositeRuntimeDirection(direction RuntimeRoutingDirection) RuntimeRoutingDirection {
	if direction == RuntimeRoutingRestoreOrigin {
		return RuntimeRoutingCompleteTarget
	}
	return RuntimeRoutingRestoreOrigin
}
