package routing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maintenanceSchemaVersion = 1
	maxMaintenanceBytes      = 16 << 10
)

var (
	ErrMaintenanceConflict         = errors.New("runtime maintenance conflicts with current routing state")
	ErrMaintenanceRecoveryRequired = errors.New("runtime maintenance requires explicit recovery")
	ErrMaintenanceWitness          = errors.New("runtime maintenance witness is invalid")
)

// MaintenanceIntent is the non-secret half of the container-runtime journal
// that the resident relay must bind to its own routing transaction. The
// container lifecycle owner keeps its more detailed journal (including live
// container IDs); this bounded witness ties both journals together through the
// operation and installation IDs, exact manifests, image digests, and state
// generations without exposing a token, path, or arbitrary command.
type MaintenanceIntent struct {
	OperationID        string `json:"operation_id"`
	InstallationID     string `json:"installation_id"`
	OldManifestSHA256  string `json:"old_manifest_sha256"`
	NewManifestSHA256  string `json:"new_manifest_sha256"`
	OldImageDigest     string `json:"old_image_digest"`
	NewImageDigest     string `json:"new_image_digest"`
	OldStateGeneration uint64 `json:"old_state_generation"`
	NewStateGeneration uint64 `json:"new_state_generation"`
}

func (i MaintenanceIntent) validate() error {
	if !validMaintenanceID(i.OperationID) || !validMaintenanceID(i.InstallationID) ||
		!validSHA256(i.OldManifestSHA256) || !validSHA256(i.NewManifestSHA256) ||
		!validOCIDigest(i.OldImageDigest) || !validOCIDigest(i.NewImageDigest) ||
		i.OldManifestSHA256 == i.NewManifestSHA256 || i.OldImageDigest == i.NewImageDigest ||
		i.OldStateGeneration == 0 || i.OldStateGeneration == ^uint64(0) ||
		i.NewStateGeneration != i.OldStateGeneration+1 {
		return ErrMaintenanceWitness
	}
	return nil
}

// MaintenanceWitness is returned only after the relay has durably recorded
// the intent and moved routing into the fail-closed applying generation. It is
// a CAS token, not a bearer secret: the socket still requires a same-user peer
// and every use re-checks the owner-only journal and durable routing state.
type MaintenanceWitness struct {
	Schema                    int               `json:"schema"`
	Backend                   Backend           `json:"backend"`
	OriginRoutingGeneration   uint64            `json:"origin_routing_generation"`
	PreparedRoutingGeneration uint64            `json:"prepared_routing_generation"`
	FinalRoutingGeneration    uint64            `json:"final_routing_generation"`
	Intent                    MaintenanceIntent `json:"intent"`
}

func newMaintenanceWitness(generation uint64, intent MaintenanceIntent) (MaintenanceWitness, error) {
	if generation == 0 || generation > ^uint64(0)-2 || intent.validate() != nil {
		return MaintenanceWitness{}, ErrMaintenanceWitness
	}
	return MaintenanceWitness{
		Schema:                    maintenanceSchemaVersion,
		Backend:                   BackendLocalAppleContainer,
		OriginRoutingGeneration:   generation,
		PreparedRoutingGeneration: generation + 1,
		FinalRoutingGeneration:    generation + 2,
		Intent:                    intent,
	}, nil
}

func (w MaintenanceWitness) validate() error {
	if w.Schema != maintenanceSchemaVersion || w.Backend != BackendLocalAppleContainer ||
		w.OriginRoutingGeneration == 0 || w.OriginRoutingGeneration > ^uint64(0)-2 ||
		w.PreparedRoutingGeneration != w.OriginRoutingGeneration+1 ||
		w.FinalRoutingGeneration != w.PreparedRoutingGeneration+1 || w.Intent.validate() != nil {
		return ErrMaintenanceWitness
	}
	return nil
}

// MaintenanceRoutingStatus is a bounded control-socket snapshot used by the
// lifecycle manager before it creates a cross-journal CAS witness.
type MaintenanceRoutingStatus struct {
	Schema            int     `json:"schema"`
	RoutingGeneration uint64  `json:"routing_generation"`
	Backend           Backend `json:"backend"`
	Phase             Phase   `json:"phase"`
	Pending           bool    `json:"pending"`
}

