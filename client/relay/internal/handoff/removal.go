package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	OpenCodexRemovalSchemaVersion   = 2
	OpenCodexInventorySchemaVersion = 2
	maxRemovalDataItems             = 128
	maxRemovalInventoryItems        = 512
	maxRemovalComponents            = 64
	maxRemovalTokenBytes            = 64
	maxRemovalMessageBytes          = 300
)

var (
	ErrInvalidRemovalRequest     = errors.New("invalid OpenCodex removal request")
	ErrRemovalConfirmationNeeded = errors.New("OpenCodex removal confirmation is required")
	ErrRemovalReceiptInvalid     = errors.New("OpenCodex removal receipt is invalid")
	ErrRemovalRoutingChanged     = errors.New("Codex routing ownership changed during OpenCodex removal")
)

type OpenCodexRemovalMode string

const (
	RemovalModePreserveData  OpenCodexRemovalMode = "preserve_data"
	RemovalModeTrashSelected OpenCodexRemovalMode = "trash_selected"
)

type OpenCodexRemovalStatus string

const (
	RemovalStatusCompleted OpenCodexRemovalStatus = "completed"
	RemovalStatusPartial   OpenCodexRemovalStatus = "partial"
	RemovalStatusFailed    OpenCodexRemovalStatus = "failed"
)

type RemovalStageStatus string

const (
	RemovalStageCompleted RemovalStageStatus = "completed"
	RemovalStageSkipped   RemovalStageStatus = "skipped"
	RemovalStageRefused   RemovalStageStatus = "refused"
	RemovalStageFailed    RemovalStageStatus = "failed"
)

type OpenCodexRemovalRequest struct {
	Selection                 NPMRemovalSelection
	Mode                      OpenCodexRemovalMode
	DataItemIDs               []string
	ExpectedRoutingGeneration uint64
	ExpectedInventoryRevision string
	ConfirmedRemoval          bool
	ConfirmedTrash            bool
	Context                   RemovalContext
	ExpectedBoundaryRevision  string
	ExpectedNativeState       string
	NativeRestoreFingerprint  string
	NativeInventoryRevision   string
}

type OpenCodexRemovalStage struct {
	Stage     string             `json:"stage"`
	Status    RemovalStageStatus `json:"status"`
	Code      string             `json:"code"`
	SubjectID string             `json:"subject_id,omitempty"`
}

// OpenCodexRemovalReceipt is intentionally path-free and message-free. The UI
// localizes finite stage/code tokens and refreshes ordinary routing status after
// completion. No child stderr or upstream message can cross this boundary.
type OpenCodexRemovalReceipt struct {
	SchemaVersion           int                     `json:"schema_version"`
	Operation               string                  `json:"operation"`
	Status                  OpenCodexRemovalStatus  `json:"status"`
	Mode                    OpenCodexRemovalMode    `json:"mode"`
	InstallationID          string                  `json:"installation_id"`
	DataScope               string                  `json:"data_scope"`
	SelectedDataItems       int                     `json:"selected_data_items"`
	MovedDataItems          int                     `json:"moved_data_items"`
	PackageRemoved          bool                    `json:"package_removed"`
	DataMovementUnknown     bool                    `json:"data_movement_unknown"`
	RoutingRecoveryRequired bool                    `json:"routing_recovery_required"`
	PermanentDeleteFallback bool                    `json:"permanent_delete_fallback"`
	Stages                  []OpenCodexRemovalStage `json:"stages"`
}

type OpenCodexDataInventoryItem struct {
	ID           string `json:"id"`
	Category     string `json:"category"`
	Scope        string `json:"scope"`
	Kind         string `json:"kind"`
	RelativePath string `json:"relative_path"`
	Exists       bool   `json:"exists"`
	Sensitive    bool   `json:"sensitive"`
	Trashable    bool   `json:"trashable"`
}

type OpenCodexDataInventoryReceipt struct {
	SchemaVersion           int                          `json:"schema_version"`
	Operation               string                       `json:"operation"`
	Status                  string                       `json:"status"`
	InstallationID          string                       `json:"installation_id"`
	InstallationFingerprint string                       `json:"installation_fingerprint"`
	InventoryRevision       string                       `json:"inventory_revision"`
	RoutingGeneration       uint64                       `json:"routing_generation"`
	Items                   []OpenCodexDataInventoryItem `json:"items"`
}

type RemovalCoordinator struct {
	Resolver                 RemovalInstallationResolver
	Runner                   OpenCodexRemovalRunner
	CheckAdmission           func(context.Context) error
	CheckResumeAdmission     func(context.Context) error
	VerifyRouting            func(context.Context) error
	VerifyPostTeardown       func(context.Context) error
	MarkRoutingRecovery      func() error
	RestoreNative            func(context.Context, NPMInstallation) (NativeRestoreResult, error)
	CompleteTeardown         func(context.Context, NPMInstallation, RemovalExecutionResult) error
	CompleteNativeRestore    func(context.Context, NPMInstallation, NativeRestoreResult) error
	TeardownAlreadyCompleted bool
	NativeBoundaryVerified   bool
	NativeAlreadyVerified    bool
	PrepareOperation         func(context.Context, NPMInstallation, OpenCodexRemovalRequest) error
	RecordDataOutcome        func(context.Context, NPMInstallation, OpenCodexRemovalRequest, int, string) error
	MarkDataRefresh          func(context.Context) error
	PreparePackageRemoval    func(context.Context, NPMInstallation, OpenCodexRemovalRequest, int) error
	PreparePackageExecution  func(context.Context) error
	CompletePackageExecution func(context.Context, RemovalExecutionResult) error
	BeginExecution           func(context.Context, RemovalExecutionKind) error
	FinishExecution          func(context.Context, RemovalExecutionKind, RemovalExecutionResult) error
	ResolveExecution         func(context.Context, RemovalExecutionKind, RemovalExecutionResolution, bool) (bool, error)
}

func (c RemovalCoordinator) Inventory(ctx context.Context, selection NPMRemovalSelection, routingGeneration uint64) (OpenCodexDataInventoryReceipt, error) {
	if c.Resolver == nil || c.Runner == nil || !validRemovalSelection(selection) || routingGeneration == 0 {
		return OpenCodexDataInventoryReceipt{}, ErrInvalidRemovalRequest
	}
	if c.CheckAdmission != nil {
		if err := c.CheckAdmission(ctx); err != nil {
			return OpenCodexDataInventoryReceipt{}, err
		}
	}
	candidate, err := c.Resolver.Resolve(ctx, selection)
	if err != nil {
		return OpenCodexDataInventoryReceipt{}, err
	}
	if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
		return OpenCodexDataInventoryReceipt{}, err
	}
	return c.inventoryForCandidate(ctx, candidate, routingGeneration)
}

func (c RemovalCoordinator) inventoryForCandidate(ctx context.Context, candidate NPMInstallation, routingGeneration uint64) (OpenCodexDataInventoryReceipt, error) {
	result, err := c.Runner.Inventory(ctx, candidate)
	if err != nil {
		return OpenCodexDataInventoryReceipt{}, err
	}
	inventory, err := parseUpstreamInventory(result)
	if err != nil {
		return OpenCodexDataInventoryReceipt{}, err
	}
	receipt := OpenCodexDataInventoryReceipt{
		SchemaVersion:           OpenCodexInventorySchemaVersion,
		Operation:               "open-codex-data-inventory",
		Status:                  inventory.Status,
		InstallationID:          candidate.ID,
		InstallationFingerprint: candidate.Fingerprint,
		RoutingGeneration:       routingGeneration,
		Items:                   inventory.Items,
	}
	receipt.InventoryRevision = inventoryRevision(receipt)
	return receipt, nil
}

