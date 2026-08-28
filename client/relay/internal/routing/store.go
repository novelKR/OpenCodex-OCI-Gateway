package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store is bound permanently to one canonical relay JSON path. A separate
// Store for another config cannot accidentally consume this state file.
type Store struct {
	configPath      string
	statePath       string
	initializedPath string
	lockPath        string
}

func Open(configPath string) (*Store, error) {
	bound, err := canonicalConfigPath(configPath)
	if err != nil {
		return nil, err
	}
	return &Store{
		configPath:      bound,
		statePath:       StatePath(bound),
		initializedPath: InitializedPath(bound),
		lockPath:        bound + ".routing.lock",
	}, nil
}

func (s *Store) ConfigPath() string { return s.configPath }
func (s *Store) StatePath() string  { return s.statePath }
func (s *Store) InitializedPath() string {
	if s == nil {
		return ""
	}
	return s.initializedPath
}

// Read returns legacy=true when no routing-state file exists. The legacy
// contract is intentionally relay_active so existing deployments keep working
// until a controller explicitly requests a native mode change.
func (s *Store) Read() (state State, legacy bool, err error) {
	payload, err := readStateFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		initialized, initializedErr := initializedMarkerPresent(s.initializedPath)
		if initializedErr != nil {
			return State{}, false, initializedErr
		}
		if initialized {
			return State{}, false, fmt.Errorf("%w: routing state is missing after initialization", ErrStateCorrupt)
		}
		state, legacyErr := NewRelayState(s.configPath)
		return state, true, legacyErr
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read routing state: %w", err)
	}
	state, err = decodeState(payload, s.configPath)
	if err != nil {
		return State{}, false, err
	}
	return state, false, nil
}

func (s *Store) Load() (State, error) {
	state, _, err := s.Read()
	return state, err
}

// Lock serializes every writer across the MenuBar helper, relayctl, and any
// recovery process. Hold it while changing both Codex TOML and routing state.
func (s *Store) Lock(ctx context.Context) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create routing lock directory: %w", err)
	}
	file, err := lockFile(ctx, s.lockPath)
	if err != nil {
		return nil, err
	}
	return &Lock{store: s, file: file}, nil
}

// ReadLock serializes a read-only proof against controller mutations without
// creating a directory, lock file, or changing its permissions. A valid
// durable state should already have a writer-created lock artifact; if that
// artifact is absent or unsafe, callers must fail closed instead of repairing
// it as part of a supposedly read-only check.
//
// The lock is exclusive rather than shared because existing controller writers
// use the same advisory lock protocol. It exposes no write operation unless a
// caller deliberately invokes Lock.Save/Replace, which read-only callers do
// not do.
func (s *Store) ReadLock(ctx context.Context) (*Lock, error) {
	if s == nil {
		return nil, errors.New("routing store is nil")
	}
	file, err := lockExistingFile(ctx, s.lockPath)
	if err != nil {
		return nil, err
	}
	return &Lock{store: s, file: file}, nil
}

type Lock struct {
	store  *Store
	file   *os.File
	closed bool
}

func (l *Lock) Load() (State, error) { return l.store.Load() }

// Save checks the canonical binding, state-machine transition, and generation
// before atomically replacing the state file. It is intentionally unsuitable
// for repairing a corrupt file; use Replace only after an explicit recovery
// command has made that authority visible to the user.
func (l *Lock) Save(next State) error {
	if l == nil || l.closed {
		return errors.New("routing lock is closed")
	}
	next = normalizeStateBackends(next)
	if err := next.ValidateFor(l.store.configPath); err != nil {
		return err
	}
	previous, legacy, err := l.store.Read()
	if err != nil {
		return fmt.Errorf("read prior routing state before save: %w", err)
	}
	if !legacy {
		if next.Generation <= previous.Generation {
			return fmt.Errorf("%w: previous=%d next=%d", ErrStateGeneration, previous.Generation, next.Generation)
		}
		if err := validateTransition(previous, next); err != nil {
			return err
		}
	}
	if err := writeInitializedMarker(l.store.initializedPath); err != nil {
		return err
	}
	return writeStateAtomically(l.store.statePath, next)
}

// Replace is deliberately explicit because it bypasses a corrupt prior state.
// It still validates the new state's config binding and shape. Callers must use
// it only from an operator-selected recovery path.
func (l *Lock) Replace(next State) error {
	if l == nil || l.closed {
		return errors.New("routing lock is closed")
	}
	next = normalizeStateBackends(next)
	if err := next.ValidateFor(l.store.configPath); err != nil {
		return err
	}
	if err := writeInitializedMarker(l.store.initializedPath); err != nil {
		return err
	}
	return writeStateAtomically(l.store.statePath, next)
}

func (l *Lock) Close() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	return unlockFile(l.file)
}

func validateTransition(previous, next State) error {
	if next.Phase == PhaseRecoveryRequired {
		return nil
	}
	if previous.Phase == PhaseRecoveryRequired {
		return fmt.Errorf("%w: explicit recovery must replace recovery_required", ErrStateTransition)
	}
	valid := false
	switch previous.Phase {
	case PhaseRelayActive:
		valid = next.Phase == PhaseRelayActive || next.Phase == PhaseNativePendingRestart ||
			next.Phase == PhaseBackendPendingRestart || next.Phase == PhaseApplying
	case PhaseNativePendingRestart:
		valid = next.Phase == PhaseNativePendingRestart || next.Phase == PhaseRelayActive || next.Phase == PhaseBackendPendingRestart || next.Phase == PhaseApplying
	case PhaseRelayPendingRestart:
		valid = next.Phase == PhaseRelayPendingRestart || next.Phase == PhaseNativeActive || next.Phase == PhaseApplying
	case PhaseBackendPendingRestart:
		valid = next.Phase == PhaseBackendPendingRestart || next.Phase == PhaseRelayActive || next.Phase == PhaseNativePendingRestart || next.Phase == PhaseApplying
	case PhaseApplying:
		valid = (previous.DesiredBackend == BackendNone && next.Phase == PhaseNativeActive) ||
			(validRelayBackend(previous.DesiredBackend) && next.Phase == PhaseRelayActive)
	case PhaseNativeActive:
		valid = next.Phase == PhaseNativeActive || next.Phase == PhaseRelayPendingRestart
	}
	if !valid {
		return fmt.Errorf("%w: %s to %s", ErrStateTransition, previous.Phase, next.Phase)
	}
	return nil
}
