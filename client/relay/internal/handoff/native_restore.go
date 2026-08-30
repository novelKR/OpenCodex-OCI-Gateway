package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	maxNativeRestoreOutputBytes = 256 << 10
	maxNativeOwnerConfigBytes   = 1 << 20
)

var (
	ErrNativeRestoreOutput             = errors.New("OpenCodex native restore returned an invalid bounded result")
	ErrNativeRestoreFailed             = errors.New("OpenCodex native restore did not restore its Codex configuration")
	ErrNativeOwnerConfigurationInvalid = errors.New("OpenCodex owner configuration is invalid")
)

type NativeRestoreOutcome string

const (
	NativeRestoreApplied             NativeRestoreOutcome = "applied"
	NativeRestoreAlreadyNative       NativeRestoreOutcome = "already_native"
	NativeRestoreRetryableNoMutation NativeRestoreOutcome = "retryable_no_mutation"
)

// NativeRestoreResult reports only bounded owner results. No OpenCodex message,
// path, stdout, stderr, or user configuration enters this boundary.
type NativeRestoreResult struct {
	Outcome           NativeRestoreOutcome
	NonRoutingWarning bool
}

type NativeOwnerConfiguration string
type NativeOwnerIntegration string
type NativeOwnerInspectionReason string

const (
	NativeOwnerConfigurationValid       NativeOwnerConfiguration = "valid"
	NativeOwnerConfigurationInvalid     NativeOwnerConfiguration = "invalid"
	NativeOwnerConfigurationUnavailable NativeOwnerConfiguration = "unavailable"

	NativeOwnerIntegrationEnabled  NativeOwnerIntegration = "enabled"
	NativeOwnerIntegrationDisabled NativeOwnerIntegration = "disabled"
	NativeOwnerIntegrationUnknown  NativeOwnerIntegration = "unknown"

	NativeOwnerReady                      NativeOwnerInspectionReason = "owner_ready"
	NativeOwnerConfigurationInvalidReason NativeOwnerInspectionReason = "owner_configuration_invalid"
	NativeOwnerProbeUnavailable           NativeOwnerInspectionReason = "owner_probe_unavailable"
)

// NativeOwnerInspection is the complete value-free projection returned to the
// local-development control center. Raw OpenCodex config, messages, paths, and
// command output never cross this boundary.
type NativeOwnerInspection struct {
	SchemaVersion int                         `json:"schema_version"`
	Owner         string                      `json:"owner"`
	Configuration NativeOwnerConfiguration    `json:"configuration"`
	Integration   NativeOwnerIntegration      `json:"integration"`
	Reason        NativeOwnerInspectionReason `json:"reason"`
}

// NativeRestoreOutputRunner is injectable so tests can validate the exact
// owner command without executing a package manager or OpenCodex process.
type NativeRestoreOutputRunner interface {
	RunOutput(context.Context, string, []string, ...string) ([]byte, bool, error)
}

type boundedCommandOutputRunner struct{}

// RunOutput returns nonZero only for a normal child exit with a non-zero code.
// Timeouts, launch failures, cancellation, and oversized stdout remain errors
// even if a prefix happened to contain valid JSON.
func (boundedCommandOutputRunner) RunOutput(
	ctx context.Context,
	program string,
	environment []string,
	args ...string,
) ([]byte, bool, error) {
	result, err := (boundedRemovalProcess{}).Run(
		ctx, program, args, environment, maxNativeRestoreOutputBytes,
	)
	if err != nil || !result.Started || !result.CleanupVerified {
		return nil, false, ErrNativeRestoreOutput
	}
	return result.Output, result.ExitCode != 0, nil
}

// NativeRestoreExecutor invokes only the verified Bun and OpenCodex package
// closure copied into a private immutable snapshot. The selected env-node
// launcher remains an installation identity witness but is never executed.
type NativeRestoreExecutor struct {
	Runner   NativeRestoreOutputRunner
	Platform string
	HomeDir  string
}

