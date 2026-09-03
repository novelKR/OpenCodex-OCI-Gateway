package main

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/activation"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

const (
	catalogGatePollInterval = 100 * time.Millisecond
	catalogRefreshTimeout   = 75 * time.Second
	connectionProbeInterval = 10 * time.Minute
	connectionProbeTimeout  = 5 * time.Second
)

type catalogRefreshFunc func(context.Context) (catalog.Result, error)
type connectionProbeFunc func(context.Context) error

// catalogLifecycle owns only resident background work. The routing controller
// remains the sole persistent state writer; this loop observes its watcher and
// cancels egress before reporting a paused catalog lifecycle to health.
type catalogLifecycle struct {
	cfg         config.Config
	tracker     *proxy.Tracker
	logger      *slog.Logger
	watcher     *routing.Watcher
	observation *proxy.ConnectionObservation
	refresh     catalogRefreshFunc
	probe       connectionProbeFunc
	probeOn     bool
}

func runCatalogLifecycle(
	ctx context.Context,
	cfg config.Config,
	load func() (credentials.Values, error),
	tracker *proxy.Tracker,
	logger *slog.Logger,
	watcher *routing.Watcher,
	observation *proxy.ConnectionObservation,
) {
	refresher := catalog.Fetcher{Config: cfg, Credentials: load}
	lifecycle := catalogLifecycle{
		cfg:         cfg,
		tracker:     tracker,
		logger:      logger,
		watcher:     watcher,
		observation: observation,
		refresh:     refresher.Refresh,
		probe:       refresher.Probe,
		probeOn:     shouldEnableConnectionProbe(cfg),
	}
	lifecycle.run(ctx)
}

// runLocalOpenCodexCatalogLifecycle owns one of the distinct relay-written
// local catalogs. LocalOpenCodexFetcher always constructs a no-proxy client;
// the fixed Apple profile additionally loads only its API token and injects it
// on /v1/models. The native profile remains credentialless.
func runLocalOpenCodexCatalogLifecycle(
	ctx context.Context,
	cfg config.Config,
	load func() (credentials.Values, error),
	tracker *proxy.Tracker,
	logger *slog.Logger,
	watcher *routing.Watcher,
	observation *proxy.ConnectionObservation,
) {
	refresher := catalog.LocalOpenCodexFetcher{
		BaseURL:               cfg.UpstreamBaseURL,
		CatalogPath:           cfg.Catalog.Path,
		ExpectedServicePort:   10100,
		AuthenticationProfile: cfg.Credentials.RemoteAuthenticationProfile(),
		Credentials:           load,
	}
	lifecycle := catalogLifecycle{
		cfg:         cfg,
		tracker:     tracker,
		logger:      logger,
		watcher:     watcher,
		observation: observation,
		refresh:     refresher.Refresh,
		probeOn:     false,
	}
	lifecycle.run(ctx)
}

func shouldEnableConnectionProbe(cfg config.Config) bool {
	return runtime.GOOS == "darwin" &&
		cfg.ConnectionProbe.Enabled &&
		cfg.UpstreamMode == config.UpstreamModeExternalGateway
}

func (l catalogLifecycle) allowsCatalog() bool {
	return l.watcher == nil || l.watcher.AllowsCatalog()
}

func (l catalogLifecycle) allowsProbe() bool {
	if !l.probeOn || l.watcher == nil {
		return false
	}
	route := l.watcher.Snapshot()
	return !route.Invalid && route.State.Phase == routing.PhaseRelayActive && route.AllowsCatalog()
}

