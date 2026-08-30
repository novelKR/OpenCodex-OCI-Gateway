package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const (
	NativeRemovalSchemaVersion     = 1
	NativeRemovalReadSchemaVersion = 2
)

const (
	NativeRemovalStatusReady                          = "ready"
	NativeRemovalStatusRecoveryRequired               = "recovery_required"
	NativeStateNative                                 = "native"
	NativeStateOpenCodex                              = "opencodex"
	NativeStateUnavailable                            = "unavailable"
	AutomaticRemovalReasonEligible                    = "eligible"
	AutomaticRemovalReasonUnreviewedPackageClosure    = "unreviewed_package_closure"
	AutomaticRemovalReasonUnsupportedPackageVersion   = "unsupported_package_version"
	AutomaticRemovalReasonPackageModuleChanged        = "package_module_changed"
	AutomaticRemovalReasonExecutionEvidenceIncomplete = "execution_evidence_incomplete"
	AutomaticRemovalReasonManualPackageManager        = "manual_package_manager"
	AutomaticRemovalReasonIdentityUnverified          = "identity_unverified"
)

var (
	ErrNativeRemovalBoundaryUnsafe   = errors.New("standalone Native removal boundary is unsafe")
	ErrNativeRemovalBoundaryChanged  = errors.New("standalone Native removal boundary changed")
	ErrNativeRemovalRecoveryRequired = errors.New("standalone Native removal recovery is required")
	ErrNativeRemovalCustomCodexHome  = errors.New("standalone Native removal requires the default Codex home")
)

// NativeRemovalSelection is the path-free caller contract. Both authorities
// rediscover their execution paths from bounded roots before any operation.
type NativeRemovalSelection struct {
	InstallationID           string
	InstallationFingerprint  string
	NativeRestoreFingerprint string
}

// DiscoveryNativeRemovalResolver joins the already independent package
// removal and Native restore authorities. A candidate is eligible only when
// both resolvers return the same immutable execution closure.
type DiscoveryNativeRemovalResolver struct {
	Removal DiscoveryRemovalResolver
	Restore DiscoveryNativeRestoreResolver
}

func NewDiscoveryNativeRemovalResolver(options DiscoveryOptions) DiscoveryNativeRemovalResolver {
	return DiscoveryNativeRemovalResolver{
		Removal: DiscoveryRemovalResolver{Options: SanitizedRemovalDiscoveryOptions(options)},
		Restore: DiscoveryNativeRestoreResolver{Options: SanitizedNativeRestoreDiscoveryOptions(options)},
	}
}

func (r DiscoveryNativeRemovalResolver) Resolve(ctx context.Context, selection NativeRemovalSelection) (NPMInstallation, error) {
	if !validNativeRemovalSelection(selection) {
		return NPMInstallation{}, ErrInvalidRemovalSelection
	}
	candidate, err := r.Removal.Resolve(ctx, NPMRemovalSelection{
		ID: selection.InstallationID, Fingerprint: selection.InstallationFingerprint,
	})
	if err != nil {
		return NPMInstallation{}, err
	}
	if !constantStringEqual(candidate.NativeRestoreFingerprint, selection.NativeRestoreFingerprint) {
		return NPMInstallation{}, ErrNativeRestoreCandidateChanged
	}
	restored, err := r.Restore.Resolve(ctx, NativeRestoreSelection{
		InstallationID:           candidate.ID,
		InstallationFingerprint:  candidate.Fingerprint,
		NativeRestoreFingerprint: candidate.NativeRestoreFingerprint,
		Executable:               candidate.Executable,
		ExecutableSHA256:         candidate.ExecutableSHA256,
	})
	if err != nil {
		return NPMInstallation{}, err
	}
	if !sameRemovalCriticalInstallation(candidate, restored) || !sameNativeRestoreExecutionProof(candidate, restored) {
		return NPMInstallation{}, ErrNativeRestoreCandidateChanged
	}
	return candidate, nil
}

