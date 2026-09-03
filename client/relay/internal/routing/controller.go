package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
)

const (
	statusSchemaVersion = 4
	maxJournalBytes     = 16 << 10

	recoveryReasonNotRequired            = "recovery_not_required"
	recoveryReasonJournalVerified        = "journal_verified"
	recoveryReasonJournalMissing         = "journal_missing"
	recoveryReasonJournalMalformed       = "journal_malformed"
	recoveryReasonJournalMismatch        = "journal_mismatch"
	recoveryReasonObservedStateVerified  = "observed_state_verified"
	recoveryReasonObservedUnavailable    = "observed_state_unavailable"
	recoveryReasonOriginNotAuthoritative = "origin_not_authoritative"

	recoveryTargetUnavailable = "unavailable"
	recoveryTargetObserved    = "observed"
	recoveryTargetJournal     = "journal"
)

var (
	ErrDesktopExitConfirmation = errors.New("Desktop exit confirmation is required before applying a routing change")
	ErrTransitionPending       = errors.New("routing change is already pending")
	ErrRecoveryRequired        = errors.New("routing recovery is required before another change")
	ErrRelayAcknowledgement    = errors.New("resident relay did not acknowledge the parked routing state")
	ErrRelayFinalization       = errors.New("resident relay did not acknowledge the finalized routing state")
	ErrRequestDrain            = errors.New("resident relay did not drain active requests before the timeout")
	ErrCredentialPreflight     = errors.New("relay credential preflight failed")
	ErrLocalOpenCodexPreflight = errors.New("local OpenCodex identity or catalog preflight failed")
	// ErrNativeVerification is deliberately bounded: a local-dev uninstall
	// caller needs a proof that Codex is back on its native route, but must not
	// receive user TOML, state-file, or journal diagnostics on failure.
	ErrNativeVerification          = errors.New("native routing is not verified")
	ErrNativeRepairUnavailable     = errors.New("local-development native repair is unavailable")
	ErrNativeRepairGenerationStale = errors.New("routing generation changed before local-development native repair")
)

// LocalRelay is the deliberately narrow health projection consumed by the
// control plane. It contains no listener address, upstream URL, credential
// source, or server diagnostic text.
type LocalRelay struct {
	General     LocalRelayEndpoint
	Interactive LocalRelayEndpoint
}

type LocalRelayEndpoint struct {
	Valid               bool
	Generation          uint64
	DesiredMode         Mode
	AppliedMode         Mode
	Phase               Phase
	RelayAdmission      string
	CatalogRefresh      string
	RoutingStateInvalid bool
	ActiveRequests      *int64
	CatalogLifecycle    string
	RemoteGateway       string
	LocalOpenCodex      string
}

// HealthReader is intentionally injectable so transition and crash-recovery
// tests do not need a listening relay. Implementations must return an invalid
// endpoint rather than forwarding raw HTTP errors to user-facing status.
type HealthReader interface {
	Read(context.Context, config.Config) LocalRelay
}

type HTTPHealthReader struct {
	client *http.Client
}