func inventoryRevision(receipt OpenCodexDataInventoryReceipt) string {
	payload, _ := json.Marshal(receipt.Items)
	hash := sha256.New()
	_, _ = hash.Write([]byte("opencodex-inventory-v2\x00"))
	for _, value := range []string{
		receipt.InstallationID,
		receipt.InstallationFingerprint,
		strconv.FormatUint(receipt.RoutingGeneration, 10),
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

func (c RemovalCoordinator) Remove(ctx context.Context, request OpenCodexRemovalRequest) OpenCodexRemovalReceipt {
	receipt := newRemovalReceipt(request)
	if err := validateRemovalRequest(request); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("request_validation", RemovalStageRefused, removalErrorCode(err), ""))
		return receipt
	}
	standaloneNative := request.Context == RemovalContextStandaloneNative
	if c.Resolver == nil || c.Runner == nil || c.VerifyRouting == nil || c.PrepareOperation == nil || c.RecordDataOutcome == nil ||
		c.PreparePackageRemoval == nil || !c.executionHooksReady() ||
		standaloneNative && (c.VerifyPostTeardown == nil || c.RestoreNative == nil ||
			c.CompleteTeardown == nil || c.CompleteNativeRestore == nil) {
		receipt.Stages = append(receipt.Stages, removalStage("request_validation", RemovalStageFailed, "coordinator_unavailable", ""))
		return receipt
	}
	if c.CheckAdmission != nil {
		if err := c.CheckAdmission(ctx); err != nil {
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageRefused, "removal_in_flight", ""))
			return receipt
		}
	}

	candidate, err := c.Resolver.Resolve(ctx, request.Selection)
	if err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageRefused, removalErrorCode(err), request.Selection.ID))
		return receipt
	}
	receipt.InstallationID = candidate.ID
	receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageCompleted, "candidate_verified", candidate.ID))
	if request.Mode == RemovalModeTrashSelected && candidate.DataCapability != DataCapabilitySelectiveTrashV1 {
		receipt.Stages = append(receipt.Stages, removalStage("data_policy", RemovalStageRefused, "teardown_unsupported", candidate.ID))
		return receipt
	}

	if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageRefused, removalErrorCode(err), candidate.ID))
		return receipt
	}
	if validator, ok := c.Resolver.(LiveRemovalInstallationValidator); ok {
		if err := validator.ValidateForMutation(ctx, candidate); err != nil {
			receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageRefused, removalErrorCode(err), candidate.ID))
			return receipt
		}
	}
	preflightResult, preflightErr := c.Runner.Preflight(ctx, candidate)
	if preflightErr != nil || !preflightResult.Started || !preflightResult.CleanupVerified {
		code := removalExecutionFailureCode(preflightResult, preflightErr, "teardown_preflight_failed")
		receipt.Stages = append(receipt.Stages, removalStage(
			"teardown_preflight", removalExecutionStageStatus(preflightResult, preflightErr), code, candidate.ID,
		))
		return receipt
	}
	preflight, err := parseRelayTeardownReceipt(
		preflightResult,
		"relay_preserving_teardown_preflight",
		candidate.TeardownAdapterID,
	)
	if err != nil {
		receipt.Stages = append(receipt.Stages, removalStage(
			"teardown_preflight", RemovalStageRefused, removalErrorCode(err), candidate.ID,
		))
		return receipt
	}
	if preflight.Status != "ready" {
		receipt.Stages = append(receipt.Stages, removalStage(
			"teardown_preflight", RemovalStageRefused, "teardown_preflight_failed", candidate.ID,
		))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage(
		"teardown_preflight", RemovalStageCompleted, "teardown_preflight_verified", candidate.ID,
	))
	if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage(
			"candidate_revalidation", RemovalStageRefused, "teardown_candidate_changed", candidate.ID,
		))
		return receipt
	}
	if err := c.PrepareOperation(ctx, candidate, request); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "cleanup_intent_unavailable", candidate.ID))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "cleanup_intent_persisted", candidate.ID))
	teardownCompleted := c.TeardownAlreadyCompleted
	nativeBoundaryVerified := c.NativeBoundaryVerified
	nativeAlreadyVerified := c.NativeAlreadyVerified
	if nativeAlreadyVerified {
		if !standaloneNative || !teardownCompleted || !nativeBoundaryVerified ||
			c.RestoreNative == nil || c.CompleteNativeRestore == nil || c.CompleteTeardown == nil {
			receipt.Stages = append(receipt.Stages, removalStage("request_validation", RemovalStageFailed, "coordinator_unavailable", ""))
			return receipt
		}
		if err := c.VerifyRouting(ctx); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_changed", ""))
			c.requireRoutingRecovery(&receipt)
			return receipt
		}
		receipt.Stages = append(receipt.Stages,
			removalStage("teardown", RemovalStageSkipped, "teardown_already_completed", candidate.ID),
			removalStage("native_restore", RemovalStageSkipped, "native_already_active", candidate.ID),
			removalStage("routing_verification", RemovalStageCompleted, "routing_ownership_reverified", ""),
		)
	}
	if !nativeAlreadyVerified {
		if !teardownCompleted {
			if err := c.VerifyRouting(ctx); err != nil {
				receipt.Stages = append(receipt.Stages, removalStage("routing_pre_teardown", RemovalStageFailed, "routing_ownership_unverified", ""))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			receipt.Stages = append(receipt.Stages, removalStage("routing_pre_teardown", RemovalStageCompleted, "routing_ownership_verified", ""))

			if err := c.beginExecution(ctx, RemovalExecutionTeardown); err != nil {
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "teardown_execution_intent_unavailable", candidate.ID))
				return receipt
			}
			teardownResult, runErr := c.Runner.Teardown(ctx, candidate)
			var teardownRoutingErr error
			if teardownResult.Started {
				// Observe routing immediately after every started mutating child, even
				// when the child reports an error or emits an invalid receipt.
				verify := c.VerifyRouting
				if c.VerifyPostTeardown != nil {
					verify = c.VerifyPostTeardown
				}
				teardownRoutingErr = verify(ctx)
			}
			if removalExecutionPreStartRoutingChanged(teardownResult, runErr) {
				receipt.Stages = append(receipt.Stages, removalStage(
					"teardown", RemovalStageRefused, "routing_ownership_changed", candidate.ID,
				))
				c.resolveExecution(ctx, &receipt, RemovalExecutionTeardown, RemovalExecutionResolutionPreStartRoutingChanged, true)
				return receipt
			}
			if runErr != nil || !teardownResult.Started || !teardownResult.CleanupVerified {
				var finishErr error
				if teardownResult.Started || !removalExecutionMayHaveMutated(teardownResult, runErr) {
					finishErr = c.finishExecution(ctx, RemovalExecutionTeardown, teardownResult)
				}
				code := removalExecutionFailureCode(teardownResult, runErr, "teardown_not_started")
				receipt.Stages = append(receipt.Stages, removalStage("teardown", removalExecutionStageStatus(teardownResult, runErr), code, candidate.ID))
				if finishErr != nil {
					receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "teardown_execution_result_unavailable", candidate.ID))
				}
				c.recordPostMutationRouting(&receipt, "routing_verification", teardownResult.Started, teardownRoutingErr)
				if removalExecutionMayHaveMutated(teardownResult, runErr) {
					c.requireRoutingRecovery(&receipt)
				}
				return receipt
			}
			teardown, err := parseRelayTeardownReceipt(
				teardownResult,
				"relay_preserving_teardown",
				candidate.TeardownAdapterID,
			)
			if err != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("teardown", RemovalStageFailed, removalErrorCode(err), candidate.ID))
				if teardownRoutingErr != nil {
					receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_changed", ""))
				} else {
					receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageCompleted, "routing_ownership_reverified", ""))
				}
				c.resolveExecution(ctx, &receipt, RemovalExecutionTeardown, RemovalExecutionResolutionTeardownReceiptInvalid, true)
				return receipt
			}
			if standaloneNative && teardown.Status == "completed" {
				if err := c.CompleteTeardown(ctx, candidate, teardownResult); err != nil {
					receipt.Status = RemovalStatusPartial
					receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "teardown_completion_unavailable", candidate.ID))
					c.requireRoutingRecovery(&receipt)
					return receipt
				}
				teardownCompleted = true
				nativeBoundaryVerified = true
				nativeAlreadyVerified = true
			} else if err := c.finishExecution(ctx, RemovalExecutionTeardown, teardownResult); err != nil {
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "teardown_execution_result_unavailable", candidate.ID))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			teardownStageStatus := RemovalStageFailed
			switch teardown.Status {
			case "completed":
				teardownStageStatus = RemovalStageCompleted
			case "partial":
				teardownStageStatus = RemovalStageRefused
			}
			teardownCode := "teardown_" + teardown.Status
			if teardown.Status != "completed" {
				teardownCode = "teardown_refused"
			}
			receipt.Stages = append(receipt.Stages, removalStage("teardown", teardownStageStatus, teardownCode, candidate.ID))
			if !c.recordPostMutationRouting(&receipt, "routing_verification", true, teardownRoutingErr) {
				if teardown.Status == "completed" || teardown.Status == "partial" {
					receipt.Status = RemovalStatusPartial
				}
				return receipt
			}
			if teardown.Status != "completed" {
				if teardown.Status == "partial" {
					receipt.Status = RemovalStatusPartial
				}
				if standaloneNative {
					c.requireRoutingRecovery(&receipt)
				}
				return receipt
			}
		} else {
			receipt.Stages = append(receipt.Stages, removalStage("teardown", RemovalStageSkipped, "teardown_already_completed", candidate.ID))
		}
		if standaloneNative && nativeBoundaryVerified {
			if err := c.VerifyRouting(ctx); err != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_changed", ""))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			receipt.Stages = append(receipt.Stages,
				removalStage("native_restore", RemovalStageSkipped, "native_already_active", candidate.ID),
				removalStage("routing_verification", RemovalStageCompleted, "routing_ownership_reverified", ""),
			)
		} else if c.RestoreNative != nil {
			if err := c.beginExecution(ctx, RemovalExecutionNativeRestore); err != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "native_restore_execution_intent_unavailable", candidate.ID))
				return receipt
			}
			restored, restoreErr := c.RestoreNative(ctx, candidate)
			if restoreErr != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("native_restore", RemovalStageFailed, "native_restore_unverified", candidate.ID))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			if c.CompleteNativeRestore == nil || c.CompleteNativeRestore(ctx, candidate, restored) != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("native_restore", RemovalStageFailed, "native_restore_unverified", candidate.ID))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			code := "native_restore_applied"
			if restored.Outcome == NativeRestoreAlreadyNative {
				code = "native_already_active"
			}
			receipt.Stages = append(receipt.Stages, removalStage("native_restore", RemovalStageCompleted, code, candidate.ID))
			if err := c.VerifyRouting(ctx); err != nil {
				receipt.Status = RemovalStatusPartial
				receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_changed", ""))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageCompleted, "routing_ownership_reverified", ""))
		}
	}

	if request.Mode == RemovalModeTrashSelected {
		if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageRefused, removalErrorCode(err), candidate.ID))
			return receipt
		}
		if err := c.VerifyRouting(ctx); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("routing_pre_trash", RemovalStageFailed, "routing_ownership_changed", ""))
			c.requireRoutingRecovery(&receipt)
			return receipt
		}
		receipt.Stages = append(receipt.Stages, removalStage("routing_pre_trash", RemovalStageCompleted, "routing_ownership_reverified", ""))
		if err := c.beginExecution(ctx, RemovalExecutionTrash); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "trash_execution_intent_unavailable", candidate.ID))
			return receipt
		}
		trashResult, trashRunErr := c.Runner.Trash(ctx, candidate, request.DataItemIDs)
		var trashRoutingErr error
		if trashResult.Started {
			trashRoutingErr = c.VerifyRouting(ctx)
		}
		if removalExecutionPreStartRoutingChanged(trashResult, trashRunErr) {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageRefused, "routing_ownership_changed", ""))
			c.resolveExecution(ctx, &receipt, RemovalExecutionTrash, RemovalExecutionResolutionPreStartRoutingChanged, true)
			return receipt
		}
		if trashRunErr != nil || !trashResult.Started || !trashResult.CleanupVerified {
			var finishErr error
			if trashResult.Started || !removalExecutionMayHaveMutated(trashResult, trashRunErr) {
				finishErr = c.finishExecution(ctx, RemovalExecutionTrash, trashResult)
			}
			receipt.Status = RemovalStatusPartial
			receipt.DataMovementUnknown = trashResult.Started
			code := removalExecutionFailureCode(trashResult, trashRunErr, "trash_not_started")
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", removalExecutionStageStatus(trashResult, trashRunErr), code, ""))
			if finishErr != nil {
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "trash_execution_result_unavailable", candidate.ID))
			}
			c.recordPostMutationRouting(&receipt, "routing_post_trash", trashResult.Started, trashRoutingErr)
			if removalExecutionMayHaveMutated(trashResult, trashRunErr) {
				c.requireRoutingRecovery(&receipt)
			}
			return receipt
		}
		trash, parseErr := parseUpstreamTrash(trashResult, request.DataItemIDs)
		if parseErr != nil {
			receipt.Status = RemovalStatusPartial
			receipt.DataMovementUnknown = true
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageFailed, "trash_receipt_invalid", ""))
			if trashRoutingErr != nil {
				receipt.Stages = append(receipt.Stages, removalStage("routing_post_trash", RemovalStageFailed, "routing_ownership_changed", ""))
			} else {
				receipt.Stages = append(receipt.Stages, removalStage("routing_post_trash", RemovalStageCompleted, "routing_ownership_reverified", ""))
			}
			if c.resolveExecution(ctx, &receipt, RemovalExecutionTrash, RemovalExecutionResolutionTrashReceiptInvalid, trashRoutingErr != nil) &&
				trashRoutingErr != nil {
				// Routing recovery must complete before the data-refresh phase
				// can admit inventory. The durable journal already retains the
				// retired selection; omit the refresh signal from this receipt so
				// the UI cannot try inventory against the still-closed gate.
				receipt.DataMovementUnknown = false
			}
			return receipt
		}
		if err := c.finishExecution(ctx, RemovalExecutionTrash, trashResult); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.DataMovementUnknown = true
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "trash_execution_result_unavailable", candidate.ID))
			c.requireRoutingRecovery(&receipt)
			return receipt
		}
		receipt.MovedDataItems = len(trash.Moved)
		if trash.Status == "unsupported" {
			receipt.Status = RemovalStatusPartial
			routingStable := c.recordPostMutationRouting(&receipt, "routing_post_trash", true, trashRoutingErr)
			if err := c.RecordDataOutcome(ctx, candidate, request, receipt.MovedDataItems, trash.Status); err != nil {
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "data_outcome_journal_unavailable", candidate.ID))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "data_outcome_persisted", candidate.ID))
			if c.MarkDataRefresh == nil || c.MarkDataRefresh(ctx) != nil {
				receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "cleanup_journal_unavailable", candidate.ID))
				c.requireRoutingRecovery(&receipt)
				return receipt
			}
			if !routingStable {
				return receipt
			}
			receipt.DataMovementUnknown = true
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageRefused, "data_selection_refresh_required", ""))
			return receipt
		}
		trashStageStatus := RemovalStageCompleted
		trashCode := "trash_completed"
		subject := ""
		if trash.Status != "completed" {
			receipt.Status = RemovalStatusPartial
			trashStageStatus = RemovalStageRefused
			if trash.Status == "failed" {
				trashStageStatus = RemovalStageFailed
			}
			trashCode = "trash_" + trash.Status
			if len(trash.Failures) == 1 {
				subject = trash.Failures[0].ItemID
			}
		}
		receipt.Stages = append(receipt.Stages, removalStage("data_trash", trashStageStatus, trashCode, subject))
		routingStable := c.recordPostMutationRouting(&receipt, "routing_post_trash", true, trashRoutingErr)
		if err := c.RecordDataOutcome(ctx, candidate, request, receipt.MovedDataItems, trash.Status); err != nil {
			receipt.Status = RemovalStatusPartial
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "data_outcome_journal_unavailable", candidate.ID))
			c.requireRoutingRecovery(&receipt)
			return receipt
		}
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "data_outcome_persisted", candidate.ID))
		if trash.Status != "completed" || !routingStable {
			return receipt
		}
	} else {
		receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageSkipped, "data_preserved", ""))
	}

	if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("npm_uninstall", RemovalStageRefused, removalErrorCode(err), candidate.ID))
		return receipt
	}
	if err := c.VerifyRouting(ctx); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("routing_reverification", RemovalStageFailed, "routing_ownership_changed", ""))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_reverification", RemovalStageCompleted, "routing_ownership_reverified", ""))
	if err := c.PreparePackageRemoval(ctx, candidate, request, receipt.MovedDataItems); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "cleanup_journal_unavailable", candidate.ID))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "cleanup_journal_persisted", candidate.ID))
	if err := c.beginExecution(ctx, RemovalExecutionPackage); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "package_execution_intent_unavailable", candidate.ID))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_in_flight", candidate.ID))

	npmResult, npmRunErr := c.Runner.Uninstall(ctx, candidate)
	var npmRoutingErr error
	if npmResult.Started {
		npmRoutingErr = c.VerifyRouting(ctx)
	}
	if removalExecutionPreStartRoutingChanged(npmResult, npmRunErr) {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages,
			removalStage("npm_uninstall", RemovalStageRefused, "routing_ownership_changed", candidate.ID),
			removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_not_started", candidate.ID),
		)
		c.resolveExecution(ctx, &receipt, RemovalExecutionPackage, RemovalExecutionResolutionPreStartRoutingChanged, true)
		return receipt
	}
	var packageExecutionErr error
	if npmResult.Started || !removalExecutionMayHaveMutated(npmResult, npmRunErr) {
		packageExecutionErr = c.finishExecution(ctx, RemovalExecutionPackage, npmResult)
	} else {
		packageExecutionErr = ErrRemovalProcessCleanup
	}
	npmCleanupVerified := npmResult.Started && npmResult.CleanupVerified && packageExecutionErr == nil
	npmSucceeded := npmRunErr == nil && npmCleanupVerified && npmResult.ExitCode == 0
	if npmSucceeded {
		receipt.Stages = append(receipt.Stages, removalStage("npm_uninstall", RemovalStageCompleted, "npm_uninstall_completed", candidate.ID))
	} else {
		receipt.Status = RemovalStatusPartial
		code := removalExecutionFailureCode(npmResult, npmRunErr, "npm_uninstall_not_started")
		if npmRunErr == nil && npmCleanupVerified && npmResult.ExitCode != 0 {
			code = "npm_uninstall_failed"
		}
		receipt.Stages = append(receipt.Stages, removalStage("npm_uninstall", removalExecutionStageStatus(npmResult, npmRunErr), code, candidate.ID))
	}
	if packageExecutionErr != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "package_execution_result_unavailable", candidate.ID))
		if npmResult.Started {
			c.requireRoutingRecovery(&receipt)
		}
	} else if npmResult.Started && npmResult.CleanupVerified {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_cleanup_verified", candidate.ID))
	} else if !npmResult.Started {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_not_started", candidate.ID))
	}
	if !npmResult.Started {
		return receipt
	}
	routingStable := c.recordPostMutationRouting(&receipt, "routing_post_verification", true, npmRoutingErr)
	if !npmCleanupVerified {
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	if err := c.Resolver.VerifyRemoved(candidate); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("package_verification", RemovalStageFailed, "package_removal_unverified", candidate.ID))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	if err := c.VerifyRouting(ctx); err != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage("routing_final_verification", RemovalStageFailed, "routing_ownership_changed", ""))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_final_verification", RemovalStageCompleted, "routing_ownership_reverified", ""))
	receipt.PackageRemoved = true
	if npmSucceeded && routingStable {
		receipt.Status = RemovalStatusCompleted
	} else {
		receipt.Status = RemovalStatusPartial
		c.requireRoutingRecovery(&receipt)
	}
	receipt.Stages = append(receipt.Stages, removalStage("package_verification", RemovalStageCompleted, "package_absent", candidate.ID))
	return receipt
}

