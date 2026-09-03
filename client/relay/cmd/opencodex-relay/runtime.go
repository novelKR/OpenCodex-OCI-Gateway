package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	gort "runtime"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

// relayRuntime owns the PID-preserving indirection for the canonical External
// profile and the optional macOS-local profile. It deliberately does not own
// routing state or Codex TOML: those remain in routing.Controller.
type relayRuntime struct {
	configPath          string
	watcher             *routing.Watcher
	tracker             *proxy.Tracker
	logger              *slog.Logger
	manager             *proxy.RuntimeManager
	localPreflight      localTargetPreflight
	loadCredentials     credentialLoader
	loadAppleCredential contextCredentialLoader
	appleLease          loopbackauth.LeaseAcquirer
	appleGuard          appleRuntimeCredentialGuard
}

// localOpenCodexPreflight is intentionally private. It keeps the runtime's
// production dependency on the bounded OpenCodex identity/catalog preflight,
// while letting startup tests prove a non-ready persisted Local selection
// without requiring a listener on 10100.
type localOpenCodexPreflight func(context.Context, string) localopencodex.Result
type localTargetPreflight func(context.Context, localopencodex.Target) localopencodex.Result
type credentialLoader func(config.CredentialsConfig) (credentials.Values, error)
type contextCredentialLoader func(context.Context, config.CredentialsConfig) (credentials.Values, error)
type appleRuntimeCredentialGuard func(context.Context, uint64) error
type appleRuntimeAccess struct {
	lease          loopbackauth.LeaseAcquirer
	guard          appleRuntimeCredentialGuard
	loadCredential contextCredentialLoader
}

func newRelayRuntime(
	ctx context.Context,
	configPath string,
	cfg config.Config,
	watcher *routing.Watcher,
	tracker *proxy.Tracker,
	logger *slog.Logger,
	apple appleRuntimeAccess,
) (*relayRuntime, error) {
	return newRelayRuntimeWithDependencies(
		ctx, configPath, cfg, watcher, tracker, logger,
		localopencodex.PreflightTarget, credentials.Load, apple,
	)
}

func newRelayRuntimeWithLocalPreflight(
	ctx context.Context,
	configPath string,
	cfg config.Config,
	watcher *routing.Watcher,
	tracker *proxy.Tracker,
	logger *slog.Logger,
	preflight localOpenCodexPreflight,
) (*relayRuntime, error) {
	if preflight == nil {
		preflight = localopencodex.Preflight
	}
	return newRelayRuntimeWithDependencies(
		ctx, configPath, cfg, watcher, tracker, logger,
		func(ctx context.Context, target localopencodex.Target) localopencodex.Result {
			if target.AuthenticationProfile == config.RemoteAuthenticationNone {
				return preflight(ctx, target.BaseURL)
			}
			return localopencodex.PreflightTarget(ctx, target)
		},
		credentials.Load,
	)
}