func (l catalogLifecycle) run(ctx context.Context) {
	if l.logger == nil {
		l.logger = slog.Default()
	}
	if l.observation == nil {
		l.observation = proxy.NewConnectionObservation(l.cfg.UpstreamMode)
	}
	// A runtime manager may cancel this lifecycle after a Local 10100 loss
	// without changing the durable routing state. Publish paused only after the
	// loop (and any context-aware refresh it owns) has returned, so status never
	// advertises a still-running catalog worker after the typed 503 park.
	defer l.observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
	l.observation.SetProbeEnabled(l.probeOn)
	interval, err := l.cfg.RefreshEvery()
	if err != nil {
		l.observation.SetCatalogLifecycle(proxy.CatalogLifecycleUnknown)
		l.logger.Error("catalog lifecycle disabled", "error", err.Error())
		return
	}
	if l.refresh == nil {
		l.observation.SetCatalogLifecycle(proxy.CatalogLifecycleUnknown)
		l.logger.Error("catalog lifecycle disabled", "error", "catalog refresher is nil")
		return
	}

	refreshTimer := time.NewTimer(0)
	defer refreshTimer.Stop()
	applyTimer := time.NewTicker(30 * time.Second)
	defer applyTimer.Stop()
	gateTimer := time.NewTicker(catalogGatePollInterval)
	defer gateTimer.Stop()

	var probeTimer *time.Timer
	var probeTick <-chan time.Time
	if l.probeOn && l.probe != nil {
		probeTimer = time.NewTimer(connectionProbeInterval)
		probeTick = probeTimer.C
		defer probeTimer.Stop()
	}

	for {
		if !l.allowsCatalog() {
			// There is no active Refresh here. refreshWhileAdmitted waits for
			// its child goroutine before returning, so this is a confirmed pause.
			l.observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
			select {
			case <-ctx.Done():
				return
			case <-gateTimer.C:
				continue
			}
		}
		l.observation.SetCatalogLifecycle(proxy.CatalogLifecycleRunning)

		select {
		case <-ctx.Done():
			return
		case <-gateTimer.C:
			// Re-evaluate the routing gate at the top of the loop even when no
			// scheduled work is due. This gives apply a bounded pause acknowledgement.
			continue
		case <-refreshTimer.C:
			l.refreshWhileAdmitted(ctx)
			refreshTimer.Reset(interval)
		case <-applyTimer.C:
			l.applyWhileAdmitted()
		case <-probeTick:
			l.probeWhileRelayActive(ctx)
			if probeTimer != nil {
				probeTimer.Reset(connectionProbeInterval)
			}
		}
	}
}

func (l catalogLifecycle) refreshWhileAdmitted(parent context.Context) {
	if !l.allowsCatalog() {
		l.observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
		return
	}
	ctx, cancel := context.WithTimeout(parent, catalogRefreshTimeout)
	defer cancel()
	type outcome struct {
		result catalog.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := l.refresh(ctx)
		done <- outcome{result: result, err: err}
	}()

	poll := time.NewTicker(catalogGatePollInterval)
	defer poll.Stop()
	for {
		select {
		case outcome := <-done:
			if outcome.err != nil {
				if parent.Err() == nil && l.allowsCatalog() {
					l.logger.Warn("catalog refresh failed", "error", outcome.err.Error())
				}
				return
			}
			l.observation.RecordCatalogRefreshSuccess(time.Now())
			if outcome.result.Changed {
				l.logger.Info("catalog updated", "models", outcome.result.Count, "hash", outcome.result.Hash)
			}
			return
		case <-parent.Done():
			cancel()
			<-done
			return
		case <-poll.C:
			if l.allowsCatalog() {
				continue
			}
			cancel()
			// Do not report paused until the context-aware Fetcher has returned.
			// This gives the controller a truthful drain point for remote catalog
			// egress rather than a best-effort cancellation signal.
			<-done
			l.observation.SetCatalogLifecycle(proxy.CatalogLifecyclePaused)
			return
		}
	}
}

func (l catalogLifecycle) applyWhileAdmitted() {
	if l.tracker == nil || !l.allowsCatalog() || !l.cfg.Catalog.ManageAppServer || !catalog.Pending(l.cfg.Catalog.Path) {
		return
	}
	var result activation.Result
	var applyErr error
	if !l.tracker.TryQuiesce(func() {
		if !l.allowsCatalog() {
			return
		}
		result, applyErr = activation.ApplyWhileQuiesced(l.cfg.Catalog.Path, true, l.cfg.Catalog.AppServerHome)
	}) {
		return
	}
	if applyErr != nil {
		l.logger.Warn("catalog activation failed", "error", applyErr.Error())
		return
	}
	if len(result.Restarted) > 0 {
		l.logger.Info("restarted idle Codex app-server processes", "count", len(result.Restarted))
	}
}

func (l catalogLifecycle) probeWhileRelayActive(parent context.Context) {
	if !l.allowsProbe() || l.probe == nil {
		return
	}
	if l.observation.HasRecentCatalogSuccess(time.Now(), connectionProbeInterval) {
		return
	}
	ctx, cancel := context.WithTimeout(parent, connectionProbeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.probe(ctx) }()
	poll := time.NewTicker(catalogGatePollInterval)
	defer poll.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				if parent.Err() == nil && l.allowsProbe() {
					l.observation.RecordGatewayUnreachable()
				}
				return
			}
			l.observation.RecordGatewayReachable()
			return
		case <-parent.Done():
			cancel()
			<-done
			return
		case <-poll.C:
			if l.allowsProbe() {
				continue
			}
			cancel()
			<-done
			return
		}
	}
}
