package routing

import (
	"context"
	"errors"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
)

const (
	nativeRepairInspectionSchema = 1
	nativeRepairOwnerSchema      = 1
	nativeRepairReceiptSchema    = 2
	maxNativeOwnerRetryCount     = 3
)

var (
	ErrNativeRepairOwnerChanged        = errors.New("native repair routing owner changed")
	ErrNativeRepairArtifactsChanged    = errors.New("native repair routing artifacts changed")
	ErrNativeRepairConfigurationFailed = errors.New("native repair configuration cleanup failed")
	ErrNativeRepairStateIncomplete     = errors.New("native routing is clean but state repair is incomplete")
	ErrNativeOwnerBusy                 = errors.New("OpenCodex owner desired state remained busy")
	ErrNativeOwnerConfigurationInvalid = errors.New("OpenCodex owner configuration is invalid")
	ErrNativeOwnerRestoreFailed        = errors.New("OpenCodex owner reported restore failure")
	ErrNativeOwnerResultInvalid        = errors.New("OpenCodex owner returned an invalid bounded result")
)

var nativeOwnerRetryDelays = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}
var waitForNativeOwnerRetry = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// NativeRepairInspectionResult is the complete read-only helper response. It
// carries only bounded tokens and booleans; the private file witness remains in
// codexconfig.NativeRepairInspection inside the helper process.
type NativeRepairInspectionResult struct {
	SchemaVersion int                          `json:"schema_version"`
	Generation    uint64                       `json:"generation"`
	Phase         Phase                        `json:"phase"`
	Kind          codexconfig.NativeRepairKind `json:"kind"`
	OpenAIBaseURL bool                         `json:"openai_base_url"`
	ModelCatalog  bool                         `json:"model_catalog_json"`
	Reason        string                       `json:"reason"`
}

type NativeOwnerConfiguration string
type NativeOwnerIntegration string
type NativeOwnerReason string

const (
	NativeOwnerConfigurationValid       NativeOwnerConfiguration = "valid"
	NativeOwnerConfigurationInvalid     NativeOwnerConfiguration = "invalid"
	NativeOwnerConfigurationUnavailable NativeOwnerConfiguration = "unavailable"
	NativeOwnerIntegrationEnabled       NativeOwnerIntegration   = "enabled"
	NativeOwnerIntegrationDisabled      NativeOwnerIntegration   = "disabled"
	NativeOwnerIntegrationUnknown       NativeOwnerIntegration   = "unknown"
	NativeOwnerReasonReady              NativeOwnerReason        = "owner_ready"
	NativeOwnerReasonConfiguration      NativeOwnerReason        = "owner_configuration_invalid"
	NativeOwnerReasonUnavailable        NativeOwnerReason        = "owner_probe_unavailable"
)

type NativeRepairOwnerStatus struct {
	Configuration NativeOwnerConfiguration
	Integration   NativeOwnerIntegration
	Reason        NativeOwnerReason
}

type NativeRepairOwnerInspectionResult struct {
	SchemaVersion int                          `json:"schema_version"`
	Generation    uint64                       `json:"generation"`
	Owner         codexconfig.NativeRepairKind `json:"owner"`
	Configuration NativeOwnerConfiguration     `json:"configuration"`
	Integration   NativeOwnerIntegration       `json:"integration"`
	Reason        NativeOwnerReason            `json:"reason"`
}

type NativeRepairOwnerOutcome string

const (
	NativeRepairOwnerApplied             NativeRepairOwnerOutcome = "applied"
	NativeRepairOwnerAlreadyNative       NativeRepairOwnerOutcome = "already_native"
	NativeRepairOwnerRetryableNoMutation NativeRepairOwnerOutcome = "retryable_no_mutation"
	NativeRepairOwnerNotApplicable       NativeRepairOwnerOutcome = "not_applicable"
)

type NativeRepairOwnerRestoreResult struct {
	Outcome              NativeRepairOwnerOutcome
	NonRoutingIncomplete bool
}

type NativeRoutingRepairReceipt struct {
	SchemaVersion               int                      `json:"schema_version"`
	Status                      Status                   `json:"status"`
	BackupCreated               bool                     `json:"backup_created"`
	NonRoutingCleanupIncomplete bool                     `json:"nonrouting_cleanup_incomplete"`
	OwnerRestoreAttempts        int                      `json:"owner_restore_attempts"`
	OwnerRestoreResult          NativeRepairOwnerOutcome `json:"owner_restore_result"`
}