func (c RemovalCoordinator) executionHooksReady() bool {
	// Every mutating execution kind must pass through the typed durable
	// begin/finish pair.  The legacy package-only callbacks remain on the
	// struct solely for source compatibility with older callers, but they are
	// never an admission path: allowing them to stand in here would let
	// teardown or Trash execute without an attested active-execution witness.
	return c.BeginExecution != nil && c.FinishExecution != nil
}

func (c RemovalCoordinator) beginExecution(ctx context.Context, kind RemovalExecutionKind) error {
	if c.BeginExecution != nil {
		return c.BeginExecution(ctx, kind)
	}
	return ErrRemovalCleanupUnsafe
}

func (c RemovalCoordinator) finishExecution(ctx context.Context, kind RemovalExecutionKind, result RemovalExecutionResult) error {
	if c.FinishExecution != nil {
		return c.FinishExecution(ctx, kind, result)
	}
	return ErrRemovalCleanupUnsafe
}

func (c RemovalCoordinator) resolveExecution(
	ctx context.Context,
	receipt *OpenCodexRemovalReceipt,
	kind RemovalExecutionKind,
	resolution RemovalExecutionResolution,
	parkRouting bool,
) bool {
	if receipt == nil {
		return false
	}
	if c.ResolveExecution == nil {
		if parkRouting {
			receipt.RoutingRecoveryRequired = true
			receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""))
		} else {
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, resolutionFailureCode(kind), ""))
		}
		return false
	}
	routingParked, err := c.ResolveExecution(ctx, kind, resolution, parkRouting)
	if err != nil {
		appendUnresolvedExecutionStage(receipt, kind)
		if parkRouting {
			receipt.RoutingRecoveryRequired = true
			receipt.Stages = append(receipt.Stages,
				removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""),
			)
			if routingParked {
				receipt.Stages = append(receipt.Stages,
					removalStage("cleanup_journal", RemovalStageFailed, resolutionFailureCode(kind), ""),
				)
			}
		} else {
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, resolutionFailureCode(kind), ""))
		}
		return false
	}
	if routingParked != parkRouting {
		if parkRouting {
			receipt.RoutingRecoveryRequired = true
			receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""))
		} else {
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, resolutionFailureCode(kind), ""))
		}
		return false
	}
	if parkRouting {
		receipt.RoutingRecoveryRequired = true
		receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageCompleted, "routing_recovery_persisted", ""))
	}
	return true
}