func newRelayRuntimeWithDependencies(
	ctx context.Context,
	configPath string,
	cfg config.Config,
	watcher *routing.Watcher,
	tracker *proxy.Tracker,
	logger *slog.Logger,
	preflight localTargetPreflight,
	loadCredentials credentialLoader,
	appleAccesses ...appleRuntimeAccess,
) (*relayRuntime, error) {
	if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		return nil, errors.New("runtime profile switching requires an external_gateway canonical config")
	}
	if preflight == nil {
		preflight = localopencodex.PreflightTarget
	}
	if loadCredentials == nil {
		loadCredentials = credentials.Load
	}
	var apple appleRuntimeAccess
	if len(appleAccesses) > 0 {
		apple = appleAccesses[0]
	}
	loadAppleCredential := apple.loadCredential
	if loadAppleCredential == nil {
		loadAppleCredential = func(ctx context.Context, cfg config.CredentialsConfig) (credentials.Values, error) {
			if ctx == nil || ctx.Err() != nil {
				return credentials.Values{}, errors.New("Apple credential request was cancelled")
			}
			return loadCredentials(cfg)
		}
	}
	runtime := &relayRuntime{
		configPath:          configPath,
		watcher:             watcher,
		tracker:             tracker,
		logger:              logger,
		localPreflight:      preflight,
		loadCredentials:     loadCredentials,
		loadAppleCredential: loadAppleCredential,
		appleLease:          apple.lease,
		appleGuard:          apple.guard,
	}

	// A durable Local selection must never restart as External merely because
	// relay.json itself remains the canonical external profile. Choose from the
	// watcher’s *applied* backend; pending/applying/native/recovery snapshots
	// start parked and expose only health until an explicit controller apply.
	initialBackend := initialRuntimeBackend(watcher.Snapshot())
	startParked := false
	if initialBackend == routing.BackendNone {
		store, storeErr := routing.Open(configPath)
		if storeErr == nil {
			if _, pending, maintenanceErr := store.MaintenanceRecoveryState(); maintenanceErr == nil && pending {
				initialBackend = routing.BackendLocalAppleContainer
				startParked = true
			}
		}
	}
	if (initialBackend == routing.BackendLocalOpenCodex || initialBackend == routing.BackendLocalAppleContainer) &&
		(gort.GOOS != "darwin" || gort.GOARCH != "arm64") {
		initialBackend = routing.BackendNone
		startParked = false
	}

	if initialBackend == routing.BackendLocalOpenCodex || initialBackend == routing.BackendLocalAppleContainer {
		var localCfg config.Config
		var localErr error
		if initialBackend == routing.BackendLocalOpenCodex {
			localCfg, localErr = cfg.LocalOpenCodexRuntimeConfig()
		} else {
			localCfg, localErr = cfg.LocalAppleContainerRuntimeConfig()
		}
		if localErr == nil {
			initial, observation, buildErr := runtime.build(ctx, localCfg, initialBackend)
			if buildErr != nil {
				return nil, buildErr
			}
			spec := runtime.specFor(localCfg, initialBackend, func() *proxy.ConnectionObservation { return observation })
			spec.StartParked = startParked
			manager, managerErr := proxy.NewRuntimeManager(
				ctx,
				tracker,
				spec,
				initial,
			)
			if managerErr != nil {
				if initial.Dispose != nil {
					initial.Dispose()
				}
				return nil, managerErr
			}
			// A persisted Local selection that fails startup preflight retains its
			// Local handler and profile behind the typed local-unavailable gate.
			// NewRuntimeManager deliberately does not start its catalog worker or
			// monitor in that case. Publish the completed paused observation so
			// health/UI cannot leave its catalog lifecycle at unknown.
			if manager.Snapshot().LocalAvailability != proxy.LocalAvailabilityReady {
				observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
			}
			runtime.manager = manager
			return runtime, nil
		}
		// An absent/invalid Local profile cannot authorize an External fallback.
		// Build the canonical handler only as a parked health surface.
		initialBackend = routing.BackendNone
	}

	initial, observation, err := runtime.build(ctx, cfg, routing.BackendExternal)
	if err != nil {
		return nil, err
	}
	spec := runtime.specFor(cfg, routing.BackendExternal, func() *proxy.ConnectionObservation { return observation })
	if initialBackend == routing.BackendNone {
		spec = proxy.RuntimeSpec{Profile: proxy.RuntimeProfileNone}
		// Native, applying, and recovery startup retains only the health
		// handler. No catalog worker is started, so report its completed parked
		// lifecycle rather than an indeterminate status.
		observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
	}
	manager, err := proxy.NewRuntimeManager(ctx, tracker, spec, initial)
	if err != nil {
		if initial.Dispose != nil {
			initial.Dispose()
		}
		return nil, err
	}
	runtime.manager = manager
	return runtime, nil
}

