package containerruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

const defaultPostStartCleanupTimeout = 30 * time.Second

var (
	errPostStartCleanupIncomplete   = errors.New("started container cleanup is incomplete")
	errForwardRecoveryIndeterminate = errors.New("forward recovery runtime state is indeterminate")
)

// ManagerOptions deliberately contains narrow capabilities rather than raw
// executable paths, URLs, header names, or credential services. Production
// construction fixes those values in relayctl; tests can replace behavior
// without weakening the public command surface.
type ManagerOptions struct {
	Root         string
	Account      string
	RelayVersion string
	PublicKeyPEM []byte
	Checker      ReleaseChecker
	Runtime      ImageRuntime
	Prober       HTTPProber
	Cloner       StateCloner
	SecretServer SecretServer
	Keychain     Keychain
	Routing      RoutingCoordinator
	Enroller     ProfileEnroller
	Locker       Locker
}

type Manager struct {
	store                   *stateStore
	account                 string
	relayVersion            string
	publicKeyPEM            []byte
	checker                 ReleaseChecker
	runtime                 ImageRuntime
	prober                  HTTPProber
	cloner                  StateCloner
	secretServer            SecretServer
	keychain                Keychain
	routing                 RoutingCoordinator
	enroller                ProfileEnroller
	locker                  Locker
	removeGeneration        func(uint64) error
	postStartCleanupTimeout time.Duration
}

func DefaultRoot(home string) (string, error) {
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return "", ErrUnsafeState
	}
	return filepath.Join(home, "Library", "Application Support", "OpenCodex Relay", "ContainerRuntime"), nil
}

func NewManager(options ManagerOptions) (*Manager, error) {
	store, err := newStateStore(options.Root)
	if err != nil {
		return nil, err
	}
	if options.Account == "" || options.RelayVersion == "" || options.Checker == nil || options.Runtime == nil || options.Prober == nil ||
		options.Cloner == nil || options.SecretServer == nil || options.Keychain == nil ||
		options.Routing == nil || options.Enroller == nil || options.Locker == nil {
		return nil, ErrInvalidRequest
	}
	if _, _, err := runtimemanifest.ParsePublicKey(options.PublicKeyPEM); err != nil {
		return nil, ErrUnsafeState
	}
	return &Manager{
		store: store, account: options.Account, relayVersion: options.RelayVersion,
		publicKeyPEM: append([]byte(nil), options.PublicKeyPEM...), checker: options.Checker,
		runtime: options.Runtime, prober: options.Prober, cloner: options.Cloner,
		secretServer: options.SecretServer, keychain: options.Keychain,
		routing: options.Routing, enroller: options.Enroller, locker: options.Locker,
		removeGeneration: store.removeGeneration, postStartCleanupTimeout: defaultPostStartCleanupTimeout,
	}, nil
}