func appendUnresolvedExecutionStage(receipt *OpenCodexRemovalReceipt, kind RemovalExecutionKind) {
	if receipt == nil {
		return
	}
	stage := "npm_uninstall"
	subject := receipt.InstallationID
	switch kind {
	case RemovalExecutionTeardown:
		stage = "teardown"
	case RemovalExecutionTrash:
		stage = "data_trash"
		subject = ""
		receipt.DataMovementUnknown = true
	case RemovalExecutionPackage:
	default:
		return
	}
	receipt.Stages = append(receipt.Stages,
		removalStage(stage, RemovalStageFailed, "process_cleanup_unverified", subject),
	)
}

func resolutionFailureCode(kind RemovalExecutionKind) string {
	switch kind {
	case RemovalExecutionTrash:
		return "trash_execution_result_unavailable"
	case RemovalExecutionTeardown:
		return "teardown_execution_result_unavailable"
	case RemovalExecutionPackage:
		return "package_execution_result_unavailable"
	default:
		return "operation_failed"
	}
}

func removalExecutionFailureCode(result RemovalExecutionResult, runErr error, notStartedCode string) string {
	if result.Started && !result.CleanupVerified {
		return "process_cleanup_unverified"
	}
	if runErr != nil {
		return removalErrorCode(runErr)
	}
	if !result.Started {
		return notStartedCode
	}
	return "operation_failed"
}

func removalExecutionStageStatus(result RemovalExecutionResult, runErr error) RemovalStageStatus {
	if !result.Started && (errors.Is(runErr, ErrRemovalCandidateChanged) ||
		errors.Is(runErr, ErrRemovalCandidateMissing) || errors.Is(runErr, ErrRemovalManualOnly) ||
		errors.Is(runErr, ErrInvalidRemovalRequest)) {
		return RemovalStageRefused
	}
	return RemovalStageFailed
}

func removalExecutionPreStartRoutingChanged(result RemovalExecutionResult, runErr error) bool {
	return !result.Started && errors.Is(runErr, ErrRemovalRoutingChanged)
}

func removalExecutionMayHaveMutated(result RemovalExecutionResult, runErr error) bool {
	if result.Started {
		return true
	}
	if runErr == nil {
		return false
	}
	if errors.Is(runErr, ErrRemovalRoutingChanged) {
		return false
	}
	return !errors.Is(runErr, ErrRemovalCandidateChanged) &&
		!errors.Is(runErr, ErrRemovalCandidateMissing) &&
		!errors.Is(runErr, ErrRemovalManualOnly) &&
		!errors.Is(runErr, ErrInvalidRemovalRequest)
}