// initialRuntimeBackend is intentionally pure so startup never consults an
// endpoint or a credential before it decides whether the resident listener is
// allowed to admit traffic. A pending Native request keeps its applied relay
// backend until Desktop-safe Apply; applying/native/recovery start parked.
func initialRuntimeBackend(snapshot routing.Snapshot) routing.Backend {
	if snapshot.Invalid || !snapshot.AllowsDataPlane() {
		return routing.BackendNone
	}
	if snapshot.State.AppliedBackend == routing.BackendExternal ||
		snapshot.State.AppliedBackend == routing.BackendLocalOpenCodex ||
		snapshot.State.AppliedBackend == routing.BackendLocalAppleContainer {
		return snapshot.State.AppliedBackend
	}
	return routing.BackendNone
}

func (r *relayRuntime) build(ctx context.Context, cfg config.Config, backend routing.Backend) (proxy.Runtime, *proxy.ConnectionObservation, error) {
	if r == nil || r.tracker == nil || r.watcher == nil {
		return proxy.Runtime{}, nil, errors.New("runtime is not initialized")
	}
	observation := proxy.NewConnectionObservation(cfg.UpstreamMode)
	loader := func() (credentials.Values, error) {
		credentialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.credentialsForBackend(credentialCtx, cfg, backend)
	}
	options := []proxy.Option{
		proxy.WithRouting(r.watcher),
		proxy.WithConnectionObservation(observation),
	}
	if backend == routing.BackendLocalAppleContainer {
		options = append(options, proxy.WithAppleRuntimeConnectionBinding(r.authorizeAppleConnection(cfg)))
	}
	server, err := proxy.New(
		cfg,
		loader,
		r.tracker,
		r.logger,
		options...,
	)
	if err != nil {
		return proxy.Runtime{}, nil, err
	}
	lifecycle := func(lifecycleCtx context.Context) {
		if cfg.Catalog.Owner != config.CatalogOwnerRelay {
			observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
			return
		}
		switch backend {
		case routing.BackendExternal:
			runCatalogLifecycle(lifecycleCtx, cfg, loader, r.tracker, r.logger, r.watcher, observation)
		case routing.BackendLocalOpenCodex:
			runLocalOpenCodexCatalogLifecycle(lifecycleCtx, cfg, nil, nil, r.tracker, r.logger, r.watcher, observation)
		case routing.BackendLocalAppleContainer:
			runLocalOpenCodexCatalogLifecycle(lifecycleCtx, cfg, r.appleLease, r.authorizeAppleConnection(cfg), r.tracker, r.logger, r.watcher, observation)
		default:
			observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
		}
	}
	built, err := proxy.RuntimeForServer(server, lifecycle)
	if err != nil {
		return proxy.Runtime{}, nil, err
	}
	return built, observation, nil
}

func (r *relayRuntime) specFor(cfg config.Config, backend routing.Backend, observation func() *proxy.ConnectionObservation) proxy.RuntimeSpec {
	spec := proxy.RuntimeSpec{}
	switch backend {
	case routing.BackendExternal:
		spec.Profile = proxy.RuntimeProfileExternal
	case routing.BackendLocalOpenCodex, routing.BackendLocalAppleContainer:
		selectedBackend := backend
		if selectedBackend == routing.BackendLocalOpenCodex {
			spec.Profile = proxy.RuntimeProfileLocalOpenCodex
		} else {
			spec.Profile = proxy.RuntimeProfileLocalAppleContainer
			spec.ConnectionLease = proxy.ConnectionLease(r.appleLease)
		}
		spec.LocalProbeAllowed = func() bool {
			// A request transition may intentionally retain the current Local
			// profile until Desktop-safe Apply, but applying/native/recovery must
			// not create another 10100 probe or local catalog egress.
			snapshot := r.watcher.Snapshot()
			return !snapshot.Invalid && snapshot.State.AllowsDataPlane() &&
				snapshot.State.AppliedBackend == selectedBackend
		}
		spec.LocalProbe = func(ctx context.Context) (proxy.LocalAvailability, error) {
			preflight := r.localPreflight
			if preflight == nil {
				preflight = localopencodex.PreflightTarget
			}
			target := localopencodex.NativeTarget(cfg.UpstreamBaseURL)
			if selectedBackend == routing.BackendLocalAppleContainer {
				target = localopencodex.AppleContainerTarget(r.appleProbeLease, r.authorizeAppleConnection(cfg))
			}
			result := preflight(ctx, target)
			return localAvailabilityForRuntime(result.Availability), nil
		}
		spec.LocalAvailabilityObserver = func(value proxy.LocalAvailability) {
			if observation != nil {
				if current := observation(); current != nil {
					current.SetLocalOpenCodex(value)
				}
			}
		}
	case routing.BackendNone:
		spec.Profile = proxy.RuntimeProfileNone
	}
	return spec
}