func (s MaintenanceRoutingStatus) Validate() error {
	if s.Schema != maintenanceSchemaVersion || s.RoutingGeneration == 0 || !validBackend(s.Backend) {
		return ErrMaintenanceRecoveryRequired
	}
	switch s.Phase {
	case PhaseRelayActive:
		if !validRelayBackend(s.Backend) {
			return ErrMaintenanceRecoveryRequired
		}
	case PhaseNativePendingRestart, PhaseBackendPendingRestart:
		if !validRelayBackend(s.Backend) {
			return ErrMaintenanceRecoveryRequired
		}
	case PhaseRelayPendingRestart, PhaseNativeActive:
		if s.Backend != BackendNone {
			return ErrMaintenanceRecoveryRequired
		}
	case PhaseApplying, PhaseRecoveryRequired:
	default:
		return ErrMaintenanceRecoveryRequired
	}
	if s.Pending && (s.Backend != BackendLocalAppleContainer ||
		(s.Phase != PhaseRelayActive && s.Phase != PhaseApplying)) {
		return ErrMaintenanceRecoveryRequired
	}
	return nil
}

type maintenanceJournal struct {
	Schema          int                `json:"schema"`
	BoundConfigPath string             `json:"bound_config_path"`
	Witness         MaintenanceWitness `json:"witness"`
}

func (j maintenanceJournal) validate(configPath string) error {
	bound, err := canonicalConfigPath(configPath)
	if err != nil || j.Schema != maintenanceSchemaVersion || j.BoundConfigPath != bound || j.Witness.validate() != nil {
		return fmt.Errorf("%w: invalid runtime maintenance journal", ErrStateCorrupt)
	}
	return nil
}

func (j maintenanceJournal) matchesState(state State) bool {
	if j.Witness.validate() != nil || state.DesiredBackend != BackendLocalAppleContainer ||
		state.AppliedBackend != BackendLocalAppleContainer || state.DesiredMode != ModeRelay || state.AppliedMode != ModeRelay {
		return false
	}
	switch state.Generation {
	case j.Witness.OriginRoutingGeneration:
		return state.Phase == PhaseRelayActive
	case j.Witness.PreparedRoutingGeneration:
		return state.Phase == PhaseApplying
	case j.Witness.FinalRoutingGeneration:
		return state.Phase == PhaseRelayActive
	default:
		return false
	}
}

// MaintenancePath is deliberately adjacent to the routing state. Its content
// is non-secret, but its presence is a fail-closed crash witness.
func MaintenancePath(configPath string) string {
	return filepath.Clean(configPath) + ".runtime-maintenance.json"
}

func (s *Store) MaintenancePath() string {
	if s == nil {
		return ""
	}
	return MaintenancePath(s.configPath)
}