func (c RemovalCoordinator) recordPostMutationRouting(receipt *OpenCodexRemovalReceipt, stage string, started bool, routingErr error) bool {
	if !started {
		return true
	}
	if routingErr != nil {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages, removalStage(stage, RemovalStageFailed, "routing_ownership_changed", ""))
		c.requireRoutingRecovery(receipt)
		return false
	}
	receipt.Stages = append(receipt.Stages, removalStage(stage, RemovalStageCompleted, "routing_ownership_reverified", ""))
	return true
}

func (c RemovalCoordinator) ResumePackageRemoval(ctx context.Context, record RemovalCleanupRecord) OpenCodexRemovalReceipt {
	receipt := removalReceiptFromCleanup(record)
	if err := validateRemovalCleanupRecord(record); err != nil ||
		(record.Phase == RemovalCleanupPhasePackagePending &&
			!record.ProcessReconciledAfterReboot && !record.PackageRetryPending) ||
		(record.Phase == RemovalCleanupPhasePackageVerified && !record.RoutingRecoveryReleased) ||
		(record.Phase != RemovalCleanupPhasePackagePending && record.Phase != RemovalCleanupPhasePackageVerified) ||
		record.ActiveExecution != nil || c.Resolver == nil || c.Runner == nil || c.VerifyRouting == nil || !c.executionHooksReady() {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "cleanup_journal_invalid", ""))
		return receipt
	}
	admission := c.CheckResumeAdmission
	if admission == nil {
		admission = c.CheckAdmission
	}
	if admission != nil {
		if err := admission(ctx); err != nil {
			receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageRefused, "removal_in_flight", ""))
			return receipt
		}
	}
	if err := c.VerifyRouting(ctx); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_unverified", ""))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageCompleted, "routing_ownership_verified", ""))
	candidate, err := c.Resolver.Resolve(ctx, NPMRemovalSelection{ID: record.InstallationID, Fingerprint: record.Fingerprint})
	if err != nil || candidate.PackageRoot != record.PackageRoot || !sameStringSet(candidate.Launchers, record.Launchers) {
		receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageRefused, "candidate_changed", record.InstallationID))
		return receipt
	}
	if err := c.Resolver.Revalidate(ctx, candidate); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageRefused, removalErrorCode(err), record.InstallationID))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("candidate_revalidation", RemovalStageCompleted, "candidate_verified", record.InstallationID))
	if err := c.VerifyRouting(ctx); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("routing_reverification", RemovalStageFailed, "routing_ownership_changed", ""))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_reverification", RemovalStageCompleted, "routing_ownership_reverified", ""))
	if err := c.beginExecution(ctx, RemovalExecutionPackage); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "package_execution_intent_unavailable", candidate.ID))
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_in_flight", candidate.ID))

	npmResult, npmRunErr := c.Runner.Uninstall(ctx, candidate)
	var npmRoutingErr error
	if npmResult.Started {
		npmRoutingErr = c.VerifyRouting(ctx)
	}
	if removalExecutionPreStartRoutingChanged(npmResult, npmRunErr) {
		receipt.Status = RemovalStatusPartial
		receipt.Stages = append(receipt.Stages,
			removalStage("npm_uninstall", RemovalStageRefused, "routing_ownership_changed", record.InstallationID),
			removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_not_started", record.InstallationID),
		)
		c.resolveExecution(ctx, &receipt, RemovalExecutionPackage, RemovalExecutionResolutionPreStartRoutingChanged, true)
		return receipt
	}
	var packageExecutionErr error
	if npmResult.Started || !removalExecutionMayHaveMutated(npmResult, npmRunErr) {
		packageExecutionErr = c.finishExecution(ctx, RemovalExecutionPackage, npmResult)
	} else {
		packageExecutionErr = ErrRemovalProcessCleanup
	}
	npmCleanupVerified := npmResult.Started && npmResult.CleanupVerified && packageExecutionErr == nil
	npmSucceeded := npmRunErr == nil && npmCleanupVerified && npmResult.ExitCode == 0
	if npmSucceeded {
		receipt.Stages = append(receipt.Stages, removalStage("npm_uninstall", RemovalStageCompleted, "npm_uninstall_completed", candidate.ID))
	} else {
		code := removalExecutionFailureCode(npmResult, npmRunErr, "npm_uninstall_not_started")
		if npmRunErr == nil && npmCleanupVerified && npmResult.ExitCode != 0 {
			code = "npm_uninstall_failed"
		}
		receipt.Stages = append(receipt.Stages, removalStage("npm_uninstall", removalExecutionStageStatus(npmResult, npmRunErr), code, candidate.ID))
	}
	if packageExecutionErr != nil {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "package_execution_result_unavailable", candidate.ID))
		if npmResult.Started {
			c.requireRoutingRecovery(&receipt)
		}
	} else if npmResult.Started && npmResult.CleanupVerified {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_cleanup_verified", candidate.ID))
	} else if !npmResult.Started {
		receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_not_started", candidate.ID))
	}
	if !npmResult.Started {
		return receipt
	}
	routingStable := c.recordPostMutationRouting(&receipt, "routing_post_verification", true, npmRoutingErr)
	if !npmCleanupVerified {
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	if err := c.Resolver.VerifyRemoved(candidate); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("package_verification", RemovalStageFailed, "package_removal_unverified", candidate.ID))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	if err := c.VerifyRouting(ctx); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("routing_final_verification", RemovalStageFailed, "routing_ownership_changed", ""))
		c.requireRoutingRecovery(&receipt)
		return receipt
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_final_verification", RemovalStageCompleted, "routing_ownership_reverified", ""))
	receipt.PackageRemoved = true
	if npmSucceeded && routingStable {
		receipt.Status = RemovalStatusCompleted
	} else {
		c.requireRoutingRecovery(&receipt)
	}
	receipt.Stages = append(receipt.Stages, removalStage("package_verification", RemovalStageCompleted, "package_absent", candidate.ID))
	return receipt
}

func RemovedPackageCleanupReceipt(record RemovalCleanupRecord) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.Phase != RemovalCleanupPhasePackageVerified {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusCompleted
	receipt.PackageRemoved = true
	receipt.Stages = append(receipt.Stages, removalStage("package_verification", RemovalStageCompleted, "package_absent", record.InstallationID))
	return receipt, nil
}

func InterruptedRemovalDataRefreshReceipt(record RemovalCleanupRecord) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.RecoveryPending ||
		record.Phase != RemovalCleanupPhaseDataRefresh ||
		record.Mode != RemovalModeTrashSelected || record.DataOutcome != removalDataOutcomeUnknown {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.DataMovementUnknown = true
	receipt.Stages = append(receipt.Stages,
		removalStage("cleanup_journal", RemovalStageCompleted, "cleanup_intent_reconciled", record.InstallationID),
		removalStage("data_trash", RemovalStageRefused, "data_selection_refresh_required", ""),
	)
	return receipt, nil
}

func RecordedRemovalDataRefreshReceipt(record RemovalCleanupRecord) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.RecoveryPending ||
		record.Phase != RemovalCleanupPhaseDataRefresh ||
		record.Mode != RemovalModeTrashSelected || record.DataOutcome == removalDataOutcomeUnknown ||
		record.DataOutcome == "completed" {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.Stages = append(receipt.Stages,
		removalStage("cleanup_journal", RemovalStageCompleted, "data_outcome_reconciled", record.InstallationID),
		removalStage("data_trash", RemovalStageRefused, "data_selection_refresh_required", ""),
	)
	return receipt, nil
}