type NativeRestoreSession struct {
	snapshot    nativeRestoreExecutionSnapshot
	environment []string
	runner      NativeRestoreOutputRunner
}

func (e NativeRestoreExecutor) Open(
	ctx context.Context,
	candidate NPMInstallation,
	codexConfigPath string,
) (*NativeRestoreSession, error) {
	if platform := e.Platform; platform != "" && platform != "darwin" {
		return nil, ErrUnsupportedPlatform
	}
	if e.Platform == "" && runtimeGOOS() != "darwin" {
		return nil, ErrUnsupportedPlatform
	}
	if !filepath.IsAbs(codexConfigPath) || filepath.Clean(codexConfigPath) != codexConfigPath ||
		filepath.Base(codexConfigPath) != "config.toml" {
		return nil, ErrNativeRestoreProofUnavailable
	}
	environment, err := nativeRestoreEnvironment(e.HomeDir, filepath.Dir(codexConfigPath))
	if err != nil {
		return nil, err
	}
	snapshot, err := prepareNativeRestoreExecutionSnapshot(ctx, candidate)
	if err != nil {
		return nil, err
	}
	runner := e.Runner
	if runner == nil {
		runner = boundedCommandOutputRunner{}
	}
	return &NativeRestoreSession{
		snapshot:    snapshot,
		environment: environment,
		runner:      runner,
	}, nil
}

func (s *NativeRestoreSession) Close() {
	if s != nil {
		s.snapshot.Close()
		s.snapshot = nativeRestoreExecutionSnapshot{}
	}
}

func (s *NativeRestoreSession) run(ctx context.Context, operation ...string) ([]byte, bool, error) {
	if s == nil || s.runner == nil || s.snapshot.root == "" ||
		s.snapshot.bun == "" || s.snapshot.cliEntry == "" || s.snapshot.bunConfigPath == "" {
		return nil, false, ErrNativeRestoreOutput
	}
	args := make([]string, 0, len(operation)+7)
	args = append(args,
		"--config", s.snapshot.bunConfigPath,
		"--no-install",
		"--no-orphans",
		"--no-env-file",
		s.snapshot.cliEntry,
	)
	args = append(args, operation...)
	payload, nonZero, err := s.runner.RunOutput(ctx, s.snapshot.bun, s.environment, args...)
	if err != nil {
		return nil, false, ErrNativeRestoreOutput
	}
	return payload, nonZero, nil
}

func (e NativeRestoreExecutor) InspectExpected(
	ctx context.Context,
	candidate NPMInstallation,
	codexConfigPath string,
) (NativeOwnerInspection, error) {
	session, err := e.Open(ctx, candidate, codexConfigPath)
	if err != nil {
		return NativeOwnerInspection{}, err
	}
	defer session.Close()
	return session.Inspect(ctx), nil
}

func (s *NativeRestoreSession) Inspect(ctx context.Context) NativeOwnerInspection {
	unavailable := NativeOwnerInspection{
		SchemaVersion: 1,
		Owner:         "opencodex",
		Configuration: NativeOwnerConfigurationUnavailable,
		Integration:   NativeOwnerIntegrationUnknown,
		Reason:        NativeOwnerProbeUnavailable,
	}
	validatePayload, nonZero, err := s.run(ctx, "config", "validate", "--json")
	if err != nil {
		return unavailable
	}
	valid, err := parseNativeOwnerValidation(validatePayload)
	if err != nil || (nonZero && valid) {
		return unavailable
	}
	if !valid {
		return NativeOwnerInspection{
			SchemaVersion: 1,
			Owner:         "opencodex",
			Configuration: NativeOwnerConfigurationInvalid,
			Integration:   NativeOwnerIntegrationUnknown,
			Reason:        NativeOwnerConfigurationInvalidReason,
		}
	}
	configPayload, nonZero, err := s.run(ctx, "config", "show", "--json")
	if err != nil || nonZero {
		return unavailable
	}
	integration, err := parseNativeOwnerIntegration(configPayload)
	if err != nil {
		return unavailable
	}
	return NativeOwnerInspection{
		SchemaVersion: 1,
		Owner:         "opencodex",
		Configuration: NativeOwnerConfigurationValid,
		Integration:   integration,
		Reason:        NativeOwnerReady,
	}
}

