// Package routing owns the durable, non-secret routing state used to park a
// resident relay while Codex is switched back to its native OpenAI backend.
//
// The state is deliberately separate from relay.json.  relay.json describes
// the relay's fixed upstream topology; this package describes whether that
// resident relay is currently admitted to carry data-plane traffic.
package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// SchemaVersion 3 adds a durable Apple Container backend while preserving
	// every v1/v2 backend meaning. The legacy relay mode remains in the wire
	// state for compatibility, but it is not sufficient to distinguish the
	// external, host-native, and Apple Container relay profiles.
	SchemaVersion                = 3
	legacySchemaVersion          = 1
	explicitBackendSchemaVersion = 2
)

const (
	maxStateBytes             = 64 << 10
	initializedMarkerContents = "opencodex-relay-routing-initialized-v1\n"
)

type Mode string

const (
	ModeUnknown Mode = "unknown"
	ModeRelay   Mode = "relay"
	ModeNative  Mode = "native"
)

// Backend is the durable upstream selection. It deliberately contains only a
// bounded label, never an endpoint or credential source. `none` means Codex
// owns native routing because the relay-owned override is absent.
type Backend string

const (
	BackendUnknown             Backend = "unknown"
	BackendExternal            Backend = "external"
	BackendLocalOpenCodex      Backend = "local_opencodex"
	BackendLocalAppleContainer Backend = "local_apple_container"
	BackendNone                Backend = "none"
)

type Phase string

const (
	PhaseRelayActive           Phase = "relay_active"
	PhaseNativePendingRestart  Phase = "native_pending_restart"
	PhaseRelayPendingRestart   Phase = "relay_pending_restart"
	PhaseBackendPendingRestart Phase = "backend_pending_restart"
	PhaseApplying              Phase = "applying"
	PhaseNativeActive          Phase = "native_active"
	PhaseRecoveryRequired      Phase = "recovery_required"
)

var (
	ErrStateCorrupt      = errors.New("routing state is corrupt")
	ErrStateIncompatible = errors.New("routing state is incompatible")
	ErrStateBinding      = errors.New("routing state is bound to another relay config")
	ErrStateTransition   = errors.New("routing state transition is invalid")
	ErrStateGeneration   = errors.New("routing state generation is stale")
)

// State contains no credentials, upstream URL, or Codex configuration
// contents. BoundConfigPath prevents a state file created for one relay JSON
// from changing admission for another relay instance.
type State struct {
	Schema         int     `json:"schema"`
	Generation     uint64  `json:"generation"`
	DesiredMode    Mode    `json:"desired_mode"`
	AppliedMode    Mode    `json:"applied_mode"`
	DesiredBackend Backend `json:"desired_backend"`
	AppliedBackend Backend `json:"applied_backend"`
	Phase          Phase   `json:"phase"`
	// KnownLegacyBackupAndMigrate is an explicit one-shot user intent. It never
	// contains a path or TOML value and is cleared when the transition is
	// cancelled or committed.
	KnownLegacyBackupAndMigrate bool   `json:"known_legacy_backup_and_migrate,omitempty"`
	BoundConfigPath             string `json:"bound_config_path"`
	// BoundCodexConfigPath is retained for the controller only. The resident
	// relay validates that it is a canonical path but never reads, logs, or
	// exposes it through health.
	BoundCodexConfigPath string `json:"bound_codex_config_path"`
}

// AllowsDataPlane is intentionally true during native_pending_restart: merely
// requesting a native switch must not interrupt the user's current relay task.
func (s State) AllowsDataPlane() bool {
	return s.Phase == PhaseRelayActive || s.Phase == PhaseNativePendingRestart || s.Phase == PhaseBackendPendingRestart
}

// AllowsCatalog follows data-plane admission. Applying, parked-native, and
// recovery states must not perform credential lookup or remote catalog egress.
func (s State) AllowsCatalog() bool { return s.AllowsDataPlane() }

func (s State) IsRecoveryRequired() bool { return s.Phase == PhaseRecoveryRequired }

