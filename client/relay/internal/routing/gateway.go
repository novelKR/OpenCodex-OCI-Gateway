package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

const gatewaySchemaVersion = 2

var (
	ErrGatewayInvalidAddress        = errors.New("gateway address is invalid")
	ErrGatewayCredentialUnavailable = errors.New("gateway credentials are unavailable")
	ErrGatewayAuthenticationFailed  = errors.New("gateway authentication failed")
	ErrGatewayUnreachable           = errors.New("gateway is unreachable")
	ErrGatewayCatalogInvalid        = errors.New("gateway catalog is invalid")
	ErrGatewayConfigChanged         = errors.New("gateway configuration changed")
	ErrGatewayRoutingChanged        = errors.New("gateway routing generation changed")
	ErrGatewayTransitionPending     = errors.New("gateway change is blocked by a routing transition")
	ErrGatewayRuntimeSwap           = errors.New("gateway runtime reload failed")
	ErrGatewayUnsupported           = errors.New("gateway settings are unsupported")
)

type GatewayCandidate struct {
	UpstreamBaseURL        string `json:"upstream_base_url"`
	AuthenticationProfile  string `json:"authentication_profile,omitempty"`
	AllowInsecurePrivateIP bool   `json:"allow_insecure_private_ip,omitempty"`
}

type GatewayInspection struct {
	SchemaVersion          int      `json:"schema_version"`
	UpstreamBaseURL        string   `json:"upstream_base_url"`
	ConfigDigest           string   `json:"config_digest"`
	RoutingGeneration      uint64   `json:"routing_generation"`
	CredentialSource       string   `json:"credential_source"`
	CredentialAccount      string   `json:"credential_account,omitempty"`
	CredentialsEditable    bool     `json:"credentials_editable"`
	AuthenticationProfile  string   `json:"authentication_profile"`
	RequiredCredentials    []string `json:"required_credentials"`
	AllowInsecurePrivateIP bool     `json:"allow_insecure_private_ip"`
}

type GatewayValidation struct {
	SchemaVersion     int    `json:"schema_version"`
	OK                bool   `json:"ok"`
	ConfigDigest      string `json:"config_digest"`
	RoutingGeneration uint64 `json:"routing_generation"`
	ModelCount        int    `json:"model_count"`
}

type GatewayApplyReceipt struct {
	SchemaVersion     int    `json:"schema_version"`
	OK                bool   `json:"ok"`
	ConfigDigest      string `json:"config_digest"`
	RoutingGeneration uint64 `json:"routing_generation"`
	RuntimeReloaded   bool   `json:"runtime_reloaded"`
}

type gatewayPreflight struct {
	current      config.Config
	candidate    config.Config
	configDigest string
	state        State
	credentials  credentials.Values
	result       catalog.Result
}

func validateExternalGatewayCatalog(ctx context.Context, cfg config.Config, values credentials.Values) (catalog.Result, error) {
	return (catalog.Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return values, nil
		},
	}).Validate(ctx)
}

func gatewayBackupPath(configPath string) string {
	return filepath.Clean(configPath) + ".gateway-backup.json"
}

func (c *Controller) GatewayInspect(ctx context.Context) (GatewayInspection, error) {
	if c == nil || c.store == nil || c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return GatewayInspection{}, ErrRecoveryRequired
	}
	if err := validateExistingControlFile(c.store.ConfigPath()); err != nil {
		return GatewayInspection{}, ErrGatewayUnsupported
	}
	cfg, err := c.loadConfig(c.store.ConfigPath())
	if err != nil || cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		return GatewayInspection{}, ErrGatewayUnsupported
	}
	state, legacy, err := c.store.Read()
	if err != nil || legacy || state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil {
		return GatewayInspection{}, ErrGatewayUnsupported
	}
	if _, found, journalErr := c.loadJournal(); journalErr != nil || found {
		return GatewayInspection{}, ErrRecoveryRequired
	}
	digest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || digest == "absent" {
		return GatewayInspection{}, ErrGatewayUnsupported
	}
	account := ""
	editable := cfg.Credentials.Source == config.CredentialsSourceKeychain
	if editable {
		account, err = credentials.ResolveKeychainAccount(cfg.Credentials.Account)
		if err != nil {
			return GatewayInspection{}, ErrGatewayUnsupported
		}
	}
	profile := cfg.Credentials.RemoteAuthenticationProfile()
	required, err := config.RequiredCredentialKinds(profile)
	if err != nil {
		return GatewayInspection{}, ErrGatewayUnsupported
	}
	return GatewayInspection{
		SchemaVersion:          gatewaySchemaVersion,
		UpstreamBaseURL:        cfg.UpstreamBaseURL,
		ConfigDigest:           digest,
		RoutingGeneration:      state.Generation,
		CredentialSource:       cfg.Credentials.Source,
		CredentialAccount:      account,
		CredentialsEditable:    editable,
		AuthenticationProfile:  profile,
		RequiredCredentials:    required,
		AllowInsecurePrivateIP: cfg.Credentials.AllowInsecurePrivateIP,
	}, nil
}