// ResolvedRemovalExecutionReceipt converts an exact durable resolution
// transition into a bounded receipt. It never describes a known-resolved child
// as process_cleanup_unverified. Trash refresh is surfaced only after any
// routing recovery gate has been released.
func ResolvedRemovalExecutionReceipt(
	before RemovalCleanupRecord,
	after RemovalCleanupRecord,
	recoveryPersisted bool,
) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(before); err != nil {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	if err := validateRemovalCleanupRecord(after); err != nil {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	pending, found, err := PendingRemovalExecutionResolution(before)
	if err != nil || !found || pending.RequiresRoutingRecovery != recoveryPersisted {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	expected, err := resolvedRemovalExecutionRecord(before)
	if err != nil || !sameRemovalCleanupRecord(expected, after) ||
		after.RecoveryPending != pending.RequiresRoutingRecovery {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}

	receipt := removalReceiptFromCleanup(after)
	receipt.Status = RemovalStatusPartial
	switch pending.Resolution {
	case RemovalExecutionResolutionPreStartRoutingChanged:
		switch pending.Kind {
		case RemovalExecutionTeardown:
			receipt.Stages = append(receipt.Stages, removalStage("teardown", RemovalStageRefused, "routing_ownership_changed", after.InstallationID))
		case RemovalExecutionTrash:
			receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageRefused, "routing_ownership_changed", ""))
		case RemovalExecutionPackage:
			receipt.Stages = append(receipt.Stages,
				removalStage("npm_uninstall", RemovalStageRefused, "routing_ownership_changed", after.InstallationID),
				removalStage("cleanup_journal", RemovalStageCompleted, "package_execution_not_started", after.InstallationID),
			)
		default:
			return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
		}
	case RemovalExecutionResolutionTeardownReceiptInvalid:
		receipt.Stages = append(receipt.Stages, removalStage("teardown", RemovalStageFailed, "teardown_receipt_invalid", after.InstallationID))
	case RemovalExecutionResolutionTrashReceiptInvalid:
		receipt.Stages = append(receipt.Stages, removalStage("data_trash", RemovalStageFailed, "trash_receipt_invalid", ""))
	default:
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}

	if pending.RequiresRoutingRecovery {
		receipt.RoutingRecoveryRequired = true
		appendRemovalRecoveryPersistence(&receipt, recoveryPersisted)
		return receipt, nil
	}
	if pending.Resolution == RemovalExecutionResolutionTrashReceiptInvalid {
		receipt.DataMovementUnknown = true
		receipt.Stages = append(receipt.Stages,
			removalStage("cleanup_journal", RemovalStageCompleted, "cleanup_intent_reconciled", after.InstallationID),
			removalStage("data_trash", RemovalStageRefused, "data_selection_refresh_required", ""),
		)
	}
	return receipt, nil
}

func InterruptedRemovalProcessReceipt(record RemovalCleanupRecord, recoveryPersisted bool) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.Phase != RemovalCleanupPhasePackageInFlight ||
		record.ActiveExecution == nil || record.ActiveExecution.Kind != RemovalExecutionPackage {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	return InterruptedRemovalExecutionReceipt(record, recoveryPersisted)
}

// InterruptedRemovalExecutionReceipt converts a typed active child into a
// bounded recovery receipt without guessing whether its mutation completed.
func InterruptedRemovalExecutionReceipt(record RemovalCleanupRecord, recoveryPersisted bool) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.ActiveExecution == nil {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.RoutingRecoveryRequired = true
	stage := "npm_uninstall"
	code := "process_cleanup_unverified"
	switch record.ActiveExecution.Kind {
	case RemovalExecutionTeardown:
		stage = "teardown"
	case RemovalExecutionNativeRestore:
		stage = "native_restore"
	case RemovalExecutionTrash:
		stage = "data_trash"
		receipt.DataMovementUnknown = true
	default:
	}
	receipt.Stages = append(receipt.Stages, removalStage(stage, RemovalStageFailed, code, record.InstallationID))
	appendRemovalRecoveryPersistence(&receipt, recoveryPersisted)
	return receipt, nil
}

func RemovalCleanupRoutingRecoveryReceipt(record RemovalCleanupRecord, recoveryPersisted bool) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.Phase != RemovalCleanupPhasePackageVerified {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.RoutingRecoveryRequired = true
	receipt.Stages = append(receipt.Stages, removalStage("routing_verification", RemovalStageFailed, "routing_ownership_unverified", ""))
	appendRemovalRecoveryPersistence(&receipt, recoveryPersisted)
	return receipt, nil
}

// PendingRemovalRoutingRecoveryReceipt reports a fully resolved execution whose
// routing recovery is still pending. Data-refresh evidence remains only in the
// owner-only journal until routing recovery completes, preventing the UI from
// attempting inventory while the recovery gate is deliberately closed.
func PendingRemovalRoutingRecoveryReceipt(record RemovalCleanupRecord) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || !record.RecoveryPending ||
		record.ActiveExecution != nil || record.ExecutionResolution != "" {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.RoutingRecoveryRequired = true
	receipt.Stages = append(receipt.Stages,
		removalStage("routing_verification", RemovalStageFailed, "routing_ownership_unverified", ""),
	)
	appendRemovalRecoveryPersistence(&receipt, true)
	return receipt, nil
}

func InterruptedRemovalReceipt(record RemovalCleanupRecord, recoveryPersisted bool) (OpenCodexRemovalReceipt, error) {
	if err := validateRemovalCleanupRecord(record); err != nil || record.Phase != RemovalCleanupPhaseIntent {
		return OpenCodexRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	receipt := removalReceiptFromCleanup(record)
	receipt.Status = RemovalStatusPartial
	receipt.DataMovementUnknown = record.Mode == RemovalModeTrashSelected
	receipt.RoutingRecoveryRequired = true
	receipt.Stages = append(receipt.Stages, removalStage("cleanup_journal", RemovalStageFailed, "cleanup_interrupted_before_package", record.InstallationID))
	appendRemovalRecoveryPersistence(&receipt, recoveryPersisted)
	return receipt, nil
}

func appendRemovalRecoveryPersistence(receipt *OpenCodexRemovalReceipt, recoveryPersisted bool) {
	if recoveryPersisted {
		receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageCompleted, "routing_recovery_persisted", ""))
	} else {
		receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""))
	}
}

func removalReceiptFromCleanup(record RemovalCleanupRecord) OpenCodexRemovalReceipt {
	dataScope := "preserved"
	if record.Mode == RemovalModeTrashSelected {
		dataScope = "explicit_items_only"
	}
	return OpenCodexRemovalReceipt{
		SchemaVersion:           OpenCodexRemovalSchemaVersion,
		Operation:               "remove-open-codex",
		Status:                  RemovalStatusPartial,
		Mode:                    record.Mode,
		InstallationID:          record.InstallationID,
		DataScope:               dataScope,
		SelectedDataItems:       record.SelectedDataItems,
		MovedDataItems:          record.MovedDataItems,
		PermanentDeleteFallback: false,
		Stages:                  []OpenCodexRemovalStage{removalStage("cleanup_journal", RemovalStageCompleted, "cleanup_resume", record.InstallationID)},
	}
}

func sameStringSet(left, right []string) bool {
	left = uniqueSortedStrings(append([]string(nil), left...))
	right = uniqueSortedStrings(append([]string(nil), right...))
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (c RemovalCoordinator) requireRoutingRecovery(receipt *OpenCodexRemovalReceipt) {
	if receipt == nil || receipt.RoutingRecoveryRequired {
		return
	}
	receipt.RoutingRecoveryRequired = true
	if c.MarkRoutingRecovery == nil {
		receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""))
		return
	}
	if err := c.MarkRoutingRecovery(); err != nil {
		receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageFailed, "routing_recovery_persist_failed", ""))
		return
	}
	receipt.Stages = append(receipt.Stages, removalStage("routing_recovery", RemovalStageCompleted, "routing_recovery_persisted", ""))
}

func ValidateOpenCodexRemovalRequest(request OpenCodexRemovalRequest) error {
	return validateRemovalRequest(request)
}