// credentialsForBackend is the last boundary before an Apple API token leaves
// Keychain. Routing state alone is not a peer identity: the lifecycle guard
// must re-read the signed durable witness and prove that the exact owned Apple
// container is still running before every proxy, catalog, or health probe can
// obtain the token.
func (r *relayRuntime) credentialsForBackend(
	ctx context.Context,
	cfg config.Config,
	backend routing.Backend,
) (credentials.Values, error) {
	if r == nil || r.loadCredentials == nil {
		return credentials.Values{}, errors.New("credential loader is unavailable")
	}
	if backend == routing.BackendLocalAppleContainer {
		return credentials.Values{}, errors.New("Apple credentials require a bound runtime connection")
	}
	return r.loadCredentials(cfg.Credentials)
}

func (r *relayRuntime) authorizeAppleConnection(cfg config.Config) loopbackauth.Authorizer {
	return func(ctx context.Context) (loopbackauth.Authorization, error) {
		if r == nil || ctx == nil || r.watcher == nil || r.appleGuard == nil || r.loadAppleCredential == nil {
			return loopbackauth.Authorization{}, errors.New("Apple runtime authority is unavailable")
		}
		bounded, cancel := context.WithTimeout(ctx, loopbackauth.AuthorizationTimeout)
		defer cancel()
		snapshot := r.watcher.Snapshot()
		if snapshot.Invalid || snapshot.State.Generation == 0 ||
			(snapshot.State.AppliedBackend != routing.BackendLocalAppleContainer &&
				snapshot.State.DesiredBackend != routing.BackendLocalAppleContainer) {
			return loopbackauth.Authorization{}, errors.New("Apple runtime authority requires recovery")
		}
		if err := r.appleGuard(bounded, snapshot.State.Generation); err != nil {
			return loopbackauth.Authorization{}, errors.New("Apple runtime authority requires recovery")
		}
		values, err := r.loadAppleCredential(bounded, cfg.Credentials)
		if err != nil || values.ValidateForProfile(config.LocalAuthenticationOpenCodexAPIKey) != nil {
			return loopbackauth.Authorization{}, errors.New("Apple runtime credential is unavailable")
		}
		return loopbackauth.Authorization{Token: []byte(values.LocalOpenCodexAPIKey)}, nil
	}
}

type lifecycleOwnedAppleProbeContextKey struct{}

func (r *relayRuntime) appleProbeLease(ctx context.Context) (func() error, error) {
	if ctx != nil {
		owned, _ := ctx.Value(lifecycleOwnedAppleProbeContextKey{}).(bool)
		if owned {
			return func() error { return nil }, nil
		}
	}
	if r == nil || r.appleLease == nil {
		return nil, errors.New("Apple runtime lifecycle lease is unavailable")
	}
	return r.appleLease(ctx)
}

func localAvailabilityForRuntime(value localopencodex.Availability) proxy.LocalAvailability {
	switch value {
	case localopencodex.AvailabilityReady:
		return proxy.LocalAvailabilityReady
	case localopencodex.AvailabilityUnavailable:
		return proxy.LocalAvailabilityUnavailable
	case localopencodex.AvailabilityForeign:
		return proxy.LocalAvailabilityForeign
	case localopencodex.AvailabilityInvalid:
		return proxy.LocalAvailabilityInvalid
	default:
		return proxy.LocalAvailabilityUnknown
	}
}