func (c *Controller) GatewayTest(ctx context.Context, candidate GatewayCandidate) (GatewayValidation, error) {
	preflight, err := c.gatewayPreflight(ctx, candidate)
	if err != nil {
		return GatewayValidation{}, err
	}
	return GatewayValidation{
		SchemaVersion:     gatewaySchemaVersion,
		OK:                true,
		ConfigDigest:      preflight.configDigest,
		RoutingGeneration: preflight.state.Generation,
		ModelCount:        preflight.result.Count,
	}, nil
}

func (c *Controller) gatewayPreflight(ctx context.Context, request GatewayCandidate) (gatewayPreflight, error) {
	if c == nil || c.store == nil || c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return gatewayPreflight{}, ErrRecoveryRequired
	}
	if err := validateExistingControlFile(c.store.ConfigPath()); err != nil {
		return gatewayPreflight{}, ErrGatewayUnsupported
	}
	current, err := c.loadConfig(c.store.ConfigPath())
	if err != nil || current.UpstreamMode != config.UpstreamModeExternalGateway {
		return gatewayPreflight{}, ErrGatewayUnsupported
	}
	candidate := current
	normalized, err := config.NormalizeExternalGatewayURL(request.UpstreamBaseURL)
	if err != nil {
		return gatewayPreflight{}, ErrGatewayInvalidAddress
	}
	candidate.UpstreamBaseURL = normalized
	if request.AuthenticationProfile != "" {
		candidate.Credentials.AuthenticationProfile = request.AuthenticationProfile
	}
	profile := candidate.Credentials.RemoteAuthenticationProfile()
	candidate.Credentials.AllowInsecurePrivateIP = request.AllowInsecurePrivateIP
	if candidate.Validate() != nil {
		return gatewayPreflight{}, ErrGatewayInvalidAddress
	}
	state, legacy, err := c.store.Read()
	if err != nil || legacy || state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil {
		return gatewayPreflight{}, ErrGatewayRoutingChanged
	}
	if state.Phase == PhaseApplying || state.Phase == PhaseRecoveryRequired {
		return gatewayPreflight{}, ErrRecoveryRequired
	}
	if state.Phase != PhaseRelayActive && state.Phase != PhaseNativeActive {
		return gatewayPreflight{}, ErrGatewayTransitionPending
	}
	if _, found, journalErr := c.loadJournal(); journalErr != nil || found {
		return gatewayPreflight{}, ErrRecoveryRequired
	}
	digest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || digest == "absent" {
		return gatewayPreflight{}, ErrGatewayUnsupported
	}
	values, err := c.loadCredentials(candidate.Credentials)
	if err != nil || values.ValidateForProfile(profile) != nil {
		return gatewayPreflight{}, ErrGatewayCredentialUnavailable
	}
	if c.validateGateway == nil {
		return gatewayPreflight{}, ErrGatewayUnsupported
	}
	result, err := c.validateGateway(ctx, candidate, values)
	if err != nil {
		return gatewayPreflight{}, classifyGatewayValidationError(err)
	}
	afterDigest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || afterDigest != digest {
		return gatewayPreflight{}, ErrGatewayConfigChanged
	}
	afterState, legacy, err := c.store.Read()
	if err != nil || legacy || afterState.Generation != state.Generation || afterState.Phase != state.Phase {
		return gatewayPreflight{}, ErrGatewayRoutingChanged
	}
	afterValues, err := c.loadCredentials(candidate.Credentials)
	if err != nil || afterValues != values {
		return gatewayPreflight{}, ErrGatewayCredentialUnavailable
	}
	return gatewayPreflight{
		current:      current,
		candidate:    candidate,
		configDigest: digest,
		state:        state,
		credentials:  values,
		result:       result,
	}, nil
}