func NewHTTPHealthReader() *HTTPHealthReader {
	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       15 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
	}
	return &HTTPHealthReader{client: &http.Client{
		Transport: transport,
		Timeout:   4 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (r *HTTPHealthReader) Read(ctx context.Context, cfg config.Config) LocalRelay {
	if r == nil || r.client == nil {
		return LocalRelay{}
	}
	return LocalRelay{
		General:     r.readEndpoint(ctx, cfg.ListenAddress, "general"),
		Interactive: r.readEndpoint(ctx, cfg.Responses.Scheduler.InteractiveListenAddress, "interactive"),
	}
}

func (r *HTTPHealthReader) readEndpoint(ctx context.Context, address, expectedLane string) LocalRelayEndpoint {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/__relay/healthz", nil)
	if err != nil {
		return LocalRelayEndpoint{}
	}
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil || response == nil {
		return LocalRelayEndpoint{}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LocalRelayEndpoint{}
	}
	var wire struct {
		OK                  bool   `json:"ok"`
		ListenerLane        string `json:"listener_lane"`
		ActiveRequests      *int64 `json:"active_requests"`
		RoutingGeneration   uint64 `json:"routing_generation"`
		RoutingDesiredMode  Mode   `json:"routing_desired_mode"`
		RoutingAppliedMode  Mode   `json:"routing_applied_mode"`
		RoutingPhase        Phase  `json:"routing_phase"`
		RelayAdmission      string `json:"relay_admission"`
		CatalogRefresh      string `json:"catalog_refresh"`
		RoutingStateInvalid bool   `json:"routing_state_invalid"`
		CatalogLifecycle    string `json:"catalog_lifecycle"`
		RemoteGateway       string `json:"remote_gateway"`
		LocalOpenCodex      string `json:"local_opencodex"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&wire); err != nil || !wire.OK || wire.ListenerLane != expectedLane {
		return LocalRelayEndpoint{}
	}
	return LocalRelayEndpoint{
		Valid:               true,
		Generation:          wire.RoutingGeneration,
		DesiredMode:         wire.RoutingDesiredMode,
		AppliedMode:         wire.RoutingAppliedMode,
		Phase:               wire.RoutingPhase,
		RelayAdmission:      wire.RelayAdmission,
		CatalogRefresh:      wire.CatalogRefresh,
		RoutingStateInvalid: wire.RoutingStateInvalid,
		ActiveRequests:      wire.ActiveRequests,
		CatalogLifecycle:    wire.CatalogLifecycle,
		RemoteGateway:       wire.RemoteGateway,
		LocalOpenCodex:      wire.LocalOpenCodex,
	}
}

type ControllerOption func(*Controller)

func WithHealthReader(reader HealthReader) ControllerOption {
	return func(controller *Controller) { controller.health = reader }
}

func WithTransitionTiming(ackTimeout, pollInterval time.Duration) ControllerOption {
	return func(controller *Controller) {
		if ackTimeout > 0 {
			controller.ackTimeout = ackTimeout
		}
		if pollInterval > 0 {
			controller.pollInterval = pollInterval
		}
	}
}

func withConfigLoader(load func(string) (config.Config, error)) ControllerOption {
	return func(controller *Controller) { controller.loadConfig = load }
}

func withCredentialLoader(load func(config.CredentialsConfig) (credentials.Values, error)) ControllerOption {
	return func(controller *Controller) { controller.loadCredentials = load }
}

func withGatewayValidator(validate func(context.Context, config.Config, credentials.Values) (catalog.Result, error)) ControllerOption {
	return func(controller *Controller) { controller.validateGateway = validate }
}

func withLocalOpenCodexPreflight(preflight func(context.Context, string) localopencodex.Result) ControllerOption {
	return func(controller *Controller) { controller.localPreflight = preflight }
}

// withLocalTargetPreflight is the typed test seam used by the authenticated
// Apple Container profile. Unlike the legacy native seam it carries the fixed
// host endpoint, expected guest identity port, authentication profile, and
// already-loaded API credential as one non-ambiguous target.
func withLocalTargetPreflight(preflight func(context.Context, localopencodex.Target) localopencodex.Result) ControllerOption {
	return func(controller *Controller) { controller.localTargetPreflight = preflight }
}

// withLocalCatalogMaterializer is a deterministic test seam for the
// synchronous Local profile catalog barrier. Production callers always use
// catalog.MaterializeLocalOpenCodexCatalog below.
func withLocalCatalogMaterializer(materialize func(context.Context, config.Config) error) ControllerOption {
	return func(controller *Controller) { controller.materializeLocalCatalog = materialize }
}

// withLocalProfileAllowed exists only for deterministic controller tests. The
// production default is deliberately macOS Apple Silicon only; callers cannot
// widen that platform boundary through relayctl input or relay.json.
func withLocalProfileAllowed(allowed bool) ControllerOption {
	return func(controller *Controller) { controller.localProfileOK = allowed }
}

// WithRuntimeControl binds a controller to the resident relay's narrow
// owner-only control socket. Tests may omit it to exercise only the durable
// transaction state machine; production relayctl must supply it so a profile
// apply cannot claim a switch without changing the live runtime.
func WithRuntimeControl(control RuntimeControl) ControllerOption {
	return func(controller *Controller) { controller.runtimeControl = control }
}

// WithCodexConfigOwner binds a controller to one compiled-in marker/profile
// namespace. The local-only development installer uses this to keep a shared
// Codex home fail-closed when production artifacts are present.
func WithCodexConfigOwner(owner codexconfig.Owner) ControllerOption {
	return func(controller *Controller) { controller.codexOwner = owner }
}

// WithControllerRecoveryGate binds ordinary routing mutations and status to an
// external durable recovery witness. Recover is permitted only after the
// witness has been safely released while routing is already recovery_required.
func WithControllerRecoveryGate(check RecoveryGate) ControllerOption {
	return func(controller *Controller) { controller.recoveryGate = check }
}

func WithControllerRecoveryGateReleasable(check func() bool) ControllerOption {
	return func(controller *Controller) { controller.recoveryGateReleasable = check }
}

// WithControllerRemovalRecoveryWitness binds Recover to an opaque routing
// snapshot captured for one removal-specific recovery. The witness only
// narrows recovery authority; the independent external recovery gate remains
// mandatory and ordinary controllers are unchanged.
func WithControllerRemovalRecoveryWitness(witness *RemovalRecoveryWitness) ControllerOption {
	return func(controller *Controller) { controller.removalRecoveryWitness = witness }
}

// Controller serializes all routing changes. Its only authority over Codex
// configuration is the exact config path bound in State; it never guesses a
// different CODEX_HOME or direct OpenAI endpoint.
type Controller struct {
	store                   *Store
	codexConfigPath         string
	health                  HealthReader
	loadConfig              func(string) (config.Config, error)
	loadCredentials         func(config.CredentialsConfig) (credentials.Values, error)
	validateGateway         func(context.Context, config.Config, credentials.Values) (catalog.Result, error)
	localPreflight          func(context.Context, string) localopencodex.Result
	localTargetPreflight    func(context.Context, localopencodex.Target) localopencodex.Result
	materializeLocalCatalog func(context.Context, config.Config) error
	ackTimeout              time.Duration
	pollInterval            time.Duration
	journalPath             string
	runtimeControl          RuntimeControl
	recoveryGate            RecoveryGate
	recoveryGateReleasable  func() bool
	removalRecoveryWitness  *RemovalRecoveryWitness
	localProfileOK          bool
	codexOwner              codexconfig.Owner
}

func NewController(configPath, codexConfigPath string, options ...ControllerOption) (*Controller, error) {
	store, err := Open(configPath)
	if err != nil {
		return nil, err
	}
	boundCodex, err := canonicalCodexConfigPath(codexConfigPath)
	if err != nil {
		return nil, err
	}
	controller := &Controller{
		store:                store,
		codexConfigPath:      boundCodex,
		health:               NewHTTPHealthReader(),
		loadConfig:           config.Load,
		loadCredentials:      credentials.Load,
		validateGateway:      validateExternalGatewayCatalog,
		localPreflight:       localopencodex.Preflight,
		localTargetPreflight: localopencodex.PreflightTarget,
		ackTimeout:           30 * time.Second,
		pollInterval:         100 * time.Millisecond,
		journalPath:          store.TransactionPath(),
		localProfileOK:       runtime.GOOS == "darwin" && runtime.GOARCH == "arm64",
		codexOwner:           codexconfig.ProductionOwner,
	}
	for _, option := range options {
		if option != nil {
			option(controller)
		}
	}
	if controller.health == nil {
		controller.health = NewHTTPHealthReader()
	}
	if controller.localPreflight == nil {
		controller.localPreflight = localopencodex.Preflight
	}
	if controller.localTargetPreflight == nil {
		controller.localTargetPreflight = localopencodex.PreflightTarget
	}
	if controller.materializeLocalCatalog == nil {
		controller.materializeLocalCatalog = controller.materializeLocalCatalogForBackend
	}
	canonicalOwner, err := codexconfig.OwnerForID(controller.codexOwner.ID)
	if err != nil {
		return nil, err
	}
	if canonicalOwner != controller.codexOwner {
		return nil, errors.New("Codex routing owner is not canonical")
	}
	return controller, nil
}

func (c *Controller) Store() *Store { return c.store }

func (c *Controller) recoveryGateActive() bool {
	return c != nil && c.recoveryGate != nil && c.recoveryGate() != nil
}

func (c *Controller) recoveryGateCanRelease() bool {
	return c != nil && c.recoveryGateReleasable != nil && c.recoveryGateReleasable()
}

// maintenancePendingOrInvalid is the ordinary routing controller's
// fail-closed interlock with lifecycle-owned container-runtime transactions.
// Only the corresponding runtime coordinator may consume either witness;
// routing, gateway, and repair operations must not race them or reinterpret
// them as an ordinary routing journal.
func (c *Controller) maintenancePendingOrInvalid() bool {
	if c == nil || c.store == nil {
		return true
	}
	_, found, err := c.store.loadMaintenance()
	if err != nil || found {
		return true
	}
	_, found, err = c.store.loadRuntimeRouting(c.codexConfigPath)
	return err != nil || found
}

// CatalogAdmissionAllowed is the non-mutating guard for ad-hoc catalog
// refresh commands. It intentionally fails closed before a caller loads
// credentials or constructs an upstream request. A pending native request is
// still admitted until Apply, matching the documented no-interruption phase.
func (c *Controller) CatalogAdmissionAllowed() bool {
	if c == nil || c.store == nil || c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return false
	}
	state, legacy, err := c.store.Read()
	if err != nil {
		return false
	}
	if legacy {
		state, err = c.inferLegacyState(state)
		if err != nil {
			return false
		}
	} else if state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil {
		return false
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return false
	}
	return state.AllowsCatalog()
}

// Status is deliberately resilient: a missing relay, invalid state, or
// malformed health response becomes a safe status value rather than an error
// whose text could be accidentally surfaced by the MenuBar.
func (c *Controller) Status(ctx context.Context) Status {
	gateActive := c.recoveryGateActive()
	state, legacy, stateErr := c.store.Read()
	stateValid := stateErr == nil
	if stateValid && legacy {
		state, stateErr = c.inferLegacyState(state)
		stateValid = stateErr == nil
	} else if stateValid {
		stateValid = state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) == nil
	}
	if !stateValid {
		state, _ = NewRecoveryState(c.store.ConfigPath())
	}

	// Observe the journal exactly once for this status snapshot. The same
	// witness determines both the fail-closed phase projection and the advisory
	// recovery capabilities, preventing a rename/remove race from advertising a
	// rollback that Recover would immediately reject.
	journal, journalFound, journalErr := c.loadJournal()
	maintenance, maintenanceFound, maintenanceErr := c.store.loadMaintenance()
	maintenanceSafeApplying := maintenanceErr == nil && maintenanceFound && !legacy && stateValid &&
		state.Phase == PhaseApplying && maintenance.matchesState(state)
	capabilities := c.recoveryCapabilities(state, stateValid, legacy, journal, journalFound, journalErr)
	gateStableJournalReady := gateActive && c.recoveryGateCanRelease() && stateValid && !legacy &&
		journalErr == nil && journalFound && stableNonLocalRecoveryCommit(state) &&
		removalRecoveryStableJournalRelation(state, journal) &&
		capabilities.CanComplete && !capabilities.CanRollback
	gateParkedJournalReady := gateActive && c.recoveryGateCanRelease() && stateValid && !legacy &&
		journalErr == nil && journalFound && state.Phase == PhaseRecoveryRequired &&
		nonLocalRecoveryTarget(state) && removalRecoveryParkedJournalRelation(state, journal) &&
		capabilities.CanComplete && !capabilities.CanRollback
	gateJournalReady := gateStableJournalReady || gateParkedJournalReady
	gateParkedObservedReady := gateActive && c.recoveryGateCanRelease() && stateValid && !legacy &&
		journalErr == nil && !journalFound && state.Phase == PhaseRecoveryRequired &&
		nonLocalRecoveryTarget(state) && capabilities.CanComplete && !capabilities.CanRollback &&
		capabilities.Target != BackendUnknown && capabilities.Target != BackendLocalOpenCodex &&
		capabilities.Target != BackendLocalAppleContainer
	gateJournalGeneration := uint64(0)
	if gateJournalReady {
		gateJournalGeneration = state.Generation
	}

	result := statusFromState(state, stateValid)
	if !stateValid {
		result.Connection.RoutingSync = RoutingSyncInvalid
	}
	if journalErr != nil || (journalFound && (legacy || state.Phase != PhaseApplying)) {
		result = statusFromState(recoveryStatusState(c.store.ConfigPath()), false)
		result.Connection.RoutingSync = RoutingSyncInvalid
		state = recoveryStatusState(c.store.ConfigPath())
		stateValid = false
	}
	if maintenanceErr != nil || (maintenanceFound && !maintenanceSafeApplying) || (maintenanceFound && journalFound) {
		result = statusFromState(recoveryStatusState(c.store.ConfigPath()), false)
		result.Connection.RoutingSync = RoutingSyncInvalid
		state = recoveryStatusState(c.store.ConfigPath())
		stateValid = false
		capabilities = unavailableRecoveryCapabilities(recoveryReasonObservedUnavailable, recoveryReasonObservedUnavailable)
	}
	gatedGeneration := uint64(0)
	gateStableCommitReady := gateActive && c.recoveryGateCanRelease() && stateValid && !legacy &&
		journalErr == nil && !journalFound && stableNonLocalRecoveryCommit(state)
	gateRecoveryReady := gateActive && c.recoveryGateCanRelease() && stateValid && !legacy &&
		(gateParkedObservedReady || gateStableCommitReady)
	gateRecoveryReady = gateRecoveryReady || gateJournalReady
	if gateStableCommitReady {
		// A releasable external gate may survive the final routing-state commit
		// when its owner crashes before clearing its own durable witness. Keep
		// the public projection fail-closed, but advertise only Complete so the
		// owner can reconcile that exact witness. Ordinary Controller.Recover
		// remains blocked by recoveryGateActive; only the owner's CLI-local
		// exact-token controller may consume this capability.
		capabilities = RecoveryCapabilities{
			CanComplete:      true,
			CompleteReason:   recoveryReasonObservedStateVerified,
			RollbackReason:   recoveryReasonOriginNotAuthoritative,
			Target:           state.AppliedBackend,
			TargetConfidence: recoveryTargetObserved,
		}
	} else if gateJournalReady {
		capabilities = RecoveryCapabilities{
			CanComplete:          true,
			CompleteReason:       recoveryReasonJournalVerified,
			RollbackReason:       recoveryReasonOriginNotAuthoritative,
			Target:               journal.TargetBackend,
			TargetConfidence:     recoveryTargetJournal,
			AuthoritativeJournal: false,
		}
	}
	if gateActive {
		// Keep the public recovery projection deliberately opaque, but retain
		// the generation only when it came from a successfully validated
		// durable routing state. A saved removal recovery needs that generation
		// to bind its next helper invocation; a malformed or legacy state must
		// never manufacture one.
		projectionState := state
		projectionValidated := stateValid && !legacy
		if gateJournalReady {
			// The stale recovery journal has already forced the public result
			// above into a synthetic recovery state. Retain only the generation
			// from the separately validated stable state observed before that
			// projection; never expose its backend fields.
			projectionState.Generation = gateJournalGeneration
			projectionValidated = true
		}
		result = statusFromState(recoveryProjectionState(c.store.ConfigPath(), projectionState, projectionValidated), true)
		result.Connection.RoutingSync = RoutingSyncInvalid
		state = recoveryProjectionState(c.store.ConfigPath(), state, stateValid && !legacy)
		gatedGeneration = result.Generation
		stateValid = false
		if !gateRecoveryReady {
			capabilities = unavailableRecoveryCapabilities(recoveryReasonObservedUnavailable, recoveryReasonObservedUnavailable)
		}
	}

	cfg, err := c.loadConfig(c.store.ConfigPath())
	if err != nil {
		return result.withRecoveryCapabilities(capabilities)
	}
	health := c.health.Read(ctx, cfg)
	dynamicLocalProfile := cfg.UpstreamMode == config.UpstreamModeExternalGateway &&
		(cfg.LocalOpenCodex != nil || cfg.LocalAppleContainer != nil)
	// The resident watcher can park on a journal/config drift before the
	// durable state file itself changes. Never let the public status retain an
	// old relay_active/allow projection in that case: callers must see the same
	// fail-closed recovery/deny state that the data plane is enforcing.
	if health.General.RoutingStateInvalid || health.Interactive.RoutingStateInvalid {
		state = recoveryStatusState(c.store.ConfigPath())
		stateValid = false
		result = statusFromState(state, false)
		if gateActive {
			// Health invalidation must not reintroduce the synthetic generation
			// that the gated projection deliberately removed.
			result.Generation = gatedGeneration
		}
	}
	result = result.withHealth(health, state, stateValid, dynamicLocalProfile)
	// Local readiness is a credentialless loopback identity/catalog check. It
	// is intentionally limited to an active External relay state: Native,
	// pending-native, applying, and recovery must not start new probes merely
	// because the MenuBar refreshes status.
	if c.localProfileOK && stateValid && state.Phase == PhaseRelayActive && state.AppliedBackend == BackendExternal && cfg.LocalOpenCodex != nil &&
		result.Connection.LocalRelay == LocalRelayHealthy && result.Connection.RoutingSync == RoutingSyncAcknowledged {
		result.Connection.LocalOpenCodex = localAvailabilityStatus(c.localPreflight(ctx, cfg.LocalOpenCodex.UpstreamBaseURL).Availability)
	}
	return result.withRecoveryCapabilities(capabilities)
}

func stableNonLocalRecoveryCommit(state State) bool {
	if state.Phase != PhaseRelayActive && state.Phase != PhaseNativeActive {
		return false
	}
	return nonLocalRecoveryTarget(state) &&
		state.DesiredMode == state.AppliedMode &&
		state.DesiredBackend == state.AppliedBackend
}

func nonLocalRecoveryTarget(state State) bool {
	return state.Generation > 0 &&
		state.DesiredBackend != BackendUnknown &&
		state.DesiredBackend != BackendLocalOpenCodex &&
		state.DesiredBackend != BackendLocalAppleContainer &&
		state.AppliedBackend != BackendUnknown &&
		state.AppliedBackend != BackendLocalOpenCodex &&
		state.AppliedBackend != BackendLocalAppleContainer
}

func recoveryStatusState(configPath string) State {
	state, err := NewRecoveryState(configPath)
	if err != nil {
		return State{Schema: SchemaVersion, Generation: 1, DesiredMode: ModeUnknown, AppliedMode: ModeUnknown, Phase: PhaseRecoveryRequired}
	}
	return state
}

// recoveryProjectionState deliberately exposes no durable routing destination
// while a separate recovery witness is active. The sole retained field is a
// generation obtained from a validated current state, so a continuation can
// still be bound to the same durable routing epoch without treating the route
// itself as safe to use.
func recoveryProjectionState(configPath string, durable State, retainGeneration bool) State {
	projected := recoveryStatusState(configPath)
	if retainGeneration && durable.Generation > 0 {
		projected.Generation = durable.Generation
	} else {
		// NewRecoveryState uses a synthetic positive generation so ordinary
		// recovery UI can render a status. That value is not a durable witness
		// and must never cross an externally gated status boundary.
		projected.Generation = 0
	}
	return projected
}

func (c *Controller) Request(ctx context.Context, target Mode) (Status, error) {
	switch target {
	case ModeNative:
		return c.RequestBackend(ctx, BackendNone)
	case ModeRelay:
		// `relay` historically meant the one static upstream configured for
		// this relay. Preserve that meaning for legacy Linux local_opencodex;
		// the canonical macOS profile remains External.
		cfg, err := c.loadConfig(c.store.ConfigPath())
		if err != nil {
			return Status{}, errors.New("relay configuration is unavailable")
		}
		if cfg.UpstreamMode == config.UpstreamModeLocalOpenCodex {
			return c.RequestBackend(ctx, BackendLocalOpenCodex)
		}
		return c.RequestBackend(ctx, BackendExternal)
	default:
		return Status{}, fmt.Errorf("unsupported routing mode %q", target)
	}
}

// SeedNativeParked writes the first durable state for an installer that has
// intentionally not taken ownership of a Codex config yet. It never reads or
// edits the Codex TOML, so a local-development bundle can install a health-only
// relay beside an existing production owner without reopening data-plane
// traffic. It is deliberately separate from RequestBackend, which infers the
// current owner in order to preserve an already active route.
func (c *Controller) SeedNativeParked(ctx context.Context) (Status, error) {
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrRecoveryRequired
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return Status{}, ErrRecoveryRequired
	}
	state, legacy, err := lock.store.Read()
	if err != nil {
		return Status{}, err
	}
	if !legacy {
		if err := state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath); err != nil {
			return Status{}, err
		}
		if state.Phase != PhaseNativeActive || state.DesiredBackend != BackendNone || state.AppliedBackend != BackendNone {
			return Status{}, ErrTransitionPending
		}
		return c.Status(ctx), nil
	}
	seed, err := NewRelayState(c.store.ConfigPath())
	if err != nil {
		return Status{}, err
	}
	seed, err = BindCodexConfig(seed, c.codexConfigPath)
	if err != nil {
		return Status{}, err
	}
	seed.DesiredMode = ModeNative
	seed.AppliedMode = ModeNative
	seed.DesiredBackend = BackendNone
	seed.AppliedBackend = BackendNone
	seed.Phase = PhaseNativeActive
	if err := lock.Save(seed); err != nil {
		return Status{}, err
	}
	return c.Status(ctx), nil
}

// VerifyNative proves the narrow ownership condition required before a
// local-development installer can remove its relay artifacts. It is strictly
// read-only: unlike Apply/Recover it never loads a credential, contacts the
// runtime control socket, or changes a file. It takes only an existing,
// no-create advisory lock so the complete state/journal/TOML proof cannot race
// a writer. A missing, legacy, malformed, pending, or unbound state is not
// proof; neither is a native-looking local marker beside an artifact owned by
// another relay namespace.
//
// The CLI limits this method to a local_development relay config. Keeping the
// config-scope check at that boundary lets the controller remain a generic
// owner-bound verifier while still validating the exact owner passed to it.
func (c *Controller) VerifyNative(ctx context.Context) (Status, error) {
	if c == nil || c.store == nil {
		return Status{}, ErrNativeVerification
	}
	lock, err := c.store.ReadLock(ctx)
	if err != nil {
		return Status{}, ErrNativeVerification
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrNativeVerification
	}

	state, legacy, err := c.store.Read()
	if err != nil || legacy {
		return Status{}, ErrNativeVerification
	}
	if err := state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath); err != nil {
		return Status{}, ErrNativeVerification
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return Status{}, ErrNativeVerification
	}
	if state.Phase != PhaseNativeActive ||
		state.DesiredMode != ModeNative || state.AppliedMode != ModeNative ||
		state.DesiredBackend != BackendNone || state.AppliedBackend != BackendNone {
		return Status{}, ErrNativeVerification
	}
	if err := codexconfig.ValidateNativeRoutingForOwner(c.codexConfigPath, c.codexOwner); err != nil {
		return Status{}, ErrNativeVerification
	}
	// Do not hold the writer protocol while the best-effort health projection
	// waits on an unreachable parked relay. The proof above is complete, and
	// this close changes no filesystem state.
	if err := lock.Close(); err != nil {
		return Status{}, ErrNativeVerification
	}

	// Status is a redacted projection and deliberately tolerates an
	// unreachable parked relay. The verification above is wholly local and
	// remains valid without a health acknowledgement.
	return c.Status(ctx), nil
}

// RepairNative replaces one valid, bound recovery_required state with a
// native_active state only after an explicit local-development repair
// confirmation and an independent proof that no known or unmanaged Codex
// routing artifact remains. It does not edit Codex TOML, load credentials,
// contact the runtime control socket, or infer a missing/corrupt state.
func (c *Controller) RepairNative(ctx context.Context, expectedGeneration uint64, confirmed bool) (Status, error) {
	if c == nil || c.store == nil ||
		c.codexOwner != codexconfig.LocalDevelopmentOwner ||
		!confirmed || expectedGeneration == 0 {
		return Status{}, ErrNativeRepairUnavailable
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, ErrNativeRepairUnavailable
	}
	defer lock.Close()

	// The removal gate and routing journal are independent durable witnesses.
	// Neither may be guessed away by this narrowly-scoped state repair.
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrNativeRepairUnavailable
	}
	if pending, transactionErr := c.store.HasPendingTransaction(); transactionErr != nil || pending {
		return Status{}, ErrNativeRepairUnavailable
	}

	state, legacy, err := c.store.Read()
	if err != nil || legacy {
		return Status{}, ErrNativeRepairUnavailable
	}
	if err := state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath); err != nil ||
		state.Phase != PhaseRecoveryRequired {
		return Status{}, ErrNativeRepairUnavailable
	}
	if state.Generation != expectedGeneration || state.Generation == ^uint64(0) {
		return Status{}, ErrNativeRepairGenerationStale
	}
	if err := codexconfig.ValidateNativeRoutingForOwner(c.codexConfigPath, c.codexOwner); err != nil {
		return Status{}, ErrNativeVerification
	}

	repaired := state
	repaired.Generation++
	repaired.DesiredMode = ModeNative
	repaired.AppliedMode = ModeNative
	repaired.DesiredBackend = BackendNone
	repaired.AppliedBackend = BackendNone
	repaired.Phase = PhaseNativeActive
	if err := lock.Replace(repaired); err != nil {
		return Status{}, ErrNativeRepairUnavailable
	}

	// Re-read both durable authorities while the writer lock is still held.
	// This proves the emitted status follows the exact state we committed.
	committed, legacy, err := c.store.Read()
	if err != nil || legacy ||
		committed.Generation != repaired.Generation ||
		committed.Phase != PhaseNativeActive ||
		committed.DesiredBackend != BackendNone ||
		committed.AppliedBackend != BackendNone ||
		committed.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath) != nil ||
		codexconfig.ValidateNativeRoutingForOwner(c.codexConfigPath, c.codexOwner) != nil {
		// A post-commit proof failure cannot be reported as an unchanged
		// refusal unless the exact recovery state is restored first.
		if restoreErr := lock.Replace(state); restoreErr != nil {
			return Status{}, ErrNativeRepairUnavailable
		}
		return Status{}, ErrNativeVerification
	}
	// The durable repair is already committed and revalidated. An advisory
	// unlock/descriptor-close error cannot be reported as a no-mutation repair
	// refusal; helper process exit will release the kernel lock.
	_ = lock.Close()
	return c.Status(ctx), nil
}

// RequestBackend records one explicit user-selected destination. It never
// changes TOML, credentials, catalog files, or the resident runtime; those
// operations remain behind Apply after a confirmed Desktop exit.
func (c *Controller) RequestBackend(ctx context.Context, target Backend) (Status, error) {
	return c.RequestBackendWithIntent(ctx, target, false)
}

// RequestBackendWithIntent records the bounded one-shot legacy migration
// intent beside an External transition. The actual TOML backup and mutation
// remain behind Apply and the confirmed Desktop-exit boundary.
func (c *Controller) RequestBackendWithIntent(ctx context.Context, target Backend, knownLegacyBackupAndMigrate bool) (Status, error) {
	return c.requestBackendWithIntentAndWitness(ctx, target, knownLegacyBackupAndMigrate, "", 0)
}

// RequestBackendWithIntentAndWitness binds a consumer switch to the exact
// gateway configuration and routing generation that passed the connection
// test. The comparisons happen under the routing-state lock immediately
// before recording the restart intent.
func (c *Controller) RequestBackendWithIntentAndWitness(
	ctx context.Context,
	target Backend,
	knownLegacyBackupAndMigrate bool,
	expectedConfigDigest string,
	expectedRoutingGeneration uint64,
) (Status, error) {
	if target != BackendExternal || len(expectedConfigDigest) != sha256.Size*2 ||
		!isLowerHex(expectedConfigDigest) || expectedRoutingGeneration == 0 {
		return Status{}, ErrGatewayRoutingChanged
	}
	return c.requestBackendWithIntentAndWitness(
		ctx,
		target,
		knownLegacyBackupAndMigrate,
		expectedConfigDigest,
		expectedRoutingGeneration,
	)
}

func (c *Controller) requestBackendWithIntentAndWitness(
	ctx context.Context,
	target Backend,
	knownLegacyBackupAndMigrate bool,
	expectedConfigDigest string,
	expectedRoutingGeneration uint64,
) (Status, error) {
	if !validBackend(target) {
		return Status{}, fmt.Errorf("unsupported routing backend %q", target)
	}
	if knownLegacyBackupAndMigrate && target != BackendExternal {
		return Status{}, ErrRecoveryRequired
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrRecoveryRequired
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return Status{}, ErrRecoveryRequired
	}
	state, legacy, err := c.boundState(lock)
	if err != nil {
		return Status{}, err
	}
	if expectedConfigDigest != "" {
		if state.Generation != expectedRoutingGeneration {
			return Status{}, ErrGatewayRoutingChanged
		}
		observedDigest, digestErr := fingerprintOptional(c.store.ConfigPath())
		if digestErr != nil || observedDigest != expectedConfigDigest {
			return Status{}, ErrGatewayConfigChanged
		}
	}
	next, changed, err := requestBackendTransition(state, target)
	if err != nil {
		return Status{}, err
	}
	// Re-selecting an already applied Local backend is an update/repair-safe
	// no-op, not a new 10100 enrollment. Avoid making an existing parked Local
	// selection impossible to preserve merely because its listener is currently
	// absent; any real destination change still performs the strict preflight.
	if changed && target != state.AppliedBackend {
		if err := c.preflightRequestedBackend(ctx, target); err != nil {
			return Status{}, err
		}
	}
	if knownLegacyBackupAndMigrate {
		if !changed || target == state.AppliedBackend {
			return Status{}, ErrTransitionPending
		}
		next.KnownLegacyBackupAndMigrate = true
	} else if changed {
		next.KnownLegacyBackupAndMigrate = false
	}
	if changed || legacy {
		next.Generation = nextGeneration(state, legacy)
		if err := lock.Save(next); err != nil {
			return Status{}, err
		}
	}
	return c.Status(ctx), nil
}

func (c *Controller) preflightRequestedBackend(ctx context.Context, target Backend) error {
	if target != BackendExternal && target != BackendLocalOpenCodex && target != BackendLocalAppleContainer {
		return nil
	}
	cfg, err := c.loadConfig(c.store.ConfigPath())
	if err != nil {
		return errors.New("relay configuration is unavailable")
	}
	switch target {
	case BackendExternal:
		if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
			return errors.New("External gateway profile is unavailable for this static relay topology")
		}
	case BackendLocalOpenCodex:
		if cfg.UpstreamMode == config.UpstreamModeLocalOpenCodex {
			// Legacy Remote local_opencodex is a different, static topology and
			// retains its remote-manager catalog contract.
			return nil
		}
		if !c.localProfileOK {
			return ErrLocalOpenCodexPreflight
		}
		local, err := cfg.LocalOpenCodexRuntimeConfig()
		if err != nil || !c.localPreflight(ctx, local.UpstreamBaseURL).Ready() {
			return ErrLocalOpenCodexPreflight
		}
	case BackendLocalAppleContainer:
		if !c.localProfileOK {
			return ErrLocalOpenCodexPreflight
		}
		local, err := cfg.LocalAppleContainerRuntimeConfig()
		if err != nil {
			return ErrLocalOpenCodexPreflight
		}
		values, err := c.loadCredentials(local.Credentials)
		if err != nil || values.ValidateForProfile(config.LocalAuthenticationOpenCodexAPIKey) != nil {
			return ErrCredentialPreflight
		}
		if !c.localTargetPreflight(ctx, localopencodex.AppleContainerTarget(values)).Ready() {
			return ErrLocalOpenCodexPreflight
		}
	}
	return nil
}

func (c *Controller) Cancel(ctx context.Context) (Status, error) {
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrRecoveryRequired
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return Status{}, ErrRecoveryRequired
	}
	state, legacy, err := c.boundState(lock)
	if err != nil {
		return Status{}, err
	}
	if legacy {
		return Status{}, ErrTransitionPending
	}
	next, err := cancelTransition(state)
	if err != nil {
		return Status{}, err
	}
	next.Generation = state.Generation + 1
	if err := lock.Save(next); err != nil {
		return Status{}, err
	}
	return c.Status(ctx), nil
}

func (c *Controller) Apply(ctx context.Context, desktopExited bool) (Status, error) {
	if !desktopExited {
		return Status{}, ErrDesktopExitConfirmation
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if c.recoveryGateActive() || c.maintenancePendingOrInvalid() {
		return Status{}, ErrRecoveryRequired
	}
	if _, found, err := c.loadJournal(); err != nil || found {
		return Status{}, ErrRecoveryRequired
	}
	state, legacy, err := c.boundState(lock)
	if err != nil {
		return Status{}, err
	}
	if legacy || (state.Phase != PhaseNativePendingRestart && state.Phase != PhaseRelayPendingRestart && state.Phase != PhaseBackendPendingRestart) {
		return Status{}, ErrTransitionPending
	}
	return c.applyLocked(ctx, lock, state)
}

// EnableCompatibility preserves the historical command spelling without its
// unsafe immediate mutation behavior. It is a deprecated request-only alias:
// a caller still needs a confirmed Desktop exit and Apply before the owned
// Codex routing block can change.
func (c *Controller) EnableCompatibility(ctx context.Context) (Status, error) {
	return c.Request(ctx, ModeRelay)
}

// DisableCompatibility preserves the historical command spelling without its
// unsafe immediate mutation behavior. It is a deprecated request-only alias.
func (c *Controller) DisableCompatibility(ctx context.Context) (Status, error) {
	return c.RequestBackend(ctx, BackendNone)
}

type RecoveryAction string

const (
	RecoveryComplete RecoveryAction = "complete"
	RecoveryRollback RecoveryAction = "rollback"
)

func (c *Controller) Recover(ctx context.Context, action RecoveryAction, desktopExited bool) (Status, error) {
	if action != RecoveryComplete && action != RecoveryRollback {
		return Status{}, errors.New("select exactly one routing recovery action")
	}
	if c.removalRecoveryWitness != nil && action != RecoveryComplete {
		return Status{}, ErrRecoveryRequired
	}
	lock, err := c.store.Lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if c.maintenancePendingOrInvalid() || (c.removalRecoveryWitness == nil && c.recoveryGateActive()) {
		return Status{}, ErrRecoveryRequired
	}

	state, legacy, stateErr := c.boundState(lock)
	transaction, journalFound, journalErr := c.loadJournal()
	if c.removalRecoveryWitness != nil {
		if c.recoveryGate == nil ||
			c.removalRecoveryWitness.matchesRecoveryInputsLocked(
				c,
				lock,
				state,
				legacy,
				stateErr,
				transaction,
				journalFound,
				journalErr,
			) != nil ||
			c.recoveryGateActive() {
			return Status{}, ErrRecoveryRequired
		}
	}
	if journalErr != nil {
		// A malformed journal cannot prove either an origin or a target, so
		// rollback remains unavailable. Completing is still possible, but only
		// through the same Desktop-confirmed park/drain/runtime-apply path as a
		// normal switch.  Never replace a parked state with an inferred active
		// profile: the resident runtime may still carry a different immutable
		// upstream after the crash.
		if action != RecoveryComplete {
			return Status{}, ErrRecoveryRequired
		}
		status, recoverErr := c.recoverObservedLocked(ctx, lock, state, legacy, stateErr, desktopExited)
		return c.finishRemovalWitnessRecoveryLocked(lock, status, recoverErr)
	}
	if !journalFound {
		// A deleted/corrupt state with no journal cannot safely tell us which
		// transition to roll back. Completing re-establishes the observed
		// marker-owned target through a fully parked runtime apply; it never
		// reopens an inferred route in place.
		if action != RecoveryComplete {
			return Status{}, ErrRecoveryRequired
		}
		if stateErr != nil || legacy || state.Phase == PhaseRecoveryRequired || state.Phase == PhaseApplying {
			status, recoverErr := c.recoverObservedLocked(ctx, lock, state, legacy, stateErr, desktopExited)
			return c.finishRemovalWitnessRecoveryLocked(lock, status, recoverErr)
		}
		return Status{}, ErrRecoveryRequired
	}
	// The journal is a crash witness, not a config backup. Before either
	// complete or rollback re-applies a managed route, prove that no tool or
	// user edited the observed files after the recorded stage. Otherwise a
	// recovery command could overwrite a legitimate intervening change.
	if err := c.journalMatchesCurrentFiles(transaction); err != nil {
		return Status{}, ErrRecoveryRequired
	}
	if transaction.Kind == transactionKindGatewayReload {
		if !desktopExited {
			return Status{}, ErrDesktopExitConfirmation
		}
		return c.recoverGatewayLocked(ctx, lock, state, legacy, stateErr, transaction, action)
	}

	// A valid journal is the only authority for completing or rolling back an
	// interrupted multi-file mutation. It also repairs a stale journal beside
	// an otherwise final-looking state: the resident watcher has already parked
	// that combination, so treating the journal as a crash witness is safer
	// than reopening traffic from the stale state.
	if stateErr != nil || legacy || state.Phase != PhaseRecoveryRequired {
		state, err = c.recoveryStateFromJournal(transaction)
		if err != nil {
			return Status{}, err
		}
		if err := lock.Replace(state); err != nil {
			return Status{}, err
		}
		legacy = false
	}

	target := state.DesiredBackend
	if target == BackendUnknown && journalFound {
		target = transaction.TargetBackend
	}
	if action == RecoveryRollback {
		if !journalFound || !transaction.OriginAuthoritative || !validBackend(transaction.OriginBackend) {
			return Status{}, ErrRecoveryRequired
		}
		target = transaction.OriginBackend
	}
	if !validBackend(target) {
		return Status{}, ErrRecoveryRequired
	}
	if !desktopExited {
		return Status{}, ErrDesktopExitConfirmation
	}
	status, recoverErr := c.applyRecoveryLockedWithOriginAuthority(
		ctx,
		lock,
		state,
		target,
		transaction.OriginAuthoritative,
		transaction.KnownLegacyBackupAndMigrate,
	)
	return c.finishRemovalWitnessRecoveryLocked(lock, status, recoverErr)
}

func (c *Controller) finishRemovalWitnessRecoveryLocked(
	lock *Lock,
	status Status,
	recoverErr error,
) (Status, error) {
	if recoverErr != nil || c.removalRecoveryWitness == nil {
		return status, recoverErr
	}
	if err := c.removalRecoveryWitness.rebindStableLocked(c, lock); err != nil {
		return Status{}, ErrRecoveryRequired
	}
	return status, nil
}

// observedRoutingState is the journal-free recovery rule. It reconciles only
// the local, marker-owned routing artifacts and never infers a target from an
// upstream URL, credential, or an opaque Desktop session.
func (c *Controller) observedRoutingState() (State, error) {
	state, err := NewRelayState(c.store.ConfigPath())
	if err != nil {
		return State{}, err
	}
	return c.inferLegacyState(state)
}

// recoverObservedLocked repairs only the journal-free crash case.  The
// marker-owned config can tell us the intended profile, but cannot prove what
// immutable upstream the resident relay still has in memory.  Represent that
// uncertainty as an opposite valid origin and force the regular parked
// runtime-control apply path.  This keeps the Desktop restart boundary and
// makes it impossible for recovery to silently turn a Local runtime into an
// External durable state (or vice versa).
func (c *Controller) recoverObservedLocked(ctx context.Context, lock *Lock, current State, legacy bool, stateErr error, desktopExited bool) (Status, error) {
	if !desktopExited {
		return Status{}, ErrDesktopExitConfirmation
	}
	observed, err := c.observedRoutingState()
	if err != nil {
		return Status{}, err
	}
	target := observed.DesiredBackend
	origin := forcedRecoveryOrigin(target)
	if !validBackend(target) || !validBackend(origin) || origin == target {
		return Status{}, ErrRecoveryRequired
	}

	recovery := observed
	recovery.Phase = PhaseRecoveryRequired
	recovery.AppliedBackend = origin
	recovery.AppliedMode = modeForBackend(origin)
	recovery.Generation++
	if stateErr == nil && !legacy && current.Generation >= recovery.Generation {
		recovery.Generation = current.Generation + 1
	}
	if err := lock.Replace(recovery); err != nil {
		return Status{}, err
	}
	return c.applyRecoveryLockedWithOriginAuthority(ctx, lock, recovery, target, false, false)
}

func forcedRecoveryOrigin(target Backend) Backend {
	switch target {
	case BackendExternal, BackendLocalOpenCodex, BackendLocalAppleContainer:
		return BackendNone
	case BackendNone:
		return BackendExternal
	default:
		return BackendUnknown
	}
}

func (c *Controller) applyLocked(ctx context.Context, lock *Lock, state State) (Status, error) {
	target := state.DesiredBackend
	if !validBackend(target) {
		return Status{}, ErrRecoveryRequired
	}
	preflight, err := c.preflightBackendWithLegacyIntent(target, state.KnownLegacyBackupAndMigrate)
	if err != nil {
		return Status{}, err
	}

	applying := state
	applying.Phase = PhaseApplying
	applying.KnownLegacyBackupAndMigrate = preflight.LegacyMigrationRequired
	applying.Generation++
	if err := lock.Save(applying); err != nil {
		return Status{}, err
	}
	return c.finishApplyingLocked(ctx, lock, applying, preflight.Config, target, preflight.LegacyMigrationRequired)
}

func (c *Controller) applyRecoveryLocked(ctx context.Context, lock *Lock, recovery State, target Backend) (Status, error) {
	return c.applyRecoveryLockedWithOriginAuthority(ctx, lock, recovery, target, true, recovery.KnownLegacyBackupAndMigrate)
}

// applyRecoveryLockedWithOriginAuthority distinguishes a journal-backed
// rollback witness from the deliberately synthetic origin used only to force
// a safe runtime rebuild after the witness was lost. The latter may complete
// a selected target, but must never later masquerade as proof of an original
// backend for rollback.
func (c *Controller) applyRecoveryLockedWithOriginAuthority(ctx context.Context, lock *Lock, recovery State, target Backend, originAuthoritative bool, preservedLegacyMigration bool) (Status, error) {
	preflight, err := c.preflightBackendWithLegacyIntent(target, recovery.KnownLegacyBackupAndMigrate)
	if err != nil {
		return Status{}, err
	}
	legacyMigrationRequired := preservedLegacyMigration || preflight.LegacyMigrationRequired
	applying := recovery
	applying.DesiredBackend = target
	if target == recovery.AppliedBackend {
		applying.AppliedBackend = recovery.DesiredBackend
	} else {
		applying.AppliedBackend = recovery.AppliedBackend
	}
	if !validBackend(applying.AppliedBackend) || applying.AppliedBackend == target {
		return Status{}, ErrRecoveryRequired
	}
	applying.DesiredMode = modeForBackend(target)
	applying.AppliedMode = modeForBackend(applying.AppliedBackend)
	applying.Phase = PhaseApplying
	applying.KnownLegacyBackupAndMigrate = legacyMigrationRequired
	applying.Generation++
	if err := lock.Replace(applying); err != nil {
		return Status{}, err
	}
	return c.finishApplyingLockedWithOriginAuthority(ctx, lock, applying, preflight.Config, target, originAuthoritative, legacyMigrationRequired)
}

func (c *Controller) finishApplyingLocked(ctx context.Context, lock *Lock, applying State, cfg config.Config, target Backend, legacyMigrationRequired bool) (Status, error) {
	return c.finishApplyingLockedWithOriginAuthority(ctx, lock, applying, cfg, target, true, legacyMigrationRequired)
}

func (c *Controller) finishApplyingLockedWithOriginAuthority(ctx context.Context, lock *Lock, applying State, cfg config.Config, target Backend, originAuthoritative bool, legacyMigrationRequired bool) (Status, error) {
	journal, err := c.newJournal(applying, target, legacyMigrationRequired)
	if err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	// The legacy migration creates a separate 0600 user backup, but this
	// privacy-bounded routing journal intentionally stores neither its path nor
	// the user's TOML. After the migration begins, it therefore cannot prove an
	// automatic restoration of the former OpenCodex root assignments. Never
	// advertise the ordinary Native rollback for this transaction: completion
	// remains recoverable, while a failed proof stays recovery_required with the
	// backup preserved for explicit review.
	journal.OriginAuthoritative = originAuthoritative && !legacyMigrationRequired
	if err := c.writeJournal(journal); err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	if err := c.awaitParked(ctx, cfg, applying); err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	// The resident Local lifecycle normally refreshes asynchronously. Materialize
	// its separate catalog while the previous runtime is still parked so the
	// Codex config below can never select a missing or unchecked Local catalog.
	// Legacy static local_opencodex keeps its Remote-manager ownership and is
	// intentionally outside this macOS relay-owned profile barrier.
	if (target == BackendLocalOpenCodex || target == BackendLocalAppleContainer) && cfg.Catalog.Owner == config.CatalogOwnerRelay {
		if err := c.materializeLocalCatalog(ctx, cfg); err != nil {
			return c.failApplyingLocked(lock, applying, ErrLocalOpenCodexPreflight)
		}
	}
	if c.runtimeControl != nil {
		if err := c.runtimeControl.Apply(ctx, applying.Generation, target); err != nil {
			return c.failApplyingLocked(lock, applying, ErrRelayAcknowledgement)
		}
	}
	if err := c.applyConfigWithLegacyIntent(target, cfg, legacyMigrationRequired); err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	updated, err := c.newJournal(applying, target, legacyMigrationRequired)
	if err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	journal.Stage = transactionConfigMutated
	journal.RelayConfigFingerprint = updated.RelayConfigFingerprint
	journal.CodexConfigFingerprint = updated.CodexConfigFingerprint
	journal.InteractiveFingerprint = updated.InteractiveFingerprint
	if err := c.writeJournal(journal); err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}

	// Make the final state durable while its crash witness still exists. A
	// restart between these writes therefore sees a final-looking state plus a
	// journal, parks the relay, and requires explicit recovery rather than
	// reopening traffic from an ambiguous half-commit.
	final := applying
	final.DesiredMode = modeForBackend(target)
	final.AppliedMode = modeForBackend(target)
	final.DesiredBackend = target
	final.AppliedBackend = target
	final.KnownLegacyBackupAndMigrate = false
	if target == BackendNone {
		final.Phase = PhaseNativeActive
	} else {
		final.Phase = PhaseRelayActive
	}
	final.Generation++
	if err := lock.Save(final); err != nil {
		return c.failApplyingLocked(lock, applying, err)
	}
	// Once the final state is durable, deleting the journal completes the
	// transaction. If this deletion fails, leave both artifacts intact: the
	// watcher will remain parked and `mode recover` can reconcile them.
	if err := c.removeJournal(); err != nil {
		return Status{}, err
	}
	// The state write is only durable intent until the resident watcher has
	// observed it and the active runtime reports the matching final phase. In
	// particular, wait for the new Local catalog lifecycle before returning to
	// the MenuBar, which may relaunch Desktop immediately after a successful
	// relayctl apply.
	if err := c.awaitFinalized(ctx, cfg, final); err != nil {
		return c.failApplyingLocked(lock, final, err)
	}
	return c.Status(ctx), nil
}

func materializeLocalOpenCodexCatalog(ctx context.Context, cfg config.Config) error {
	if cfg.UpstreamMode != config.UpstreamModeLocalOpenCodex || cfg.Catalog.Owner != config.CatalogOwnerRelay {
		return ErrLocalOpenCodexPreflight
	}
	if _, err := catalog.MaterializeLocalOpenCodexCatalog(ctx, cfg.UpstreamBaseURL, cfg.Catalog.Path); err != nil {
		return ErrLocalOpenCodexPreflight
	}
	return nil
}

func (c *Controller) materializeLocalCatalogForBackend(ctx context.Context, cfg config.Config) error {
	if cfg.Catalog.Owner != config.CatalogOwnerRelay {
		return ErrLocalOpenCodexPreflight
	}
	switch cfg.UpstreamMode {
	case config.UpstreamModeLocalOpenCodex:
		return materializeLocalOpenCodexCatalog(ctx, cfg)
	case config.UpstreamModeLocalAppleContainer:
		fetcher := catalog.LocalOpenCodexFetcher{
			BaseURL:               cfg.UpstreamBaseURL,
			CatalogPath:           cfg.Catalog.Path,
			ExpectedServicePort:   10100,
			AuthenticationProfile: config.LocalAuthenticationOpenCodexAPIKey,
			Credentials: func() (credentials.Values, error) {
				return c.loadCredentials(cfg.Credentials)
			},
		}
		if _, err := fetcher.Refresh(ctx); err != nil {
			return ErrLocalOpenCodexPreflight
		}
		return nil
	default:
		return ErrLocalOpenCodexPreflight
	}
}

func (c *Controller) failApplyingLocked(lock *Lock, state State, cause error) (Status, error) {
	recovery := state
	recovery.Phase = PhaseRecoveryRequired
	recovery.Generation++
	if err := lock.Save(recovery); err != nil {
		return Status{}, fmt.Errorf("%w; mark recovery: %v", cause, err)
	}
	return Status{}, cause
}

func (c *Controller) preflightBackend(target Backend) (config.Config, error) {
	preflight, err := c.preflightBackendWithLegacyIntent(target, false)
	return preflight.Config, err
}

type backendPreflight struct {
	Config                  config.Config
	LegacyMigrationRequired bool
}

func (c *Controller) preflightBackendWithLegacyIntent(target Backend, knownLegacyBackupAndMigrate bool) (backendPreflight, error) {
	cfg, err := c.loadConfig(c.store.ConfigPath())
	if err != nil {
		return backendPreflight{}, errors.New("relay configuration is unavailable")
	}
	result := backendPreflight{Config: cfg}
	switch target {
	case BackendNone:
		if err := c.preflightNative(); err != nil {
			return backendPreflight{}, err
		}
	case BackendExternal:
		if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
			return backendPreflight{}, errors.New("External gateway profile is unavailable for this static relay topology")
		}
		var preflightErr error
		if knownLegacyBackupAndMigrate {
			legacyPreflight, legacyErr := codexconfig.PlanLegacyMigrationWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner)
			preflightErr = legacyErr
			result.LegacyMigrationRequired = legacyPreflight.RequiresMigration
		} else {
			preflightErr = codexconfig.PreflightEnableWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner)
		}
		if preflightErr != nil {
			return backendPreflight{}, preflightErr
		}
		if cfg.UpstreamMode == config.UpstreamModeExternalGateway {
			values, err := c.loadCredentials(cfg.Credentials)
			if err != nil || values.ValidateForProfile(cfg.Credentials.RemoteAuthenticationProfile()) != nil {
				return backendPreflight{}, ErrCredentialPreflight
			}
		}
	case BackendLocalOpenCodex:
		if err := codexconfig.PreflightEnableWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner); err != nil {
			return backendPreflight{}, err
		}
		if cfg.UpstreamMode == config.UpstreamModeLocalOpenCodex {
			return result, nil
		}
		if !c.localProfileOK {
			return backendPreflight{}, ErrLocalOpenCodexPreflight
		}
		local, err := cfg.LocalOpenCodexRuntimeConfig()
		if err != nil {
			return backendPreflight{}, err
		}
		if result := c.localPreflight(context.Background(), local.UpstreamBaseURL); !result.Ready() {
			return backendPreflight{}, ErrCredentialPreflight
		}
		result.Config = local
		return result, nil
	case BackendLocalAppleContainer:
		if err := codexconfig.PreflightEnableWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner); err != nil {
			return backendPreflight{}, err
		}
		if !c.localProfileOK {
			return backendPreflight{}, ErrLocalOpenCodexPreflight
		}
		local, err := cfg.LocalAppleContainerRuntimeConfig()
		if err != nil {
			return backendPreflight{}, err
		}
		values, err := c.loadCredentials(local.Credentials)
		if err != nil || values.ValidateForProfile(config.LocalAuthenticationOpenCodexAPIKey) != nil {
			return backendPreflight{}, ErrCredentialPreflight
		}
		if !c.localTargetPreflight(context.Background(), localopencodex.AppleContainerTarget(values)).Ready() {
			return backendPreflight{}, ErrLocalOpenCodexPreflight
		}
		result.Config = local
		return result, nil
	default:
		return backendPreflight{}, ErrRecoveryRequired
	}
	return result, nil
}

func (c *Controller) preflightNative() error {
	inspection, err := codexconfig.InspectRoutingForOwner(c.codexConfigPath, c.codexOwner)
	if err != nil {
		return err
	}
	if inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile || inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider {
		return errors.New("Codex config contains a foreign routing override")
	}
	if inspection.ManagedRoot != inspection.InteractiveProfileManaged {
		return errors.New("relay-managed Codex routing artifacts are incomplete")
	}
	return nil
}

func (c *Controller) applyConfig(target Backend, cfg config.Config) error {
	return c.applyConfigWithLegacyIntent(target, cfg, false)
}

func (c *Controller) applyConfigWithLegacyIntent(target Backend, cfg config.Config, knownLegacyBackupAndMigrate bool) error {
	// Recovery may discover that the marker-owned artifacts already describe
	// the selected target. Preserve those byte-for-byte rather than needlessly
	// replacing a valid config/profile while the only repair required is the
	// parked in-memory runtime/state transaction.
	if routingArtifactsMatch(c.codexConfigPath, c.codexOwner, target, cfg) {
		return nil
	}
	switch target {
	case BackendNone:
		return codexconfig.DisableWithInteractiveProfileForOwner(c.codexConfigPath, c.codexOwner)
	case BackendExternal, BackendLocalOpenCodex, BackendLocalAppleContainer:
		if knownLegacyBackupAndMigrate {
			if target != BackendExternal {
				return ErrRecoveryRequired
			}
			_, err := codexconfig.EnableWithLegacyMigrationForOwner(
				c.codexConfigPath,
				c.codexOwner,
				"http://"+cfg.ListenAddress+"/v1",
				"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
				cfg.Catalog.Path,
			)
			return err
		}
		return codexconfig.EnableWithInteractiveProfileForOwner(
			c.codexConfigPath,
			c.codexOwner,
			"http://"+cfg.ListenAddress+"/v1",
			"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
			cfg.Catalog.Path,
		)
	default:
		return ErrRecoveryRequired
	}
}

func routingArtifactsMatch(codexConfigPath string, owner codexconfig.Owner, target Backend, cfg config.Config) bool {
	switch target {
	case BackendNone:
		return codexconfig.ValidateNativeRoutingForOwner(codexConfigPath, owner) == nil
	case BackendExternal, BackendLocalOpenCodex, BackendLocalAppleContainer:
		return codexconfig.ValidateManagedRoutingForOwner(
			codexConfigPath,
			owner,
			"http://"+cfg.ListenAddress+"/v1",
			"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
			cfg.Catalog.Path,
		) == nil
	default:
		return false
	}
}

func (c *Controller) awaitParked(ctx context.Context, cfg config.Config, state State) error {
	deadline, cancel := context.WithTimeout(ctx, c.ackTimeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	acknowledged := false
	for {
		health := c.health.Read(deadline, cfg)
		if healthAcknowledges(health.General, state) && healthAcknowledges(health.Interactive, state) {
			acknowledged = true
			if health.General.ActiveRequests != nil && *health.General.ActiveRequests == 0 {
				return nil
			}
		}
		select {
		case <-deadline.Done():
			if acknowledged {
				return ErrRequestDrain
			}
			return ErrRelayAcknowledgement
		case <-ticker.C:
		}
	}
}

func healthAcknowledges(endpoint LocalRelayEndpoint, state State) bool {
	return endpoint.Valid && !endpoint.RoutingStateInvalid &&
		endpoint.Generation == state.Generation &&
		endpoint.DesiredMode == state.DesiredMode &&
		endpoint.AppliedMode == state.AppliedMode &&
		endpoint.Phase == state.Phase &&
		endpoint.RelayAdmission == "deny" && endpoint.CatalogRefresh == "pause" &&
		endpoint.CatalogLifecycle == "paused"
}

// awaitFinalized proves that the resident watcher has moved out of applying
// before relayctl reports success to the MenuBar.  Native stays deliberately
// parked; a relay-owned Local profile additionally waits for its new catalog
// lifecycle and bounded identity projection to become usable.
func (c *Controller) awaitFinalized(ctx context.Context, cfg config.Config, state State) error {
	deadline, cancel := context.WithTimeout(ctx, c.ackTimeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	requireLocalCatalog := (state.AppliedBackend == BackendLocalOpenCodex || state.AppliedBackend == BackendLocalAppleContainer) &&
		cfg.Catalog.Owner == config.CatalogOwnerRelay
	for {
		health := c.health.Read(deadline, cfg)
		if finalHealthAcknowledges(health.General, state, requireLocalCatalog) &&
			finalHealthAcknowledges(health.Interactive, state, requireLocalCatalog) {
			return nil
		}
		select {
		case <-deadline.Done():
			return ErrRelayFinalization
		case <-ticker.C:
		}
	}
}

func finalHealthAcknowledges(endpoint LocalRelayEndpoint, state State, requireLocalCatalog bool) bool {
	if !healthAcknowledgesForStatus(endpoint, state) {
		return false
	}
	switch state.Phase {
	case PhaseNativeActive:
		return endpoint.RelayAdmission == "deny" && endpoint.CatalogRefresh == "pause" && endpoint.CatalogLifecycle == "paused"
	case PhaseRelayActive:
		if endpoint.RelayAdmission != "allow" || endpoint.CatalogRefresh != "run" {
			return false
		}
		if requireLocalCatalog {
			return endpoint.CatalogLifecycle == "running" && endpoint.LocalOpenCodex == string(LocalOpenCodexReady)
		}
		return true
	default:
		return false
	}
}

func (c *Controller) boundState(lock *Lock) (State, bool, error) {
	state, legacy, err := lock.store.Read()
	if err != nil {
		return State{}, false, err
	}
	if legacy {
		state, err = c.inferLegacyState(state)
		return state, true, err
	}
	if err := state.ValidateForCodexConfig(c.store.ConfigPath(), c.codexConfigPath); err != nil {
		return State{}, false, err
	}
	return state, false, nil
}

// inferLegacyState makes the first persisted state match the marker-owned
// Codex routing artifacts. A missing state file predates the controller, so
// assuming relay_active for an otherwise native config would make an initial
// `mode request relay` look applied before any Desktop restart occurred.
// Ambiguous or foreign routing stays fail-closed until the operator resolves
// it rather than guessing which backend owns a live Desktop session.
func (c *Controller) inferLegacyState(state State) (State, error) {
	inspection, err := codexconfig.InspectRoutingForOwner(c.codexConfigPath, c.codexOwner)
	if err != nil {
		return State{}, fmt.Errorf("inspect legacy Codex routing: %w", err)
	}
	if inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile || inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider ||
		inspection.ManagedRoot != inspection.InteractiveProfileManaged {
		return State{}, fmt.Errorf("%w: legacy Codex routing is ambiguous", ErrRecoveryRequired)
	}
	if !inspection.ManagedRoot {
		state.DesiredMode = ModeNative
		state.AppliedMode = ModeNative
		state.DesiredBackend = BackendNone
		state.AppliedBackend = BackendNone
		state.Phase = PhaseNativeActive
	} else {
		cfg, configErr := c.loadConfig(c.store.ConfigPath())
		if configErr != nil {
			return State{}, errors.New("relay configuration is unavailable")
		}
		backend, backendErr := managedBackendFromArtifacts(c.codexConfigPath, c.codexOwner, cfg)
		if backendErr != nil {
			return State{}, fmt.Errorf("%w: managed profile cannot be identified", ErrRecoveryRequired)
		}
		state.DesiredBackend = backend
		state.AppliedBackend = backend
		state.DesiredMode = ModeRelay
		state.AppliedMode = ModeRelay
		state.Phase = PhaseRelayActive
	}
	return BindCodexConfig(state, c.codexConfigPath)
}

// managedBackendFromArtifacts distinguishes the two relay-owned profiles
// using their separate catalog bindings, not the canonical relay.json
// upstream alone.  A macOS Local profile deliberately keeps relay.json as the
// External canonical profile, so treating every managed block as External
// would make journal-free recovery misreport (and potentially reopen) a
// resident Local runtime.  The helper returns only a bounded backend label.
func managedBackendFromArtifacts(codexConfigPath string, owner codexconfig.Owner, cfg config.Config) (Backend, error) {
	validate := func(candidate config.Config) error {
		return codexconfig.ValidateManagedRoutingForOwner(
			codexConfigPath,
			owner,
			"http://"+candidate.ListenAddress+"/v1",
			"http://"+candidate.Responses.Scheduler.InteractiveListenAddress+"/v1",
			candidate.Catalog.Path,
		)
	}

	switch cfg.UpstreamMode {
	case config.UpstreamModeLocalOpenCodex:
		// Preserve the pre-existing static Linux topology. Its one managed
		// catalog is the Remote-manager-owned local profile.
		if err := validate(cfg); err != nil {
			return BackendUnknown, err
		}
		return BackendLocalOpenCodex, nil
	case config.UpstreamModeExternalGateway:
		externalMatches := validate(cfg) == nil
		localMatches := false
		appleMatches := false
		if cfg.LocalOpenCodex != nil {
			local, err := cfg.LocalOpenCodexRuntimeConfig()
			if err == nil {
				localMatches = validate(local) == nil
			}
		}
		if cfg.LocalAppleContainer != nil {
			apple, err := cfg.LocalAppleContainerRuntimeConfig()
			if err == nil {
				appleMatches = validate(apple) == nil
			}
		}
		switch {
		case externalMatches && !localMatches && !appleMatches:
			return BackendExternal, nil
		case localMatches && !externalMatches && !appleMatches:
			return BackendLocalOpenCodex, nil
		case appleMatches && !externalMatches && !localMatches:
			return BackendLocalAppleContainer, nil
		default:
			// Both matches would make ownership ambiguous; neither match means a
			// managed block drifted. In either case, do not guess a backend.
			return BackendUnknown, ErrRecoveryRequired
		}
	default:
		return BackendUnknown, ErrRecoveryRequired
	}
}

func requestTransition(state State, target Mode) (State, bool, error) {
	return requestBackendTransition(state, backendForLegacyMode(target))
}

func requestBackendTransition(state State, target Backend) (State, bool, error) {
	if !validBackend(target) {
		return State{}, false, ErrRecoveryRequired
	}
	state = normalizeStateBackends(state)
	next := state
	if state.Phase == PhaseApplying {
		return State{}, false, ErrTransitionPending
	}
	if state.Phase == PhaseRecoveryRequired {
		return State{}, false, ErrRecoveryRequired
	}
	if target == state.DesiredBackend {
		return next, false, nil
	}
	// A request which returns to the actually applied backend simply cancels
	// the staged Desktop restart. It must never mutate the current runtime.
	if target == state.AppliedBackend {
		next.DesiredBackend = target
		next.DesiredMode = modeForBackend(target)
		next.AppliedMode = modeForBackend(target)
		next.Phase = activePhaseForBackend(target)
		next.KnownLegacyBackupAndMigrate = false
		return next, true, nil
	}
	next.DesiredBackend = target
	next.DesiredMode = modeForBackend(target)
	next.AppliedMode = modeForBackend(state.AppliedBackend)
	next.Phase = pendingPhaseForBackends(target, state.AppliedBackend)
	if next.Phase == "" {
		return State{}, false, ErrRecoveryRequired
	}
	return next, true, nil
}

func activePhaseForBackend(backend Backend) Phase {
	if backend == BackendNone {
		return PhaseNativeActive
	}
	if validRelayBackend(backend) {
		return PhaseRelayActive
	}
	return PhaseRecoveryRequired
}

func pendingPhaseForBackends(desired, applied Backend) Phase {
	switch {
	case desired == BackendNone && validRelayBackend(applied):
		return PhaseNativePendingRestart
	case validRelayBackend(desired) && applied == BackendNone:
		return PhaseRelayPendingRestart
	case validRelayBackend(desired) && validRelayBackend(applied) && desired != applied:
		return PhaseBackendPendingRestart
	default:
		return ""
	}
}

func cancelTransition(state State) (State, error) {
	next := state
	switch state.Phase {
	case PhaseNativePendingRestart, PhaseRelayPendingRestart, PhaseBackendPendingRestart:
		next.DesiredBackend = state.AppliedBackend
		next.DesiredMode = modeForBackend(state.AppliedBackend)
		next.AppliedMode = modeForBackend(state.AppliedBackend)
		next.Phase = activePhaseForBackend(state.AppliedBackend)
		next.KnownLegacyBackupAndMigrate = false
	default:
		return State{}, ErrTransitionPending
	}
	return next, nil
}

func nextGeneration(state State, legacy bool) uint64 {
	if legacy {
		return state.Generation
	}
	return state.Generation + 1
}

func oppositeMode(mode Mode) Mode {
	switch mode {
	case ModeRelay:
		return ModeNative
	case ModeNative:
		return ModeRelay
	default:
		return ModeUnknown
	}
}

// Status is the only public control-plane projection. It intentionally does
// not mirror arbitrary health JSON, config paths, upstream topology, or error
// strings into the MenuBar process.
// RecoveryCapabilities is a bounded, advisory projection of the exact
// evidence Recover will re-check while holding the routing writer lock. It
// never grants authority by itself and contains no path or journal payload.
type RecoveryCapabilities struct {
	CanComplete          bool    `json:"can_complete"`
	CanRollback          bool    `json:"can_rollback"`
	CompleteReason       string  `json:"complete_reason"`
	RollbackReason       string  `json:"rollback_reason"`
	Target               Backend `json:"target"`
	TargetConfidence     string  `json:"target_confidence"`
	AuthoritativeJournal bool    `json:"authoritative_journal"`
}

type Status struct {
	SchemaVersion          int                  `json:"schema_version"`
	DesiredMode            Mode                 `json:"desired_mode"`
	AppliedMode            Mode                 `json:"applied_mode"`
	DesiredBackend         Backend              `json:"desired_backend"`
	AppliedBackend         Backend              `json:"applied_backend"`
	Phase                  Phase                `json:"phase"`
	Generation             uint64               `json:"generation"`
	RelayAdmission         string               `json:"relay_admission"`
	CatalogRefresh         string               `json:"catalog_refresh"`
	RelayRunning           bool                 `json:"relay_running"`
	ActiveRequests         *int64               `json:"active_requests"`
	DesktopRestartRequired bool                 `json:"desktop_restart_required"`
	DesktopEffectiveMode   string               `json:"desktop_effective_mode"`
	Connection             Connection           `json:"connection"`
	RecoveryCapabilities   RecoveryCapabilities `json:"recovery_capabilities"`
}

type Connection struct {
	LocalRelay     LocalRelayStatus     `json:"local_relay"`
	LocalOpenCodex LocalOpenCodexStatus `json:"local_opencodex"`
	RoutingSync    RoutingSync          `json:"routing_sync"`
	RemoteGateway  RemoteGateway        `json:"remote_gateway"`
	Catalog        CatalogStatus        `json:"catalog"`
}

type LocalRelayStatus string
type LocalOpenCodexStatus string
type RoutingSync string
type RemoteGateway string
type CatalogStatus string

const (
	LocalRelayHealthy     LocalRelayStatus = "healthy"
	LocalRelayDegraded    LocalRelayStatus = "degraded"
	LocalRelayUnreachable LocalRelayStatus = "unreachable"
	LocalRelayUnknown     LocalRelayStatus = "unknown"

	LocalOpenCodexReady       LocalOpenCodexStatus = "ready"
	LocalOpenCodexUnavailable LocalOpenCodexStatus = "unavailable"
	LocalOpenCodexForeign     LocalOpenCodexStatus = "foreign"
	LocalOpenCodexInvalid     LocalOpenCodexStatus = "invalid"
	LocalOpenCodexUnknown     LocalOpenCodexStatus = "unknown"

	RoutingSyncAcknowledged RoutingSync = "acknowledged"
	RoutingSyncPending      RoutingSync = "pending"
	RoutingSyncUnreachable  RoutingSync = "unreachable"
	RoutingSyncInvalid      RoutingSync = "invalid"

	RemoteGatewayReachable     RemoteGateway = "reachable"
	RemoteGatewayUnreachable   RemoteGateway = "unreachable"
	RemoteGatewayUnknown       RemoteGateway = "unknown"
	RemoteGatewayNotApplicable RemoteGateway = "not_applicable"

	CatalogRunning CatalogStatus = "running"
	CatalogPaused  CatalogStatus = "paused"
	CatalogUnknown CatalogStatus = "unknown"
)

func statusFromState(state State, valid bool) Status {
	if !valid {
		state = recoveryStatusState(state.BoundConfigPath)
	}
	admission := "deny"
	catalogRefresh := "pause"
	if valid && state.AllowsDataPlane() {
		admission = "allow"
		catalogRefresh = "run"
	}
	return Status{
		SchemaVersion:          statusSchemaVersion,
		DesiredMode:            state.DesiredMode,
		AppliedMode:            state.AppliedMode,
		DesiredBackend:         state.DesiredBackend,
		AppliedBackend:         state.AppliedBackend,
		Phase:                  state.Phase,
		Generation:             state.Generation,
		RelayAdmission:         admission,
		CatalogRefresh:         catalogRefresh,
		DesktopRestartRequired: state.DesiredBackend != state.AppliedBackend || state.Phase == PhaseRecoveryRequired,
		DesktopEffectiveMode:   "unverifiable",
		Connection: Connection{
			LocalRelay:     LocalRelayUnknown,
			LocalOpenCodex: LocalOpenCodexUnknown,
			RoutingSync:    RoutingSyncUnreachable,
			RemoteGateway:  RemoteGatewayUnknown,
			Catalog:        CatalogUnknown,
		},
	}
}

func (c *Controller) recoveryCapabilities(
	state State,
	stateValid bool,
	legacy bool,
	journal transactionJournal,
	journalFound bool,
	journalErr error,
) RecoveryCapabilities {
	if journalErr != nil {
		return c.observedRecoveryCapabilities(recoveryReasonJournalMalformed)
	}
	if !journalFound {
		if stateValid && !legacy && state.Phase != PhaseRecoveryRequired && state.Phase != PhaseApplying {
			return unavailableRecoveryCapabilities(recoveryReasonNotRequired, recoveryReasonNotRequired)
		}
		return c.observedRecoveryCapabilities(recoveryReasonJournalMissing)
	}

	capabilities := RecoveryCapabilities{
		CompleteReason:   recoveryReasonJournalMismatch,
		RollbackReason:   recoveryReasonJournalMismatch,
		Target:           journal.TargetBackend,
		TargetConfidence: recoveryTargetUnavailable,
	}
	if err := c.journalMatchesCurrentFiles(journal); err != nil {
		return capabilities
	}
	if journal.Kind == transactionKindGatewayReload {
		observedStage, stageErr := c.gatewayJournalObservedStage(journal)
		if stageErr != nil {
			return capabilities
		}
		capabilities.CanRollback = journal.OriginAuthoritative
		capabilities.RollbackReason = recoveryReasonJournalVerified
		capabilities.CanComplete = observedStage == transactionConfigMutated
		capabilities.CompleteReason = recoveryReasonJournalVerified
		capabilities.Target = BackendExternal
		capabilities.TargetConfidence = recoveryTargetJournal
		capabilities.AuthoritativeJournal = journal.OriginAuthoritative
		if !capabilities.CanComplete {
			capabilities.CompleteReason = recoveryReasonObservedUnavailable
		}
		return capabilities
	}
	if !validBackend(journal.TargetBackend) {
		capabilities.CompleteReason = recoveryReasonObservedUnavailable
		capabilities.RollbackReason = recoveryReasonObservedUnavailable
		return capabilities
	}

	capabilities.CanComplete = true
	capabilities.CompleteReason = recoveryReasonJournalVerified
	capabilities.TargetConfidence = recoveryTargetJournal
	capabilities.AuthoritativeJournal = journal.OriginAuthoritative
	if journal.OriginAuthoritative && validBackend(journal.OriginBackend) {
		capabilities.CanRollback = true
		capabilities.RollbackReason = recoveryReasonJournalVerified
	} else {
		capabilities.RollbackReason = recoveryReasonOriginNotAuthoritative
	}
	return capabilities
}

func (c *Controller) observedRecoveryCapabilities(rollbackReason string) RecoveryCapabilities {
	observed, err := c.observedRoutingState()
	if err != nil {
		return unavailableRecoveryCapabilities(recoveryReasonObservedUnavailable, rollbackReason)
	}
	target := observed.DesiredBackend
	origin := forcedRecoveryOrigin(target)
	if !validBackend(target) || !validBackend(origin) || target == origin {
		return unavailableRecoveryCapabilities(recoveryReasonObservedUnavailable, rollbackReason)
	}
	return RecoveryCapabilities{
		CanComplete:      true,
		CompleteReason:   recoveryReasonObservedStateVerified,
		RollbackReason:   rollbackReason,
		Target:           target,
		TargetConfidence: recoveryTargetObserved,
	}
}

func unavailableRecoveryCapabilities(completeReason, rollbackReason string) RecoveryCapabilities {
	return RecoveryCapabilities{
		CompleteReason:   completeReason,
		RollbackReason:   rollbackReason,
		Target:           BackendUnknown,
		TargetConfidence: recoveryTargetUnavailable,
	}
}

func (s Status) withRecoveryCapabilities(capabilities RecoveryCapabilities) Status {
	if s.Phase != PhaseRecoveryRequired {
		capabilities = unavailableRecoveryCapabilities(recoveryReasonNotRequired, recoveryReasonNotRequired)
	}
	s.RecoveryCapabilities = capabilities
	return s
}

func (s Status) withHealth(health LocalRelay, state State, stateValid bool, dynamicLocalProfile bool) Status {
	if !health.General.Valid {
		s.Connection.LocalRelay = LocalRelayUnreachable
		s.Connection.RoutingSync = RoutingSyncUnreachable
		return s
	}
	if !health.Interactive.Valid {
		s.Connection.LocalRelay = LocalRelayDegraded
	} else {
		s.Connection.LocalRelay = LocalRelayHealthy
		s.RelayRunning = true
	}
	if health.General.ActiveRequests != nil {
		value := *health.General.ActiveRequests
		if value >= 0 {
			s.ActiveRequests = &value
		}
	}
	if !stateValid || health.General.RoutingStateInvalid {
		s.Connection.RoutingSync = RoutingSyncInvalid
	} else if healthAcknowledgesForStatus(health.General, state) && healthAcknowledgesForStatus(health.Interactive, state) {
		s.Connection.RoutingSync = RoutingSyncAcknowledged
	} else {
		s.Connection.RoutingSync = RoutingSyncPending
	}
	s.Connection.RemoteGateway = normalizeRemoteGateway(health.General.RemoteGateway)
	s.Connection.LocalOpenCodex = normalizeLocalOpenCodex(health.General.LocalOpenCodex)
	s.Connection.Catalog = normalizeCatalog(health.General.CatalogLifecycle, health.General.CatalogRefresh)
	// A runtime-local loss is intentionally not rewritten as External. The
	// durable state continues to record the user's Local selection, while this
	// status projection tells the MenuBar that the resident manager has parked
	// admission and catalog work until an explicit External/Native apply.
	if dynamicLocalProfile &&
		(state.AppliedBackend == BackendLocalOpenCodex || state.AppliedBackend == BackendLocalAppleContainer) &&
		s.Connection.LocalOpenCodex != LocalOpenCodexReady {
		s.RelayAdmission = "deny"
		s.CatalogRefresh = "pause"
	}
	return s
}

func normalizeLocalOpenCodex(value string) LocalOpenCodexStatus {
	switch LocalOpenCodexStatus(value) {
	case LocalOpenCodexReady, LocalOpenCodexUnavailable, LocalOpenCodexForeign, LocalOpenCodexInvalid:
		return LocalOpenCodexStatus(value)
	default:
		return LocalOpenCodexUnknown
	}
}

func localAvailabilityStatus(value localopencodex.Availability) LocalOpenCodexStatus {
	switch value {
	case localopencodex.AvailabilityReady:
		return LocalOpenCodexReady
	case localopencodex.AvailabilityUnavailable:
		return LocalOpenCodexUnavailable
	case localopencodex.AvailabilityForeign:
		return LocalOpenCodexForeign
	case localopencodex.AvailabilityInvalid:
		return LocalOpenCodexInvalid
	default:
		return LocalOpenCodexUnknown
	}
}

func healthAcknowledgesForStatus(endpoint LocalRelayEndpoint, state State) bool {
	return endpoint.Valid && !endpoint.RoutingStateInvalid && endpoint.Generation == state.Generation &&
		endpoint.DesiredMode == state.DesiredMode && endpoint.AppliedMode == state.AppliedMode && endpoint.Phase == state.Phase
}

func normalizeRemoteGateway(value string) RemoteGateway {
	switch RemoteGateway(value) {
	case RemoteGatewayReachable, RemoteGatewayUnreachable, RemoteGatewayNotApplicable:
		return RemoteGateway(value)
	default:
		return RemoteGatewayUnknown
	}
}

func normalizeCatalog(lifecycle, refresh string) CatalogStatus {
	switch CatalogStatus(lifecycle) {
	case CatalogRunning, CatalogPaused:
		return CatalogStatus(lifecycle)
	}
	switch refresh {
	case "run":
		return CatalogRunning
	case "pause":
		return CatalogPaused
	default:
		return CatalogUnknown
	}
}

type transactionKind string

const (
	transactionKindRoutingSwitch transactionKind = "routing_switch"
	transactionKindGatewayReload transactionKind = "gateway_reload"
)

type transactionStage string

const (
	transactionPrepared      transactionStage = "prepared"
	transactionConfigMutated transactionStage = "config_mutated"
)

// transactionJournal keeps only non-secret fingerprints. It is a crash
// witness, not a configuration backup: recovery always re-inspects current
// files and refuses foreign routing rather than restoring stale TOML content.
type transactionJournal struct {
	Schema        int             `json:"schema"`
	Kind          transactionKind `json:"kind"`
	Generation    uint64          `json:"generation"`
	Target        Mode            `json:"target"`
	Origin        Mode            `json:"origin"`
	TargetBackend Backend         `json:"target_backend"`
	OriginBackend Backend         `json:"origin_backend"`
	// OriginAuthoritative is false only for a journal synthesized while
	// recovering after the original witness was lost. Such a transaction may
	// complete its explicit target but can never be used to guess a rollback
	// destination.
	OriginAuthoritative bool `json:"origin_authoritative"`
	// KnownLegacyBackupAndMigrate records that a recognized legacy migration
	// was actually required, or that an older durable journal says it may have
	// begun. It is not merely the user's one-shot request intent.
	KnownLegacyBackupAndMigrate  bool             `json:"known_legacy_backup_and_migrate,omitempty"`
	Stage                        transactionStage `json:"stage"`
	RelayConfigFingerprint       string           `json:"relay_config_fingerprint"`
	CodexConfigFingerprint       string           `json:"codex_config_fingerprint"`
	InteractiveFingerprint       string           `json:"interactive_fingerprint"`
	OriginRelayConfigFingerprint string           `json:"origin_relay_config_fingerprint,omitempty"`
	TargetRelayConfigFingerprint string           `json:"target_relay_config_fingerprint,omitempty"`
	BackupConfigFingerprint      string           `json:"backup_config_fingerprint,omitempty"`
}

// ValidateTransaction verifies the complete non-secret journal shape for the
// resident watcher. The watcher calls this only after a safe presence check;
// exposing the validator prevents malformed JSON from masquerading as an
// active controller transaction.
func ValidateTransaction(configPath string) error {
	store, err := Open(configPath)
	if err != nil {
		return err
	}
	controller := &Controller{store: store, journalPath: store.TransactionPath()}
	_, found, err := controller.loadJournal()
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	return nil
}

func (c *Controller) newJournal(state State, target Backend, legacyMigrationRequired bool) (transactionJournal, error) {
	relayFingerprint, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil {
		return transactionJournal{}, err
	}
	codexFingerprint, err := fingerprintOptional(c.codexConfigPath)
	if err != nil {
		return transactionJournal{}, err
	}
	interactiveFingerprint, err := fingerprintOptional(codexconfig.InteractiveProfilePathForOwner(c.codexConfigPath, c.codexOwner))
	if err != nil {
		return transactionJournal{}, err
	}
	return transactionJournal{
		Schema:                      SchemaVersion,
		Kind:                        transactionKindRoutingSwitch,
		Generation:                  state.Generation,
		Target:                      modeForBackend(target),
		Origin:                      modeForBackend(state.AppliedBackend),
		TargetBackend:               target,
		OriginBackend:               state.AppliedBackend,
		OriginAuthoritative:         true,
		KnownLegacyBackupAndMigrate: legacyMigrationRequired,
		Stage:                       transactionPrepared,
		RelayConfigFingerprint:      relayFingerprint,
		CodexConfigFingerprint:      codexFingerprint,
		InteractiveFingerprint:      interactiveFingerprint,
	}, nil
}

func (c *Controller) journalMatchesCurrentFiles(journal transactionJournal) error {
	relayFingerprint, err := fingerprintOptional(c.store.ConfigPath())
	expectedRelayFingerprint := journal.RelayConfigFingerprint
	if journal.Kind == transactionKindGatewayReload {
		if _, stageErr := c.gatewayJournalObservedStage(journal); stageErr != nil {
			return ErrRecoveryRequired
		}
		// The observed config may be either side of the atomic replacement.
		// gatewayJournalObservedStage proves which side without trusting the
		// last journal-stage write to have completed before a crash.
		expectedRelayFingerprint = relayFingerprint
	}
	if err != nil || relayFingerprint != expectedRelayFingerprint {
		return ErrRecoveryRequired
	}
	codexFingerprint, err := fingerprintOptional(c.codexConfigPath)
	if err != nil || codexFingerprint != journal.CodexConfigFingerprint {
		return ErrRecoveryRequired
	}
	interactiveFingerprint, err := fingerprintOptional(codexconfig.InteractiveProfilePathForOwner(c.codexConfigPath, c.codexOwner))
	if err != nil || interactiveFingerprint != journal.InteractiveFingerprint {
		return ErrRecoveryRequired
	}
	return nil
}

func (c *Controller) gatewayJournalObservedStage(journal transactionJournal) (transactionStage, error) {
	if journal.Kind != transactionKindGatewayReload {
		return "", ErrRecoveryRequired
	}
	backupFingerprint, err := fingerprintOptional(gatewayBackupPath(c.store.ConfigPath()))
	if err != nil || backupFingerprint != journal.BackupConfigFingerprint {
		return "", ErrRecoveryRequired
	}
	relayFingerprint, err := fingerprintOptional(c.store.ConfigPath())
	if err != nil {
		return "", ErrRecoveryRequired
	}
	switch relayFingerprint {
	case journal.OriginRelayConfigFingerprint:
		return transactionPrepared, nil
	case journal.TargetRelayConfigFingerprint:
		return transactionConfigMutated, nil
	default:
		return "", ErrRecoveryRequired
	}
}

func (c *Controller) loadJournal() (transactionJournal, bool, error) {
	payload, err := readStateFile(c.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return transactionJournal{}, false, nil
	}
	if err != nil {
		return transactionJournal{}, false, err
	}
	if len(payload) > maxJournalBytes {
		return transactionJournal{}, false, fmt.Errorf("%w: routing transaction journal exceeds limit", ErrStateCorrupt)
	}
	decoder := json.NewDecoder(bytesReader(payload))
	decoder.DisallowUnknownFields()
	var journal transactionJournal
	if err := decoder.Decode(&journal); err != nil {
		return transactionJournal{}, false, fmt.Errorf("%w: decode routing transaction journal", ErrStateCorrupt)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return transactionJournal{}, false, fmt.Errorf("%w: trailing routing transaction journal data", ErrStateCorrupt)
	}
	journal = normalizeTransactionJournal(journal)
	if err := journal.validate(); err != nil {
		return transactionJournal{}, false, err
	}
	return journal, true, nil
}

// bytesReader keeps controller.go independent from any accidental config
// string conversion. It is a named helper solely to make journal decoding
// conspicuous in code review.
func bytesReader(payload []byte) io.Reader { return &sliceReader{payload: payload} }

type sliceReader struct{ payload []byte }

func (r *sliceReader) Read(target []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, io.EOF
	}
	count := copy(target, r.payload)
	r.payload = r.payload[count:]
	return count, nil
}

func (j transactionJournal) validate() error {
	if j.Schema != SchemaVersion || (j.Kind != transactionKindRoutingSwitch && j.Kind != transactionKindGatewayReload) ||
		j.Generation == 0 || !validBackend(j.TargetBackend) || !validBackend(j.OriginBackend) ||
		j.Target != modeForBackend(j.TargetBackend) || j.Origin != modeForBackend(j.OriginBackend) ||
		(j.Stage != transactionPrepared && j.Stage != transactionConfigMutated) {
		return fmt.Errorf("%w: invalid routing transaction journal", ErrStateCorrupt)
	}
	if j.Kind == transactionKindGatewayReload &&
		(j.TargetBackend != BackendExternal || j.OriginBackend != BackendExternal ||
			j.OriginRelayConfigFingerprint == "" || j.TargetRelayConfigFingerprint == "" || j.BackupConfigFingerprint == "") {
		return fmt.Errorf("%w: invalid gateway transaction journal", ErrStateCorrupt)
	}
	if j.KnownLegacyBackupAndMigrate && (j.Kind != transactionKindRoutingSwitch || j.TargetBackend != BackendExternal) {
		return fmt.Errorf("%w: invalid legacy migration transaction", ErrStateCorrupt)
	}
	for _, fingerprint := range []string{
		j.RelayConfigFingerprint,
		j.CodexConfigFingerprint,
		j.InteractiveFingerprint,
		j.OriginRelayConfigFingerprint,
		j.TargetRelayConfigFingerprint,
		j.BackupConfigFingerprint,
	} {
		if fingerprint == "" {
			continue
		}
		if fingerprint != "absent" && (len(fingerprint) != sha256.Size*2 || !isLowerHex(fingerprint)) {
			return fmt.Errorf("%w: invalid routing transaction fingerprint", ErrStateCorrupt)
		}
	}
	return nil
}

func normalizeTransactionJournal(journal transactionJournal) transactionJournal {
	if journal.Kind == "" {
		journal.Kind = transactionKindRoutingSwitch
	}
	if journal.Schema == legacySchemaVersion {
		journal.Schema = SchemaVersion
		journal.TargetBackend = backendForLegacyMode(journal.Target)
		journal.OriginBackend = backendForLegacyMode(journal.Origin)
		journal.OriginAuthoritative = true
	} else if journal.Schema == explicitBackendSchemaVersion {
		// A v2 journal could not name the Apple Container backend. Leave a
		// future-labelled v2 document at its old schema so validate rejects it
		// instead of silently granting it v3 meaning.
		if validSchemaV2Backend(journal.TargetBackend) && validSchemaV2Backend(journal.OriginBackend) {
			journal.Schema = SchemaVersion
		}
	}
	return journal
}

func (c *Controller) writeJournal(journal transactionJournal) error {
	journal = normalizeTransactionJournal(journal)
	if err := journal.validate(); err != nil {
		return err
	}
	if err := validateExistingControlFile(c.journalPath); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(filepath.Dir(c.journalPath), 0o700); err != nil {
		return fmt.Errorf("create routing transaction directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(c.journalPath), ".routing-transaction.")
	if err != nil {
		return fmt.Errorf("create routing transaction journal: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, c.journalPath); err != nil {
		return fmt.Errorf("replace routing transaction journal: %w", err)
	}
	return syncDirectory(filepath.Dir(c.journalPath))
}

func (c *Controller) removeJournal() error {
	if err := validateExistingControlFile(c.journalPath); err != nil {
		return err
	}
	err := os.Remove(c.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove routing transaction journal: %w", err)
	}
	return syncDirectory(filepath.Dir(c.journalPath))
}

func (c *Controller) recoveryStateFromJournal(journal transactionJournal) (State, error) {
	state, err := NewRecoveryState(c.store.ConfigPath())
	if err != nil {
		return State{}, err
	}
	state, err = BindCodexConfig(state, c.codexConfigPath)
	if err != nil {
		return State{}, err
	}
	state.Generation = journal.Generation + 1
	state.DesiredMode = journal.Target
	state.AppliedMode = journal.Origin
	state.DesiredBackend = journal.TargetBackend
	state.AppliedBackend = journal.OriginBackend
	state.KnownLegacyBackupAndMigrate = journal.KnownLegacyBackupAndMigrate
	return state, nil
}

func fingerprintOptional(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: transaction fingerprint target must be regular", ErrStateCorrupt)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if count > 1<<20 {
		return "", fmt.Errorf("%w: transaction fingerprint target exceeds limit", ErrStateCorrupt)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer directory.Close()
	// Filesystems without directory fsync support have already completed the
	// atomic rename/remove; do not turn that platform limitation into a false
	// transition failure.
	_ = directory.Sync()
	return nil
}
