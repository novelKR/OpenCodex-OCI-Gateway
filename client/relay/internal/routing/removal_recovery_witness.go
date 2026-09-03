package routing

import (
	"sync"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
)

type removalRecoveryWitnessKind uint8

const (
	removalRecoveryWitnessInvalid removalRecoveryWitnessKind = iota
	removalRecoveryWitnessParked
	removalRecoveryWitnessStableWithJournal
	removalRecoveryWitnessStable
)

type removalRecoveryFileWitness struct {
	relayConfig string
	codexConfig string
	interactive string
}

type removalRecoveryWitnessSnapshot struct {
	state        State
	journal      transactionJournal
	journalFound bool
	files        removalRecoveryFileWitness
	kind         removalRecoveryWitnessKind
	target       Backend
}

// RemovalRecoveryWitness is an opaque, non-secret capability that binds one
// removal-specific recovery attempt to the exact validated routing state,
// transaction journal, and managed configuration observation captured while
// the routing writer lock is held. It contains no caller-supplied paths or
// mutation authority; a Controller still requires its independent external
// removal gate before Recover can proceed.
type RemovalRecoveryWitness struct {
	mu              sync.RWMutex
	configPath      string
	codexConfigPath string
	codexOwner      codexconfig.Owner
	snapshot        removalRecoveryWitnessSnapshot
}

// Generation returns the durable routing generation bound into the opaque
// witness. A zero value is never a valid removal recovery witness.
func (witness *RemovalRecoveryWitness) Generation() uint64 {
	if witness == nil {
		return 0
	}
	witness.mu.RLock()
	defer witness.mu.RUnlock()
	return witness.snapshot.state.Generation
}

// AlreadyRecovered reports whether the witness is bound to a stable,
// journal-free routing commit. The caller must still re-prove live health and
// exact managed ownership before releasing its separate removal journal.
func (witness *RemovalRecoveryWitness) AlreadyRecovered() bool {
	if witness == nil {
		return false
	}
	witness.mu.RLock()
	defer witness.mu.RUnlock()
	return witness.snapshot.kind == removalRecoveryWitnessStable
}

// CaptureRemovalRecoveryWitness validates and captures the exact
// removal-specific routing recovery authority while the supplied writer lock
// is held. The returned value is intentionally opaque to callers.
func (controller *Controller) CaptureRemovalRecoveryWitness(lock *Lock) (*RemovalRecoveryWitness, error) {
	snapshot, err := controller.captureRemovalRecoveryWitnessLocked(lock)
	if err != nil {
		return nil, ErrRecoveryRequired
	}
	return &RemovalRecoveryWitness{
		configPath:      controller.store.ConfigPath(),
		codexConfigPath: controller.codexConfigPath,
		codexOwner:      controller.codexOwner,
		snapshot:        snapshot,
	}, nil
}

// RebindStable replaces a previously captured witness only with the exact
// stable, journal-free state observed under the same routing writer lock. The
// Controller invokes this before returning a successful token-scoped Recover;
// it is also useful to narrow tests around the recovery callback boundary.
func (witness *RemovalRecoveryWitness) RebindStable(lock *Lock) error {
	controller, err := witness.controllerForLock(lock)
	if err != nil {
		return ErrRecoveryRequired
	}
	return witness.rebindStableLocked(controller, lock)
}

// ValidateStable proves that the witness still names the exact stable,
// journal-free routing and configuration snapshot while the caller holds the
// writer lock. It is the final durable proof before a separate removal token
// may be released.
func (witness *RemovalRecoveryWitness) ValidateStable(lock *Lock) error {
	controller, err := witness.controllerForLock(lock)
	if err != nil {
		return ErrRecoveryRequired
	}
	if err := witness.matchesLocked(controller, lock); err != nil {
		return ErrRecoveryRequired
	}
	if !witness.AlreadyRecovered() {
		return ErrRecoveryRequired
	}
	return nil
}

func (witness *RemovalRecoveryWitness) controllerForLock(lock *Lock) (*Controller, error) {
	if witness == nil || lock == nil || lock.closed || lock.store == nil {
		return nil, ErrRecoveryRequired
	}
	witness.mu.RLock()
	configPath := witness.configPath
	codexConfigPath := witness.codexConfigPath
	codexOwner := witness.codexOwner
	witness.mu.RUnlock()
	if lock.store.ConfigPath() != configPath {
		return nil, ErrRecoveryRequired
	}
	return &Controller{
		store:           lock.store,
		codexConfigPath: codexConfigPath,
		codexOwner:      codexOwner,
		journalPath:     lock.store.TransactionPath(),
	}, nil
}