func classifyGatewayValidationError(err error) error {
	switch {
	case errors.Is(err, catalog.ErrValidationAuthentication):
		return ErrGatewayAuthenticationFailed
	case errors.Is(err, catalog.ErrValidationUnreachable):
		return ErrGatewayUnreachable
	case errors.Is(err, catalog.ErrValidationCatalog):
		return ErrGatewayCatalogInvalid
	default:
		return ErrGatewayCatalogInvalid
	}
}

func (c *Controller) GatewayApply(
	ctx context.Context,
	request GatewayCandidate,
	expectedConfigDigest string,
	expectedRoutingGeneration uint64,
) (GatewayApplyReceipt, error) {
	if expectedConfigDigest == "" || expectedRoutingGeneration == 0 {
		return GatewayApplyReceipt{}, ErrGatewayConfigChanged
	}
	preflight, err := c.gatewayPreflight(ctx, request)
	if err != nil {
		return GatewayApplyReceipt{}, err
	}
	if preflight.configDigest != expectedConfigDigest {
		return GatewayApplyReceipt{}, ErrGatewayConfigChanged
	}
	if preflight.state.Generation != expectedRoutingGeneration {
		return GatewayApplyReceipt{}, ErrGatewayRoutingChanged
	}

	lock, err := c.store.Lock(ctx)
	if err != nil {
		return GatewayApplyReceipt{}, err
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return GatewayApplyReceipt{}, ErrRecoveryRequired
	}
	if _, found, journalErr := c.loadJournal(); journalErr != nil || found {
		return GatewayApplyReceipt{}, ErrRecoveryRequired
	}
	state, legacy, err := c.boundState(lock)
	if err != nil || legacy {
		return GatewayApplyReceipt{}, ErrGatewayRoutingChanged
	}
	if state.Generation != expectedRoutingGeneration {
		return GatewayApplyReceipt{}, ErrGatewayRoutingChanged
	}
	if state.Phase != PhaseRelayActive && state.Phase != PhaseNativeActive {
		return GatewayApplyReceipt{}, ErrGatewayTransitionPending
	}
	// A backup without a journal can only be left before the applying state was
	// committed or after the final state was committed. Both states are stable,
	// so the reserved backup is no longer authoritative and may be removed.
	if err := removeGatewayBackup(gatewayBackupPath(c.store.ConfigPath())); err != nil {
		return GatewayApplyReceipt{}, ErrRecoveryRequired
	}
	if err := validateExistingControlFile(c.store.ConfigPath()); err != nil {
		return GatewayApplyReceipt{}, ErrGatewayUnsupported
	}
	currentDigest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || currentDigest != expectedConfigDigest {
		return GatewayApplyReceipt{}, ErrGatewayConfigChanged
	}
	current, err := c.loadConfig(c.store.ConfigPath())
	currentConfigDigest, digestErr := configDigest(current)
	preflightCurrentDigest, preflightDigestErr := configDigest(preflight.current)
	if err != nil || digestErr != nil || preflightDigestErr != nil || currentConfigDigest != preflightCurrentDigest {
		return GatewayApplyReceipt{}, ErrGatewayConfigChanged
	}
	values, err := c.loadCredentials(preflight.candidate.Credentials)
	if err != nil || values != preflight.credentials {
		return GatewayApplyReceipt{}, ErrGatewayCredentialUnavailable
	}
	targetDigest, err := configDigest(preflight.candidate)
	if err != nil {
		return GatewayApplyReceipt{}, ErrGatewayInvalidAddress
	}
	if currentDigest == targetDigest {
		return GatewayApplyReceipt{
			SchemaVersion:     gatewaySchemaVersion,
			OK:                true,
			ConfigDigest:      currentDigest,
			RoutingGeneration: state.Generation,
			RuntimeReloaded:   false,
		}, nil
	}

	activeExternal := state.Phase == PhaseRelayActive && state.AppliedBackend == BackendExternal
	if !activeExternal {
		if err := config.Write(c.store.ConfigPath(), preflight.candidate); err != nil {
			return GatewayApplyReceipt{}, ErrGatewayConfigChanged
		}
		afterValues, credentialErr := c.loadCredentials(preflight.candidate.Credentials)
		if credentialErr != nil || afterValues != preflight.credentials {
			observedDigest, digestErr := fingerprintOptional(c.store.ConfigPath())
			if digestErr != nil || observedDigest != targetDigest {
				return GatewayApplyReceipt{}, ErrGatewayConfigChanged
			}
			if err := config.Write(c.store.ConfigPath(), current); err != nil {
				return GatewayApplyReceipt{}, ErrGatewayConfigChanged
			}
			restoredDigest, restoreErr := fingerprintOptional(c.store.ConfigPath())
			if restoreErr != nil || restoredDigest != currentDigest {
				return GatewayApplyReceipt{}, ErrGatewayConfigChanged
			}
			return GatewayApplyReceipt{}, ErrGatewayCredentialUnavailable
		}
		nextDigest, err := fingerprintOptional(c.store.ConfigPath())
		if err != nil || nextDigest != targetDigest {
			return GatewayApplyReceipt{}, ErrGatewayConfigChanged
		}
		return GatewayApplyReceipt{
			SchemaVersion:     gatewaySchemaVersion,
			OK:                true,
			ConfigDigest:      nextDigest,
			RoutingGeneration: state.Generation,
			RuntimeReloaded:   false,
		}, nil
	}
	return c.applyGatewayReloadLocked(ctx, lock, state, current, preflight.candidate, preflight.credentials)
}

