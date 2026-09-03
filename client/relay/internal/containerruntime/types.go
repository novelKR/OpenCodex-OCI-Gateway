// Package containerruntime owns the Apple Container CLI mutation boundary for
// the separately released OpenCodex runtime image. The package never modifies
// the upstream OpenCodex checkout and never puts runtime credentials in argv,
// durable state, receipts, or diagnostic errors.
package containerruntime

import (
	"context"
	"errors"
	"io"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

const (
	SchemaVersion                = 1
	StateFormatRevision          = 1
	GuestServicePort             = 10100
	HostServicePort              = 10210
	ContainerName                = "opencodex-relay-runtime"
	GuestStatePath               = "/var/lib/opencodex"
	GuestBootstrapSocket         = "/run/opencodex/bootstrap.sock"
	APIKeychainService           = "opencodex-relay-apple-container-api-auth-token"
	AdminKeychainService         = "opencodex-relay-apple-container-admin-auth-token"
	MinimumAppleContainerVersion = "1.3.1"
	MaximumReceiptBytes          = 64 << 10
	MaximumOAuthInputBytes       = 16 << 10
	MaximumOAuthSubmissionBytes  = 4 << 10
)

var (
	ErrUnavailable      = errors.New("Apple Container runtime is unavailable")
	ErrUnsafeState      = errors.New("container runtime state is unsafe")
	ErrStateChanged     = errors.New("container runtime state changed")
	ErrRoutingChanged   = errors.New("container runtime routing generation changed")
	ErrRecoveryRequired = errors.New("container runtime recovery is required")
	ErrInvalidRequest   = errors.New("container runtime request is invalid")
	ErrCredential       = errors.New("container runtime credentials are unavailable")
	ErrForeignResource  = errors.New("container runtime resource is not owned by this installation")
)

type State string

const (
	StateUnavailable      State = "unavailable"
	StateStopped          State = "stopped"
	StateStaging          State = "staging"
	StateHealthy          State = "healthy"
	StateUpdating         State = "updating"
	StateRecoveryRequired State = "recovery_required"
)

// FixedContainerState is the bounded result of re-reading the one fixed-name
// Apple Container resource.  Callers must distinguish a currently running
// owned resource from a stopped resource: Apple Container releases its socket
// forwarder when the guest exits, so static labels alone are not authority to
// send credentials to the fixed loopback port.
type FixedContainerState string

const (
	FixedContainerAbsent       FixedContainerState = "absent"
	FixedContainerRunningOwned FixedContainerState = "running_owned"
	FixedContainerStoppedOwned FixedContainerState = "stopped_owned"
	FixedContainerForeign      FixedContainerState = "foreign"
	FixedContainerUnknown      FixedContainerState = "unknown"
)

type Capability struct {
	Available             bool   `json:"available"`
	Reason                string `json:"reason"`
	MacOSVersion          string `json:"macos_version"`
	AppleContainerVersion string `json:"apple_container_version"`
	SystemServiceState    string `json:"system_service_state"`
}

type ArtifactSummary struct {
	ArtifactVersion string `json:"artifact_version"`
	ReleaseSequence uint64 `json:"release_sequence"`
	ManifestSHA256  string `json:"manifest_sha256"`
	IndexDigest     string `json:"index_digest"`
	ARM64Digest     string `json:"arm64_digest"`
}

type Inspection struct {
	SchemaVersion     int              `json:"schema_version"`
	OK                bool             `json:"ok"`
	State             State            `json:"state"`
	Capability        Capability       `json:"capability"`
	Staged            *ArtifactSummary `json:"staged,omitempty"`
	Active            *ArtifactSummary `json:"active,omitempty"`
	StateDigest       string           `json:"state_digest"`
	RoutingGeneration uint64           `json:"routing_generation"`
	RecoveryRequired  bool             `json:"recovery_required"`
}

type CheckReceipt struct {
	Inspection
	Status     runtimemanifest.CheckStatus `json:"status"`
	Candidate  *ArtifactSummary            `json:"candidate,omitempty"`
	Compatible bool                        `json:"compatible"`
	Reason     string                      `json:"reason"`
}

type MutationReceipt = Inspection

type StageRequest struct {
	ExpectedManifestSHA256    string
	ExpectedStateDigest       string
	ExpectedRoutingGeneration uint64
}

type ActivateRequest struct {
	ExpectedStateDigest       string
	ExpectedRoutingGeneration uint64
	ConfirmDesktopExited      bool
}

type StopRequest struct {
	ExpectedStateDigest       string
	ExpectedRoutingGeneration uint64
	ConfirmDesktopExited      bool
}

// ParkRequest is the emergency fail-closed path used only when the exact
// Desktop bundle reappears during a lifecycle mutation. It creates a durable
// stop-recovery witness without requiring Desktop exit and without mutating
// routing, Codex configuration, or the container.
type ParkRequest struct {
	ExpectedStateDigest       string
	ExpectedRoutingGeneration uint64
}

type RecoverRequest struct {
	ExpectedStateDigest  string
	ConfirmDesktopExited bool
}

type Secrets struct {
	APIToken   []byte
	AdminToken []byte
}

type SecretLease interface {
	Path() string
	Wait(context.Context) error
	Close() error
}

type SecretServer interface {
	Open(context.Context, Secrets) (SecretLease, error)
}

type Keychain interface {
	Load(context.Context, string) (Secrets, error)
	// Ensure is called only from the explicit activation path. It creates any
	// missing fixed-service token using the platform Keychain API, never argv,
	// a config file, or a receipt, and returns both values to the caller only in
	// process memory.
	Ensure(context.Context, string) (Secrets, error)
}

type StartSpec struct {
	InstallationID string
	OperationID    string
	ImageReference string
	IndexDigest    string
	ARM64Digest    string
	StatePath      string
	SocketPath     string
	Generation     uint64
	ManifestSHA256 string
}

type ImageRuntime interface {
	Capability(context.Context, string, string) (Capability, error)
	Pull(context.Context, string) error
	VerifyImage(context.Context, StartSpec, runtimemanifest.Manifest) error
	Start(context.Context, StartSpec) (string, error)
	Stop(context.Context, string, StartSpec) error
	Delete(context.Context, string, StartSpec) error
	VerifyContainer(context.Context, string, StartSpec) error
	ContainerState(context.Context, string, StartSpec) (FixedContainerState, error)
	VerifyAbsent(context.Context, string) error
}

type HTTPProber interface {
	Verify(context.Context, []byte, []byte, func(context.Context) error) error
}

type StateCloner interface {
	Clone(context.Context, string, string) error
}

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

type MaintenanceRequest struct {
	OperationID               string
	InstallationID            string
	ExpectedRoutingGeneration uint64
	OldManifestSHA256         string
	NewManifestSHA256         string
	OldImageDigest            string
	NewImageDigest            string
	OldStateGeneration        uint64
	NewStateGeneration        uint64
}

type MaintenanceWitness struct {
	Schema                    int               `json:"schema"`
	Backend                   string            `json:"backend"`
	OriginRoutingGeneration   uint64            `json:"origin_routing_generation"`
	PreparedRoutingGeneration uint64            `json:"prepared_routing_generation"`
	FinalRoutingGeneration    uint64            `json:"final_routing_generation"`
	Intent                    MaintenanceIntent `json:"intent"`
}

type RoutingDirection string

const (
	RoutingCompleteTarget RoutingDirection = "complete_target"
	RoutingRestoreOrigin  RoutingDirection = "restore_origin"
)

type RoutingIntent struct {
	OperationID        string
	InstallationID     string
	OldManifestSHA256  string
	NewManifestSHA256  string
	OldImageDigest     string
	NewImageDigest     string
	OldStateGeneration uint64
	NewStateGeneration uint64
}

// RoutingRequest is the complete non-secret cross-journal identity for one
// ordinary activation, stopped restart, or stop. The routing implementation
// must persist it before its first mutation and retain it until Acknowledge.
type RoutingRequest struct {
	Intent                          RoutingIntent
	ExpectedOriginRoutingGeneration uint64
	TargetAppleActive               bool
	Direction                       RoutingDirection
}

type RoutingSnapshot struct {
	Generation            uint64
	AppleActive           bool
	RecoveryRequired      bool
	MaintenancePending    bool
	RuntimeRoutingPending bool
}

type RoutingCoordinator interface {
	Current(context.Context) (RoutingSnapshot, error)
	ActivateApple(context.Context, RoutingRequest, bool) (uint64, error)
	Reconcile(context.Context, RoutingRequest, bool) (uint64, error)
	Acknowledge(context.Context, RoutingRequest, uint64) error
	Prepare(context.Context, MaintenanceRequest) (MaintenanceWitness, error)
	Commit(context.Context, MaintenanceWitness) (uint64, error)
	Rollback(context.Context, MaintenanceWitness) (uint64, error)
	StopApple(context.Context, RoutingRequest, bool) (uint64, error)
}

// ProfileEnroller persists only the fixed, non-secret Apple runtime profile.
// It is invoked from the explicit stage path while the lifecycle lock is held;
// it cannot select a route or provision credentials.
type ProfileEnroller interface {
	Ensure(context.Context) (string, error)
}

type ReleaseChecker interface {
	Check(context.Context, runtimemanifest.CheckRequest) (runtimemanifest.CheckResult, error)
	ResolveExpected(context.Context, string, runtimemanifest.CheckRequest) (runtimemanifest.Candidate, error)
}

type Locker interface {
	Lock(context.Context) (func() error, error)
}

type OAuthKind string

const (
	OAuthKindGeneric OAuthKind = "generic"
	OAuthKindCodex   OAuthKind = "codex"
)

type OAuthStatus string

const (
	OAuthStatusPending      OAuthStatus = "pending"
	OAuthStatusAwaitingUser OAuthStatus = "awaiting_user"
	OAuthStatusComplete     OAuthStatus = "complete"
	OAuthStatusCancelled    OAuthStatus = "cancelled"
	OAuthStatusFailed       OAuthStatus = "failed"
)

type OAuthProvider struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Kind               OAuthKind `json:"kind"`
	SupportsDeviceFlow bool      `json:"supports_device_flow"`
}