type NativeRepairOwnerInspect func(context.Context) (NativeRepairOwnerStatus, error)
type NativeRepairOwnerRestore func(context.Context) (NativeRepairOwnerRestoreResult, error)

func (c *Controller) InspectNativeRepair(ctx context.Context, expectedGeneration uint64) (NativeRepairInspectionResult, error) {
	if c == nil || c.store == nil || c.codexOwner != codexconfig.LocalDevelopmentOwner || expectedGeneration == 0 {
		return NativeRepairInspectionResult{}, ErrNativeRepairUnavailable
	}
	lock, err := c.store.ReadLock(ctx)
	if err != nil {
		return NativeRepairInspectionResult{}, ErrNativeRepairUnavailable
	}
	defer lock.Close()
	state, inspection, err := c.inspectNativeRepairLocked(expectedGeneration)
	if err != nil {
		return NativeRepairInspectionResult{}, err
	}
	return NativeRepairInspectionResult{
		SchemaVersion: nativeRepairInspectionSchema,
		Generation:    state.Generation, Phase: state.Phase, Kind: inspection.Kind,
		OpenAIBaseURL: inspection.OpenAIBaseURL, ModelCatalog: inspection.ModelCatalog, Reason: inspection.Reason,
	}, nil
}

// InspectNativeRepairOwner binds a read-only owner probe to the exact recovery
// generation and TOML witness before and after the child process runs.
func (c *Controller) InspectNativeRepairOwner(
	ctx context.Context,
	expectedGeneration uint64,
	expectedOwner codexconfig.NativeRepairKind,
	inspectOwner NativeRepairOwnerInspect,
) (NativeRepairOwnerInspectionResult, error) {
	if expectedOwner != codexconfig.NativeRepairOpenCodex || inspectOwner == nil {
		return NativeRepairOwnerInspectionResult{}, ErrNativeRepairUnavailable
	}
	lock, err := c.store.ReadLock(ctx)
	if err != nil {
		return NativeRepairOwnerInspectionResult{}, ErrNativeRepairUnavailable
	}
	defer lock.Close()
	state, inspection, err := c.inspectNativeRepairLocked(expectedGeneration)
	if err != nil {
		return NativeRepairOwnerInspectionResult{}, err
	}
	if inspection.Kind != expectedOwner {
		return NativeRepairOwnerInspectionResult{}, ErrNativeRepairOwnerChanged
	}
	if err := codexconfig.RevalidateNativeRepairInspection(c.codexConfigPath, c.codexOwner, inspection); err != nil {
		return NativeRepairOwnerInspectionResult{}, ErrNativeRepairArtifactsChanged
	}
	owner, probeErr := inspectOwner(ctx)
	if err := codexconfig.RevalidateNativeRepairInspection(c.codexConfigPath, c.codexOwner, inspection); err != nil {
		return NativeRepairOwnerInspectionResult{}, ErrNativeRepairArtifactsChanged
	}
	if probeErr != nil {
		return NativeRepairOwnerInspectionResult{}, probeErr
	}
	if !validNativeOwnerStatus(owner) {
		return NativeRepairOwnerInspectionResult{}, ErrNativeOwnerResultInvalid
	}
	return NativeRepairOwnerInspectionResult{
		SchemaVersion: nativeRepairOwnerSchema, Generation: state.Generation, Owner: expectedOwner,
		Configuration: owner.Configuration, Integration: owner.Integration, Reason: owner.Reason,
	}, nil
}