func (c *Controller) applyGatewayReloadLocked(
	ctx context.Context,
	lock *Lock,
	state State,
	current config.Config,
	candidate config.Config,
	expectedCredentials credentials.Values,
) (GatewayApplyReceipt, error) {
	if c.runtimeControl == nil {
		return GatewayApplyReceipt{}, ErrGatewayRuntimeSwap
	}
	originDigest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || originDigest == "absent" {
		return GatewayApplyReceipt{}, ErrGatewayConfigChanged
	}
	targetDigest, err := configDigest(candidate)
	if err != nil {
		return GatewayApplyReceipt{}, ErrGatewayInvalidAddress
	}
	backupPath := gatewayBackupPath(c.store.ConfigPath())
	if err := createGatewayBackup(c.store.ConfigPath(), backupPath); err != nil {
		return GatewayApplyReceipt{}, ErrRecoveryRequired
	}
	backupDigest, err := fingerprintOptional(backupPath)
	if err != nil || backupDigest != originDigest {
		return GatewayApplyReceipt{}, ErrRecoveryRequired
	}

	applying := state
	applying.Phase = PhaseApplying
	applying.Generation++
	if err := lock.Save(applying); err != nil {
		_ = removeGatewayBackup(backupPath)
		return GatewayApplyReceipt{}, err
	}
	journal, err := c.newJournal(applying, BackendExternal, false)
	if err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, transactionJournal{}, ErrGatewayRuntimeSwap)
	}
	journal.Kind = transactionKindGatewayReload
	journal.Stage = transactionPrepared
	journal.OriginRelayConfigFingerprint = originDigest
	journal.TargetRelayConfigFingerprint = targetDigest
	journal.BackupConfigFingerprint = backupDigest
	journal.RelayConfigFingerprint = originDigest
	if err := c.writeJournal(journal); err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayRuntimeSwap)
	}
	if err := c.awaitParked(ctx, current, applying); err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayRuntimeSwap)
	}
	if err := config.Write(c.store.ConfigPath(), candidate); err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayConfigChanged)
	}
	journal.Stage = transactionConfigMutated
	journal.RelayConfigFingerprint = targetDigest
	if err := c.writeJournal(journal); err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayRuntimeSwap)
	}
	afterCredentials, err := c.loadCredentials(candidate.Credentials)
	if err != nil || afterCredentials != expectedCredentials {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayCredentialUnavailable)
	}
	if err := c.runtimeControl.Apply(ctx, applying.Generation, BackendExternal); err != nil {
		return c.rollbackGatewayReloadLocked(ctx, lock, applying, current, journal, ErrGatewayRuntimeSwap)
	}

	final := applying
	final.Phase = PhaseRelayActive
	final.Generation++
	if err := lock.Save(final); err != nil {
		return c.parkGatewayReloadLocked(lock, applying, ErrGatewayRuntimeSwap)
	}
	if err := c.awaitFinalized(ctx, candidate, final); err != nil {
		return c.parkGatewayReloadLocked(lock, final, ErrGatewayRuntimeSwap)
	}
	digest, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil || digest != targetDigest {
		return c.parkGatewayReloadLocked(lock, final, ErrGatewayConfigChanged)
	}
	if err := c.removeJournal(); err != nil {
		return c.parkGatewayReloadLocked(lock, final, ErrGatewayRuntimeSwap)
	}
	if err := removeGatewayBackup(backupPath); err != nil {
		_ = c.writeJournal(journal)
		return c.parkGatewayReloadLocked(lock, final, ErrGatewayRuntimeSwap)
	}
	return GatewayApplyReceipt{
		SchemaVersion:     gatewaySchemaVersion,
		OK:                true,
		ConfigDigest:      digest,
		RoutingGeneration: final.Generation,
		RuntimeReloaded:   true,
	}, nil
}