func (witness *RemovalRecoveryWitness) matchesLocked(controller *Controller, lock *Lock) error {
	if witness == nil || controller == nil || lock == nil || lock.closed || lock.store == nil ||
		controller.store == nil || controller.store.ConfigPath() != lock.store.ConfigPath() {
		return ErrRecoveryRequired
	}
	witness.mu.RLock()
	configPath := witness.configPath
	codexConfigPath := witness.codexConfigPath
	codexOwner := witness.codexOwner
	expected := witness.snapshot
	witness.mu.RUnlock()
	if controller.store.ConfigPath() != configPath ||
		controller.codexConfigPath != codexConfigPath ||
		controller.codexOwner != codexOwner {
		return ErrRecoveryRequired
	}
	current, err := controller.captureRemovalRecoveryWitnessLocked(lock)
	if err != nil || current != expected {
		return ErrRecoveryRequired
	}
	return nil
}

func (witness *RemovalRecoveryWitness) matchesRecoveryInputsLocked(
	controller *Controller,
	lock *Lock,
	state State,
	legacy bool,
	stateErr error,
	journal transactionJournal,
	journalFound bool,
	journalErr error,
) error {
	if witness == nil || controller == nil || lock == nil || lock.closed || lock.store == nil ||
		controller.store == nil || controller.store.ConfigPath() != lock.store.ConfigPath() ||
		stateErr != nil || legacy || journalErr != nil {
		return ErrRecoveryRequired
	}
	witness.mu.RLock()
	configPath := witness.configPath
	codexConfigPath := witness.codexConfigPath
	codexOwner := witness.codexOwner
	expected := witness.snapshot
	witness.mu.RUnlock()
	if controller.store.ConfigPath() != configPath ||
		controller.codexConfigPath != codexConfigPath ||
		controller.codexOwner != codexOwner ||
		state != expected.state ||
		journalFound != expected.journalFound ||
		journal != expected.journal {
		return ErrRecoveryRequired
	}
	files, err := controller.currentRemovalRecoveryFiles()
	if err != nil || files != expected.files {
		return ErrRecoveryRequired
	}
	return nil
}

func (witness *RemovalRecoveryWitness) rebindStableLocked(controller *Controller, lock *Lock) error {
	if witness == nil {
		return ErrRecoveryRequired
	}
	current, err := controller.captureRemovalRecoveryWitnessLocked(lock)
	if err != nil || current.kind != removalRecoveryWitnessStable || current.journalFound {
		return ErrRecoveryRequired
	}
	witness.mu.Lock()
	defer witness.mu.Unlock()
	if witness.configPath != controller.store.ConfigPath() ||
		witness.codexConfigPath != controller.codexConfigPath ||
		witness.codexOwner != controller.codexOwner {
		return ErrRecoveryRequired
	}
	witness.snapshot = current
	return nil
}

func (controller *Controller) captureRemovalRecoveryWitnessLocked(lock *Lock) (removalRecoveryWitnessSnapshot, error) {
	if controller == nil || controller.store == nil || lock == nil || lock.closed || lock.store == nil ||
		controller.store.ConfigPath() != lock.store.ConfigPath() {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}
	state, legacy, err := controller.boundState(lock)
	if err != nil || legacy {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}
	journal, journalFound, err := controller.loadJournal()
	if err != nil {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}
	files, err := controller.currentRemovalRecoveryFiles()
	if err != nil {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}
	if journalFound && !removalRecoveryJournalMatchesFiles(journal, files) {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}

	kind, target, err := controller.classifyRemovalRecoveryWitness(state, journal, journalFound)
	if err != nil {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}

	// Re-read every durable component after classification. Controller writers
	// are serialized by lock; this second observation also rejects an
	// owner-local atomic replacement that races the individual file reads.
	confirmedState, confirmedLegacy, stateErr := controller.boundState(lock)
	confirmedJournal, confirmedFound, journalErr := controller.loadJournal()
	confirmedFiles, filesErr := controller.currentRemovalRecoveryFiles()
	if stateErr != nil || confirmedLegacy || journalErr != nil || filesErr != nil ||
		confirmedState != state || confirmedFound != journalFound ||
		confirmedJournal != journal || confirmedFiles != files {
		return removalRecoveryWitnessSnapshot{}, ErrRecoveryRequired
	}
	return removalRecoveryWitnessSnapshot{
		state:        state,
		journal:      journal,
		journalFound: journalFound,
		files:        files,
		kind:         kind,
		target:       target,
	}, nil
}

