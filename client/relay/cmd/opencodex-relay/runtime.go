package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	gort "runtime"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

// relayRuntime owns the PID-preserving indirection for the canonical External
// profile and the optional macOS-local profile. It deliberately does not own
// routing state or Codex TOML: those remain in routing.Controller.
type relayRuntime struct {
	configPath     string
	watcher        *routing.Watcher
	tracker        *proxy.Tracker
	logger         *slog.Logger
	manager        *proxy.RuntimeManager
	localPreflight localOpenCodexPreflight
}

// localOpenCodexPreflight is intentionally private. It keeps the runtime's
// production dependency on the bounded OpenCodex identity/catalog preflight,
// while letting startup tests prove a non-ready persisted Local selection
// without requiring a listener on 10100.
type localOpenCodexPreflight func(context.Context, string) localopencodex.Result

func newRelayRuntime(
	ctx context.Context,
	configPath string,
	cfg config.Config,
	watcher *routing.Watcher,
	tracker *proxy.Tracker,
	logger *slog.Logger,
) (*relayRuntime, error) {
	return newRelayRuntimeWithLocalPreflight(ctx, configPath, cfg, watcher, tracker, logger, localopencodex.Preflight)
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
	if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		return nil, errors.New("runtime profile switching requires an external_gateway canonical config")
	}
	if preflight == nil {
		preflight = localopencodex.Preflight
	}
	runtime := &relayRuntime{
		configPath:     configPath,
		watcher:        watcher,
		tracker:        tracker,
		logger:         logger,
		localPreflight: preflight,
	}

	// A durable Local selection must never restart as External merely because
	// relay.json itself remains the canonical external profile. Choose from the
	// watcher’s *applied* backend; pending/applying/native/recovery snapshots
	// start parked and expose only health until an explicit controller apply.
	initialBackend := initialRuntimeBackend(watcher.Snapshot())
	if initialBackend == routing.BackendLocalOpenCodex && (gort.GOOS != "darwin" || gort.GOARCH != "arm64") {
		initialBackend = routing.BackendNone
	}

	if initialBackend == routing.BackendLocalOpenCodex {
		localCfg, localErr := cfg.LocalOpenCodexRuntimeConfig()
		if localErr == nil {
			initial, observation, buildErr := runtime.build(ctx, localCfg, routing.BackendLocalOpenCodex)
			if buildErr != nil {
				return nil, buildErr
			}
			manager, managerErr := proxy.NewRuntimeManager(
				ctx,
				tracker,
				runtime.specFor(localCfg, routing.BackendLocalOpenCodex, func() *proxy.ConnectionObservation { return observation }),
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
	if snapshot.State.AppliedBackend == routing.BackendExternal || snapshot.State.AppliedBackend == routing.BackendLocalOpenCodex {
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
		return credentials.Load(cfg.Credentials)
	}
	server, err := proxy.New(
		cfg,
		loader,
		r.tracker,
		r.logger,
		proxy.WithRouting(r.watcher),
		proxy.WithConnectionObservation(observation),
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
			runLocalOpenCodexCatalogLifecycle(lifecycleCtx, cfg, r.tracker, r.logger, r.watcher, observation)
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
	case routing.BackendLocalOpenCodex:
		spec.Profile = proxy.RuntimeProfileLocalOpenCodex
		spec.LocalProbeAllowed = func() bool {
			// A request transition may intentionally retain the current Local
			// profile until Desktop-safe Apply, but applying/native/recovery must
			// not create another 10100 probe or local catalog egress.
			snapshot := r.watcher.Snapshot()
			return !snapshot.Invalid && snapshot.State.AllowsDataPlane() &&
				snapshot.State.AppliedBackend == routing.BackendLocalOpenCodex
		}
		spec.LocalProbe = func(ctx context.Context) (proxy.LocalAvailability, error) {
			preflight := r.localPreflight
			if preflight == nil {
				preflight = localopencodex.Preflight
			}
			result := preflight(ctx, cfg.UpstreamBaseURL)
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
	if err := r.manager.Apply(ctx, r.specFor(runtimeCfg, request.Backend, func() *proxy.ConnectionObservation { return candidateObservation }), factory); err != nil {
		return routing.ControlResponse{}, fmt.Errorf("apply runtime: %w", err)
	}
	return routing.ControlResponse{OK: true, Generation: request.Generation, Backend: request.Backend}, nil
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