func (m *Manager) Inspect(ctx context.Context) (Inspection, error) {
	state, err := m.readState()
	if err != nil {
		return Inspection{}, err
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) Check(ctx context.Context) (CheckReceipt, error) {
	// The first opted-in metadata check establishes the stable installation ID
	// and state digest used by the subsequent stage CAS. Plain Inspect remains
	// read-only, but an ephemeral first-use digest must never be offered as a
	// stage witness.
	unlock, err := m.locker.Lock(ctx)
	if err != nil {
		return CheckReceipt{}, err
	}
	state, err := m.loadForMutation()
	unlockErr := unlock()
	if err != nil {
		return CheckReceipt{}, err
	}
	if unlockErr != nil {
		return CheckReceipt{}, unlockErr
	}
	inspection, err := m.inspectState(ctx, state)
	if err != nil {
		return CheckReceipt{}, err
	}
	if inspection.RecoveryRequired {
		return CheckReceipt{}, ErrRecoveryRequired
	}
	request := m.checkRequest(state, inspection.Capability)
	result, err := m.checker.Check(ctx, request)
	if err != nil {
		return CheckReceipt{}, err
	}
	receipt := CheckReceipt{Inspection: inspection, Status: result.Status, Reason: result.Reason}
	if result.Candidate != nil {
		record, recordErr := recordFromCandidate(*result.Candidate)
		if recordErr != nil {
			return CheckReceipt{}, recordErr
		}
		summary := record.ArtifactSummary
		receipt.Candidate = &summary
	}
	receipt.Compatible = result.Status == runtimemanifest.CheckStatusCurrent || result.Status == runtimemanifest.CheckStatusUpdateAvailable
	// A host with a working Apple Container installation still has no usable
	// runtime until a separately signed stable manifest exists. Project that
	// distribution-level absence into the check receipt without mutating the
	// durable lifecycle state. Locally retained signed staged/active artifacts
	// remain visible for stop/recovery and are not reclassified.
	if result.Status == runtimemanifest.CheckStatusUnavailable && state.Staged == nil && state.Active == nil {
		receipt.State = StateUnavailable
	}
	return receipt, nil
}

func (m *Manager) Stage(ctx context.Context, request StageRequest) (MutationReceipt, error) {
	unlock, err := m.locker.Lock(ctx)
	if err != nil {
		return MutationReceipt{}, err
	}
	defer unlock()
	state, err := m.loadForMutation()
	if err != nil {
		return MutationReceipt{}, err
	}
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.RecoveryRequired {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if err := m.requireCAS(state, routing, request.ExpectedStateDigest, request.ExpectedRoutingGeneration); err != nil {
		return MutationReceipt{}, err
	}
	if found, err := m.transactionPresent(); err != nil || found {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if runtimeRouteMismatch(state, routing) {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	capability, err := m.runtime.Capability(ctx, MinimumAppleContainerVersion, state.InstallationID)
	if err != nil || !capability.Available {
		return MutationReceipt{}, ErrUnavailable
	}
	candidate, err := m.checker.ResolveExpected(ctx, request.ExpectedManifestSHA256, m.checkRequest(state, capability))
	if err != nil {
		return MutationReceipt{}, err
	}
	record, err := recordFromCandidate(candidate)
	if err != nil || candidate.Manifest.ReleaseSequence < state.HighestSeenSequence {
		return MutationReceipt{}, ErrUnsafeState
	}
	account, err := m.enroller.Ensure(ctx)
	if err != nil || account != m.account {
		return MutationReceipt{}, ErrUnsafeState
	}
	before := state
	state.Status = StateStaging
	state.RoutingGeneration = routing.Generation
	if err := m.store.save(state); err != nil {
		return MutationReceipt{}, err
	}
	exactReference := candidate.Manifest.Image.Repository + "@" + candidate.Manifest.Image.IndexDigest
	if err := m.runtime.Pull(ctx, exactReference); err != nil {
		return MutationReceipt{}, m.restoreStageState(before, err)
	}
	spec := startSpec(state.InstallationID, "", record, 0, "", "")
	if err := m.runtime.VerifyImage(ctx, spec, candidate.Manifest); err != nil {
		return MutationReceipt{}, m.restoreStageState(before, err)
	}
	if err := m.store.saveManifest(candidate); err != nil {
		return MutationReceipt{}, m.restoreStageState(before, err)
	}
	state.Staged = &record
	state.HighestSeenSequence = max(state.HighestSeenSequence, record.ReleaseSequence)
	state.Status = statusForStoppedState(state)
	if err := m.store.save(state); err != nil {
		return MutationReceipt{}, m.restoreStageState(before, err)
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) restoreStageState(before durableState, cause error) error {
	if err := m.store.save(before); err != nil {
		return ErrRecoveryRequired
	}
	return cause
}

func (m *Manager) Activate(ctx context.Context, request ActivateRequest) (MutationReceipt, error) {
	unlock, err := m.locker.Lock(ctx)
	if err != nil {
		return MutationReceipt{}, err
	}
	defer unlock()
	state, err := m.loadForMutation()
	if err != nil {
		return MutationReceipt{}, err
	}
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.RecoveryRequired {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if err := m.requireCAS(state, routing, request.ExpectedStateDigest, request.ExpectedRoutingGeneration); err != nil {
		return MutationReceipt{}, err
	}
	restartingStopped := state.Staged == nil && state.Status == StateStopped && state.Active != nil &&
		state.ContainerID == "" && state.ActiveOperationID == "" && !routing.AppleActive
	if !request.ConfirmDesktopExited || state.Staged == nil && !restartingStopped || state.Status == StateRecoveryRequired {
		return MutationReceipt{}, ErrInvalidRequest
	}
	if found, err := m.transactionPresent(); err != nil || found {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if runtimeRouteMismatch(state, routing) {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	capability, err := m.runtime.Capability(ctx, MinimumAppleContainerVersion, state.InstallationID)
	if err != nil || !capability.Available {
		return MutationReceipt{}, ErrUnavailable
	}
	var manifest runtimemanifest.Manifest
	if restartingStopped {
		manifest, err = m.verifyRollbackRecord(*state.Active, capability)
	} else {
		manifest, err = m.verifyRecord(*state.Staged, state.HighestSeenSequence, capability)
	}
	if err != nil {
		return MutationReceipt{}, err
	}
	secrets, err := m.keychain.Ensure(ctx, m.account)
	if err != nil || !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) || bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		zeroSecrets(&secrets)
		return MutationReceipt{}, ErrCredential
	}
	defer zeroSecrets(&secrets)
	if restartingStopped {
		return m.restartStoppedLocked(ctx, state, routing, manifest, secrets)
	}
	return m.activateLocked(ctx, state, routing, manifest, secrets)
}

func (m *Manager) Stop(ctx context.Context, request StopRequest) (MutationReceipt, error) {
	unlock, err := m.locker.Lock(ctx)
	if err != nil {
		return MutationReceipt{}, err
	}
	defer unlock()
	if !request.ConfirmDesktopExited {
		return MutationReceipt{}, ErrInvalidRequest
	}
	state, err := m.loadForMutation()
	if err != nil {
		return MutationReceipt{}, err
	}
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.RecoveryRequired {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if err := m.requireCAS(state, routing, request.ExpectedStateDigest, request.ExpectedRoutingGeneration); err != nil {
		return MutationReceipt{}, err
	}
	if state.Status == StateRecoveryRequired {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if found, err := m.transactionPresent(); err != nil || found {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if state.Active == nil || state.ContainerID == "" || state.ActiveOperationID == "" {
		if routing.AppleActive || state.Active == nil && state.ContainerID != "" {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		state.Status = StateStopped
		state.RoutingGeneration = routing.Generation
		if err := m.store.save(state); err != nil {
			return MutationReceipt{}, err
		}
		return m.inspectState(ctx, state)
	}
	if !routing.AppleActive || routing.RuntimeRoutingPending || routing.MaintenancePending {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	operationID, err := randomHex(32)
	if err != nil {
		return MutationReceipt{}, err
	}
	statePath, err := m.store.generationPath(state.ActiveGeneration)
	if err != nil {
		return MutationReceipt{}, ErrUnsafeState
	}
	activeSpec := startSpec(state.InstallationID, state.ActiveOperationID, *state.Active, state.ActiveGeneration, statePath, "")
	journal := stopTransactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: stopPhasePrepared, ExpectedStateDigest: mustStateDigest(m.store, state),
		ExpectedRoutingGeneration: routing.Generation, Artifact: *state.Active,
		StateGeneration: state.ActiveGeneration, ContainerID: state.ContainerID,
		ActiveOperationID: state.ActiveOperationID,
	}
	if journal.ExpectedStateDigest == "" || m.store.saveStopJournal(journal) != nil {
		return MutationReceipt{}, ErrUnsafeState
	}
	state.Status = StateUpdating
	if err := m.store.save(state); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	routingRequest := stopRoutingRequest(journal, RoutingCompleteTarget)
	newGeneration, err := m.routing.StopApple(ctx, routingRequest, request.ConfirmDesktopExited)
	if err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	journal.FinalRoutingGeneration = newGeneration
	journal.Phase = stopPhaseRouteStopped
	if err := m.store.saveStopJournal(journal); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.stopAndDelete(ctx, state.ContainerID, activeSpec); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	journal.Phase = stopPhaseRuntimeStopped
	if err := m.store.saveStopJournal(journal); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	state.ContainerID = ""
	state.ActiveOperationID = ""
	state.Status = StateStopped
	state.RoutingGeneration = newGeneration
	if err := m.store.save(state); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.finishOrdinaryRoutingCommit(ctx, routingRequest, newGeneration); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.store.removeStopJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) Recover(ctx context.Context, request RecoverRequest) (MutationReceipt, error) {
	unlock, err := m.locker.Lock(ctx)
	if err != nil {
		return MutationReceipt{}, err
	}
	defer unlock()
	if !request.ConfirmDesktopExited {
		return MutationReceipt{}, ErrInvalidRequest
	}
	state, err := m.loadForMutation()
	if err != nil {
		return MutationReceipt{}, err
	}
	digest, err := m.store.digest(state)
	if err != nil || digest != request.ExpectedStateDigest {
		return MutationReceipt{}, ErrStateChanged
	}
	stopJournal, stopFound, stopErr := m.store.loadStopJournal()
	journal, found, err := m.store.loadJournal()
	if stopErr != nil || err != nil || stopFound && found {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if stopFound {
		if m.validateStopRecoveryBinding(state, stopJournal) != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		return m.recoverStop(ctx, state, stopJournal, request.ConfirmDesktopExited)
	}
	if !found || m.validateRecoveryBinding(state, journal) != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	secrets, err := m.keychain.Load(ctx, m.account)
	if err != nil || !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) || bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		zeroSecrets(&secrets)
		return MutationReceipt{}, ErrCredential
	}
	defer zeroSecrets(&secrets)
	capability, err := m.runtime.Capability(ctx, MinimumAppleContainerVersion, state.InstallationID)
	if err != nil || !capability.Available {
		return MutationReceipt{}, ErrUnavailable
	}
	// An active update records its local journal before asking the resident
	// Relay to durably park admissions. If the prepare reply is lost, the
	// routing side still owns the exact witness while the runtime journal has
	// not learned it yet. Prepare is retry-idempotent for the same intent, so
	// reacquire and persist that witness before either forward or backward
	// recovery. Guessing without it could strand the route in applying state.
	if journalRequiresMaintenanceWitness(journal) {
		witness, prepareErr := m.routing.Prepare(ctx, maintenanceRequest(journal))
		if prepareErr != nil {
			return m.parkRecovery(state, journal, prepareErr)
		}
		journal.Maintenance = &witness
		if err := m.store.saveJournal(journal); err != nil {
			return m.parkRecovery(state, journal, err)
		}
	}
	if !journal.CleanupNewGeneration {
		receipt, ok, forwardErr := m.recoverForward(ctx, state, &journal, capability, secrets, request.ConfirmDesktopExited)
		if forwardErr != nil {
			return m.parkForwardRecovery(journal, forwardErr)
		}
		if ok {
			return receipt, nil
		}
	}
	if receipt, ok := m.recoverBackward(ctx, state, &journal, capability, secrets, request.ConfirmDesktopExited); ok {
		return receipt, nil
	}
	// Recovery helpers can advance an in-memory container witness before its
	// first journal write fails, or durably complete/remove the transaction
	// before a final inspection fails. Do not reconstruct either case from the
	// stale values loaded at Recover entry.
	currentState, stateFound, stateErr := m.store.load()
	_, journalFound, journalErr := m.store.loadJournal()
	if stateErr != nil || !stateFound || journalErr != nil || !journalFound {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return m.parkRecovery(currentState, journal, ErrRecoveryRequired)
}

func (m *Manager) stopAndDelete(ctx context.Context, containerID string, spec StartSpec) error {
	stopErr := m.runtime.Stop(ctx, containerID, spec)
	deleteErr := m.runtime.Delete(ctx, containerID, spec)
	absentErr := m.runtime.VerifyAbsent(ctx, spec.InstallationID)
	if absentErr == nil {
		return nil
	}
	if errors.Is(stopErr, ErrForeignResource) || errors.Is(deleteErr, ErrForeignResource) || errors.Is(absentErr, ErrForeignResource) {
		return ErrForeignResource
	}
	if errors.Is(stopErr, ErrUnsafeState) || errors.Is(deleteErr, ErrUnsafeState) {
		return ErrUnsafeState
	}
	return ErrUnavailable
}

func (m *Manager) parkStopRecovery(state durableState, journal stopTransactionJournal, _ error) (MutationReceipt, error) {
	state.Status = StateRecoveryRequired
	if journal.FinalRoutingGeneration != 0 {
		state.RoutingGeneration = journal.FinalRoutingGeneration
	}
	journal.Phase = stopPhaseRecoveryRequired
	_ = m.store.saveStopJournal(journal)
	_ = m.store.save(state)
	return MutationReceipt{}, ErrRecoveryRequired
}

func (m *Manager) validateStopRecoveryBinding(state durableState, journal stopTransactionJournal) error {
	if state.InstallationID != journal.InstallationID || state.Active == nil || *state.Active != journal.Artifact ||
		state.ActiveGeneration != journal.StateGeneration {
		return ErrUnsafeState
	}
	if state.ContainerID == "" && state.ActiveOperationID == "" &&
		(state.Status == StateStopped || state.Status == StateRecoveryRequired) {
		if journal.FinalRoutingGeneration != 0 && state.RoutingGeneration != journal.FinalRoutingGeneration {
			return ErrUnsafeState
		}
		return nil
	}
	normalized := state
	normalized.Status = StateHealthy
	normalized.ContainerID = journal.ContainerID
	normalized.ActiveOperationID = journal.ActiveOperationID
	normalized.RoutingGeneration = journal.ExpectedRoutingGeneration
	digest, err := m.store.digest(normalized)
	if err != nil || digest != journal.ExpectedStateDigest {
		return ErrUnsafeState
	}
	return nil
}

func (m *Manager) recoverStop(ctx context.Context, state durableState, journal stopTransactionJournal, desktopExited bool) (MutationReceipt, error) {
	routingRequest := stopRoutingRequest(journal, RoutingCompleteTarget)
	// The lifecycle state is durable before the routing witness is acknowledged.
	// If the process then loses the ACK response or crashes before removing this
	// stop journal, the exact stopped state plus this journal proves the commit.
	// Accept only the same stable routing generation/backend; do not issue a
	// second route mutation or infer completion from a newer generation.
	if stopStateMatchesCommittedJournal(state, journal) {
		if err := m.runtime.VerifyAbsent(ctx, journal.InstallationID); err != nil {
			return m.parkStopRecovery(state, journal, err)
		}
		if err := m.finishOrdinaryRoutingCommit(ctx, routingRequest, state.RoutingGeneration); err != nil {
			return m.parkStopRecovery(state, journal, err)
		}
		state.Status = StateStopped
		if err := m.store.save(state); err != nil {
			return m.parkStopRecovery(state, journal, err)
		}
		if err := m.store.removeStopJournal(); err != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		return m.inspectState(ctx, state)
	}
	finalRouting, _, err := m.reconcileOrdinaryRouting(ctx, routingRequest, desktopExited)
	if err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	journal.FinalRoutingGeneration = finalRouting
	journal.Phase = stopPhaseRouteStopped
	if err := m.store.saveStopJournal(journal); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.runtime.VerifyAbsent(ctx, journal.InstallationID); err != nil {
		statePath, pathErr := m.store.generationPath(journal.StateGeneration)
		if pathErr != nil {
			return m.parkStopRecovery(state, journal, pathErr)
		}
		spec := startSpec(journal.InstallationID, journal.ActiveOperationID, journal.Artifact, journal.StateGeneration, statePath, "")
		if err := m.stopAndDelete(ctx, journal.ContainerID, spec); err != nil {
			return m.parkStopRecovery(state, journal, err)
		}
	}
	journal.Phase = stopPhaseRuntimeStopped
	if err := m.store.saveStopJournal(journal); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	state.Status = StateStopped
	state.ContainerID = ""
	state.ActiveOperationID = ""
	state.RoutingGeneration = finalRouting
	if err := m.store.save(state); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.finishOrdinaryRoutingCommit(ctx, routingRequest, finalRouting); err != nil {
		return m.parkStopRecovery(state, journal, err)
	}
	if err := m.store.removeStopJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) activateLocked(ctx context.Context, state durableState, routing RoutingSnapshot, manifest runtimemanifest.Manifest, secrets Secrets) (MutationReceipt, error) {
	oldState := state
	newRecord := *state.Staged
	operationID, err := randomHex(32)
	if err != nil {
		return MutationReceipt{}, err
	}
	newGeneration := state.NextGeneration
	if newGeneration == 0 {
		return MutationReceipt{}, ErrUnsafeState
	}
	newPath, err := m.store.generationPath(newGeneration)
	if err != nil {
		return MutationReceipt{}, err
	}
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: phasePrepared, ExpectedStateDigest: mustStateDigest(m.store, state),
		ExpectedRoutingGeneration: routing.Generation, OldArtifact: cloneRecord(state.Active),
		NewArtifact: newRecord, OldGeneration: state.ActiveGeneration, NewGeneration: newGeneration,
		OldContainerID: state.ContainerID, OldOperationID: state.ActiveOperationID,
	}
	if journal.ExpectedStateDigest == "" {
		return MutationReceipt{}, ErrUnsafeState
	}
	state.Status = StateUpdating
	if err := m.store.saveJournal(journal); err != nil {
		return MutationReceipt{}, err
	}
	if err := m.store.save(state); err != nil {
		return MutationReceipt{}, err
	}

	isUpdate := routing.AppleActive && state.Active != nil
	if isUpdate {
		witness, prepareErr := m.routing.Prepare(ctx, maintenanceRequest(journal))
		if prepareErr != nil {
			return m.parkRecovery(state, journal, prepareErr)
		}
		journal.Maintenance = &witness
		if err := m.store.saveJournal(journal); err != nil {
			return m.parkRecovery(state, journal, err)
		}
	}
	if state.Active == nil {
		newPath, err = m.store.createEmptyGeneration(newGeneration)
	} else {
		oldPath, pathErr := m.store.generationPath(state.ActiveGeneration)
		if pathErr != nil {
			err = pathErr
		} else {
			err = m.cloner.Clone(ctx, oldPath, newPath)
		}
	}
	if err != nil {
		return m.abortBeforeRuntime(ctx, oldState, state, journal, newGeneration, err)
	}
	if state.ContainerID != "" {
		oldPath, pathErr := m.store.generationPath(state.ActiveGeneration)
		if pathErr != nil {
			return m.rollbackUpdate(ctx, oldState, state, journal, manifest, secrets, pathErr)
		}
		oldSpec := startSpec(state.InstallationID, state.ActiveOperationID, *state.Active, state.ActiveGeneration, oldPath, "")
		if err := m.runtime.Stop(ctx, state.ContainerID, oldSpec); err != nil {
			return m.rollbackUpdate(ctx, oldState, state, journal, manifest, secrets, err)
		}
		if err := m.runtime.Delete(ctx, state.ContainerID, oldSpec); err != nil {
			return m.rollbackUpdate(ctx, oldState, state, journal, manifest, secrets, err)
		}
		journal.Phase = phaseOldStopped
		if err := m.store.saveJournal(journal); err != nil {
			return m.parkRecovery(state, journal, err)
		}
	}
	spec := startSpec(state.InstallationID, operationID, newRecord, newGeneration, newPath, "")
	containerID, err := m.startAndVerify(ctx, spec, manifest, secrets, func(startedID string) error {
		journal.NewContainerID = startedID
		journal.Phase = phaseNewStarted
		return m.store.saveJournal(journal)
	})
	if err != nil {
		if errors.Is(err, errPostStartCleanupIncomplete) {
			return m.parkRecovery(state, journal, err)
		}
		return m.rollbackUpdate(ctx, oldState, state, journal, manifest, secrets, err)
	}
	journal.Phase = phaseVerified
	if err := m.store.saveJournal(journal); err != nil {
		return m.parkRecovery(state, journal, err)
	}
	finalRouting := routing.Generation
	if isUpdate {
		finalRouting, err = m.routing.Commit(ctx, *journal.Maintenance)
	} else {
		finalRouting, err = m.routing.ActivateApple(ctx, activationRoutingRequest(journal, RoutingCompleteTarget), true)
	}
	if err != nil {
		return m.parkRecovery(state, journal, err)
	}
	return m.commitNew(ctx, state, journal, containerID, finalRouting)
}

func (m *Manager) restartStoppedLocked(ctx context.Context, state durableState, routing RoutingSnapshot, manifest runtimemanifest.Manifest, secrets Secrets) (MutationReceipt, error) {
	operationID, err := randomHex(32)
	if err != nil {
		return MutationReceipt{}, err
	}
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: phasePrepared, ExpectedStateDigest: mustStateDigest(m.store, state),
		ExpectedRoutingGeneration: routing.Generation, OldArtifact: cloneRecord(state.Active),
		NewArtifact: *state.Active, OldGeneration: state.ActiveGeneration, NewGeneration: state.ActiveGeneration,
		ReuseGeneration: true,
	}
	if journal.ExpectedStateDigest == "" || m.store.saveJournal(journal) != nil {
		return MutationReceipt{}, ErrUnsafeState
	}
	working := state
	working.Status = StateUpdating
	if err := m.store.save(working); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	statePath, err := m.store.generationPath(state.ActiveGeneration)
	if err != nil {
		return m.abortRestart(ctx, state, working, journal, err)
	}
	spec := startSpec(state.InstallationID, operationID, *state.Active, state.ActiveGeneration, statePath, "")
	containerID, err := m.startAndVerify(ctx, spec, manifest, secrets, func(startedID string) error {
		journal.NewContainerID = startedID
		journal.Phase = phaseNewStarted
		return m.store.saveJournal(journal)
	})
	if err != nil {
		if errors.Is(err, errPostStartCleanupIncomplete) {
			return m.parkRecovery(working, journal, err)
		}
		return m.abortRestart(ctx, state, working, journal, err)
	}
	journal.Phase = phaseVerified
	if err := m.store.saveJournal(journal); err != nil {
		return m.parkRecovery(working, journal, err)
	}
	finalRouting, err := m.routing.ActivateApple(ctx, activationRoutingRequest(journal, RoutingCompleteTarget), true)
	if err != nil {
		return m.parkRecovery(working, journal, err)
	}
	return m.commitRestart(ctx, state, journal, containerID, finalRouting)
}

func (m *Manager) startAndVerify(ctx context.Context, spec StartSpec, manifest runtimemanifest.Manifest, secrets Secrets, onStarted func(string) error) (string, error) {
	lease, err := m.secretServer.Open(ctx, secrets)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	spec.SocketPath = lease.Path()
	containerID, err := m.runtime.Start(ctx, spec)
	if err != nil {
		if !errors.Is(err, ErrUnavailable) {
			return "", err
		}
		return m.resolveIndeterminateStart(ctx, spec, err, onStarted)
	}
	failAfterStart := func(cause error) (string, error) {
		if cleanupErr := m.cleanupStartedRuntime(ctx, containerID, spec); cleanupErr != nil {
			return containerID, errors.Join(errPostStartCleanupIncomplete, cause, cleanupErr)
		}
		return "", cause
	}
	if onStarted != nil {
		if err := onStarted(containerID); err != nil {
			return failAfterStart(err)
		}
	}
	if err := lease.Wait(ctx); err != nil {
		return failAfterStart(err)
	}
	if err := m.runtime.VerifyContainer(ctx, containerID, spec); err != nil {
		return failAfterStart(err)
	}
	if err := m.prober.Verify(ctx, secrets.APIToken, secrets.AdminToken); err != nil {
		return failAfterStart(err)
	}
	return containerID, nil
}

func (m *Manager) resolveIndeterminateStart(parent context.Context, spec StartSpec, startErr error, onStarted func(string) error) (string, error) {
	cleanupContext, cancel, err := m.postStartCleanupContext(parent)
	if err != nil {
		journalErr := error(nil)
		if onStarted != nil {
			journalErr = onStarted(ContainerName)
		}
		return ContainerName, errors.Join(
			errPostStartCleanupIncomplete, startErr, err, journalErr,
		)
	}
	defer cancel()

	verifyErr := m.runtime.VerifyContainer(cleanupContext, ContainerName, spec)
	if verifyErr == nil {
		journalErr := error(nil)
		if onStarted != nil {
			journalErr = onStarted(ContainerName)
		}
		cleanupErr := m.stopAndDelete(cleanupContext, ContainerName, spec)
		if cleanupErr != nil {
			return ContainerName, errors.Join(
				errPostStartCleanupIncomplete, startErr, journalErr, cleanupErr,
			)
		}
		return "", errors.Join(startErr, journalErr)
	}
	if absentErr := m.runtime.VerifyAbsent(cleanupContext, spec.InstallationID); absentErr == nil {
		return "", startErr
	} else {
		// The fixed name is neither proven absent nor proven to match the exact
		// new spec. Record the deterministic name and operation witness, but do
		// not mutate the resource. Recovery will retry exact verification and
		// remains fail-closed for foreign or drifted labels.
		journalErr := error(nil)
		if onStarted != nil {
			journalErr = onStarted(ContainerName)
		}
		return ContainerName, errors.Join(
			errPostStartCleanupIncomplete, startErr, verifyErr, absentErr, journalErr,
		)
	}
}

func (m *Manager) cleanupStartedRuntime(parent context.Context, containerID string, spec StartSpec) error {
	cleanupContext, cancel, err := m.postStartCleanupContext(parent)
	if err != nil {
		return err
	}
	defer cancel()
	return m.stopAndDelete(cleanupContext, containerID, spec)
}

func (m *Manager) postStartCleanupContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if m.postStartCleanupTimeout <= 0 || m.postStartCleanupTimeout > time.Minute {
		return nil, nil, ErrUnsafeState
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(parent), m.postStartCleanupTimeout)
	return cleanupContext, cancel, nil
}

func (m *Manager) rollbackUpdate(ctx context.Context, oldState, working durableState, journal transactionJournal, newManifest runtimemanifest.Manifest, secrets Secrets, cause error) (MutationReceipt, error) {
	if err := m.cleanupJournalNew(ctx, &journal); err != nil {
		return m.parkRecovery(working, journal, err)
	}
	journal, err := m.removeGenerationForRollback(working, journal, journal.NewGeneration)
	if err != nil {
		return m.parkRecovery(working, journal, err)
	}
	if journal.OldArtifact != nil && journal.OldContainerID == "" && journal.Maintenance == nil {
		// The old runtime was deliberately stopped before this activation.
		// Preserve that stopped state; recreating it without reopening routing
		// would manufacture an inconsistent "healthy" durable record.
		if err := m.store.save(oldState); err != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		if err := m.store.removeJournal(); err != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		return MutationReceipt{}, cause
	}
	if journal.OldArtifact == nil {
		if journal.Maintenance != nil {
			return m.parkRecovery(working, journal, cause)
		}
		if err := m.store.save(oldState); err != nil {
			return MutationReceipt{}, err
		}
		if err := m.store.removeJournal(); err != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		return MutationReceipt{}, cause
	}
	capability, err := m.runtime.Capability(ctx, MinimumAppleContainerVersion, oldState.InstallationID)
	if err != nil || !capability.Available {
		return m.parkRecovery(working, journal, cause)
	}
	oldManifest, err := m.verifyRollbackRecord(*journal.OldArtifact, capability)
	if err != nil {
		return m.parkRecovery(working, journal, cause)
	}
	oldPath, _ := m.store.generationPath(journal.OldGeneration)
	existingSpec := startSpec(journal.InstallationID, journal.OldOperationID, *journal.OldArtifact, journal.OldGeneration, oldPath, "")
	replacementSpec := startSpec(journal.InstallationID, journal.OperationID, *journal.OldArtifact, journal.OldGeneration, oldPath, "")
	oldContainer, activeOperationID, err := m.restoreOrRecreate(ctx, existingSpec, replacementSpec, oldManifest, secrets, func(startedID string) error {
		journal.OldContainerID = startedID
		journal.OldOperationID = replacementSpec.OperationID
		return m.store.saveJournal(journal)
	})
	if err != nil {
		return m.parkRecovery(working, journal, cause)
	}
	if journal.Maintenance != nil {
		rollbackGeneration, err := m.routing.Rollback(ctx, *journal.Maintenance)
		if err != nil {
			_ = oldContainer
			return m.parkRecovery(working, journal, cause)
		}
		oldState.RoutingGeneration = rollbackGeneration
	}
	oldState.ContainerID = oldContainer
	oldState.ActiveOperationID = activeOperationID
	oldState.Status = StateHealthy
	if err := m.store.save(oldState); err != nil || m.store.removeJournal() != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return MutationReceipt{}, cause
}

func (m *Manager) abortRestart(ctx context.Context, stopped, working durableState, journal transactionJournal, cause error) (MutationReceipt, error) {
	if err := m.cleanupJournalNew(ctx, &journal); err != nil {
		return m.parkRecovery(working, journal, err)
	}
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.RecoveryRequired || routing.AppleActive {
		return m.parkRecovery(working, journal, cause)
	}
	stopped.Status = StateStopped
	stopped.RoutingGeneration = routing.Generation
	if err := m.store.save(stopped); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if err := m.store.removeJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return MutationReceipt{}, cause
}

func (m *Manager) abortBeforeRuntime(ctx context.Context, old, working durableState, journal transactionJournal, generation uint64, cause error) (MutationReceipt, error) {
	journal, err := m.removeGenerationForRollback(working, journal, generation)
	if err != nil {
		return m.parkRecovery(working, journal, err)
	}
	if journal.Maintenance != nil {
		rollbackGeneration, err := m.routing.Rollback(ctx, *journal.Maintenance)
		if err != nil {
			return m.parkRecovery(working, journal, cause)
		}
		old.RoutingGeneration = rollbackGeneration
	}
	if err := m.store.save(old); err != nil {
		return MutationReceipt{}, err
	}
	if err := m.store.removeJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return MutationReceipt{}, cause
}

func (m *Manager) removeGenerationForRollback(state durableState, journal transactionJournal, generation uint64) (transactionJournal, error) {
	if generation == 0 || generation != journal.NewGeneration || journal.ReuseGeneration || m.removeGeneration == nil {
		return journal, ErrUnsafeState
	}
	// Record backward-only intent before beginning a recursive delete. A crash
	// during removal must not let Recover treat a partially deleted generation
	// as a valid forward-activation candidate.
	journal.CleanupNewGeneration = true
	journal.Phase = phaseRecoveryRequired
	if err := m.store.saveJournal(journal); err != nil {
		return journal, err
	}
	state.Status = StateRecoveryRequired
	if err := m.store.save(state); err != nil {
		return journal, err
	}
	if err := m.removeGeneration(generation); err != nil {
		return journal, err
	}
	return journal, nil
}

func (m *Manager) parkRecovery(state durableState, journal transactionJournal, cause error) (MutationReceipt, error) {
	state.Status = StateRecoveryRequired
	journal.Phase = phaseRecoveryRequired
	_ = m.store.saveJournal(journal)
	_ = m.store.save(state)
	if cause == nil {
		cause = ErrRecoveryRequired
	}
	return MutationReceipt{}, ErrRecoveryRequired
}

func (m *Manager) parkForwardRecovery(journal transactionJournal, cause error) (MutationReceipt, error) {
	currentState, stateFound, stateErr := m.store.load()
	_, journalFound, journalErr := m.store.loadJournal()
	if stateErr != nil || !stateFound || journalErr != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if !journalFound {
		// Only an exact committed state proves that commitNew removed the journal
		// before a final live-inspection error. A missing journal can also mean its
		// parent disappeared while a newly started container survived; in that
		// case recreate the exact in-memory witness instead of declaring success.
		if !durableTransactionCommitted(currentState, journal) {
			return m.parkRecovery(currentState, journal, cause)
		}
		if cause == nil {
			cause = ErrRecoveryRequired
		}
		return MutationReceipt{}, cause
	}
	return m.parkRecovery(currentState, journal, cause)
}

func (m *Manager) commitNew(ctx context.Context, state durableState, journal transactionJournal, containerID string, routingGeneration uint64) (MutationReceipt, error) {
	if journal.ReuseGeneration {
		return m.commitRestart(ctx, state, journal, containerID, routingGeneration)
	}
	if committedStateMatchesJournal(state, journal, containerID) {
		if state.RoutingGeneration != routingGeneration {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		if state.Status != StateHealthy {
			state.Status = StateHealthy
			if err := m.store.save(state); err != nil {
				return MutationReceipt{}, ErrRecoveryRequired
			}
		}
		if journal.Maintenance == nil {
			if err := m.finishOrdinaryRoutingCommit(ctx, activationRoutingRequest(journal, RoutingCompleteTarget), routingGeneration); err != nil {
				return MutationReceipt{}, ErrRecoveryRequired
			}
		}
		if err := m.cleanupObsoleteGeneration(state, journal); err != nil {
			return m.parkRecovery(state, journal, err)
		}
		if err := m.store.removeJournal(); err != nil {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		return m.inspectState(ctx, state)
	}
	obsoleteGeneration := state.PreviousGeneration
	if obsoleteGeneration != 0 {
		if journal.ObsoleteGeneration != 0 && journal.ObsoleteGeneration != obsoleteGeneration {
			return MutationReceipt{}, ErrRecoveryRequired
		}
		journal.ObsoleteGeneration = obsoleteGeneration
		// Persist the exact cleanup target before rotating durable state. A crash
		// after the state commit can then retry this one generation without
		// inferring it from the new active/previous pair.
		if err := m.store.saveJournal(journal); err != nil {
			return m.parkRecovery(state, journal, err)
		}
	} else if journal.ObsoleteGeneration != 0 {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	state.Previous = cloneRecord(state.Active)
	state.PreviousGeneration = state.ActiveGeneration
	state.Active = cloneRecord(&journal.NewArtifact)
	state.ActiveGeneration = journal.NewGeneration
	state.NextGeneration = journal.NewGeneration + 1
	state.ContainerID = containerID
	state.ActiveOperationID = journal.OperationID
	state.RoutingGeneration = routingGeneration
	state.Status = StateHealthy
	state.Staged = nil
	if err := m.store.save(state); err != nil {
		return m.parkRecovery(state, journal, err)
	}
	if journal.Maintenance == nil {
		if err := m.finishOrdinaryRoutingCommit(ctx, activationRoutingRequest(journal, RoutingCompleteTarget), routingGeneration); err != nil {
			return m.parkRecovery(state, journal, err)
		}
	}
	if err := m.cleanupObsoleteGeneration(state, journal); err != nil {
		return m.parkRecovery(state, journal, err)
	}
	if err := m.store.removeJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) cleanupObsoleteGeneration(state durableState, journal transactionJournal) error {
	generation := journal.ObsoleteGeneration
	if generation == 0 {
		return nil
	}
	if m.removeGeneration == nil || generation == state.ActiveGeneration || generation == state.PreviousGeneration ||
		generation >= state.NextGeneration || generation >= journal.OldGeneration {
		return ErrUnsafeState
	}
	return m.removeGeneration(generation)
}

func (m *Manager) commitRestart(ctx context.Context, state durableState, journal transactionJournal, containerID string, routingGeneration uint64) (MutationReceipt, error) {
	if !journal.ReuseGeneration || containerID == "" || journal.OldArtifact == nil ||
		journal.NewArtifact != *journal.OldArtifact || state.Active == nil || *state.Active != journal.NewArtifact ||
		state.ActiveGeneration != journal.NewGeneration {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	if state.ContainerID != "" && (state.ContainerID != containerID || state.ActiveOperationID != journal.OperationID || state.RoutingGeneration != routingGeneration) {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	state.ContainerID = containerID
	state.ActiveOperationID = journal.OperationID
	state.RoutingGeneration = routingGeneration
	state.Status = StateHealthy
	state.Staged = nil
	if err := m.store.save(state); err != nil {
		return m.parkRecovery(state, journal, err)
	}
	if err := m.finishOrdinaryRoutingCommit(ctx, activationRoutingRequest(journal, RoutingCompleteTarget), routingGeneration); err != nil {
		return m.parkRecovery(state, journal, err)
	}
	if err := m.store.removeJournal(); err != nil {
		return MutationReceipt{}, ErrRecoveryRequired
	}
	return m.inspectState(ctx, state)
}

func (m *Manager) recoverForward(ctx context.Context, state durableState, journal *transactionJournal, capability Capability, secrets Secrets, desktopExited bool) (MutationReceipt, bool, error) {
	if journal == nil {
		return MutationReceipt{}, false, ErrUnsafeState
	}
	newPath, err := m.store.generationPath(journal.NewGeneration)
	if err != nil {
		return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
	}
	spec := startSpec(journal.InstallationID, journal.OperationID, journal.NewArtifact, journal.NewGeneration, newPath, "")
	containerID := journal.NewContainerID
	if containerID == "" {
		identityErr := m.runtime.VerifyContainer(ctx, ContainerName, spec)
		if identityErr == nil {
			// Start uses a deterministic fixed name. A crash can occur after the
			// container is created but before onStarted persists its ID. Adopt only
			// the complete exact spec, and durably record that authority before any
			// manifest/health probe or subsequent mutation.
			containerID = ContainerName
			journal.NewContainerID = containerID
			if journal.ObsoleteGeneration != 0 {
				journal.Phase = phaseRecoveryRequired
			} else {
				journal.Phase = phaseNewStarted
			}
			if err := m.store.saveJournal(*journal); err != nil {
				return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
			}
		} else if m.recoveryHasExactOldContainer(ctx, *journal) {
			// A previously active container can legitimately occupy the fixed name
			// while the new-container witness is empty. It is bound by the journal's
			// complete old spec, so let the existing backward path verify/restore it.
			return MutationReceipt{}, false, nil
		} else if absentErr := m.runtime.VerifyAbsent(ctx, journal.InstallationID); absentErr != nil {
			// A mismatched, foreign, or unreadable fixed-name resource may still be
			// using the journal-pinned generation. Never fall backward and delete
			// that generation until exact absence is positively established.
			return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, identityErr, absentErr)
		}
	}
	var manifest runtimemanifest.Manifest
	if journal.ReuseGeneration {
		manifest, err = m.verifyRollbackRecord(journal.NewArtifact, capability)
	} else {
		manifest, err = m.verifyRecord(journal.NewArtifact, state.HighestSeenSequence, capability)
	}
	if err != nil {
		if containerID != "" {
			return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
		}
		return MutationReceipt{}, false, nil
	}
	if containerID != "" && (m.runtime.VerifyContainer(ctx, containerID, spec) != nil || m.prober.Verify(ctx, secrets.APIToken, secrets.AdminToken) != nil) {
		if err := m.cleanupJournalNew(ctx, journal); err != nil {
			return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
		}
		containerID = ""
	}
	if containerID == "" {
		containerID, err = m.startAndVerify(ctx, spec, manifest, secrets, func(startedID string) error {
			journal.NewContainerID = startedID
			if journal.ObsoleteGeneration != 0 {
				journal.Phase = phaseRecoveryRequired
			} else {
				journal.Phase = phaseNewStarted
			}
			return m.store.saveJournal(*journal)
		})
		if err != nil {
			if errors.Is(err, errPostStartCleanupIncomplete) {
				return MutationReceipt{}, false, err
			}
			// Runtime.Start may have created the fixed-name resource before
			// returning an error. Without a positive absence read-back, backward
			// cleanup could delete the mounted generation out from under it.
			if absentErr := m.runtime.VerifyAbsent(ctx, journal.InstallationID); absentErr != nil {
				return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err, absentErr)
			}
			return MutationReceipt{}, false, nil
		}
	}
	journal.NewContainerID = containerID
	journal.Phase = phaseVerified
	if err := m.store.saveJournal(*journal); err != nil {
		return MutationReceipt{}, false, nil
	}
	// A prior invocation may have durably committed lifecycle state and then
	// lost the routing ACK response (or crashed before removing this journal).
	// At this point the exact container and health have been reverified. Reuse
	// only the routing generation bound in that committed state; commitNew will
	// either acknowledge the still-present exact cross witness or require an
	// already-stable route at that same generation when the ACK had completed.
	if committedRuntimeStateMatchesJournal(state, *journal, containerID) {
		receipt, commitErr := m.commitNew(ctx, state, *journal, containerID, state.RoutingGeneration)
		if commitErr != nil {
			return MutationReceipt{}, false, commitErr
		}
		return receipt, true, nil
	}
	finalGeneration := uint64(0)
	if journal.Maintenance != nil {
		finalGeneration, err = m.recoverMaintenanceFinish(ctx, *journal.Maintenance, true)
		if err != nil {
			return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
		}
	} else {
		finalGeneration, _, err = m.reconcileOrdinaryRouting(ctx, activationRoutingRequest(*journal, RoutingCompleteTarget), desktopExited)
		if err != nil {
			return MutationReceipt{}, false, errors.Join(errForwardRecoveryIndeterminate, err)
		}
	}
	receipt, err := m.commitNew(ctx, state, *journal, containerID, finalGeneration)
	if err != nil {
		return MutationReceipt{}, false, err
	}
	return receipt, true, nil
}

func (m *Manager) recoveryHasExactOldContainer(ctx context.Context, journal transactionJournal) bool {
	if journal.OldArtifact == nil || journal.OldContainerID != ContainerName || journal.OldOperationID == "" {
		return false
	}
	oldPath, err := m.store.generationPath(journal.OldGeneration)
	if err != nil {
		return false
	}
	oldSpec := startSpec(
		journal.InstallationID,
		journal.OldOperationID,
		*journal.OldArtifact,
		journal.OldGeneration,
		oldPath,
		"",
	)
	return m.runtime.VerifyContainer(ctx, journal.OldContainerID, oldSpec) == nil
}

func (m *Manager) recoverBackward(ctx context.Context, state durableState, journal *transactionJournal, capability Capability, secrets Secrets, desktopExited bool) (MutationReceipt, bool) {
	if journal == nil {
		return MutationReceipt{}, false
	}
	if ordinaryRollbackStateMatchesJournal(state, *journal) && m.ordinaryRollbackCleanupComplete(ctx, *journal) {
		return m.finishCommittedOrdinaryRollback(ctx, state, journal)
	}
	if journal.ReuseGeneration {
		return m.recoverRestartBackward(ctx, state, journal, desktopExited)
	}
	if journal.OldArtifact == nil {
		routingRequest := activationRoutingRequest(*journal, RoutingRestoreOrigin)
		finalRouting, _, err := m.reconcileOrdinaryRouting(ctx, routingRequest, desktopExited)
		if err != nil {
			return MutationReceipt{}, false
		}
		if err := m.cleanupJournalNew(ctx, journal); err != nil {
			return MutationReceipt{}, false
		}
		var cleanupErr error
		*journal, cleanupErr = m.removeGenerationForRollback(state, *journal, journal.NewGeneration)
		if cleanupErr != nil {
			return MutationReceipt{}, false
		}
		state.Status, state.ContainerID, state.Active, state.ActiveGeneration = StateStopped, "", nil, 0
		state.RoutingGeneration = finalRouting
		if m.store.save(state) != nil {
			return MutationReceipt{}, false
		}
		if m.finishOrdinaryRoutingCommit(ctx, routingRequest, finalRouting) != nil {
			return MutationReceipt{}, false
		}
		if m.store.removeJournal() != nil {
			return MutationReceipt{}, false
		}
		receipt, err := m.inspectState(ctx, state)
		return receipt, err == nil
	}
	if journal.OldContainerID == "" && journal.Maintenance == nil {
		return m.recoverStoppedUpgradeBackward(ctx, state, journal, desktopExited)
	}
	oldManifest, err := m.verifyRollbackRecord(*journal.OldArtifact, capability)
	if err != nil {
		return MutationReceipt{}, false
	}
	if err := m.cleanupJournalNew(ctx, journal); err != nil {
		return MutationReceipt{}, false
	}
	var cleanupErr error
	*journal, cleanupErr = m.removeGenerationForRollback(state, *journal, journal.NewGeneration)
	if cleanupErr != nil {
		return MutationReceipt{}, false
	}
	oldPath, _ := m.store.generationPath(journal.OldGeneration)
	existingSpec := startSpec(journal.InstallationID, journal.OldOperationID, *journal.OldArtifact, journal.OldGeneration, oldPath, "")
	replacementSpec := startSpec(journal.InstallationID, journal.OperationID, *journal.OldArtifact, journal.OldGeneration, oldPath, "")
	oldContainer, activeOperationID, err := m.restoreOrRecreate(ctx, existingSpec, replacementSpec, oldManifest, secrets, func(startedID string) error {
		journal.OldContainerID = startedID
		journal.OldOperationID = replacementSpec.OperationID
		return m.store.saveJournal(*journal)
	})
	if err != nil {
		return MutationReceipt{}, false
	}
	if journal.Maintenance != nil {
		rollbackGeneration, err := m.recoverMaintenanceFinish(ctx, *journal.Maintenance, false)
		if err != nil {
			return MutationReceipt{}, false
		}
		state.RoutingGeneration = rollbackGeneration
	}
	state.Active = cloneRecord(journal.OldArtifact)
	state.ActiveGeneration = journal.OldGeneration
	state.ContainerID = oldContainer
	state.ActiveOperationID = activeOperationID
	state.Status = StateHealthy
	state.Staged = cloneRecord(&journal.NewArtifact)
	if m.store.save(state) != nil || m.store.removeJournal() != nil {
		return MutationReceipt{}, false
	}
	receipt, err := m.inspectState(ctx, state)
	return receipt, err == nil
}

func (m *Manager) recoverStoppedUpgradeBackward(ctx context.Context, state durableState, journal *transactionJournal, desktopExited bool) (MutationReceipt, bool) {
	if journal == nil {
		return MutationReceipt{}, false
	}
	routingRequest := activationRoutingRequest(*journal, RoutingRestoreOrigin)
	finalRouting, _, err := m.reconcileOrdinaryRouting(ctx, routingRequest, desktopExited)
	if err != nil {
		return MutationReceipt{}, false
	}
	if err := m.cleanupJournalNew(ctx, journal); err != nil {
		return MutationReceipt{}, false
	}
	var cleanupErr error
	*journal, cleanupErr = m.removeGenerationForRollback(state, *journal, journal.NewGeneration)
	if cleanupErr != nil {
		return MutationReceipt{}, false
	}
	state.Active = cloneRecord(journal.OldArtifact)
	state.ActiveGeneration = journal.OldGeneration
	state.ContainerID = ""
	state.ActiveOperationID = ""
	state.Status = StateStopped
	state.Staged = cloneRecord(&journal.NewArtifact)
	state.RoutingGeneration = finalRouting
	if m.store.save(state) != nil {
		return MutationReceipt{}, false
	}
	if m.finishOrdinaryRoutingCommit(ctx, routingRequest, finalRouting) != nil {
		return MutationReceipt{}, false
	}
	if m.store.removeJournal() != nil {
		return MutationReceipt{}, false
	}
	receipt, inspectErr := m.inspectState(ctx, state)
	return receipt, inspectErr == nil
}

func (m *Manager) finishCommittedOrdinaryRollback(ctx context.Context, state durableState, journal *transactionJournal) (MutationReceipt, bool) {
	if journal == nil || !ordinaryRollbackStateMatchesJournal(state, *journal) ||
		!m.ordinaryRollbackCleanupComplete(ctx, *journal) {
		return MutationReceipt{}, false
	}
	routingRequest := activationRoutingRequest(*journal, RoutingRestoreOrigin)
	if m.finishOrdinaryRoutingCommit(ctx, routingRequest, state.RoutingGeneration) != nil {
		return MutationReceipt{}, false
	}
	state.Status = StateStopped
	if m.store.save(state) != nil || m.store.removeJournal() != nil {
		return MutationReceipt{}, false
	}
	receipt, err := m.inspectState(ctx, state)
	return receipt, err == nil
}

func (m *Manager) ordinaryRollbackCleanupComplete(ctx context.Context, journal transactionJournal) bool {
	if m.runtime.VerifyAbsent(ctx, journal.InstallationID) != nil {
		return false
	}
	if journal.ReuseGeneration {
		return true
	}
	path, err := m.store.generationPath(journal.NewGeneration)
	if err != nil {
		return false
	}
	_, err = os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (m *Manager) recoverRestartBackward(ctx context.Context, state durableState, journal *transactionJournal, desktopExited bool) (MutationReceipt, bool) {
	if journal == nil {
		return MutationReceipt{}, false
	}
	routingRequest := activationRoutingRequest(*journal, RoutingRestoreOrigin)
	finalRouting, _, err := m.reconcileOrdinaryRouting(ctx, routingRequest, desktopExited)
	if err != nil {
		return MutationReceipt{}, false
	}
	if err := m.cleanupJournalNew(ctx, journal); err != nil {
		return MutationReceipt{}, false
	}
	state.Active = cloneRecord(journal.OldArtifact)
	state.ActiveGeneration = journal.OldGeneration
	state.ContainerID = ""
	state.ActiveOperationID = ""
	state.Status = StateStopped
	state.RoutingGeneration = finalRouting
	if m.store.save(state) != nil {
		return MutationReceipt{}, false
	}
	if m.finishOrdinaryRoutingCommit(ctx, routingRequest, finalRouting) != nil {
		return MutationReceipt{}, false
	}
	if m.store.removeJournal() != nil {
		return MutationReceipt{}, false
	}
	receipt, inspectErr := m.inspectState(ctx, state)
	return receipt, inspectErr == nil
}

func (m *Manager) verifyRecord(record artifactRecord, highest uint64, capability Capability) (runtimemanifest.Manifest, error) {
	manifestBytes, signatureBytes, err := m.store.loadManifest(record.ManifestSHA256)
	if err != nil {
		return runtimemanifest.Manifest{}, err
	}
	manifest, err := runtimemanifest.Verify(manifestBytes, signatureBytes, m.publicKeyPEM, runtimemanifest.VerifyOptions{
		HighestSeenSequence: highest, RelayVersion: m.relayVersion,
		MacOSVersion: capability.MacOSVersion, AppleContainerVersion: capability.AppleContainerVersion,
	})
	if err != nil {
		return runtimemanifest.Manifest{}, err
	}
	candidate := runtimemanifest.Candidate{ReleaseID: record.ReleaseID, Tag: record.ReleaseTag, ManifestSHA256: record.ManifestSHA256, Manifest: manifest}
	verified, err := recordFromCandidate(candidate)
	if err != nil || verified != record {
		return runtimemanifest.Manifest{}, ErrUnsafeState
	}
	return manifest, nil
}

// A rollback is authorized by the owner-only journal's exact prior manifest
// hash, digest, and generation witness. It must not lower highest-seen, but the
// signature verifier must admit that pinned older sequence for this one
// transaction instead of treating it as a new installation candidate.
func (m *Manager) verifyRollbackRecord(record artifactRecord, capability Capability) (runtimemanifest.Manifest, error) {
	return m.verifyRecord(record, record.ReleaseSequence, capability)
}

func (m *Manager) validateRecoveryBinding(state durableState, journal transactionJournal) error {
	if journal.InstallationID != state.InstallationID {
		return ErrUnsafeState
	}
	committed := committedStateMatchesJournal(state, journal, journal.NewContainerID)
	awaitingReplacement := committedStateAwaitingReplacementMatchesJournal(state, journal)
	if journal.ObsoleteGeneration != 0 {
		if journal.ObsoleteGeneration == state.ActiveGeneration || journal.ObsoleteGeneration == state.PreviousGeneration {
			return ErrUnsafeState
		}
		if !committed && !awaitingReplacement && state.PreviousGeneration != journal.ObsoleteGeneration {
			return ErrUnsafeState
		}
	}
	if committed || awaitingReplacement {
		return nil
	}
	// A backward ordinary transaction can durably save its exact stopped
	// lifecycle result before the routing ACK or local journal removal. Its
	// routing generation is necessarily newer than the original state digest,
	// so recognize the complete journal-bound shape here and defer the exact
	// stable-route/cleanup proof to finishCommittedOrdinaryRollback.
	if ordinaryRollbackStateMatchesJournal(state, journal) {
		return nil
	}
	normalized := state
	if journal.OldArtifact == nil {
		normalized.Status = StateStopped
	} else if journal.OldContainerID == "" {
		normalized.Status = StateStopped
	} else {
		normalized.Status = StateHealthy
	}
	normalized.Active = cloneRecord(journal.OldArtifact)
	normalized.ActiveGeneration = journal.OldGeneration
	if journal.OldContainerID == ContainerName && journal.OldOperationID == journal.OperationID {
		// Rollback recreation uses the transaction operation ID. The journal must
		// bind that replacement before health/mutation, while ExpectedStateDigest
		// remains a witness over the original pre-update container operation. Keep
		// the still-durable state's original identity solely for reconstructing
		// that pre-update digest; all later runtime mutations use the replacement
		// spec pinned by the journal.
		if normalized.ContainerID != ContainerName || !isSHA256(normalized.ActiveOperationID) {
			return ErrUnsafeState
		}
	} else {
		normalized.ContainerID = journal.OldContainerID
		normalized.ActiveOperationID = journal.OldOperationID
	}
	if journal.ReuseGeneration {
		// A restart can crash after the healthy state is durable and before the
		// journal is removed. Reconstruct the exact stopped pre-state, including
		// its pre-activation routing generation, so recovery remains bound to the
		// original CAS rather than accepting an unrelated healthy state.
		normalized.Status = StateStopped
		normalized.RoutingGeneration = journal.ExpectedRoutingGeneration
	} else if journal.Maintenance != nil {
		if state.RoutingGeneration != journal.ExpectedRoutingGeneration &&
			state.RoutingGeneration != journal.Maintenance.FinalRoutingGeneration {
			return ErrUnsafeState
		}
		normalized.RoutingGeneration = journal.ExpectedRoutingGeneration
	}
	digest, err := m.store.digest(normalized)
	if err != nil || digest != journal.ExpectedStateDigest {
		return ErrUnsafeState
	}
	return nil
}

func (m *Manager) restoreOrRecreate(ctx context.Context, existingSpec, replacementSpec StartSpec, manifest runtimemanifest.Manifest, secrets Secrets, onRecreated func(string) error) (string, string, error) {
	// A failed stop/delete can leave the exact owned old container running or
	// stopped under the fixed name. Reuse it only after full metadata and HTTP
	// verification. Otherwise make a best-effort non-force cleanup and recreate
	// it from the journal-pinned digest and generation.
	// Recovery can re-enter after a replacement of the pinned old container was
	// started but before its first journal write. Bind that exact identity before
	// probing health or mutating it; an unhealthy replacement must be stopped
	// with its replacement operation witness, never the stale old operation ID.
	if m.runtime.VerifyContainer(ctx, ContainerName, replacementSpec) == nil {
		if onRecreated != nil {
			if err := onRecreated(ContainerName); err != nil {
				return ContainerName, replacementSpec.OperationID, err
			}
		}
		if m.prober.Verify(ctx, secrets.APIToken, secrets.AdminToken) == nil {
			return ContainerName, replacementSpec.OperationID, nil
		}
		if err := m.stopAndDelete(ctx, ContainerName, replacementSpec); err != nil {
			return ContainerName, replacementSpec.OperationID, err
		}
	} else if m.runtime.VerifyContainer(ctx, ContainerName, existingSpec) == nil {
		if m.prober.Verify(ctx, secrets.APIToken, secrets.AdminToken) == nil {
			return ContainerName, existingSpec.OperationID, nil
		}
		if err := m.stopAndDelete(ctx, ContainerName, existingSpec); err != nil {
			return "", "", err
		}
	} else {
		// Neither complete owned identity matched. Only a positive absence proof
		// permits creation; a mismatched, foreign, or unreadable fixed-name
		// resource remains untouched and requires recovery.
		if err := m.runtime.VerifyAbsent(ctx, replacementSpec.InstallationID); err != nil {
			return "", "", err
		}
	}
	containerID, err := m.startAndVerify(ctx, replacementSpec, manifest, secrets, onRecreated)
	if err != nil {
		return "", "", err
	}
	return containerID, replacementSpec.OperationID, nil
}

func (m *Manager) recoverMaintenanceFinish(ctx context.Context, witness MaintenanceWitness, commit bool) (uint64, error) {
	routing, err := m.routing.Current(ctx)
	if err != nil {
		return 0, err
	}
	if maintenanceFinishComplete(routing, witness) {
		return witness.FinalRoutingGeneration, nil
	}
	if !maintenanceFinishPending(routing, witness) {
		return 0, ErrRoutingChanged
	}
	var generation uint64
	if commit {
		generation, err = m.routing.Commit(ctx, witness)
	} else {
		generation, err = m.routing.Rollback(ctx, witness)
	}
	if err == nil && generation != witness.FinalRoutingGeneration {
		err = ErrRoutingChanged
	}
	// The routing side removes its journal only after the stable final state is
	// durable. Re-read on both success and response loss: only that exact final,
	// non-pending Apple snapshot acknowledges completion.
	current, currentErr := m.routing.Current(ctx)
	if currentErr == nil && maintenanceFinishComplete(current, witness) {
		return witness.FinalRoutingGeneration, nil
	}
	if err != nil {
		return 0, err
	}
	if currentErr != nil {
		return 0, currentErr
	}
	return 0, ErrRoutingChanged
}

func maintenanceFinishComplete(routing RoutingSnapshot, witness MaintenanceWitness) bool {
	return routing.Generation == witness.FinalRoutingGeneration && routing.AppleActive &&
		!routing.RecoveryRequired && !routing.MaintenancePending
}

func maintenanceFinishPending(routing RoutingSnapshot, witness MaintenanceWitness) bool {
	if !routing.MaintenancePending || !routing.RecoveryRequired {
		return false
	}
	switch routing.Generation {
	case witness.OriginRoutingGeneration, witness.FinalRoutingGeneration:
		return routing.AppleActive
	case witness.PreparedRoutingGeneration:
		return !routing.AppleActive
	default:
		return false
	}
}

// reconcileOrdinaryRouting distinguishes a route that was never started from
// one whose response was lost. A completed target is accepted only while the
// routing side still holds the exact operation witness; the origin may be a
// no-op only at the journal-pinned generation before any route mutation.
func (m *Manager) reconcileOrdinaryRouting(ctx context.Context, request RoutingRequest, desktopExited bool) (uint64, bool, error) {
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.MaintenancePending {
		return 0, false, errOrRouting(err)
	}
	// An ordinary routing witness is the sole authority for recovering its own
	// requested/applying/recovery state. The public routing snapshot is expected
	// to report RecoveryRequired while that fail-closed state is parked; consult
	// the exact operation witness before rejecting the generic recovery flag.
	if routing.RuntimeRoutingPending {
		generation, reconcileErr := m.routing.Reconcile(ctx, request, desktopExited)
		return generation, true, reconcileErr
	}
	if routing.RecoveryRequired {
		return 0, false, ErrRoutingChanged
	}
	selectedApple := request.TargetAppleActive
	if request.Direction == RoutingRestoreOrigin {
		selectedApple = !request.TargetAppleActive
		if routing.Generation == request.ExpectedOriginRoutingGeneration && routing.AppleActive == selectedApple {
			return routing.Generation, false, nil
		}
		return 0, false, ErrRoutingChanged
	}
	if routing.Generation != request.ExpectedOriginRoutingGeneration || routing.AppleActive == selectedApple {
		return 0, false, ErrRoutingChanged
	}
	var generation uint64
	if request.TargetAppleActive {
		generation, err = m.routing.ActivateApple(ctx, request, desktopExited)
	} else {
		generation, err = m.routing.StopApple(ctx, request, desktopExited)
	}
	return generation, true, err
}

// finishOrdinaryRoutingCommit closes the cross-journal handshake only after
// the lifecycle manager has durably committed its matching state. If the
// witness remains, Acknowledge performs the exact operation/installation and
// generation checks. If it is already absent, the only admissible explanation
// is a lost ACK response or crash after ACK: require the identical stable
// backend and generation and perform no routing mutation.
func (m *Manager) finishOrdinaryRoutingCommit(ctx context.Context, request RoutingRequest, generation uint64) error {
	routing, err := m.routing.Current(ctx)
	if err != nil || routing.MaintenancePending || generation == 0 {
		return errOrRouting(err)
	}
	if routing.RuntimeRoutingPending {
		return m.routing.Acknowledge(ctx, request, generation)
	}
	selectedApple := request.TargetAppleActive
	if request.Direction == RoutingRestoreOrigin {
		selectedApple = !selectedApple
	}
	if routing.RecoveryRequired || routing.Generation != generation || routing.AppleActive != selectedApple {
		return ErrRoutingChanged
	}
	return nil
}

func errOrRouting(err error) error {
	if err != nil {
		return err
	}
	return ErrRoutingChanged
}

func (m *Manager) cleanupJournalNew(ctx context.Context, journal *transactionJournal) error {
	if journal == nil {
		return ErrUnsafeState
	}
	if journal.NewContainerID == "" {
		return nil
	}
	statePath, err := m.store.generationPath(journal.NewGeneration)
	if err != nil {
		return ErrUnsafeState
	}
	spec := startSpec(journal.InstallationID, journal.OperationID, journal.NewArtifact, journal.NewGeneration, statePath, "")
	if err := m.stopAndDelete(ctx, journal.NewContainerID, spec); err != nil {
		return err
	}
	// The fixed name may immediately be reused for the old replacement. Clear
	// and persist the now-absent new-container witness first, so a later routing
	// response loss cannot make recovery apply the stale new spec to that old
	// container.
	journal.NewContainerID = ""
	switch {
	case journal.ObsoleteGeneration != 0:
		journal.Phase = phaseRecoveryRequired
	case journal.CleanupNewGeneration:
		journal.Phase = phaseRecoveryRequired
	case !journal.ReuseGeneration && journal.OldContainerID != "":
		journal.Phase = phaseOldStopped
	default:
		journal.Phase = phasePrepared
	}
	return m.store.saveJournal(*journal)
}

func committedStateAwaitingReplacementMatchesJournal(state durableState, journal transactionJournal) bool {
	if journal.ObsoleteGeneration == 0 || journal.NewContainerID != "" || journal.Phase != phaseRecoveryRequired ||
		(state.Status != StateHealthy && state.Status != StateRecoveryRequired) || state.InstallationID != journal.InstallationID ||
		state.Active == nil || *state.Active != journal.NewArtifact || state.ActiveGeneration != journal.NewGeneration ||
		state.ContainerID != ContainerName || state.ActiveOperationID != journal.OperationID || state.Staged != nil ||
		state.NextGeneration != journal.NewGeneration+1 || state.RoutingGeneration <= journal.ExpectedRoutingGeneration {
		return false
	}
	return journal.OldArtifact != nil && state.Previous != nil && *state.Previous == *journal.OldArtifact &&
		state.PreviousGeneration == journal.OldGeneration && journal.ObsoleteGeneration != state.ActiveGeneration &&
		journal.ObsoleteGeneration != state.PreviousGeneration
}

func committedStateMatchesJournal(state durableState, journal transactionJournal, containerID string) bool {
	if containerID == "" || (state.Status != StateHealthy && state.Status != StateRecoveryRequired) || state.ContainerID != containerID ||
		state.ActiveOperationID != journal.OperationID ||
		state.ActiveGeneration != journal.NewGeneration || state.Staged != nil ||
		state.NextGeneration != journal.NewGeneration+1 || state.Active == nil || *state.Active != journal.NewArtifact {
		return false
	}
	if journal.OldArtifact == nil {
		return state.Previous == nil && state.PreviousGeneration == 0
	}
	return state.Previous != nil && *state.Previous == *journal.OldArtifact &&
		state.PreviousGeneration == journal.OldGeneration
}

func committedRuntimeStateMatchesJournal(state durableState, journal transactionJournal, containerID string) bool {
	if !journal.ReuseGeneration {
		return committedStateMatchesJournal(state, journal, containerID)
	}
	return containerID != "" && (state.Status == StateHealthy || state.Status == StateRecoveryRequired) &&
		journal.OldArtifact != nil && journal.NewArtifact == *journal.OldArtifact &&
		state.Active != nil && *state.Active == journal.NewArtifact && state.Staged == nil &&
		state.ActiveGeneration == journal.NewGeneration && state.ContainerID == containerID &&
		state.ActiveOperationID == journal.OperationID && state.RoutingGeneration > journal.ExpectedRoutingGeneration
}

func durableTransactionCommitted(state durableState, journal transactionJournal) bool {
	if state.Status != StateHealthy || journal.NewContainerID == "" {
		return false
	}
	if !journal.ReuseGeneration {
		return committedStateMatchesJournal(state, journal, journal.NewContainerID)
	}
	return journal.OldArtifact != nil && journal.NewArtifact == *journal.OldArtifact &&
		state.Active != nil && *state.Active == journal.NewArtifact && state.Staged == nil &&
		state.ActiveGeneration == journal.NewGeneration && state.ContainerID == journal.NewContainerID &&
		state.ActiveOperationID == journal.OperationID
}

func stopStateMatchesCommittedJournal(state durableState, journal stopTransactionJournal) bool {
	return journal.FinalRoutingGeneration != 0 && state.RoutingGeneration == journal.FinalRoutingGeneration &&
		state.RoutingGeneration > journal.ExpectedRoutingGeneration && state.Active != nil &&
		*state.Active == journal.Artifact && state.ActiveGeneration == journal.StateGeneration &&
		state.ContainerID == "" && state.ActiveOperationID == "" &&
		(state.Status == StateStopped || state.Status == StateRecoveryRequired)
}

func ordinaryRollbackStateMatchesJournal(state durableState, journal transactionJournal) bool {
	if journal.Maintenance != nil || journal.NewContainerID != "" || state.RoutingGeneration == 0 ||
		state.RoutingGeneration < journal.ExpectedRoutingGeneration ||
		(state.Status != StateStopped && state.Status != StateRecoveryRequired) ||
		state.ContainerID != "" || state.ActiveOperationID != "" {
		return false
	}
	if journal.ReuseGeneration {
		return !journal.CleanupNewGeneration && journal.OldArtifact != nil &&
			journal.NewArtifact == *journal.OldArtifact && state.Active != nil &&
			*state.Active == *journal.OldArtifact && state.ActiveGeneration == journal.OldGeneration &&
			state.Staged == nil
	}
	if !journal.CleanupNewGeneration {
		return false
	}
	if journal.OldArtifact == nil {
		return state.Active == nil && state.ActiveGeneration == 0 && state.Staged != nil &&
			*state.Staged == journal.NewArtifact
	}
	return journal.OldContainerID == "" && state.Active != nil && *state.Active == *journal.OldArtifact &&
		state.ActiveGeneration == journal.OldGeneration && state.Staged != nil && *state.Staged == journal.NewArtifact
}

func (m *Manager) inspectState(ctx context.Context, state durableState) (Inspection, error) {
	capability, capErr := m.runtime.Capability(ctx, MinimumAppleContainerVersion, state.InstallationID)
	if capErr != nil {
		capability = Capability{Available: false, Reason: "capability_probe_failed", SystemServiceState: "unknown"}
	}
	routing, err := m.routing.Current(ctx)
	if err != nil {
		return Inspection{}, err
	}
	_, journalFound, journalErr := m.store.loadJournal()
	_, stopFound, stopErr := m.store.loadStopJournal()
	if journalErr != nil || stopErr != nil || journalFound && stopFound {
		return Inspection{}, ErrUnsafeState
	}
	stateValue := state.Status
	if journalFound || stopFound || routing.RecoveryRequired || routing.RuntimeRoutingPending || state.Status == StateRecoveryRequired || state.Status == StateUpdating {
		stateValue = StateRecoveryRequired
	} else if state.ContainerID != "" && state.Active != nil {
		if !routing.AppleActive {
			stateValue = StateRecoveryRequired
		} else if !capability.Available {
			// A stopped service or unavailable CLI is an environmental
			// capability loss, not proof that the durable transaction was
			// corrupted. A positive ownership/port conflict while an active
			// container is expected is evidence of external runtime drift.
			if capabilityImpliesActiveDrift(capability) {
				stateValue = StateRecoveryRequired
			} else {
				stateValue = StateUnavailable
			}
		} else if m.verifyActiveRuntime(ctx, state) != nil {
			stateValue = StateRecoveryRequired
		}
	} else if routing.AppleActive {
		// Relay must never advertise the Apple backend without the exact
		// runtime container represented by durable state.
		stateValue = StateRecoveryRequired
	} else if capability.Available {
		// Stopped state also has an ownership invariant: an externally
		// recreated fixed-name container must not be silently ignored.
		if m.runtime.VerifyAbsent(ctx, state.InstallationID) != nil {
			stateValue = StateRecoveryRequired
		}
	} else {
		stateValue = StateUnavailable
	}
	digest, err := m.store.digest(state)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{
		SchemaVersion: SchemaVersion, OK: true, State: stateValue, Capability: capability,
		Staged: summaryOf(state.Staged), Active: summaryOf(state.Active), StateDigest: digest,
		RoutingGeneration: routing.Generation, RecoveryRequired: stateValue == StateRecoveryRequired,
	}, nil
}

func (m *Manager) verifyActiveRuntime(ctx context.Context, state durableState) error {
	statePath, err := m.store.generationPath(state.ActiveGeneration)
	if err != nil || state.Active == nil {
		return ErrUnsafeState
	}
	spec := startSpec(state.InstallationID, state.ActiveOperationID, *state.Active, state.ActiveGeneration, statePath, "")
	if err := m.runtime.VerifyContainer(ctx, state.ContainerID, spec); err != nil {
		return err
	}
	// Inspect is read-only: Load may consult the two fixed Keychain items but
	// must never create or rotate them. Without both existing credentials the
	// manager cannot authenticate the models/admin separation contract, so a
	// previously healthy active runtime is no longer trustworthy.
	secrets, err := m.keychain.Load(ctx, m.account)
	if err != nil {
		zeroSecrets(&secrets)
		return ErrCredential
	}
	defer zeroSecrets(&secrets)
	if !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) || bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		return ErrCredential
	}
	return m.prober.Verify(ctx, secrets.APIToken, secrets.AdminToken)
}

func capabilityImpliesActiveDrift(capability Capability) bool {
	return capability.Reason == "apple_container_foreign_container" ||
		capability.Reason == "apple_container_port_unavailable"
}

func (m *Manager) transactionPresent() (bool, error) {
	_, activation, activationErr := m.store.loadJournal()
	_, stop, stopErr := m.store.loadStopJournal()
	if activationErr != nil || stopErr != nil || activation && stop {
		return true, ErrUnsafeState
	}
	return activation || stop, nil
}

func (m *Manager) readState() (durableState, error) {
	state, found, err := m.store.load()
	if err != nil {
		return durableState{}, err
	}
	if found {
		return state, nil
	}
	return m.store.initialize()
}

func (m *Manager) loadForMutation() (durableState, error) {
	if err := m.store.prepareRoot(); err != nil {
		return durableState{}, err
	}
	state, found, err := m.store.load()
	if err != nil {
		return durableState{}, err
	}
	if found {
		return state, nil
	}
	state, err = m.store.initialize()
	if err != nil {
		return durableState{}, err
	}
	if err := m.store.save(state); err != nil {
		return durableState{}, err
	}
	return state, nil
}

func (m *Manager) requireCAS(state durableState, routing RoutingSnapshot, stateDigest string, routingGeneration uint64) error {
	digest, err := m.store.digest(state)
	if err != nil || digest != stateDigest {
		return ErrStateChanged
	}
	if routing.Generation == 0 || routing.Generation != routingGeneration {
		return ErrRoutingChanged
	}
	return nil
}

func (m *Manager) checkRequest(state durableState, capability Capability) runtimemanifest.CheckRequest {
	current := ""
	if state.Active != nil {
		current = state.Active.ArtifactVersion
	}
	return runtimemanifest.CheckRequest{
		CurrentArtifactVersion: current, PublicKeyPEM: append([]byte(nil), m.publicKeyPEM...),
		VerifyOptions: runtimemanifest.VerifyOptions{
			HighestSeenSequence: state.HighestSeenSequence, RelayVersion: m.relayVersion,
			MacOSVersion: capability.MacOSVersion, AppleContainerVersion: capability.AppleContainerVersion,
		},
	}
}

func startSpec(installationID, operationID string, record artifactRecord, generation uint64, statePath, socketPath string) StartSpec {
	return StartSpec{
		InstallationID: installationID, OperationID: operationID,
		ImageReference: runtimemanifest.ProductionImageRepository + "@" + record.IndexDigest,
		IndexDigest:    record.IndexDigest, ARM64Digest: record.ARM64Digest,
		StatePath: statePath, SocketPath: socketPath, Generation: generation,
		ManifestSHA256: record.ManifestSHA256,
	}
}

func maintenanceRequest(journal transactionJournal) MaintenanceRequest {
	return MaintenanceRequest{
		OperationID: journal.OperationID, InstallationID: journal.InstallationID,
		ExpectedRoutingGeneration: journal.ExpectedRoutingGeneration,
		OldManifestSHA256:         journal.OldArtifact.ManifestSHA256, NewManifestSHA256: journal.NewArtifact.ManifestSHA256,
		OldImageDigest: journal.OldArtifact.IndexDigest, NewImageDigest: journal.NewArtifact.IndexDigest,
		OldStateGeneration: journal.OldGeneration, NewStateGeneration: journal.NewGeneration,
	}
}

func activationRoutingRequest(journal transactionJournal, direction RoutingDirection) RoutingRequest {
	oldManifest, oldImage := "absent", "absent"
	oldGeneration := uint64(0)
	if journal.OldArtifact != nil {
		oldManifest = journal.OldArtifact.ManifestSHA256
		oldImage = journal.OldArtifact.IndexDigest
		oldGeneration = journal.OldGeneration
	}
	return RoutingRequest{
		Intent: RoutingIntent{
			OperationID: journal.OperationID, InstallationID: journal.InstallationID,
			OldManifestSHA256: oldManifest, NewManifestSHA256: journal.NewArtifact.ManifestSHA256,
			OldImageDigest: oldImage, NewImageDigest: journal.NewArtifact.IndexDigest,
			OldStateGeneration: oldGeneration, NewStateGeneration: journal.NewGeneration,
		},
		ExpectedOriginRoutingGeneration: journal.ExpectedRoutingGeneration,
		TargetAppleActive:               true,
		Direction:                       direction,
	}
}

func stopRoutingRequest(journal stopTransactionJournal, direction RoutingDirection) RoutingRequest {
	return RoutingRequest{
		Intent: RoutingIntent{
			OperationID: journal.OperationID, InstallationID: journal.InstallationID,
			OldManifestSHA256: journal.Artifact.ManifestSHA256, NewManifestSHA256: "absent",
			OldImageDigest: journal.Artifact.IndexDigest, NewImageDigest: "absent",
			OldStateGeneration: journal.StateGeneration,
		},
		ExpectedOriginRoutingGeneration: journal.ExpectedRoutingGeneration,
		TargetAppleActive:               false,
		Direction:                       direction,
	}
}

func summaryOf(record *artifactRecord) *ArtifactSummary {
	if record == nil {
		return nil
	}
	result := record.ArtifactSummary
	return &result
}

func cloneRecord(record *artifactRecord) *artifactRecord {
	if record == nil {
		return nil
	}
	result := *record
	return &result
}

func statusForStoppedState(state durableState) State {
	if state.ContainerID != "" {
		return StateHealthy
	}
	return StateStopped
}

func runtimeRouteMismatch(state durableState, routing RoutingSnapshot) bool {
	return routing.RuntimeRoutingPending || (state.ContainerID != "") != routing.AppleActive
}

func journalRequiresMaintenanceWitness(journal transactionJournal) bool {
	return !journal.ReuseGeneration && journal.OldArtifact != nil && journal.OldContainerID != "" && journal.Maintenance == nil
}

func mustStateDigest(store *stateStore, state durableState) string {
	digest, _ := store.digest(state)
	return digest
}

func zeroSecrets(secrets *Secrets) {
	if secrets == nil {
		return
	}
	zeroBytes(secrets.APIToken)
	zeroBytes(secrets.AdminToken)
}