func (controller *Controller) classifyRemovalRecoveryWitness(
	state State,
	journal transactionJournal,
	journalFound bool,
) (removalRecoveryWitnessKind, Backend, error) {
	if journalFound {
		switch {
		case removalRecoveryStableJournalRelation(state, journal):
			return removalRecoveryWitnessStableWithJournal, journal.TargetBackend, nil
		case removalRecoveryParkedJournalRelation(state, journal):
			return removalRecoveryWitnessParked, journal.TargetBackend, nil
		default:
			return removalRecoveryWitnessInvalid, BackendUnknown, ErrRecoveryRequired
		}
	}
	if stableNonLocalRecoveryCommit(state) {
		return removalRecoveryWitnessStable, state.AppliedBackend, nil
	}
	if state.Phase != PhaseRecoveryRequired || !nonLocalRecoveryTarget(state) {
		return removalRecoveryWitnessInvalid, BackendUnknown, ErrRecoveryRequired
	}
	observed, err := controller.observedRoutingState()
	if err != nil || !stableNonLocalRecoveryCommit(observed) {
		return removalRecoveryWitnessInvalid, BackendUnknown, ErrRecoveryRequired
	}
	return removalRecoveryWitnessParked, observed.DesiredBackend, nil
}

func (controller *Controller) currentRemovalRecoveryFiles() (removalRecoveryFileWitness, error) {
	relayConfig, err := fingerprintOptional(controller.store.ConfigPath())
	if err != nil {
		return removalRecoveryFileWitness{}, err
	}
	codexConfig, err := fingerprintOptional(controller.codexConfigPath)
	if err != nil {
		return removalRecoveryFileWitness{}, err
	}
	interactive, err := fingerprintOptional(
		codexconfig.InteractiveProfilePathForOwner(controller.codexConfigPath, controller.codexOwner),
	)
	if err != nil {
		return removalRecoveryFileWitness{}, err
	}
	return removalRecoveryFileWitness{
		relayConfig: relayConfig,
		codexConfig: codexConfig,
		interactive: interactive,
	}, nil
}

func removalRecoveryJournalMatchesFiles(
	journal transactionJournal,
	files removalRecoveryFileWitness,
) bool {
	return journal.RelayConfigFingerprint == files.relayConfig &&
		journal.CodexConfigFingerprint == files.codexConfig &&
		journal.InteractiveFingerprint == files.interactive
}

func removalRecoveryStableJournalRelation(state State, journal transactionJournal) bool {
	return stableNonLocalRecoveryCommit(state) &&
		journal.Stage == transactionConfigMutated &&
		!journal.OriginAuthoritative &&
		state.Generation == journal.Generation+1 &&
		state.DesiredMode == journal.Target &&
		state.AppliedMode == journal.Target &&
		state.DesiredBackend == journal.TargetBackend &&
		state.AppliedBackend == journal.TargetBackend &&
		journal.TargetBackend != BackendUnknown &&
		journal.TargetBackend != BackendLocalOpenCodex &&
		journal.TargetBackend != BackendLocalAppleContainer &&
		journal.OriginBackend != BackendUnknown &&
		journal.OriginBackend != BackendLocalOpenCodex &&
		journal.OriginBackend != BackendLocalAppleContainer &&
		journal.TargetBackend != journal.OriginBackend
}

func removalRecoveryParkedJournalRelation(state State, journal transactionJournal) bool {
	return state.Phase == PhaseRecoveryRequired &&
		nonLocalRecoveryTarget(state) &&
		!journal.OriginAuthoritative &&
		state.Generation == journal.Generation+1 &&
		state.DesiredMode == journal.Target &&
		state.DesiredBackend == journal.TargetBackend &&
		state.AppliedMode == journal.Origin &&
		state.AppliedBackend == journal.OriginBackend &&
		journal.TargetBackend != BackendUnknown &&
		journal.TargetBackend != BackendLocalOpenCodex &&
		journal.TargetBackend != BackendLocalAppleContainer &&
		journal.OriginBackend != BackendUnknown &&
		journal.OriginBackend != BackendLocalOpenCodex &&
		journal.OriginBackend != BackendLocalAppleContainer &&
		journal.TargetBackend != journal.OriginBackend
}