func (e NativeRestoreExecutor) ExecuteExpected(
	ctx context.Context,
	candidate NPMInstallation,
	codexConfigPath string,
) (NativeRestoreResult, error) {
	session, err := e.Open(ctx, candidate, codexConfigPath)
	if err != nil {
		return NativeRestoreResult{}, err
	}
	defer session.Close()
	return session.Execute(ctx)
}

func (s *NativeRestoreSession) Execute(ctx context.Context) (NativeRestoreResult, error) {
	payload, nonZero, err := s.run(ctx, "restore", "--json")
	if err != nil {
		return NativeRestoreResult{}, err
	}
	outcome, overallSuccess, err := parseNativeRestoreEnvelope(payload)
	if err != nil {
		return NativeRestoreResult{}, err
	}
	if err := s.VerifyIntegrationDisabled(ctx); err != nil {
		return NativeRestoreResult{}, err
	}
	return NativeRestoreResult{
		Outcome:           outcome,
		NonRoutingWarning: nonZero || !overallSuccess,
	}, nil
}

// VerifyIntegrationDisabled reuses the immutable OpenCodex execution snapshot
// to prove that the owner configuration remains valid and that the Codex
// integration is still explicitly disabled. Callers keep the session open
// across every post-restore destructive checkpoint so removing the source
// package cannot remove the proof mechanism mid-operation.
func (s *NativeRestoreSession) VerifyIntegrationDisabled(ctx context.Context) error {
	inspection := s.Inspect(ctx)
	switch inspection.Configuration {
	case NativeOwnerConfigurationInvalid:
		return ErrNativeOwnerConfigurationInvalid
	case NativeOwnerConfigurationUnavailable:
		return ErrNativeRestoreOutput
	case NativeOwnerConfigurationValid:
		if inspection.Integration != NativeOwnerIntegrationDisabled {
			return ErrNativeRestoreFailed
		}
		return nil
	default:
		return ErrNativeRestoreOutput
	}
}