func StatePath(configPath string) string {
	return filepath.Clean(configPath) + ".routing-state.json"
}

// InitializedPath is a non-secret, owner-only sentinel written before the
// first durable state. It distinguishes an untouched legacy installation from
// a deleted state file: once a controller has taken ownership, a missing state
// must park the resident relay instead of silently reopening remote traffic.
func InitializedPath(configPath string) string {
	return filepath.Clean(configPath) + ".routing-initialized"
}

func canonicalConfigPath(configPath string) (string, error) {
	if configPath == "" {
		return "", fmt.Errorf("%w: relay config path is empty", ErrStateBinding)
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve relay config path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func canonicalCodexConfigPath(configPath string) (string, error) {
	if configPath == "" {
		return "", fmt.Errorf("%w: Codex config path is empty", ErrStateBinding)
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve Codex config path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func NewRelayState(configPath string) (State, error) {
	bound, err := canonicalConfigPath(configPath)
	if err != nil {
		return State{}, err
	}
	return State{
		Schema:          SchemaVersion,
		Generation:      1,
		DesiredMode:     ModeRelay,
		AppliedMode:     ModeRelay,
		DesiredBackend:  BackendExternal,
		AppliedBackend:  BackendExternal,
		Phase:           PhaseRelayActive,
		BoundConfigPath: bound,
	}, nil
}

// BindCodexConfig binds a controller-created state to the exact user config
// that it is allowed to edit. Missing legacy state is intentionally unbound
// until the controller supplies this value before its first Save.
func BindCodexConfig(state State, codexConfigPath string) (State, error) {
	bound, err := canonicalCodexConfigPath(codexConfigPath)
	if err != nil {
		return State{}, err
	}
	state.BoundCodexConfigPath = bound
	return state, nil
}

// NewRecoveryState is only used as an in-memory, fail-closed watcher snapshot
// after a state read/validation failure. It must not be persisted implicitly:
// the controller needs an explicit recover command to replace corrupt state.
func NewRecoveryState(configPath string) (State, error) {
	bound, err := canonicalConfigPath(configPath)
	if err != nil {
		return State{}, err
	}
	return State{
		Schema:          SchemaVersion,
		Generation:      1,
		DesiredMode:     ModeUnknown,
		AppliedMode:     ModeUnknown,
		DesiredBackend:  BackendUnknown,
		AppliedBackend:  BackendUnknown,
		Phase:           PhaseRecoveryRequired,
		BoundConfigPath: bound,
	}, nil
}

func (s State) ValidateFor(configPath string) error {
	bound, err := canonicalConfigPath(configPath)
	if err != nil {
		return err
	}
	return s.validateForBound(bound)
}

// ValidateForCodexConfig is the controller-side validation used before any
// TOML mutation. The relay process deliberately cannot infer this path, so it
// only calls ValidateFor when reading the resident state file.
func (s State) ValidateForCodexConfig(configPath, codexConfigPath string) error {
	if err := s.ValidateFor(configPath); err != nil {
		return err
	}
	bound, err := canonicalCodexConfigPath(codexConfigPath)
	if err != nil {
		return err
	}
	if s.BoundCodexConfigPath != bound {
		return fmt.Errorf("%w: state=%q Codex=%q", ErrStateBinding, s.BoundCodexConfigPath, bound)
	}
	return nil
}

func (s State) validateForBound(bound string) error {
	if s.Schema != SchemaVersion {
		return fmt.Errorf("%w: schema %d", ErrStateIncompatible, s.Schema)
	}
	if s.Generation == 0 {
		return fmt.Errorf("%w: generation must be positive", ErrStateCorrupt)
	}
	if s.BoundConfigPath != bound {
		return fmt.Errorf("%w: state=%q relay=%q", ErrStateBinding, s.BoundConfigPath, bound)
	}
	canonicalCodex, err := canonicalCodexConfigPath(s.BoundCodexConfigPath)
	if err != nil {
		return err
	}
	if s.BoundCodexConfigPath != canonicalCodex {
		return fmt.Errorf("%w: Codex config path is not canonical", ErrStateBinding)
	}
	if modeForBackend(s.DesiredBackend) != s.DesiredMode || modeForBackend(s.AppliedBackend) != s.AppliedMode {
		return fmt.Errorf("%w: mode/backend mismatch", ErrStateCorrupt)
	}
	if s.KnownLegacyBackupAndMigrate && s.DesiredBackend != BackendExternal {
		return fmt.Errorf("%w: legacy migration intent requires External", ErrStateCorrupt)
	}
	switch s.Phase {
	case PhaseRelayActive:
		if s.DesiredMode == ModeRelay && s.AppliedMode == ModeRelay && validRelayBackend(s.DesiredBackend) && s.DesiredBackend == s.AppliedBackend {
			return nil
		}
	case PhaseNativePendingRestart:
		if s.DesiredBackend == BackendNone && validRelayBackend(s.AppliedBackend) {
			return nil
		}
	case PhaseRelayPendingRestart:
		if validRelayBackend(s.DesiredBackend) && s.AppliedBackend == BackendNone {
			return nil
		}
	case PhaseBackendPendingRestart:
		if validRelayBackend(s.DesiredBackend) && validRelayBackend(s.AppliedBackend) && s.DesiredBackend != s.AppliedBackend {
			return nil
		}
	case PhaseApplying:
		if s.DesiredBackend != s.AppliedBackend && validBackend(s.DesiredBackend) && validBackend(s.AppliedBackend) {
			return nil
		}
		// A gateway URL reload rebuilds the immutable External runtime without
		// changing the selected backend. The adjacent validated transaction
		// journal distinguishes this path from an ordinary backend switch.
		if (s.DesiredBackend == BackendExternal && s.AppliedBackend == BackendExternal) ||
			(s.DesiredBackend == BackendLocalAppleContainer && s.AppliedBackend == BackendLocalAppleContainer) {
			return nil
		}
	case PhaseNativeActive:
		if s.DesiredBackend == BackendNone && s.AppliedBackend == BackendNone {
			return nil
		}
	case PhaseRecoveryRequired:
		if validRecoveryMode(s.DesiredMode) && validRecoveryMode(s.AppliedMode) &&
			validRecoveryBackend(s.DesiredBackend) && validRecoveryBackend(s.AppliedBackend) {
			return nil
		}
	default:
		return fmt.Errorf("%w: phase %q", ErrStateIncompatible, s.Phase)
	}
	return fmt.Errorf("%w: phase=%q desired=%q applied=%q", ErrStateCorrupt, s.Phase, s.DesiredMode, s.AppliedMode)
}

func validRecoveryMode(mode Mode) bool {
	return mode == ModeUnknown || mode == ModeRelay || mode == ModeNative
}

func validRecoveryBackend(backend Backend) bool {
	return backend == BackendUnknown || validBackend(backend)
}

func validBackend(backend Backend) bool {
	return backend == BackendExternal || backend == BackendLocalOpenCodex || backend == BackendLocalAppleContainer || backend == BackendNone
}

func validRelayBackend(backend Backend) bool {
	return backend == BackendExternal || backend == BackendLocalOpenCodex || backend == BackendLocalAppleContainer
}

func modeForBackend(backend Backend) Mode {
	switch backend {
	case BackendExternal, BackendLocalOpenCodex, BackendLocalAppleContainer:
		return ModeRelay
	case BackendNone:
		return ModeNative
	default:
		return ModeUnknown
	}
}

func decodeState(payload []byte, bound string) (State, error) {
	if len(payload) > maxStateBytes {
		return State{}, fmt.Errorf("%w: file exceeds %d bytes", ErrStateCorrupt, maxStateBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("%w: decode JSON: %v", ErrStateCorrupt, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return State{}, fmt.Errorf("%w: multiple JSON values", ErrStateCorrupt)
		}
		return State{}, fmt.Errorf("%w: trailing JSON: %v", ErrStateCorrupt, err)
	}
	if state.Schema == legacySchemaVersion {
		state = migrateSchemaV1(state)
	} else if state.Schema == explicitBackendSchemaVersion {
		// Schema v2 predates the Apple Container backend. Preserve only labels
		// that a genuine v2 writer could have emitted; otherwise a crafted or
		// corrupt old file could opt into a newly privileged local profile merely
		// by being decoded by newer software.
		if !validSchemaV2Backend(state.DesiredBackend) || !validSchemaV2Backend(state.AppliedBackend) {
			return State{}, fmt.Errorf("%w: schema v2 contains a future backend", ErrStateIncompatible)
		}
		state.Schema = SchemaVersion
	}
	// A persisted v2 file must carry its own explicit backend labels.  Do not
	// repair a mode/backend disagreement from disk by inferring the old
	// External label: that could turn a damaged Local selection into External
	// admission after restart.  In-memory compatibility callers are still
	// normalized by Lock.Save/Replace before they become durable.
	if err := state.validateForBound(bound); err != nil {
		return State{}, err
	}
	return state, nil
}

// migrateSchemaV1 converts only the bounded legacy mode labels. A v1 relay
// deployment had exactly one relay profile, which is the canonical External
// profile in v2; no URL or configuration data is inferred or copied here.
func migrateSchemaV1(state State) State {
	if state.Schema != legacySchemaVersion {
		return state
	}
	state.Schema = SchemaVersion
	state.DesiredBackend = backendForLegacyMode(state.DesiredMode)
	state.AppliedBackend = backendForLegacyMode(state.AppliedMode)
	return state
}

// normalizeStateBackends accepts only a temporary in-memory State whose
// backend fields are absent.  It must never overwrite an explicitly supplied
// backend just because it disagrees with the mode: that disagreement is a
// corrupt durable v2 state and must fail closed during validation.
func normalizeStateBackends(state State) State {
	if state.DesiredBackend == "" {
		state.DesiredBackend = backendForLegacyMode(state.DesiredMode)
	}
	if state.AppliedBackend == "" {
		state.AppliedBackend = backendForLegacyMode(state.AppliedMode)
	}
	return state
}

func backendForLegacyMode(mode Mode) Backend {
	switch mode {
	case ModeRelay:
		return BackendExternal
	case ModeNative:
		return BackendNone
	default:
		return BackendUnknown
	}
}

func validSchemaV2Backend(backend Backend) bool {
	return backend == BackendUnknown || backend == BackendExternal || backend == BackendLocalOpenCodex || backend == BackendNone
}

func encodeState(state State) ([]byte, error) {
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode routing state: %w", err)
	}
	return append(payload, '\n'), nil
}

func writeStateAtomically(path string, state State) error {
	state = normalizeStateBackends(state)
	payload, err := encodeState(state)
	if err != nil {
		return err
	}
	return writeControlFileAtomically(path, ".routing-state.", payload)
}

func writeInitializedMarker(path string) error {
	present, err := initializedMarkerPresent(path)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	return writeControlFileAtomically(path, ".routing-initialized.", []byte(initializedMarkerContents))
}

func initializedMarkerPresent(path string) (bool, error) {
	payload, err := readStateFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read routing initialization marker: %w", err)
	}
	if string(payload) != initializedMarkerContents {
		return false, fmt.Errorf("%w: routing initialization marker is invalid", ErrStateCorrupt)
	}
	return true, nil
}

func writeControlFileAtomically(path, temporaryPrefix string, payload []byte) error {
	if err := validateExistingControlFile(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create routing state directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), temporaryPrefix)
	if err != nil {
		return fmt.Errorf("create temporary routing state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary routing state: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write routing state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync routing state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close routing state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace routing state: %w", err)
	}
	// A directory sync makes the rename durable on the macOS/Linux filesystems
	// supported by this relay. Some filesystems do not support it; the atomic
	// rename has still completed, so retain availability rather than failing a
	// completed state transition for that platform limitation.
	directory, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