func (c *Controller) rollbackGatewayReloadLocked(
	ctx context.Context,
	lock *Lock,
	applying State,
	origin config.Config,
	journal transactionJournal,
	cause error,
) (GatewayApplyReceipt, error) {
	originDigest, digestErr := configDigest(origin)
	if digestErr != nil || config.Write(c.store.ConfigPath(), origin) != nil {
		return c.parkGatewayReloadLocked(lock, applying, cause)
	}
	if journal.Schema == 0 {
		var err error
		journal, err = c.newJournal(applying, BackendExternal, false)
		if err != nil {
			return c.parkGatewayReloadLocked(lock, applying, cause)
		}
		journal.Kind = transactionKindGatewayReload
		journal.OriginRelayConfigFingerprint = originDigest
		journal.TargetRelayConfigFingerprint = originDigest
		journal.BackupConfigFingerprint = originDigest
	}
	journal.Stage = transactionPrepared
	journal.RelayConfigFingerprint = originDigest
	if journal.OriginRelayConfigFingerprint == "" {
		journal.OriginRelayConfigFingerprint = originDigest
	}
	if journal.TargetRelayConfigFingerprint == "" {
		journal.TargetRelayConfigFingerprint = originDigest
	}
	if journal.BackupConfigFingerprint == "" {
		journal.BackupConfigFingerprint = originDigest
	}
	if err := c.writeJournal(journal); err != nil {
		return c.parkGatewayReloadLocked(lock, applying, cause)
	}
	if c.runtimeControl == nil || c.runtimeControl.Apply(ctx, applying.Generation, BackendExternal) != nil {
		return c.parkGatewayReloadLocked(lock, applying, cause)
	}
	final := applying
	final.Phase = PhaseRelayActive
	final.Generation++
	if err := lock.Save(final); err != nil {
		return c.parkGatewayReloadLocked(lock, applying, cause)
	}
	if err := c.removeJournal(); err != nil {
		return c.parkGatewayReloadLocked(lock, final, cause)
	}
	if err := c.awaitFinalized(ctx, origin, final); err != nil {
		_ = c.writeJournal(journal)
		return c.parkGatewayReloadLocked(lock, final, cause)
	}
	if err := removeGatewayBackup(gatewayBackupPath(c.store.ConfigPath())); err != nil {
		_ = c.writeJournal(journal)
		return c.parkGatewayReloadLocked(lock, final, cause)
	}
	return GatewayApplyReceipt{}, cause
}

func (c *Controller) parkGatewayReloadLocked(lock *Lock, state State, cause error) (GatewayApplyReceipt, error) {
	recovery := state
	recovery.Phase = PhaseRecoveryRequired
	recovery.Generation++
	if err := lock.Save(recovery); err != nil {
		return GatewayApplyReceipt{}, fmt.Errorf("%w; mark recovery: %v", cause, err)
	}
	return GatewayApplyReceipt{}, cause
}

