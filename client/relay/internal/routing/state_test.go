package routing

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestDecodeSchemaV1MigratesRelayToExternalBackend(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	state.Schema = legacySchemaVersion
	state.DesiredBackend = ""
	state.AppliedBackend = ""
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeState(payload, state.BoundConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != SchemaVersion || decoded.DesiredBackend != BackendExternal || decoded.AppliedBackend != BackendExternal || decoded.Phase != PhaseRelayActive {
		t.Fatalf("migrated state = %#v", decoded)
	}
}

func TestBackendPendingStatePreservesCurrentRelayAdmission(t *testing.T) {
	state := State{
		Schema:               SchemaVersion,
		Generation:           2,
		DesiredMode:          ModeRelay,
		AppliedMode:          ModeRelay,
		DesiredBackend:       BackendLocalOpenCodex,
		AppliedBackend:       BackendExternal,
		Phase:                PhaseBackendPendingRestart,
		BoundConfigPath:      filepath.Join(t.TempDir(), "relay.json"),
		BoundCodexConfigPath: filepath.Join(t.TempDir(), "config.toml"),
	}
	if !state.AllowsDataPlane() || !state.AllowsCatalog() {
		t.Fatalf("backend pending interrupted current relay traffic: %#v", state)
	}
}

func TestDecodeSchemaV2RejectsModeBackendMismatch(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// This is deliberately an explicit Local label paired with the wrong
	// legacy mode.  A damaged v2 state must park rather than silently becoming
	// External through legacy inference.
	state.Schema = explicitBackendSchemaVersion
	state.DesiredBackend = BackendLocalOpenCodex
	state.DesiredMode = ModeNative
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeState(payload, state.BoundConfigPath); err == nil {
		t.Fatal("schema v2 mode/backend mismatch was accepted")
	}
}

func TestDecodeSchemaV2PreservesExplicitNativeAndLocalBackends(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	for _, backend := range []Backend{BackendExternal, BackendLocalOpenCodex, BackendNone} {
		t.Run(string(backend), func(t *testing.T) {
			state, err := NewRelayState(configPath)
			if err != nil {
				t.Fatal(err)
			}
			state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			state.Schema = explicitBackendSchemaVersion
			state.DesiredBackend = backend
			state.AppliedBackend = backend
			state.DesiredMode = modeForBackend(backend)
			state.AppliedMode = modeForBackend(backend)
			if backend == BackendNone {
				state.Phase = PhaseNativeActive
			}
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeState(payload, state.BoundConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Schema != SchemaVersion || decoded.DesiredBackend != backend || decoded.AppliedBackend != backend ||
				decoded.DesiredMode != state.DesiredMode || decoded.AppliedMode != state.AppliedMode || decoded.Phase != state.Phase {
				t.Fatalf("schema v2 migration changed meaning: got=%#v want=%#v", decoded, state)
			}
		})
	}
}

func TestDecodeSchemaV2RejectsFutureAppleBackendLabel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	state.Schema = explicitBackendSchemaVersion
	state.DesiredBackend = BackendLocalAppleContainer
	state.AppliedBackend = BackendLocalAppleContainer
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeState(payload, state.BoundConfigPath); !errors.Is(err, ErrStateIncompatible) {
		t.Fatalf("schema v2 future Apple backend error = %v", err)
	}
}

func TestDecodeRecoveryRejectsUnboundedBackendLabels(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	state, err := NewRecoveryState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredBackend = Backend("https://credential-like.example/v1")
	state.AppliedBackend = Backend("https://credential-like.example/v1")
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeState(payload, state.BoundConfigPath); err == nil {
		t.Fatal("recovery state accepted an unbounded backend label")
	}
}