func (c *Controller) RepairNativeRouting(
	ctx context.Context,
	expectedGeneration uint64,
	expectedOwner codexconfig.NativeRepairKind,
	confirmed bool,
	desktopExited bool,
	inspectOwner NativeRepairOwnerInspect,
	restoreOwner NativeRepairOwnerRestore,
) (NativeRoutingRepairReceipt, error) {
	if c == nil || c.store == nil || c.codexOwner != codexconfig.LocalDevelopmentOwner ||
		expectedGeneration == 0 || !confirmed || !desktopExited ||
		(expectedOwner != codexconfig.NativeRepairLocalRelay && expectedOwner != codexconfig.NativeRepairOpenCodex) {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairUnavailable
	}
	if expectedOwner == codexconfig.NativeRepairOpenCodex && (inspectOwner == nil || restoreOwner == nil) {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairUnavailable
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairUnavailable
	}
	defer lock.Close()

	state, inspection, err := c.inspectNativeRepairLocked(expectedGeneration)
	if err != nil {
		return NativeRoutingRepairReceipt{}, err
	}
	if inspection.Kind != expectedOwner {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairOwnerChanged
	}
	if err := c.revalidateNativeRepairAttempt(expectedGeneration, inspection); err != nil {
		return NativeRoutingRepairReceipt{}, err
	}
	if expectedOwner == codexconfig.NativeRepairOpenCodex {
		owner, probeErr := inspectOwner(ctx)
		if probeErr != nil {
			return NativeRoutingRepairReceipt{}, probeErr
		}
		if !validNativeOwnerStatus(owner) {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
		}
		if owner.Configuration == NativeOwnerConfigurationInvalid {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerConfigurationInvalid
		}
		if owner.Configuration == NativeOwnerConfigurationUnavailable || owner.Integration == NativeOwnerIntegrationUnknown {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
		}
	}
	backupCreated, err := codexconfig.CreateNativeRepairBackup(c.codexConfigPath)
	if err != nil {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairConfigurationFailed
	}
	if err := c.revalidateNativeRepairAttempt(expectedGeneration, inspection); err != nil {
		return NativeRoutingRepairReceipt{}, err
	}

	nonRoutingIncomplete := false
	attempts := 0
	ownerResult := NativeRepairOwnerNotApplicable
	switch expectedOwner {
	case codexconfig.NativeRepairLocalRelay:
		if err := codexconfig.DisableWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner); err != nil {
			return NativeRoutingRepairReceipt{}, ErrNativeRepairConfigurationFailed
		}
	case codexconfig.NativeRepairOpenCodex:
		for {
			if err := c.revalidateNativeRepairAttempt(expectedGeneration, inspection); err != nil {
				return NativeRoutingRepairReceipt{}, err
			}
			owner, probeErr := inspectOwner(ctx)
			if probeErr != nil {
				return NativeRoutingRepairReceipt{}, probeErr
			}
			if !validNativeOwnerStatus(owner) {
				return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
			}
			if owner.Configuration == NativeOwnerConfigurationInvalid {
				return NativeRoutingRepairReceipt{}, ErrNativeOwnerConfigurationInvalid
			}
			if owner.Configuration == NativeOwnerConfigurationUnavailable || owner.Integration == NativeOwnerIntegrationUnknown {
				return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
			}
			attempts++
			result, restoreErr := restoreOwner(ctx)
			if restoreErr != nil {
				return NativeRoutingRepairReceipt{}, restoreErr
			}
			switch result.Outcome {
			case NativeRepairOwnerApplied, NativeRepairOwnerAlreadyNative:
				ownerResult = result.Outcome
				nonRoutingIncomplete = result.NonRoutingIncomplete
			case NativeRepairOwnerRetryableNoMutation:
				if err := c.revalidateNativeRepairAttempt(expectedGeneration, inspection); err != nil {
					return NativeRoutingRepairReceipt{}, err
				}
				if attempts > maxNativeOwnerRetryCount {
					return NativeRoutingRepairReceipt{}, ErrNativeOwnerBusy
				}
				if err := waitForNativeOwnerRetry(ctx, nativeOwnerRetryDelays[attempts-1]); err != nil {
					return NativeRoutingRepairReceipt{}, err
				}
				continue
			default:
				return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
			}
			break
		}
	}
	if err := codexconfig.ValidateNativeRoutingForOwner(c.codexConfigPath, c.codexOwner); err != nil {
		return NativeRoutingRepairReceipt{}, ErrNativeVerification
	}
	if expectedOwner == codexconfig.NativeRepairOpenCodex {
		owner, probeErr := inspectOwner(ctx)
		if probeErr != nil {
			return NativeRoutingRepairReceipt{}, probeErr
		}
		if !validNativeOwnerStatus(owner) {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
		}
		if owner.Configuration == NativeOwnerConfigurationInvalid {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerConfigurationInvalid
		}
		if owner.Configuration == NativeOwnerConfigurationUnavailable || owner.Integration == NativeOwnerIntegrationUnknown {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerResultInvalid
		}
		if owner.Integration != NativeOwnerIntegrationDisabled {
			return NativeRoutingRepairReceipt{}, ErrNativeOwnerRestoreFailed
		}
	}

	repaired := state
	repaired.Generation++
	repaired.DesiredMode, repaired.AppliedMode = ModeNative, ModeNative
	repaired.DesiredBackend, repaired.AppliedBackend = BackendNone, BackendNone
	repaired.Phase = PhaseNativeActive
	if err := lock.Replace(repaired); err != nil {
		return NativeRoutingRepairReceipt{}, ErrNativeRepairStateIncomplete
	}
	committed, legacy, err := c.store.Read()
	if err != nil || legacy || committed.Generation != repaired.Generation || committed.Phase != PhaseNativeActive ||
		committed.DesiredBackend != BackendNone || committed.AppliedBackend != BackendNone ||
		committed.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil ||
		codexconfig.ValidateNativeRoutingForOwner(c.codexConfigPath, c.codexOwner) != nil {
		_ = lock.Replace(state)
		return NativeRoutingRepairReceipt{}, ErrNativeRepairStateIncomplete
	}
	_ = lock.Close()
	return NativeRoutingRepairReceipt{
		SchemaVersion: nativeRepairReceiptSchema, Status: c.Status(ctx), BackupCreated: backupCreated,
		NonRoutingCleanupIncomplete: nonRoutingIncomplete, OwnerRestoreAttempts: attempts, OwnerRestoreResult: ownerResult,
	}, nil
}