// OpenCodexOwnerConfigurationRevision binds the standalone Native boundary to
// the exact default OpenCodex owner configuration without exposing its path or
// contents. The semantic clientIntegrations.codex=false proof still comes from
// the verified OpenCodex closure; this witness makes any later direct rewrite
// fail closed, including after the package itself has been removed.
func OpenCodexOwnerConfigurationRevision(home string) (string, error) {
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) {
		return "", ErrNativeRestoreProofUnavailable
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil || resolved != home {
		return "", ErrNativeRestoreProofUnavailable
	}
	homeInfo, err := os.Lstat(home)
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 ||
		homeInfo.Mode().Perm()&0o022 != 0 || !ownedByEffectiveUser(homeInfo) {
		return "", ErrNativeRestoreProofUnavailable
	}
	configDirectory := filepath.Join(home, ".opencodex")
	configPath := filepath.Join(configDirectory, "config.json")
	payload := []byte(nil)
	state := "absent"
	if _, err := os.Lstat(configPath); err == nil {
		var info os.FileInfo
		var relaxed bool
		payload, info, relaxed, err = readDiscoveryRegularFile(configPath, maxNativeOwnerConfigBytes)
		if err != nil || relaxed || !ownedByEffectiveUser(info) {
			return "", ErrNativeRestoreProofUnavailable
		}
		state = "present"
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", ErrNativeRestoreProofUnavailable
	} else if directoryInfo, directoryErr := os.Lstat(configDirectory); directoryErr == nil {
		if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 ||
			directoryInfo.Mode().Perm()&0o022 != 0 || !ownedByEffectiveUser(directoryInfo) {
			return "", ErrNativeRestoreProofUnavailable
		}
	} else if !errors.Is(directoryErr, os.ErrNotExist) {
		return "", ErrNativeRestoreProofUnavailable
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("opencodex-owner-configuration-boundary-v1\x00"))
	_, _ = hash.Write([]byte(state))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Indirections keep the platform/fingerprint checks shared with handoff.go
// without exporting internal security helpers.
var runtimeGOOS = func() string { return runtime.GOOS }

func nativeRestoreEnvironment(home, codexHome string) ([]string, error) {
	codexHome = filepath.Clean(codexHome)
	if !filepath.IsAbs(codexHome) {
		return nil, ErrNativeRestoreProofUnavailable
	}
	resolvedCodexHome, err := filepath.EvalSymlinks(codexHome)
	if err != nil || resolvedCodexHome != codexHome {
		return nil, ErrNativeRestoreProofUnavailable
	}
	environment, err := removalEnvironment(home)
	if err != nil {
		return nil, ErrNativeRestoreProofUnavailable
	}
	return append(environment, "CODEX_HOME="+codexHome), nil
}

type nativeRestoreEnvelope struct {
	Success   *bool `json:"success"`
	Artifacts *struct {
		Config *struct {
			State   string `json:"state"`
			Changed *bool  `json:"changed"`
			Action  string `json:"action"`
		} `json:"config"`
	} `json:"artifacts"`
}

func decodeSingleJSON(payload []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(value); err != nil {
		return ErrNativeRestoreOutput
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrNativeRestoreOutput
	}
	return nil
}

func parseNativeRestoreEnvelope(payload []byte) (NativeRestoreOutcome, bool, error) {
	var envelope nativeRestoreEnvelope
	if err := decodeSingleJSON(payload, &envelope); err != nil {
		return "", false, err
	}
	if envelope.Success == nil || envelope.Artifacts == nil || envelope.Artifacts.Config == nil ||
		envelope.Artifacts.Config.Changed == nil {
		return "", false, ErrNativeRestoreOutput
	}
	config := envelope.Artifacts.Config
	allowedAppliedAction := config.Action == "journal-restored" || config.Action == "owned-fields-stripped"
	switch {
	case config.State == "ok" && allowedAppliedAction:
		return NativeRestoreApplied, *envelope.Success, nil
	case config.State == "skipped" && !*config.Changed &&
		config.Action == "owned-fields-stripped" && *envelope.Success:
		return NativeRestoreAlreadyNative, true, nil
	case config.State == "skipped" && !*config.Changed &&
		config.Action == "owned-fields-stripped" && !*envelope.Success:
		return NativeRestoreRetryableNoMutation, false, nil
	case (config.State == "failed" || config.State == "skipped") && config.Action == "failed":
		return "", *envelope.Success, ErrNativeRestoreFailed
	default:
		return "", *envelope.Success, ErrNativeRestoreOutput
	}
}

func parseNativeOwnerValidation(payload []byte) (bool, error) {
	var envelope struct {
		OK *bool `json:"ok"`
	}
	if err := decodeSingleJSON(payload, &envelope); err != nil || envelope.OK == nil {
		return false, ErrNativeRestoreOutput
	}
	return *envelope.OK, nil
}

func parseNativeOwnerIntegration(payload []byte) (NativeOwnerIntegration, error) {
	var config struct {
		ClientIntegrations *struct {
			Codex *bool `json:"codex"`
		} `json:"clientIntegrations"`
	}
	if err := decodeSingleJSON(payload, &config); err != nil {
		return NativeOwnerIntegrationUnknown, err
	}
	if config.ClientIntegrations != nil &&
		config.ClientIntegrations.Codex != nil &&
		!*config.ClientIntegrations.Codex {
		return NativeOwnerIntegrationDisabled, nil
	}
	return NativeOwnerIntegrationEnabled, nil
}