type OAuthProvidersReceipt struct {
	SchemaVersion int             `json:"schema_version"`
	OK            bool            `json:"ok"`
	Providers     []OAuthProvider `json:"providers"`
}

type OAuthReceipt struct {
	SchemaVersion    int         `json:"schema_version"`
	OK               bool        `json:"ok"`
	OperationID      string      `json:"operation_id"`
	Provider         string      `json:"provider"`
	Kind             OAuthKind   `json:"kind"`
	Status           OAuthStatus `json:"status"`
	AuthorizationURL string      `json:"authorization_url,omitempty"`
	Instructions     string      `json:"instructions,omitempty"`
	UserCode         string      `json:"user_code,omitempty"`
}

type OAuthSubmitRequest struct {
	OperationID string
	Input       io.Reader
}

// ManagementAPI is the narrow adapter over OpenCodex's existing, version-
// pinned management endpoints. Implementations use only the fixed loopback
// endpoint and the Admin token; redirects, proxies, and connection reuse are
// prohibited.
type ManagementAPI interface {
	Providers(context.Context, []byte) ([]OAuthProvider, error)
	Start(context.Context, []byte, string, OAuthKind) (ManagementFlow, error)
	Status(context.Context, []byte, ManagementFlow) (OAuthStatus, error)
	Submit(context.Context, []byte, ManagementFlow, string) error
	Cancel(context.Context, []byte, ManagementFlow) error
}

type ManagementFlow struct {
	Provider         string    `json:"provider"`
	Kind             OAuthKind `json:"kind"`
	UpstreamFlowID   string    `json:"upstream_flow_id,omitempty"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	Instructions     string    `json:"instructions,omitempty"`
	UserCode         string    `json:"user_code,omitempty"`
}
