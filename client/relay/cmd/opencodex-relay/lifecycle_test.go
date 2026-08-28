package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestCatalogLifecycleDoesNotRefreshWhileParked(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired routing.Mode
		applied routing.Mode
		phase   routing.Phase
	}{
		{name: "native active", desired: routing.ModeNative, applied: routing.ModeNative, phase: routing.PhaseNativeActive},
		{name: "recovery required", desired: routing.ModeUnknown, applied: routing.ModeUnknown, phase: routing.PhaseRecoveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, state := lifecycleStoreAndState(t)
			state.DesiredMode = test.desired
			state.AppliedMode = test.applied
			state.Phase = test.phase
			lifecycleWriteState(t, store, state)
			watcher := routing.NewWatcher(store, 0)
			observation := proxy.NewConnectionObservation(config.UpstreamModeExternalGateway)
			var refreshes atomic.Int64

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				catalogLifecycle{
					cfg:         lifecycleConfig(t),
					tracker:     proxy.NewTracker(),
					logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
					watcher:     watcher,
					observation: observation,
					refresh: func(context.Context) (catalog.Result, error) {
						refreshes.Add(1)
						return catalog.Result{}, nil
					},
				}.run(ctx)
				close(done)
			}()

			waitForLifecycle(t, time.Second, func() bool {
				return observation.Snapshot().CatalogLifecycle == proxy.CatalogLifecyclePaused
			})
			if refreshes.Load() != 0 {
				t.Fatalf("parked lifecycle refreshed %d time(s)", refreshes.Load())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("parked lifecycle did not stop")
			}
		})
	}
}

func TestCatalogLifecycleCancelsRefreshAndConfirmsPauseOnApplying(t *testing.T) {
	store, state := lifecycleStoreAndState(t)
	lifecycleWriteState(t, store, state)
	watcher := routing.NewWatcher(store, 0)
	observation := proxy.NewConnectionObservation(config.UpstreamModeExternalGateway)
	started := make(chan struct{})
	canceled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		catalogLifecycle{
			cfg:         lifecycleConfig(t),
			tracker:     proxy.NewTracker(),
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			watcher:     watcher,
			observation: observation,
			refresh: func(refreshCtx context.Context) (catalog.Result, error) {
				close(started)
				<-refreshCtx.Done()
				close(canceled)
				return catalog.Result{}, refreshCtx.Err()
			},
		}.run(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("catalog refresh did not start")
	}

	state.Generation++
	state.DesiredMode = routing.ModeNative
	state.AppliedMode = routing.ModeRelay
	state.Phase = routing.PhaseNativePendingRestart
	lifecycleWriteState(t, store, state)
	state.Generation++
	state.Phase = routing.PhaseApplying
	lifecycleWriteState(t, store, state)
	watcher.Refresh()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("applying state did not cancel in-flight catalog refresh")
	}
	waitForLifecycle(t, time.Second, func() bool {
		return observation.Snapshot().CatalogLifecycle == proxy.CatalogLifecyclePaused
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("catalog lifecycle did not stop")
	}
}

func TestConnectionProbeAdmissionRequiresRelayActive(t *testing.T) {
	store, state := lifecycleStoreAndState(t)
	lifecycleWriteState(t, store, state)
	watcher := routing.NewWatcher(store, 0)
	lifecycle := catalogLifecycle{watcher: watcher, probeOn: true}
	if !lifecycle.allowsProbe() {
		t.Fatal("relay_active unexpectedly denied connection probe")
	}
	state.Generation++
	state.DesiredMode = routing.ModeNative
	state.AppliedMode = routing.ModeRelay
	state.Phase = routing.PhaseNativePendingRestart
	lifecycleWriteState(t, store, state)
	watcher.Refresh()
	if lifecycle.allowsProbe() {
		t.Fatal("native_pending_restart admitted a new connection probe")
	}
	state.Generation++
	state.Phase = routing.PhaseApplying
	lifecycleWriteState(t, store, state)
	watcher.Refresh()
	if lifecycle.allowsProbe() {
		t.Fatal("applying admitted a connection probe")
	}
}

func TestConnectionProbeCoalescesRecentCatalogSuccess(t *testing.T) {
	store, state := lifecycleStoreAndState(t)
	lifecycleWriteState(t, store, state)
	watcher := routing.NewWatcher(store, 0)
	observation := proxy.NewConnectionObservation(config.UpstreamModeExternalGateway)
	observation.RecordCatalogRefreshSuccess(time.Now())
	var probes atomic.Int64
	catalogLifecycle{
		watcher:     watcher,
		observation: observation,
		probeOn:     true,
		probe: func(context.Context) error {
			probes.Add(1)
			return errors.New("coalesced probe must not run")
		},
	}.probeWhileRelayActive(context.Background())
	if probes.Load() != 0 {
		t.Fatalf("recent catalog success did not coalesce probe: calls=%d", probes.Load())
	}
}

func lifecycleConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(t.TempDir(), "catalog.json")
	return cfg
}

func lifecycleStoreAndState(t *testing.T) (*routing.Store, routing.State) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	store, err := routing.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "codex-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return store, state
}

func lifecycleWriteState(t *testing.T, store *routing.Store, state routing.State) {
	t.Helper()
	// These fixtures express the original mode-only state machine.  Persist its
	// explicit v2 backend counterparts so the test exercises a valid durable
	// record rather than relying on legacy inference.
	state.DesiredBackend = lifecycleBackendForMode(state.DesiredMode)
	state.AppliedBackend = lifecycleBackendForMode(state.AppliedMode)
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lock.Save(state); err != nil {
		t.Fatal(err)
	}
}

func lifecycleBackendForMode(mode routing.Mode) routing.Backend {
	switch mode {
	case routing.ModeRelay:
		return routing.BackendExternal
	case routing.ModeNative:
		return routing.BackendNone
	default:
		return routing.BackendUnknown
	}
}

func waitForLifecycle(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !predicate() {
		t.Fatal(errors.New("timed out waiting for catalog lifecycle state"))
	}
}
