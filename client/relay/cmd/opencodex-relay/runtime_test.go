package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	gort "runtime"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestInitialRuntimeBackendUsesAppliedBackendOrParks(t *testing.T) {
	tests := []struct {
		name     string
		snapshot routing.Snapshot
		want     routing.Backend
	}{
		{
			name: "external active",
			snapshot: routing.Snapshot{State: routing.State{
				AppliedBackend: routing.BackendExternal,
				Phase:          routing.PhaseRelayActive,
			}},
			want: routing.BackendExternal,
		},
		{
			name: "local active",
			snapshot: routing.Snapshot{State: routing.State{
				AppliedBackend: routing.BackendLocalOpenCodex,
				Phase:          routing.PhaseRelayActive,
			}},
			want: routing.BackendLocalOpenCodex,
		},
		{
			name: "native pending retains applied local",
			snapshot: routing.Snapshot{State: routing.State{
				AppliedBackend: routing.BackendLocalOpenCodex,
				Phase:          routing.PhaseNativePendingRestart,
			}},
			want: routing.BackendLocalOpenCodex,
		},
		{
			name: "applying parks",
			snapshot: routing.Snapshot{State: routing.State{
				AppliedBackend: routing.BackendExternal,
				Phase:          routing.PhaseApplying,
			}},
			want: routing.BackendNone,
		},
		{
			name: "invalid parks",
			snapshot: routing.Snapshot{Invalid: true, State: routing.State{
				AppliedBackend: routing.BackendLocalOpenCodex,
				Phase:          routing.PhaseRelayActive,
			}},
			want: routing.BackendNone,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := initialRuntimeBackend(test.snapshot); got != test.want {
				t.Fatalf("initial runtime backend = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRelayRuntimeInitialLocalUnavailableStaysRecoverableThroughControl(t *testing.T) {
	if gort.GOOS != "darwin" || gort.GOARCH != "arm64" {
		t.Skip("the optional Local runtime profile is macOS Apple Silicon only")
	}
	for _, test := range []struct {
		name      string
		target    routing.Backend
		admission string
		profile   proxy.RuntimeProfile
	}{
		{
			name:      "explicit external",
			target:    routing.BackendExternal,
			admission: "active",
			profile:   proxy.RuntimeProfileExternal,
		},
		{
			name:      "explicit native",
			target:    routing.BackendNone,
			admission: "paused",
			// Native keeps the retired Local handler for health only, so its
			// immutable profile remains visible while admission is parked.
			profile: proxy.RuntimeProfileLocalOpenCodex,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := runtimeState(t, routing.BackendLocalOpenCodex, routing.BackendLocalOpenCodex, routing.PhaseRelayActive)
			runtime, fixture := newRuntimeFixture(t, state, func(context.Context, string) localopencodex.Result {
				return localopencodex.Result{Availability: localopencodex.AvailabilityUnavailable}
			})
			defer fixture.close(t, runtime)

			initial := runtime.Handler().Snapshot()
			if initial.Profile != proxy.RuntimeProfileLocalOpenCodex || initial.Admission != "local_unavailable" || initial.LocalAvailability != proxy.LocalAvailabilityUnavailable {
				t.Fatalf("initial Local-unavailable runtime = %#v", initial)
			}
			if initial.CatalogRunning {
				t.Fatal("failed Local bootstrap started a catalog worker")
			}
			assertRuntimeStatus(t, runtime, "paused", "unavailable")
			assertLocalUnavailable503(t, runtime)

			applying := fixture.state
			applying.Generation++
			applying.DesiredBackend = test.target
			applying.DesiredMode = runtimeModeForBackend(test.target)
			applying.AppliedBackend = routing.BackendLocalOpenCodex
			applying.AppliedMode = routing.ModeRelay
			applying.Phase = routing.PhaseApplying
			fixture.replaceState(t, applying)

			applyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			response, err := runtime.apply(applyCtx, routing.ControlRequest{
				Schema:     1,
				Generation: applying.Generation,
				Backend:    test.target,
			})
			if err != nil {
				t.Fatalf("explicit %s control apply: %v", test.target, err)
			}
			if !response.OK || response.Generation != applying.Generation || response.Backend != test.target {
				t.Fatalf("explicit %s control response = %#v", test.target, response)
			}

			after := runtime.Handler().Snapshot()
			if after.Profile != test.profile || after.Admission != test.admission {
				t.Fatalf("runtime after explicit %s apply = %#v", test.target, after)
			}
			if test.target == routing.BackendNone && after.CatalogRunning {
				t.Fatal("native apply left a catalog worker running")
			}
		})
	}
}

func TestRelayRuntimeInitialParkedStatesPublishPausedCatalog(t *testing.T) {
	for _, test := range []struct {
		name  string
		state func(*testing.T) routing.State
	}{
		{
			name: "native active",
			state: func(t *testing.T) routing.State {
				return runtimeState(t, routing.BackendNone, routing.BackendNone, routing.PhaseNativeActive)
			},
		},
		{
			name: "applying",
			state: func(t *testing.T) routing.State {
				return runtimeState(t, routing.BackendNone, routing.BackendExternal, routing.PhaseApplying)
			},
		},
		{
			name: "recovery required",
			state: func(t *testing.T) routing.State {
				state, err := routing.NewRecoveryState(filepath.Join(t.TempDir(), "relay.json"))
				if err != nil {
					t.Fatal(err)
				}
				return state
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, fixture := newRuntimeFixture(t, test.state(t), nil)
			defer fixture.close(t, runtime)

			snapshot := runtime.Handler().Snapshot()
			if snapshot.Admission != "paused" || snapshot.CatalogRunning {
				t.Fatalf("parked startup runtime = %#v", snapshot)
			}
			assertRuntimeStatus(t, runtime, "paused", "unknown")
		})
	}
}

type runtimeFixture struct {
	cancel  context.CancelFunc
	store   *routing.Store
	watcher *routing.Watcher
	state   routing.State
}

func newRuntimeFixture(t *testing.T, state routing.State, preflight localOpenCodexPreflight) (*relayRuntime, *runtimeFixture) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.File = filepath.Join(filepath.Dir(configPath), "credentials.env")
	cfg.Catalog.Path = filepath.Join(filepath.Dir(configPath), "external-catalog.json")
	cfg.LocalOpenCodex = &config.LocalOpenCodexProfile{
		UpstreamBaseURL: "http://127.0.0.1:10100/v1",
		CatalogPath:     filepath.Join(filepath.Dir(configPath), "local-catalog.json"),
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := routing.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state.BoundConfigPath = store.ConfigPath()
	state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &runtimeFixture{store: store, state: state}
	fixture.replaceState(t, state)
	fixture.watcher = routing.NewWatcher(store, 0)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.cancel = cancel
	runtime, err := newRelayRuntimeWithLocalPreflight(
		ctx,
		configPath,
		cfg,
		fixture.watcher,
		proxy.NewTracker(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		preflight,
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return runtime, fixture
}

func (f *runtimeFixture) replaceState(t *testing.T, state routing.State) {
	t.Helper()
	lock, err := f.store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lock.Replace(state); err != nil {
		t.Fatal(err)
	}
	f.state = state
	if f.watcher != nil {
		f.watcher.Refresh()
	}
}

func (f *runtimeFixture) close(t *testing.T, runtime *relayRuntime) {
	t.Helper()
	f.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		t.Errorf("close runtime: %v", err)
	}
}

func runtimeState(t *testing.T, desired, applied routing.Backend, phase routing.Phase) routing.State {
	t.Helper()
	state, err := routing.NewRelayState(filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredBackend = desired
	state.AppliedBackend = applied
	state.DesiredMode = runtimeModeForBackend(desired)
	state.AppliedMode = runtimeModeForBackend(applied)
	state.Phase = phase
	return state
}

func runtimeModeForBackend(backend routing.Backend) routing.Mode {
	switch backend {
	case routing.BackendExternal, routing.BackendLocalOpenCodex:
		return routing.ModeRelay
	case routing.BackendNone:
		return routing.ModeNative
	default:
		return routing.ModeUnknown
	}
}

func assertRuntimeStatus(t *testing.T, runtime *relayRuntime, wantCatalog, wantLocal string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	runtime.Handler().Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__relay/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var status struct {
		CatalogLifecycle string `json:"catalog_lifecycle"`
		LocalOpenCodex   string `json:"local_opencodex"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.CatalogLifecycle != wantCatalog || status.LocalOpenCodex != wantLocal {
		t.Fatalf("health catalog/local = %q/%q, want %q/%q", status.CatalogLifecycle, status.LocalOpenCodex, wantCatalog, wantLocal)
	}
}

func assertLocalUnavailable503(t *testing.T, runtime *relayRuntime) {
	t.Helper()
	recorder := httptest.NewRecorder()
	runtime.Handler().Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("Local-unavailable data plane status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "local_opencodex_unavailable" {
		t.Fatalf("Local-unavailable error code = %q", response.Error.Code)
	}
}