func validateRemovalRequest(request OpenCodexRemovalRequest) error {
	if !validRemovalSelection(request.Selection) {
		return ErrInvalidRemovalRequest
	}
	context := request.Context
	if context == "" {
		context = RemovalContextIntegrated
	}
	switch context {
	case RemovalContextIntegrated:
		if request.ExpectedBoundaryRevision != "" || request.ExpectedNativeState != "" || request.NativeRestoreFingerprint != "" ||
			request.NativeInventoryRevision != "" {
			return ErrInvalidRemovalRequest
		}
	case RemovalContextStandaloneNative:
		if !isFingerprint(request.ExpectedBoundaryRevision) ||
			(request.ExpectedNativeState != "native" && request.ExpectedNativeState != "opencodex") ||
			!isFingerprint(request.NativeRestoreFingerprint) ||
			(request.Mode == RemovalModePreserveData && request.NativeInventoryRevision != "") ||
			(request.Mode == RemovalModeTrashSelected && !isFingerprint(request.NativeInventoryRevision)) {
			return ErrInvalidRemovalRequest
		}
	default:
		return ErrInvalidRemovalRequest
	}
	if !request.ConfirmedRemoval {
		return ErrRemovalConfirmationNeeded
	}
	seen := make(map[string]struct{}, len(request.DataItemIDs))
	for _, itemID := range request.DataItemIDs {
		if !validOpenCodexDataItemID(itemID) {
			return ErrInvalidRemovalRequest
		}
		if _, exists := seen[itemID]; exists {
			return ErrInvalidRemovalRequest
		}
		seen[itemID] = struct{}{}
	}
	switch request.Mode {
	case RemovalModePreserveData:
		if len(request.DataItemIDs) != 0 || request.ConfirmedTrash || request.ExpectedInventoryRevision != "" {
			return ErrInvalidRemovalRequest
		}
	case RemovalModeTrashSelected:
		if !request.ConfirmedTrash || len(request.DataItemIDs) == 0 || len(request.DataItemIDs) > maxRemovalDataItems ||
			(request.ExpectedRoutingGeneration > 0 && !isFingerprint(request.ExpectedInventoryRevision)) {
			return ErrRemovalConfirmationNeeded
		}
	default:
		return ErrInvalidRemovalRequest
	}
	return nil
}

func ValidateOpenCodexInventoryForRequest(inventory OpenCodexDataInventoryReceipt, request OpenCodexRemovalRequest) bool {
	return request.Mode == RemovalModeTrashSelected &&
		validateRefreshInventoryForRequest(inventory, request)
}

func newRemovalReceipt(request OpenCodexRemovalRequest) OpenCodexRemovalReceipt {
	dataScope := "preserved"
	if request.Mode == RemovalModeTrashSelected {
		dataScope = "explicit_items_only"
	}
	return OpenCodexRemovalReceipt{
		SchemaVersion:           OpenCodexRemovalSchemaVersion,
		Operation:               "remove-open-codex",
		Status:                  RemovalStatusFailed,
		Mode:                    request.Mode,
		InstallationID:          request.Selection.ID,
		DataScope:               dataScope,
		SelectedDataItems:       len(request.DataItemIDs),
		PermanentDeleteFallback: false,
		Stages:                  make([]OpenCodexRemovalStage, 0, 8),
	}
}

func removalStage(stage string, status RemovalStageStatus, code, subject string) OpenCodexRemovalStage {
	return OpenCodexRemovalStage{Stage: stage, Status: status, Code: code, SubjectID: subject}
}

func removalErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrRemovalConfirmationNeeded):
		return "confirmation_required"
	case errors.Is(err, ErrInvalidRemovalRequest), errors.Is(err, ErrInvalidRemovalSelection):
		return "invalid_request"
	case errors.Is(err, ErrRemovalCandidateMissing):
		return "candidate_not_found"
	case errors.Is(err, ErrRemovalCandidateChanged):
		return "candidate_changed"
	case errors.Is(err, ErrTeardownCandidateChanged):
		return "teardown_candidate_changed"
	case errors.Is(err, ErrRemovalManualOnly):
		return "manual_removal_required"
	case errors.Is(err, ErrTeardownUnsupported):
		return "teardown_unsupported"
	case errors.Is(err, ErrTeardownPreflightFailed):
		return "teardown_preflight_failed"
	case errors.Is(err, ErrTeardownRefused):
		return "teardown_refused"
	case errors.Is(err, ErrTeardownResultInvalid):
		return "teardown_result_invalid"
	case errors.Is(err, ErrTeardownVerificationFailed):
		return "teardown_verification_failed"
	case errors.Is(err, ErrRemovalRoutingChanged):
		return "routing_ownership_changed"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation_timed_out"
	case errors.Is(err, context.Canceled):
		return "operation_cancelled"
	case errors.Is(err, ErrRemovalProcessCleanup):
		return "process_cleanup_unverified"
	case errors.Is(err, ErrRemovalOutputInvalid):
		return "child_output_invalid"
	default:
		return "operation_failed"
	}
}

func validOpenCodexDataItemID(value string) bool {
	const prefix = "ocx-data-v1:"
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+32 && isLowerHex(value[len(prefix):])
}

type relayTeardownReceipt struct {
	SchemaVersion     int                      `json:"schema_version"`
	Operation         string                   `json:"operation"`
	AdapterID         string                   `json:"adapter_id"`
	Status            string                   `json:"status"`
	DataPreserved     bool                     `json:"data_preserved"`
	ConfigRootRemoved bool                     `json:"config_root_removed"`
	Components        []relayTeardownComponent `json:"components"`
}

type relayTeardownComponent struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
}

func parseRelayTeardownReceipt(
	result RemovalExecutionResult,
	expectedOperation string,
	expectedAdapterID string,
) (relayTeardownReceipt, error) {
	var receipt relayTeardownReceipt
	if !result.Started || !result.CleanupVerified {
		return receipt, ErrTeardownResultInvalid
	}
	if err := decodeStrictRemovalJSON(result.Output, &receipt); err != nil {
		return receipt, ErrTeardownResultInvalid
	}
	if receipt.SchemaVersion != 1 || receipt.Operation != expectedOperation ||
		receipt.AdapterID != expectedAdapterID || !safeBoundedToken(receipt.AdapterID) ||
		len(receipt.Components) == 0 || len(receipt.Components) > maxRemovalComponents {
		return receipt, ErrTeardownResultInvalid
	}
	if !receipt.DataPreserved || receipt.ConfigRootRemoved {
		return receipt, ErrTeardownVerificationFailed
	}
	if expectedOperation == "relay_preserving_teardown_preflight" {
		if (receipt.Status != "ready" && receipt.Status != "failed") ||
			(receipt.Status == "ready" && result.ExitCode != 0) ||
			(receipt.Status == "failed" && result.ExitCode != 1) {
			return receipt, ErrTeardownResultInvalid
		}
	} else if (receipt.Status != "completed" && receipt.Status != "partial" && receipt.Status != "failed") ||
		!exitMatchesTeardownStatus(result.ExitCode, receipt.Status) {
		return receipt, ErrTeardownResultInvalid
	}
	componentStatuses := make(map[string]string, len(receipt.Components))
	hasProblemComponent := false
	for _, component := range receipt.Components {
		if !safeBoundedToken(component.Component) || (component.Code != "" && !safeBoundedToken(component.Code)) ||
			(component.Status != "completed" && component.Status != "unchanged" && component.Status != "refused" && component.Status != "failed" && component.Status != "skipped") {
			return receipt, ErrTeardownResultInvalid
		}
		if _, exists := componentStatuses[component.Component]; exists {
			return receipt, ErrTeardownResultInvalid
		}
		componentStatuses[component.Component] = component.Status
		if component.Status == "refused" || component.Status == "failed" {
			hasProblemComponent = true
		}
	}
	if receipt.Status == "ready" {
		if hasProblemComponent {
			return receipt, ErrTeardownVerificationFailed
		}
		for _, name := range []string{"config", "service", "codex_shim"} {
			if status, exists := componentStatuses[name]; !exists || (status != "completed" && status != "unchanged") {
				return receipt, ErrTeardownVerificationFailed
			}
		}
	} else if receipt.Status == "completed" {
		if hasProblemComponent {
			return receipt, ErrTeardownVerificationFailed
		}
		requiredStatuses := map[string]map[string]bool{
			"quiescence":         {"completed": true},
			"service":            {"completed": true, "unchanged": true},
			"native_codex":       {"completed": true},
			"grok":               {"completed": true, "unchanged": true},
			"client_opencode":    {"completed": true, "unchanged": true},
			"client_pi":          {"completed": true, "unchanged": true},
			"client_omp":         {"completed": true, "unchanged": true},
			"client_hermes":      {"completed": true, "unchanged": true},
			"client_openclaw":    {"completed": true, "unchanged": true},
			"client_kimi":        {"completed": true, "unchanged": true},
			"client_gajae":       {"completed": true, "unchanged": true},
			"client_dsh":         {"completed": true, "unchanged": true},
			"client_mcode":       {"completed": true, "unchanged": true},
			"system_environment": {"completed": true, "unchanged": true},
			"shell_hook":         {"completed": true, "unchanged": true},
			"codex_shim":         {"completed": true, "unchanged": true},
		}
		if len(componentStatuses) != len(requiredStatuses) {
			return receipt, ErrTeardownVerificationFailed
		}
		for component, allowed := range requiredStatuses {
			status, exists := componentStatuses[component]
			if !exists || !allowed[status] {
				return receipt, ErrTeardownVerificationFailed
			}
		}
	} else if !hasProblemComponent {
		return receipt, ErrTeardownResultInvalid
	}
	return receipt, nil
}