func (r DiscoveryNativeRemovalResolver) Revalidate(ctx context.Context, expected NPMInstallation) error {
	candidate, err := r.Resolve(ctx, NativeRemovalSelection{
		InstallationID:           expected.ID,
		InstallationFingerprint:  expected.Fingerprint,
		NativeRestoreFingerprint: expected.NativeRestoreFingerprint,
	})
	if err != nil {
		return err
	}
	if !sameRemovalCriticalInstallation(expected, candidate) || !sameNativeRestoreExecutionProof(expected, candidate) {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func (r DiscoveryNativeRemovalResolver) ValidateForMutation(ctx context.Context, candidate NPMInstallation) error {
	if err := r.Removal.ValidateForMutation(ctx, candidate); err != nil {
		return err
	}
	return r.Restore.Revalidate(ctx, candidate)
}

func (r DiscoveryNativeRemovalResolver) VerifyRemoved(candidate NPMInstallation) error {
	return r.Removal.VerifyRemoved(candidate)
}

func validNativeRemovalSelection(selection NativeRemovalSelection) bool {
	return validRemovalSelection(NPMRemovalSelection{
		ID: selection.InstallationID, Fingerprint: selection.InstallationFingerprint,
	}) && isFingerprint(selection.NativeRestoreFingerprint)
}

// NativeRemovalResolverAdapter binds the third opaque witness while satisfying
// the existing removal coordinator's two-field resolver contract.
type NativeRemovalResolverAdapter struct {
	Resolver    DiscoveryNativeRemovalResolver
	Fingerprint string
}

func (a NativeRemovalResolverAdapter) Resolve(ctx context.Context, selection NPMRemovalSelection) (NPMInstallation, error) {
	return a.Resolver.Resolve(ctx, NativeRemovalSelection{
		InstallationID:           selection.ID,
		InstallationFingerprint:  selection.Fingerprint,
		NativeRestoreFingerprint: a.Fingerprint,
	})
}

func (a NativeRemovalResolverAdapter) Revalidate(ctx context.Context, candidate NPMInstallation) error {
	if !constantStringEqual(candidate.NativeRestoreFingerprint, a.Fingerprint) {
		return ErrNativeRestoreCandidateChanged
	}
	return a.Resolver.Revalidate(ctx, candidate)
}

func (a NativeRemovalResolverAdapter) ValidateForMutation(ctx context.Context, candidate NPMInstallation) error {
	return a.Resolver.ValidateForMutation(ctx, candidate)
}

func (a NativeRemovalResolverAdapter) VerifyRemoved(candidate NPMInstallation) error {
	return a.Resolver.VerifyRemoved(candidate)
}

type NativeHomebrewGuardSnapshot struct {
	Prefix           string   `json:"prefix"`
	PackageRoot      string   `json:"package_root"`
	Executable       string   `json:"executable"`
	ExecutableSHA256 string   `json:"executable_sha256"`
	CLIEntry         string   `json:"cli_entry"`
	CLIEntrySHA256   string   `json:"cli_entry_sha256"`
	BunExecutable    string   `json:"bun_executable"`
	BunSHA256        string   `json:"bun_sha256"`
	NodeExecutable   string   `json:"node_executable"`
	NodeSHA256       string   `json:"node_sha256"`
	NPMCLI           string   `json:"npm_cli"`
	NPMCLISHA256     string   `json:"npm_cli_sha256"`
	Launchers        []string `json:"launchers"`
}

type NativeRemovalCandidate struct {
	InstallationID           string                       `json:"installation_id"`
	InstallationFingerprint  string                       `json:"installation_fingerprint"`
	NativeRestoreFingerprint string                       `json:"native_restore_fingerprint,omitempty"`
	Version                  string                       `json:"version"`
	Manager                  DiscoveryManager             `json:"manager"`
	RemovalCapability        RemovalCapability            `json:"removal_capability"`
	RemovalAuthority         RemovalAuthority             `json:"removal_authority"`
	DataCapability           DataCapability               `json:"data_capability,omitempty"`
	AutomaticRemovalEligible bool                         `json:"automatic_removal_eligible"`
	AutomaticRemovalReason   string                       `json:"automatic_removal_reason"`
	HomebrewGuardRequired    bool                         `json:"homebrew_guard_required"`
	HomebrewGuard            *NativeHomebrewGuardSnapshot `json:"homebrew_guard,omitempty"`
}

func ProjectNativeRemovalCandidate(candidate NPMInstallation) NativeRemovalCandidate {
	eligible := automaticRemovalAuthorityCandidate(candidate) &&
		candidate.NativeRestoreCapability == NativeRestoreCapabilityVerifiedSnapshot &&
		isFingerprint(candidate.NativeRestoreFingerprint) && candidate.nativeRestoreProof != nil
	projected := NativeRemovalCandidate{
		InstallationID:           candidate.ID,
		InstallationFingerprint:  candidate.Fingerprint,
		NativeRestoreFingerprint: candidate.NativeRestoreFingerprint,
		Version:                  candidate.Version,
		Manager:                  candidate.Manager,
		RemovalCapability:        candidate.RemovalCapability,
		RemovalAuthority:         candidate.RemovalAuthority,
		DataCapability:           candidate.DataCapability,
		AutomaticRemovalEligible: eligible,
		AutomaticRemovalReason:   automaticRemovalReason(candidate, eligible),
		HomebrewGuardRequired:    candidate.HomebrewGuardRequired,
	}
	if candidate.HomebrewGuardRequired {
		projected.HomebrewGuard = &NativeHomebrewGuardSnapshot{
			Prefix: candidate.Prefix, PackageRoot: candidate.PackageRoot,
			Executable: candidate.Executable, ExecutableSHA256: candidate.ExecutableSHA256,
			CLIEntry: candidate.CLIEntry, CLIEntrySHA256: candidate.CLIEntrySHA256,
			BunExecutable: candidate.BunExecutable, BunSHA256: candidate.BunSHA256,
			NodeExecutable: candidate.NodeExecutable, NodeSHA256: candidate.NodeSHA256,
			NPMCLI: candidate.NPMCLI, NPMCLISHA256: candidate.NPMCLISHA256,
			Launchers: append([]string(nil), candidate.Launchers...),
		}
	}
	return projected
}

func automaticRemovalReason(candidate NPMInstallation, eligible bool) string {
	if eligible {
		return AutomaticRemovalReasonEligible
	}
	switch candidate.TeardownCompatibility {
	case teardownCompatibilityClosureChanged:
		return AutomaticRemovalReasonUnreviewedPackageClosure
	case teardownCompatibilityUnsupportedVersion:
		return AutomaticRemovalReasonUnsupportedPackageVersion
	case teardownCompatibilityModuleChanged:
		return AutomaticRemovalReasonPackageModuleChanged
	}
	if candidate.Manager != DiscoveryManagerNPM && candidate.Manager != DiscoveryManagerHomebrew {
		return AutomaticRemovalReasonManualPackageManager
	}
	if candidate.TeardownCompatibility == teardownCompatibilityCompatible &&
		(candidate.RemovalAuthority != RemovalAuthorityAutomatic ||
			candidate.RemovalCapability == RemovalCapabilityManual ||
			candidate.NodeExecutable == "" || candidate.NPMCLI == "" || candidate.CLIEntry == "" ||
			candidate.BunExecutable == "" || candidate.PackageTreeSHA256 == "" || candidate.NPMTreeSHA256 == "") {
		return AutomaticRemovalReasonExecutionEvidenceIncomplete
	}
	return AutomaticRemovalReasonIdentityUnverified
}

type NativeRemovalDiscovery struct {
	SchemaVersion          int                      `json:"schema_version"`
	Operation              string                   `json:"operation"`
	Context                RemovalContext           `json:"context"`
	Status                 string                   `json:"status"`
	BoundaryRevision       string                   `json:"boundary_revision"`
	NativeState            string                   `json:"native_state"`
	NativeRecoveryRequired bool                     `json:"native_recovery_required"`
	Candidates             []NativeRemovalCandidate `json:"candidates"`
	Rejected               int                      `json:"rejected"`
	Truncated              bool                     `json:"truncated"`
}

type NativeRemovalInspection struct {
	SchemaVersion          int                     `json:"schema_version"`
	Operation              string                  `json:"operation"`
	Context                RemovalContext          `json:"context"`
	Status                 string                  `json:"status"`
	BoundaryRevision       string                  `json:"boundary_revision"`
	NativeState            string                  `json:"native_state"`
	NativeRecoveryRequired bool                    `json:"native_recovery_required"`
	Candidate              *NativeRemovalCandidate `json:"candidate,omitempty"`
}

type NativeDataInventoryReceipt struct {
	SchemaVersion            int                          `json:"schema_version"`
	Operation                string                       `json:"operation"`
	Context                  RemovalContext               `json:"context"`
	Status                   string                       `json:"status"`
	BoundaryRevision         string                       `json:"boundary_revision"`
	NativeState              string                       `json:"native_state"`
	NativeRecoveryRequired   bool                         `json:"native_recovery_required"`
	InstallationID           string                       `json:"installation_id"`
	InstallationFingerprint  string                       `json:"installation_fingerprint"`
	NativeRestoreFingerprint string                       `json:"native_restore_fingerprint"`
	InventoryRevision        string                       `json:"inventory_revision"`
	Items                    []OpenCodexDataInventoryItem `json:"items"`
}

func NewNativeDataInventoryReceipt(
	base OpenCodexDataInventoryReceipt,
	boundaryRevision, nativeState, nativeRestoreFingerprint string,
) NativeDataInventoryReceipt {
	receipt := NativeDataInventoryReceipt{
		SchemaVersion: NativeRemovalSchemaVersion, Operation: "open-codex-native-data-inventory",
		Context: RemovalContextStandaloneNative, Status: base.Status,
		BoundaryRevision: boundaryRevision, NativeState: nativeState,
		InstallationID: base.InstallationID, InstallationFingerprint: base.InstallationFingerprint,
		NativeRestoreFingerprint: nativeRestoreFingerprint,
		Items:                    append([]OpenCodexDataInventoryItem(nil), base.Items...),
	}
	payload, _ := json.Marshal(receipt.Items)
	hash := sha256.New()
	for _, value := range []string{
		"opencodex-native-inventory-v1", receipt.BoundaryRevision, receipt.NativeState,
		receipt.InstallationID, receipt.InstallationFingerprint, receipt.NativeRestoreFingerprint,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(payload)
	receipt.InventoryRevision = hex.EncodeToString(hash.Sum(nil))
	return receipt
}

type NativeRemovalReceipt struct {
	SchemaVersion           int                     `json:"schema_version"`
	Operation               string                  `json:"operation"`
	Context                 RemovalContext          `json:"context"`
	Status                  OpenCodexRemovalStatus  `json:"status"`
	BoundaryRevision        string                  `json:"boundary_revision"`
	NativeState             string                  `json:"native_state"`
	NativeRecoveryRequired  bool                    `json:"native_recovery_required"`
	Mode                    OpenCodexRemovalMode    `json:"mode"`
	InstallationID          string                  `json:"installation_id"`
	DataScope               string                  `json:"data_scope"`
	SelectedDataItems       int                     `json:"selected_data_items"`
	MovedDataItems          int                     `json:"moved_data_items"`
	PackageRemoved          bool                    `json:"package_removed"`
	DataMovementUnknown     bool                    `json:"data_movement_unknown"`
	PermanentDeleteFallback bool                    `json:"permanent_delete_fallback"`
	TerminalReceiptDigest   string                  `json:"terminal_receipt_digest,omitempty"`
	Stages                  []OpenCodexRemovalStage `json:"stages"`
}

func NewNativeRemovalReceipt(base OpenCodexRemovalReceipt, boundaryRevision, nativeState string) NativeRemovalReceipt {
	stages := make([]OpenCodexRemovalStage, 0, len(base.Stages))
	for _, stage := range base.Stages {
		mapped := stage
		switch mapped.Stage {
		case "routing_pre_teardown":
			mapped.Stage = "native_boundary_pre_teardown"
		case "routing_verification":
			mapped.Stage = "native_boundary_verification"
		case "routing_pre_trash":
			mapped.Stage = "native_boundary_pre_trash"
		case "routing_post_trash":
			mapped.Stage = "native_boundary_post_trash"
		case "routing_reverification":
			mapped.Stage = "native_boundary_reverification"
		case "routing_post_verification":
			mapped.Stage = "native_boundary_final_verification"
			if mapped.Code == "routing_ownership_reverified" {
				mapped.Code = "native_ownership_post_package_verified"
			}
		case "routing_final_verification":
			mapped.Stage = "native_boundary_final_verification"
		case "routing_recovery":
			mapped.Stage = "native_recovery"
			if mapped.Code == "routing_recovery_persisted" {
				mapped.Code = "native_recovery_persisted"
			} else if mapped.Code == "routing_recovery_persist_failed" {
				mapped.Code = "native_recovery_persist_failed"
			}
		}
		if mapped.Code == "routing_ownership_changed" {
			mapped.Code = "native_boundary_changed"
		} else if mapped.Code == "routing_ownership_unverified" {
			mapped.Code = "native_ownership_unverified"
		} else if mapped.Code == "routing_ownership_verified" || mapped.Code == "routing_ownership_reverified" {
			mapped.Code = "native_ownership_reverified"
		}
		switch mapped.Stage {
		case "native_boundary_pre_teardown", "native_boundary_verification", "native_restore",
			"native_boundary_pre_trash", "native_boundary_post_trash", "native_boundary_reverification",
			"native_boundary_final_verification", "native_recovery", "cleanup_journal_retained":
			mapped.SubjectID = ""
		}
		stages = append(stages, mapped)
	}
	if base.RoutingRecoveryRequired {
		nativeState = NativeStateUnavailable
	}
	return NativeRemovalReceipt{
		SchemaVersion: NativeRemovalSchemaVersion, Operation: "remove-open-codex-native",
		Context: RemovalContextStandaloneNative, Status: base.Status,
		BoundaryRevision: boundaryRevision, NativeState: nativeState,
		NativeRecoveryRequired: base.RoutingRecoveryRequired,
		Mode:                   base.Mode, InstallationID: base.InstallationID, DataScope: base.DataScope,
		SelectedDataItems: base.SelectedDataItems, MovedDataItems: base.MovedDataItems,
		PackageRemoved: base.PackageRemoved, DataMovementUnknown: base.DataMovementUnknown,
		PermanentDeleteFallback: base.PermanentDeleteFallback, Stages: stages,
	}
}

// NewTerminalNativeRemovalReceipt projects a completed standalone receipt and
// attaches the digest of the exact retained terminal witness. Callers must not
// expose a terminal success unless that witness remains replayable on disk.
func NewTerminalNativeRemovalReceipt(
	base OpenCodexRemovalReceipt,
	boundaryRevision string,
	record RemovalCleanupRecord,
) (NativeRemovalReceipt, error) {
	if base.Status != RemovalStatusCompleted || !base.PackageRemoved || base.RoutingRecoveryRequired ||
		base.InstallationID != record.InstallationID || record.NativeOriginBoundaryRevision != boundaryRevision {
		return NativeRemovalReceipt{}, ErrRemovalCleanupUnsafe
	}
	digest, err := StandaloneTerminalReceiptDigest(record)
	if err != nil {
		return NativeRemovalReceipt{}, err
	}
	receipt := NewNativeRemovalReceipt(base, boundaryRevision, NativeStateNative)
	receipt.TerminalReceiptDigest = digest
	return receipt, nil
}