func validNativeOwnerStatus(status NativeRepairOwnerStatus) bool {
	switch status.Configuration {
	case NativeOwnerConfigurationValid:
		return status.Reason == NativeOwnerReasonReady &&
			(status.Integration == NativeOwnerIntegrationEnabled || status.Integration == NativeOwnerIntegrationDisabled)
	case NativeOwnerConfigurationInvalid:
		return status.Reason == NativeOwnerReasonConfiguration && status.Integration == NativeOwnerIntegrationUnknown
	case NativeOwnerConfigurationUnavailable:
		return status.Reason == NativeOwnerReasonUnavailable && status.Integration == NativeOwnerIntegrationUnknown
	default:
		return false
	}
}

func (c *Controller) revalidateNativeRepairAttempt(expectedGeneration uint64, inspection codexconfig.NativeRepairInspection) error {
	state, current, err := c.inspectNativeRepairLocked(expectedGeneration)
	if err != nil {
		return err
	}
	if state.Generation != expectedGeneration || current.Kind != inspection.Kind {
		return ErrNativeRepairOwnerChanged
	}
	if err := codexconfig.RevalidateNativeRepairInspection(c.codexConfigPath, c.codexOwner, inspection); err != nil {
		return ErrNativeRepairArtifactsChanged
	}
	return nil
}

func (c *Controller) inspectNativeRepairLocked(expectedGeneration uint64) (State, codexconfig.NativeRepairInspection, error) {
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return State{}, codexconfig.NativeRepairInspection{}, ErrNativeRepairUnavailable
	}
	if pending, err := c.store.HasPendingTransaction(); err != nil || pending {
		return State{}, codexconfig.NativeRepairInspection{}, ErrNativeRepairUnavailable
	}
	state, legacy, err := c.store.Read()
	if err != nil || legacy || state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil || state.Phase != PhaseRecoveryRequired {
		return State{}, codexconfig.NativeRepairInspection{}, ErrNativeRepairUnavailable
	}
	if state.Generation != expectedGeneration || state.Generation == ^uint64(0) {
		return State{}, codexconfig.NativeRepairInspection{}, ErrNativeRepairGenerationStale
	}
	inspection, err := codexconfig.InspectNativeRepairForOwner(c.codexConfigPath, c.codexOwner)
	if err != nil {
		return State{}, codexconfig.NativeRepairInspection{}, ErrNativeRepairUnavailable
	}
	return state, inspection, nil
}