func (r *relayRuntime) apply(ctx context.Context, request routing.ControlRequest) (routing.ControlResponse, error) {
	if r == nil || r.manager == nil || r.watcher == nil {
		return routing.ControlResponse{}, errors.New("runtime manager is unavailable")
	}
	snapshot := r.watcher.Snapshot()
	if snapshot.Invalid || snapshot.State.Phase != routing.PhaseApplying || snapshot.State.Generation != request.Generation || snapshot.State.DesiredBackend != request.Backend {
		return routing.ControlResponse{}, errors.New("routing state does not authorize runtime apply")
	}
	canonical, err := config.Load(r.configPath)
	if err != nil {
		return routing.ControlResponse{}, err
	}
	runtimeCfg := canonical
	switch request.Backend {
	case routing.BackendExternal:
		if canonical.UpstreamMode != config.UpstreamModeExternalGateway {
			return routing.ControlResponse{}, errors.New("external profile is unavailable")
		}
	case routing.BackendLocalOpenCodex:
		if gort.GOOS != "darwin" || gort.GOARCH != "arm64" {
			return routing.ControlResponse{}, errors.New("local OpenCodex runtime profile is macOS Apple Silicon only")
		}
		runtimeCfg, err = canonical.LocalOpenCodexRuntimeConfig()
		if err != nil {
			return routing.ControlResponse{}, err
		}
	case routing.BackendLocalAppleContainer:
		if gort.GOOS != "darwin" || gort.GOARCH != "arm64" {
			return routing.ControlResponse{}, errors.New("Apple Container runtime profile is macOS Apple Silicon only")
		}
		runtimeCfg, err = canonical.LocalAppleContainerRuntimeConfig()
		if err != nil {
			return routing.ControlResponse{}, err
		}
	case routing.BackendNone:
		// No candidate server is needed. RuntimeManager retains a retired
		// handler only for health while all data-plane requests are parked.
	default:
		return routing.ControlResponse{}, errors.New("unsupported runtime backend")
	}
	var factory proxy.RuntimeFactory
	var candidateObservation *proxy.ConnectionObservation
	if request.Backend != routing.BackendNone {
		factory = func(factoryCtx context.Context, _ *proxy.Tracker) (proxy.Runtime, error) {
			built, observation, buildErr := r.build(factoryCtx, runtimeCfg, request.Backend)
			candidateObservation = observation
			return built, buildErr
		}
	}
	// RuntimeManager builds the candidate before invoking a Local probe, so the
	// observer has the candidate Server health projection available by then.
	applyCtx := ctx
	if request.Backend == routing.BackendLocalAppleContainer {
		applyCtx = context.WithValue(ctx, lifecycleOwnedAppleProbeContextKey{}, true)
	}
	if err := r.manager.Apply(applyCtx, r.specFor(runtimeCfg, request.Backend, func() *proxy.ConnectionObservation { return candidateObservation }), factory); err != nil {
		return routing.ControlResponse{}, fmt.Errorf("apply runtime: %w", err)
	}
	return routing.ControlResponse{OK: true, Generation: request.Generation, Backend: request.Backend}, nil
}

func (r *relayRuntime) Prepare(ctx context.Context) error {
	if r == nil || r.manager == nil {
		return proxy.ErrRuntimeManagerClosed
	}
	return r.manager.PrepareMaintenance(ctx)
}

func (r *relayRuntime) Verify(ctx context.Context, _ routing.ControlOperation) error {
	if r == nil || r.manager == nil {
		return proxy.ErrRuntimeManagerClosed
	}
	return r.manager.VerifyMaintenance(context.WithValue(ctx, lifecycleOwnedAppleProbeContextKey{}, true))
}

func (r *relayRuntime) Resume() {
	if r != nil && r.manager != nil {
		r.manager.ResumeMaintenance()
	}
}

func (r *relayRuntime) Handler() *proxy.RuntimeManager {
	if r == nil {
		return nil
	}
	return r.manager
}

func (r *relayRuntime) Close(ctx context.Context) error {
	if r == nil || r.manager == nil {
		return nil
	}
	return r.manager.Close(ctx)
}