func configDigest(cfg config.Config) (string, error) {
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	payload = append(payload, '\n')
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func createGatewayBackup(configPath, backupPath string) error {
	if err := validateExistingControlFile(configPath); err != nil {
		return err
	}
	if _, err := os.Lstat(backupPath); err == nil {
		return ErrRecoveryRequired
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A same-directory hard link preserves the exact config bytes and owner-only
	// mode while the subsequent config.Write atomically replaces the live path.
	if err := os.Link(configPath, backupPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(backupPath))
}

func removeGatewayBackup(path string) error {
	if err := validateExistingControlFile(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (c *Controller) recoverGatewayLocked(
	ctx context.Context,
	lock *Lock,
	state State,
	legacy bool,
	stateErr error,
	journal transactionJournal,
	action RecoveryAction,
) (Status, error) {
	if c.runtimeControl == nil || journal.Kind != transactionKindGatewayReload {
		return Status{}, ErrRecoveryRequired
	}
	observedStage, err := c.gatewayJournalObservedStage(journal)
	if err != nil {
		return Status{}, ErrRecoveryRequired
	}
	journal.Stage = observedStage
	var selected config.Config
	var expectedDigest string
	var stage transactionStage
	switch action {
	case RecoveryComplete:
		if journal.Stage != transactionConfigMutated {
			return Status{}, ErrRecoveryRequired
		}
		var err error
		selected, err = c.loadConfig(c.store.ConfigPath())
		if err != nil {
			return Status{}, ErrRecoveryRequired
		}
		expectedDigest = journal.TargetRelayConfigFingerprint
		stage = transactionConfigMutated
	case RecoveryRollback:
		var err error
		selected, err = config.Load(gatewayBackupPath(c.store.ConfigPath()))
		if err != nil {
			return Status{}, ErrRecoveryRequired
		}
		expectedDigest = journal.OriginRelayConfigFingerprint
		stage = transactionPrepared
	default:
		return Status{}, ErrRecoveryRequired
	}
	selectedDigest, err := configDigest(selected)
	if err != nil || selectedDigest != expectedDigest {
		return Status{}, ErrRecoveryRequired
	}

	recovery := state
	if stateErr != nil || legacy || state.Phase != PhaseRecoveryRequired {
		recovery, err = c.recoveryStateFromJournal(journal)
		if err != nil {
			return Status{}, ErrRecoveryRequired
		}
		if stateErr == nil && !legacy && state.Generation >= recovery.Generation {
			recovery.Generation = state.Generation + 1
		}
		if err := lock.Replace(recovery); err != nil {
			return Status{}, ErrRecoveryRequired
		}
	}
	if err := config.Write(c.store.ConfigPath(), selected); err != nil {
		return Status{}, ErrRecoveryRequired
	}
	applying := recovery
	applying.DesiredMode = ModeRelay
	applying.AppliedMode = ModeRelay
	applying.DesiredBackend = BackendExternal
	applying.AppliedBackend = BackendExternal
	applying.Phase = PhaseApplying
	applying.Generation++
	if err := lock.Replace(applying); err != nil {
		return Status{}, ErrRecoveryRequired
	}
	journal.Generation = applying.Generation
	journal.Stage = stage
	journal.RelayConfigFingerprint = selectedDigest
	if err := c.writeJournal(journal); err != nil {
		return c.parkGatewayRecoveryLocked(lock, applying)
	}
	if err := c.awaitParked(ctx, selected, applying); err != nil {
		return c.parkGatewayRecoveryLocked(lock, applying)
	}
	if err := c.runtimeControl.Apply(ctx, applying.Generation, BackendExternal); err != nil {
		return c.parkGatewayRecoveryLocked(lock, applying)
	}
	final := applying
	final.Phase = PhaseRelayActive
	final.Generation++
	if err := lock.Save(final); err != nil {
		return c.parkGatewayRecoveryLocked(lock, applying)
	}
	if err := c.removeJournal(); err != nil {
		return c.parkGatewayRecoveryLocked(lock, final)
	}
	if err := c.awaitFinalized(ctx, selected, final); err != nil {
		_ = c.writeJournal(journal)
		return c.parkGatewayRecoveryLocked(lock, final)
	}
	if err := removeGatewayBackup(gatewayBackupPath(c.store.ConfigPath())); err != nil {
		_ = c.writeJournal(journal)
		return c.parkGatewayRecoveryLocked(lock, final)
	}
	return c.Status(ctx), nil
}

func (c *Controller) parkGatewayRecoveryLocked(lock *Lock, state State) (Status, error) {
	recovery := state
	recovery.Phase = PhaseRecoveryRequired
	recovery.Generation++
	if err := lock.Save(recovery); err != nil {
		return Status{}, ErrRecoveryRequired
	}
	return Status{}, ErrRecoveryRequired
}