func exitMatchesTeardownStatus(exitCode int, status string) bool {
	return status == "completed" && exitCode == 0 || status == "partial" && exitCode == 2 || status == "failed" && exitCode == 1
}

type upstreamInventoryReceipt struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Operation     string                  `json:"operation"`
	Status        string                  `json:"status"`
	Root          string                  `json:"root"`
	Items         []upstreamInventoryItem `json:"items"`
	Reason        string                  `json:"reason,omitempty"`
}

type upstreamInventoryItem struct {
	ID            string `json:"id"`
	Category      string `json:"category"`
	Scope         string `json:"scope"`
	Kind          string `json:"kind"`
	Exists        bool   `json:"exists"`
	Sensitive     bool   `json:"sensitive"`
	CanonicalPath string `json:"canonicalPath"`
	RelativePath  string `json:"relativePath"`
	Trashable     bool   `json:"trashable"`
}

type parsedUpstreamInventory struct {
	Status string
	Items  []OpenCodexDataInventoryItem
}

func parseUpstreamInventory(result RemovalExecutionResult) (parsedUpstreamInventory, error) {
	var receipt upstreamInventoryReceipt
	if err := decodeStrictRemovalJSON(result.Output, &receipt); err != nil {
		return parsedUpstreamInventory{}, err
	}
	if receipt.SchemaVersion != 1 || receipt.Operation != "data-inventory" ||
		(receipt.Status != "absent" && receipt.Status != "refused" && receipt.Status != "verified") ||
		len(receipt.Root) == 0 || len(receipt.Root) > 4096 || !filepath.IsAbs(receipt.Root) || filepath.Clean(receipt.Root) != receipt.Root ||
		len(receipt.Reason) > maxRemovalMessageBytes || len(receipt.Items) > maxRemovalInventoryItems ||
		(receipt.Status == "refused" && result.ExitCode != 1) || (receipt.Status != "refused" && result.ExitCode != 0) ||
		(receipt.Status != "verified" && len(receipt.Items) != 0) {
		return parsedUpstreamInventory{}, ErrRemovalReceiptInvalid
	}
	items := make([]OpenCodexDataInventoryItem, 0, len(receipt.Items))
	seen := make(map[string]struct{}, len(receipt.Items))
	for _, item := range receipt.Items {
		expectedCanonical := receipt.Root
		if item.RelativePath != "." {
			expectedCanonical = filepath.Join(receipt.Root, filepath.FromSlash(item.RelativePath))
		}
		if !validOpenCodexDataItemID(item.ID) || !validInventoryCategory(item.Category) || !validInventoryScope(item.Scope) ||
			!validInventoryKind(item.Kind) || !validInventoryRelativePath(item.Scope, item.RelativePath) ||
			len(item.CanonicalPath) == 0 || len(item.CanonicalPath) > 4096 || !filepath.IsAbs(item.CanonicalPath) || filepath.Clean(item.CanonicalPath) != item.CanonicalPath ||
			item.CanonicalPath != expectedCanonical ||
			(item.Trashable && (item.Scope != "owned" || !item.Exists)) {
			return parsedUpstreamInventory{}, ErrRemovalReceiptInvalid
		}
		if _, exists := seen[item.ID]; exists {
			return parsedUpstreamInventory{}, ErrRemovalReceiptInvalid
		}
		seen[item.ID] = struct{}{}
		items = append(items, OpenCodexDataInventoryItem{
			ID: item.ID, Category: item.Category, Scope: item.Scope, Kind: item.Kind,
			RelativePath: item.RelativePath, Exists: item.Exists, Sensitive: item.Sensitive, Trashable: item.Trashable,
		})
	}
	return parsedUpstreamInventory{Status: receipt.Status, Items: items}, nil
}

func validInventoryCategory(value string) bool {
	switch value {
	case "credentials", "configuration", "integration-backups", "logs", "runtime", "artifacts", "ownership-metadata", "root", "other":
		return true
	default:
		return false
	}
}

func validInventoryScope(value string) bool {
	return value == "owned" || value == "ownership-metadata" || value == "config-root"
}

func validInventoryKind(value string) bool {
	return value == "absent" || value == "file" || value == "directory" || value == "symlink" || value == "other"
}

func validInventoryRelativePath(scope, value string) bool {
	if len(value) == 0 || len(value) > 512 || strings.Contains(value, "\\") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	if scope == "config-root" {
		return value == "."
	}
	if value == "." || path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

type upstreamTrashReceipt struct {
	SchemaVersion           int                    `json:"schemaVersion"`
	Operation               string                 `json:"operation"`
	Status                  string                 `json:"status"`
	Selected                []string               `json:"selected"`
	Moved                   []string               `json:"moved"`
	Failures                []upstreamTrashFailure `json:"failures"`
	PermanentDeleteFallback bool                   `json:"permanentDeleteFallback"`
}

type upstreamTrashFailure struct {
	ItemID  string `json:"itemId,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseUpstreamTrash(result RemovalExecutionResult, requested []string) (upstreamTrashReceipt, error) {
	var receipt upstreamTrashReceipt
	if !result.Started || !result.CleanupVerified {
		return receipt, ErrRemovalReceiptInvalid
	}
	if err := decodeStrictRemovalJSON(result.Output, &receipt); err != nil {
		return receipt, err
	}
	if receipt.SchemaVersion != 1 || receipt.Operation != "data-trash" || receipt.PermanentDeleteFallback ||
		(receipt.Status != "completed" && receipt.Status != "partial" && receipt.Status != "failed" && receipt.Status != "unsupported") ||
		len(receipt.Selected) != len(requested) || len(receipt.Moved) > len(requested) || len(receipt.Failures) > 1 ||
		!exitMatchesTrashStatus(result.ExitCode, receipt.Status) {
		return receipt, ErrRemovalReceiptInvalid
	}
	for index, itemID := range receipt.Selected {
		if itemID != requested[index] || !validOpenCodexDataItemID(itemID) {
			return receipt, ErrRemovalReceiptInvalid
		}
	}
	for index, itemID := range receipt.Moved {
		if itemID != requested[index] {
			return receipt, ErrRemovalReceiptInvalid
		}
	}
	if receipt.Status == "completed" {
		if len(receipt.Moved) != len(requested) || len(receipt.Failures) != 0 {
			return receipt, ErrRemovalReceiptInvalid
		}
	} else if len(receipt.Failures) != 1 {
		return receipt, ErrRemovalReceiptInvalid
	}
	for _, failure := range receipt.Failures {
		if !safeBoundedToken(failure.Code) || len(failure.Message) > maxRemovalMessageBytes ||
			(failure.ItemID != "" && !containsRemovalItem(requested, failure.ItemID)) {
			return receipt, ErrRemovalReceiptInvalid
		}
	}
	return receipt, nil
}

func exitMatchesTrashStatus(exitCode int, status string) bool {
	return status == "completed" && exitCode == 0 || status == "partial" && exitCode == 2 ||
		(status == "failed" || status == "unsupported") && exitCode == 1
}

func containsRemovalItem(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func decodeStrictRemovalJSON(payload []byte, target any) error {
	if len(payload) == 0 || len(payload) > maxRemovalJSONOutput {
		return ErrRemovalReceiptInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrRemovalReceiptInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrRemovalReceiptInvalid
	}
	return nil
}

func safeBoundedToken(value string) bool {
	if value == "" || len(value) > maxRemovalTokenBytes {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == ':' || character == '-') {
			return false
		}
	}
	return true
}