func (s *Store) loadMaintenance() (maintenanceJournal, bool, error) {
	if s == nil {
		return maintenanceJournal{}, false, errors.New("routing store is nil")
	}
	payload, err := readStateFile(s.MaintenancePath())
	if errors.Is(err, os.ErrNotExist) {
		return maintenanceJournal{}, false, nil
	}
	if err != nil {
		return maintenanceJournal{}, false, err
	}
	if len(payload) > maxMaintenanceBytes {
		return maintenanceJournal{}, false, fmt.Errorf("%w: runtime maintenance journal exceeds limit", ErrStateCorrupt)
	}
	if rejectDuplicateJSONKeys(payload) != nil {
		return maintenanceJournal{}, false, fmt.Errorf("%w: duplicate runtime maintenance journal key", ErrStateCorrupt)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var journal maintenanceJournal
	if err := decoder.Decode(&journal); err != nil {
		return maintenanceJournal{}, false, fmt.Errorf("%w: decode runtime maintenance journal", ErrStateCorrupt)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return maintenanceJournal{}, false, fmt.Errorf("%w: trailing runtime maintenance journal data", ErrStateCorrupt)
	}
	if err := journal.validate(s.configPath); err != nil {
		return maintenanceJournal{}, false, err
	}
	return journal, true, nil
}

// HasPendingMaintenance validates the owner, mode, strict JSON shape, and
// config binding before reporting a crash witness to a watcher/controller.
func (s *Store) HasPendingMaintenance() (bool, error) {
	_, found, err := s.loadMaintenance()
	return found, err
}

// MaintenanceRecoveryState returns the exact durable Apple routing state that
// a resident relay may reconstruct behind a parked admission gate. It accepts
// all three crash-safe journal positions (origin, prepared, or final), but
// never guesses through a malformed witness or a competing routing journal.
// The returned state contains no runtime path, container identifier, or
// credential.
func (s *Store) MaintenanceRecoveryState() (State, bool, error) {
	if s == nil {
		return State{}, false, ErrMaintenanceRecoveryRequired
	}
	state, legacy, err := s.Read()
	if err != nil || legacy {
		return State{}, false, ErrMaintenanceRecoveryRequired
	}
	if _, runtimeRoutingFound, runtimeRoutingErr := s.loadRuntimeRouting(state.BoundCodexConfigPath); runtimeRoutingErr != nil || runtimeRoutingFound {
		return State{}, false, ErrMaintenanceRecoveryRequired
	}
	journal, found, err := s.loadMaintenance()
	if err != nil {
		return State{}, false, ErrMaintenanceRecoveryRequired
	}
	if !found {
		return State{}, false, nil
	}
	if pending, pendingErr := s.HasPendingTransaction(); pendingErr != nil || pending || !journal.matchesState(state) {
		return State{}, false, ErrMaintenanceRecoveryRequired
	}
	return state, true, nil
}

func (s *Store) writeMaintenance(journal maintenanceJournal) error {
	if err := journal.validate(s.configPath); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeControlFileAtomically(s.MaintenancePath(), ".runtime-maintenance.", payload)
}

func (s *Store) removeMaintenance() error {
	path := s.MaintenancePath()
	if err := validateExistingControlFile(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove runtime maintenance journal: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

// MaintenanceRuntime is the resident relay's in-memory half of the
// transaction. Prepare must synchronously block both listeners, cancel the
// catalog worker, and drain active requests. Verify must prove the selected
// old/new Apple runtime is ready while admission remains parked. Resume is a
// no-fail in-memory commit performed only after the final routing state and
// journal removal are durable.
type MaintenanceRuntime interface {
	Prepare(context.Context) error
	Verify(context.Context, ControlOperation) error
	Resume()
}

// MaintenanceCoordinator binds the durable routing state to the in-memory
// resident relay. It never owns or invokes Apple Container itself.
type MaintenanceCoordinator struct {
	store   *Store
	watcher *Watcher
	runtime MaintenanceRuntime
}

func NewMaintenanceCoordinator(configPath string, watcher *Watcher, runtime MaintenanceRuntime) (*MaintenanceCoordinator, error) {
	if watcher == nil || runtime == nil {
		return nil, ErrControlRequest
	}
	store, err := Open(configPath)
	if err != nil {
		return nil, err
	}
	return &MaintenanceCoordinator{store: store, watcher: watcher, runtime: runtime}, nil
}

// Handle maps the finite maintenance operations onto the coordinator and
// emits only bounded receipts. The resident relay may dispatch Apply to its
// existing runtime switcher and every other operation to this method.
func (c *MaintenanceCoordinator) Handle(ctx context.Context, request ControlRequest) (ControlResponse, error) {
	if request.validate() != nil || request.Operation == ControlOperationApply {
		return ControlResponse{}, ErrControlRequest
	}
	switch request.Operation {
	case ControlOperationMaintenanceStatus:
		status, err := c.Status(ctx)
		if err != nil {
			return ControlResponse{}, err
		}
		return ControlResponse{
			OK:          true,
			Generation:  status.RoutingGeneration,
			Backend:     status.Backend,
			Maintenance: &status,
		}, nil
	case ControlOperationMaintenancePrepare:
		witness, err := c.Prepare(ctx, request.Generation, *request.Intent)
		if err != nil {
			return ControlResponse{}, err
		}
		return ControlResponse{
			OK:         true,
			Generation: witness.PreparedRoutingGeneration,
			Backend:    BackendLocalAppleContainer,
			Witness:    &witness,
		}, nil
	case ControlOperationMaintenanceCommit:
		if err := c.Commit(ctx, *request.Witness); err != nil {
			return ControlResponse{}, err
		}
	case ControlOperationMaintenanceRollback:
		if err := c.Rollback(ctx, *request.Witness); err != nil {
			return ControlResponse{}, err
		}
	default:
		return ControlResponse{}, ErrControlRequest
	}
	return ControlResponse{
		OK:         true,
		Generation: request.Witness.FinalRoutingGeneration,
		Backend:    BackendLocalAppleContainer,
	}, nil
}

func (c *MaintenanceCoordinator) Status(ctx context.Context) (MaintenanceRoutingStatus, error) {
	if c == nil || c.store == nil {
		return MaintenanceRoutingStatus{}, ErrControlUnavailable
	}
	lock, err := c.store.ReadLock(ctx)
	if err != nil {
		return MaintenanceRoutingStatus{}, ErrControlUnavailable
	}
	defer lock.Close()
	state, legacy, err := c.store.Read()
	if err != nil || legacy || state.ValidateFor(c.store.ConfigPath()) != nil {
		return MaintenanceRoutingStatus{}, ErrMaintenanceRecoveryRequired
	}
	if _, _, runtimeRoutingErr := c.store.loadRuntimeRouting(state.BoundCodexConfigPath); runtimeRoutingErr != nil {
		return MaintenanceRoutingStatus{}, ErrMaintenanceRecoveryRequired
	}
	journal, pending, err := c.store.loadMaintenance()
	if err != nil {
		return MaintenanceRoutingStatus{}, ErrMaintenanceRecoveryRequired
	}
	if pending && !journal.matchesState(state) {
		return MaintenanceRoutingStatus{}, ErrMaintenanceRecoveryRequired
	}
	return MaintenanceRoutingStatus{
		Schema:            maintenanceSchemaVersion,
		RoutingGeneration: state.Generation,
		Backend:           state.AppliedBackend,
		Phase:             state.Phase,
		Pending:           pending,
	}, nil
}

func (c *MaintenanceCoordinator) Prepare(ctx context.Context, generation uint64, intent MaintenanceIntent) (MaintenanceWitness, error) {
	if c == nil || c.store == nil || c.runtime == nil || generation == 0 || intent.validate() != nil {
		return MaintenanceWitness{}, ErrMaintenanceWitness
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return MaintenanceWitness{}, ErrControlUnavailable
	}
	defer lock.Close()
	if pending, pendingErr := c.store.HasPendingTransaction(); pendingErr != nil || pending {
		return MaintenanceWitness{}, ErrMaintenanceConflict
	}
	existing, pending, pendingErr := c.store.loadMaintenance()
	if pendingErr != nil {
		return MaintenanceWitness{}, ErrMaintenanceRecoveryRequired
	}
	state, legacy, err := c.store.Read()
	if err != nil || legacy {
		return MaintenanceWitness{}, ErrMaintenanceRecoveryRequired
	}
	if _, runtimeRoutingFound, runtimeRoutingErr := c.store.loadRuntimeRouting(state.BoundCodexConfigPath); runtimeRoutingErr != nil || runtimeRoutingFound {
		return MaintenanceWitness{}, ErrMaintenanceConflict
	}
	if pending {
		witness := existing.Witness
		if witness.OriginRoutingGeneration != generation || witness.Intent != intent || !existing.matchesState(state) {
			return MaintenanceWitness{}, ErrMaintenanceConflict
		}
		if state.Generation == witness.OriginRoutingGeneration {
			applying := state
			applying.Generation = witness.PreparedRoutingGeneration
			applying.Phase = PhaseApplying
			if err := lock.Save(applying); err != nil {
				return witness, ErrMaintenanceRecoveryRequired
			}
		}
		c.watcher.Refresh()
		if err := c.runtime.Prepare(ctx); err != nil {
			return witness, ErrMaintenanceRecoveryRequired
		}
		return witness, nil
	}
	if !stableAppleState(state) || state.Generation != generation {
		return MaintenanceWitness{}, ErrMaintenanceConflict
	}
	witness, err := newMaintenanceWitness(generation, intent)
	if err != nil {
		return MaintenanceWitness{}, err
	}
	journal := maintenanceJournal{Schema: maintenanceSchemaVersion, BoundConfigPath: c.store.ConfigPath(), Witness: witness}
	// The journal is durable before routing is parked. If the following state
	// write or process crashes, its mere presence keeps restart fail-closed.
	if err := c.store.writeMaintenance(journal); err != nil {
		return MaintenanceWitness{}, err
	}
	applying := state
	applying.Generation = witness.PreparedRoutingGeneration
	applying.Phase = PhaseApplying
	if err := lock.Save(applying); err != nil {
		c.watcher.Refresh()
		return witness, ErrMaintenanceRecoveryRequired
	}
	c.watcher.Refresh()
	if err := c.runtime.Prepare(ctx); err != nil {
		return witness, ErrMaintenanceRecoveryRequired
	}
	return witness, nil
}

func (c *MaintenanceCoordinator) Commit(ctx context.Context, witness MaintenanceWitness) error {
	return c.finish(ctx, ControlOperationMaintenanceCommit, witness)
}

func (c *MaintenanceCoordinator) Rollback(ctx context.Context, witness MaintenanceWitness) error {
	return c.finish(ctx, ControlOperationMaintenanceRollback, witness)
}

func (c *MaintenanceCoordinator) finish(ctx context.Context, operation ControlOperation, witness MaintenanceWitness) error {
	if c == nil || c.store == nil || c.runtime == nil || witness.validate() != nil ||
		(operation != ControlOperationMaintenanceCommit && operation != ControlOperationMaintenanceRollback) {
		return ErrMaintenanceWitness
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return ErrControlUnavailable
	}
	defer lock.Close()
	journal, found, err := c.store.loadMaintenance()
	if err != nil || !found {
		return ErrMaintenanceRecoveryRequired
	}
	if journal.Witness != witness {
		return ErrMaintenanceWitness
	}
	state, legacy, err := c.store.Read()
	if err != nil || legacy || !journal.matchesState(state) {
		return ErrMaintenanceRecoveryRequired
	}
	if state.Generation == witness.OriginRoutingGeneration {
		applying := state
		applying.Generation = witness.PreparedRoutingGeneration
		applying.Phase = PhaseApplying
		if err := lock.Save(applying); err != nil {
			return ErrMaintenanceRecoveryRequired
		}
		state = applying
	}
	c.watcher.Refresh()
	// Prepare is idempotent after a crash or a failed drain. It must finish the
	// fail-closed drain before either endpoint can be accepted for resumption.
	if err := c.runtime.Prepare(ctx); err != nil {
		return ErrMaintenanceRecoveryRequired
	}
	if err := c.runtime.Verify(ctx, operation); err != nil {
		return ErrMaintenanceRecoveryRequired
	}
	if state.Generation == witness.PreparedRoutingGeneration {
		final := state
		final.Generation = witness.FinalRoutingGeneration
		final.Phase = PhaseRelayActive
		if err := lock.Save(final); err != nil {
			return ErrMaintenanceRecoveryRequired
		}
	}
	// Remove the crash witness only after the selected container endpoint has
	// been verified and the final stable state is durable. Resume cannot fail;
	// a crash after removal therefore safely reconstructs the same stable Apple
	// backend on process startup.
	if err := c.store.removeMaintenance(); err != nil {
		return ErrMaintenanceRecoveryRequired
	}
	c.watcher.Refresh()
	c.runtime.Resume()
	return nil
}

func stableAppleState(state State) bool {
	return state.Phase == PhaseRelayActive && state.DesiredMode == ModeRelay && state.AppliedMode == ModeRelay &&
		state.DesiredBackend == BackendLocalAppleContainer && state.AppliedBackend == BackendLocalAppleContainer
}

func validMaintenanceID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	return len(value) == sha256.Size*2 && isLowerHex(value)
}

func validOCIDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256(strings.TrimPrefix(value, "sha256:"))
}
