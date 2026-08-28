package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/scheduler"
)

func TestRuntimeManagerSwapsStableHandlerOnlyAfterDrain(t *testing.T) {
	tracker := NewTracker()
	started := make(chan struct{})
	release := make(chan struct{})
	oldRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		finish := tracker.Begin()
		defer finish()
		close(started)
		<-release
		_, _ = io.WriteString(w, "old")
	})
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileExternal}, oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)

	stableHandler := manager.HandlerForLane(scheduler.LaneGeneral)
	oldResponse := httptest.NewRecorder()
	oldDone := make(chan struct{})
	go func() {
		stableHandler.ServeHTTP(oldResponse, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(oldDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old runtime did not begin")
	}
	if got := manager.Tracker(); got != tracker {
		t.Fatal("manager did not preserve the shared tracker")
	}
	if got := manager.Snapshot().ActiveRequests; got != 1 {
		t.Fatalf("active requests = %d, want 1", got)
	}

	applyResult := make(chan error, 1)
	go func() {
		applyResult <- manager.Apply(context.Background(), RuntimeSpec{Profile: RuntimeProfileExternal}, func(context.Context, *Tracker) (Runtime, error) {
			return runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
				finish := tracker.Begin()
				defer finish()
				_, _ = io.WriteString(w, "new")
			}), nil
		})
	}()
	waitForRuntime(t, manager, time.Second, func(snapshot RuntimeSnapshot) bool {
		return snapshot.Admission == string(runtimeAdmissionTransitioning)
	})

	newResponse := httptest.NewRecorder()
	newDone := make(chan struct{})
	go func() {
		stableHandler.ServeHTTP(newResponse, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(newDone)
	}()
	select {
	case <-newDone:
		t.Fatal("new request bypassed the transition admission gate")
	case <-time.After(40 * time.Millisecond):
	}

	close(release)
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old request did not finish")
	}
	select {
	case err := <-applyResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime apply did not finish")
	}
	select {
	case <-newDone:
	case <-time.After(time.Second):
		t.Fatal("new request did not resume after apply")
	}
	if got := oldResponse.Body.String(); got != "old" {
		t.Fatalf("old response = %q", got)
	}
	if got := newResponse.Body.String(); got != "new" {
		t.Fatalf("new response = %q", got)
	}
	snapshot := manager.Snapshot()
	if snapshot.Generation != 2 || snapshot.Profile != RuntimeProfileExternal || snapshot.Admission != string(runtimeAdmissionActive) {
		t.Fatalf("snapshot after swap = %#v", snapshot)
	}
}

func TestRuntimeManagerRejectsLocalApplyBeforePausingExternal(t *testing.T) {
	tracker := NewTracker()
	var externalCalls atomic.Int64
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileExternal}, runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		externalCalls.Add(1)
		_, _ = io.WriteString(w, "external")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)

	var factories atomic.Int64
	err = manager.Apply(context.Background(), RuntimeSpec{
		Profile: RuntimeProfileLocalOpenCodex,
		LocalProbe: func(context.Context) (LocalAvailability, error) {
			return LocalAvailabilityForeign, nil
		},
	}, func(context.Context, *Tracker) (Runtime, error) {
		factories.Add(1)
		return runtimeForTest(tracker, func(http.ResponseWriter, *http.Request) {}), nil
	})
	if !errors.Is(err, ErrLocalOpenCodexUnavailable) {
		t.Fatalf("apply error = %v, want local unavailable", err)
	}
	if factories.Load() != 1 {
		t.Fatalf("runtime factory calls = %d, want 1", factories.Load())
	}
	snapshot := manager.Snapshot()
	if snapshot.Profile != RuntimeProfileExternal || snapshot.Admission != string(runtimeAdmissionActive) || snapshot.LocalAvailability != LocalAvailabilityForeign {
		t.Fatalf("failed local apply changed runtime: %#v", snapshot)
	}
	recorder := httptest.NewRecorder()
	manager.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Body.String() != "external" || externalCalls.Load() != 1 {
		t.Fatalf("external runtime was not preserved: body=%q calls=%d", recorder.Body.String(), externalCalls.Load())
	}
}

func TestRuntimeManagerParksActiveLocalWithTyped503AndStopsCatalog(t *testing.T) {
	tracker := NewTracker()
	catalogStarted := make(chan struct{})
	catalogStopped := make(chan struct{})
	var forwarded atomic.Int64
	localRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		_, _ = io.WriteString(w, "local")
	})
	localRuntime.CatalogLifecycle = func(ctx context.Context) {
		close(catalogStarted)
		<-ctx.Done()
		close(catalogStopped)
	}
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{
		Profile: RuntimeProfileLocalOpenCodex,
		LocalProbe: func(context.Context) (LocalAvailability, error) {
			return LocalAvailabilityReady, nil
		},
	}, localRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)
	select {
	case <-catalogStarted:
	case <-time.After(time.Second):
		t.Fatal("local catalog lifecycle did not start")
	}

	manager.ObserveLocalAvailability(LocalAvailabilityUnavailable)
	recorder := httptest.NewRecorder()
	manager.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "service_unavailable" || body.Error.Code != "local_opencodex_unavailable" {
		t.Fatalf("typed error = %#v", body.Error)
	}
	if forwarded.Load() != 0 {
		t.Fatalf("parked local runtime forwarded %d request(s)", forwarded.Load())
	}
	select {
	case <-catalogStopped:
	case <-time.After(time.Second):
		t.Fatal("local catalog lifecycle was not canceled")
	}

	manager.ObserveLocalAvailability(LocalAvailabilityReady)
	stillParked := httptest.NewRecorder()
	manager.Handler().ServeHTTP(stillParked, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if stillParked.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready observation automatically re-admitted local runtime: %d", stillParked.Code)
	}
	snapshot := manager.Snapshot()
	if snapshot.Profile != RuntimeProfileLocalOpenCodex || snapshot.Admission != string(runtimeAdmissionLocalUnavailable) || snapshot.LocalAvailability != LocalAvailabilityReady {
		t.Fatalf("local loss snapshot = %#v", snapshot)
	}
}

func TestRuntimeManagerStartsLocalUnavailableWithHealthOnlyRuntime(t *testing.T) {
	tracker := NewTracker()
	var probes atomic.Int64
	var catalogStarted atomic.Int64
	var forwarded atomic.Int64
	localRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		_, _ = io.WriteString(w, "local")
	})
	localRuntime.CatalogLifecycle = func(context.Context) { catalogStarted.Add(1) }
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{
		Profile:            RuntimeProfileLocalOpenCodex,
		LocalProbeInterval: 5 * time.Millisecond,
		LocalProbe: func(context.Context) (LocalAvailability, error) {
			probes.Add(1)
			return LocalAvailabilityUnavailable, nil
		},
	}, localRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)

	snapshot := manager.Snapshot()
	if snapshot.Profile != RuntimeProfileLocalOpenCodex || snapshot.Admission != string(runtimeAdmissionLocalUnavailable) || snapshot.LocalAvailability != LocalAvailabilityUnavailable || snapshot.CatalogRunning {
		t.Fatalf("initial unavailable local snapshot = %#v", snapshot)
	}
	time.Sleep(30 * time.Millisecond)
	if got := probes.Load(); got != 1 {
		t.Fatalf("local probes = %d, want only the bootstrap preflight", got)
	}
	if got := catalogStarted.Load(); got != 0 {
		t.Fatalf("unavailable local startup started catalog lifecycle %d time(s)", got)
	}

	recorder := httptest.NewRecorder()
	manager.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable local status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "service_unavailable" || body.Error.Code != "local_opencodex_unavailable" {
		t.Fatalf("unavailable local typed error = %#v", body.Error)
	}
	if got := forwarded.Load(); got != 0 {
		t.Fatalf("unavailable local forwarded %d request(s)", got)
	}

	if err := manager.Apply(context.Background(), RuntimeSpec{Profile: RuntimeProfileExternal}, func(context.Context, *Tracker) (Runtime, error) {
		return runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "external")
		}), nil
	}); err != nil {
		t.Fatalf("explicit external recovery: %v", err)
	}
	external := httptest.NewRecorder()
	manager.Handler().ServeHTTP(external, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if external.Code != http.StatusOK || external.Body.String() != "external" {
		t.Fatalf("external recovery response = %d %q", external.Code, external.Body.String())
	}
	if err := manager.Apply(context.Background(), RuntimeSpec{Profile: RuntimeProfileNone}, nil); err != nil {
		t.Fatalf("explicit native recovery: %v", err)
	}
	if snapshot := manager.Snapshot(); snapshot.Admission != string(runtimeAdmissionPaused) {
		t.Fatalf("native recovery snapshot = %#v", snapshot)
	}
}

func TestRuntimeManagerCanStartParkedWithHealthOnlyRuntime(t *testing.T) {
	tracker := NewTracker()
	var catalogStarted atomic.Int64
	healthRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__relay/healthz" {
			_, _ = io.WriteString(w, "health")
			return
		}
		_, _ = io.WriteString(w, "must-not-forward")
	})
	healthRuntime.CatalogLifecycle = func(context.Context) { catalogStarted.Add(1) }
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileNone}, healthRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)

	health := httptest.NewRecorder()
	manager.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/__relay/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "health" {
		t.Fatalf("parked health = %d %q", health.Code, health.Body.String())
	}
	dataPlane := httptest.NewRecorder()
	manager.Handler().ServeHTTP(dataPlane, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if dataPlane.Code != http.StatusServiceUnavailable || dataPlane.Body.String() == "must-not-forward" {
		t.Fatalf("parked data plane = %d %q", dataPlane.Code, dataPlane.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dataPlane.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "service_unavailable" || body.Error.Code != "relay_native_mode" {
		t.Fatalf("parked typed error = %#v", body.Error)
	}
	if catalogStarted.Load() != 0 {
		t.Fatalf("parked startup started catalog lifecycle %d time(s)", catalogStarted.Load())
	}
}

func TestRuntimeManagerStopsOldCatalogBeforeStartingReplacement(t *testing.T) {
	tracker := NewTracker()
	oldStopped := make(chan struct{})
	newStarted := make(chan struct{})
	oldRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {})
	oldRuntime.CatalogLifecycle = func(ctx context.Context) {
		<-ctx.Done()
		close(oldStopped)
	}
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileExternal}, oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)

	err = manager.Apply(context.Background(), RuntimeSpec{Profile: RuntimeProfileExternal}, func(context.Context, *Tracker) (Runtime, error) {
		runtime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {})
		runtime.CatalogLifecycle = func(ctx context.Context) {
			select {
			case <-oldStopped:
			case <-time.After(time.Second):
				t.Error("replacement catalog started before old catalog stopped")
			}
			close(newStarted)
			<-ctx.Done()
		}
		return runtime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldStopped:
	case <-time.After(time.Second):
		t.Fatal("old catalog did not stop")
	}
	select {
	case <-newStarted:
	case <-time.After(time.Second):
		t.Fatal("new catalog did not start")
	}
}

func TestRuntimeManagerMonitorParksLocalOnNonReadyProbe(t *testing.T) {
	tracker := NewTracker()
	var probes atomic.Int64
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{
		Profile:            RuntimeProfileLocalOpenCodex,
		LocalProbeInterval: 10 * time.Millisecond,
		LocalProbeTimeout:  time.Second,
		LocalProbe: func(context.Context) (LocalAvailability, error) {
			if probes.Add(1) == 1 {
				return LocalAvailabilityReady, nil
			}
			return LocalAvailabilityInvalid, nil
		},
	}, runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "unexpected")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)
	waitForRuntime(t, manager, time.Second, func(snapshot RuntimeSnapshot) bool {
		return snapshot.Admission == string(runtimeAdmissionLocalUnavailable) && snapshot.LocalAvailability == LocalAvailabilityInvalid
	})
	recorder := httptest.NewRecorder()
	manager.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status after monitor loss = %d", recorder.Code)
	}
}

func TestRuntimeManagerLocalMonitorStopsBeforeProbeWhenAdmissionIsParked(t *testing.T) {
	tracker := NewTracker()
	var probes atomic.Int64
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{
		Profile:            RuntimeProfileLocalOpenCodex,
		LocalProbeInterval: 5 * time.Millisecond,
		LocalProbeTimeout:  time.Second,
		LocalProbeAllowed:  func() bool { return false },
		LocalProbe: func(context.Context) (LocalAvailability, error) {
			probes.Add(1)
			return LocalAvailabilityReady, nil
		},
	}, runtimeForTest(tracker, func(http.ResponseWriter, *http.Request) {}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntimeManager(t, manager)
	// NewRuntimeManager performs the one required pre-admission probe.  Once
	// the embedding routing gate is parked, the periodic monitor must not make
	// another loopback request.
	time.Sleep(40 * time.Millisecond)
	if got := probes.Load(); got != 1 {
		t.Fatalf("local probes after parked admission = %d, want 1 initial probe only", got)
	}
}

func TestRuntimeManagerCloseHonorsGateDeadlineAndClosesAfterRelease(t *testing.T) {
	tracker := NewTracker()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestDone := make(chan struct{})
	catalogStarted := make(chan struct{})
	catalogStopped := make(chan struct{})
	runtime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		finish := tracker.Begin()
		defer finish()
		close(requestStarted)
		<-releaseRequest
	})
	runtime.CatalogLifecycle = func(ctx context.Context) {
		close(catalogStarted)
		<-ctx.Done()
		close(catalogStopped)
	}
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileExternal}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-releaseRequest:
		default:
			close(releaseRequest)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	}()
	select {
	case <-catalogStarted:
	case <-time.After(time.Second):
		t.Fatal("catalog lifecycle did not start")
	}
	go func() {
		manager.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(requestDone)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("long request did not enter runtime")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	started := time.Now()
	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.Close(closeCtx) }()
	// Lifecycle cancellation happens before Close starts waiting for the
	// read-side gate. In particular, it must not wait for the long request
	// below to release that gate.
	select {
	case err := <-closeResult:
		closeCancel()
		t.Fatalf("close returned before lifecycle cancellation: %v", err)
	case <-catalogStopped:
	}
	err = <-closeResult
	closeCancel()
	if !errors.Is(err, ErrRuntimeDrainTimeout) {
		t.Fatalf("close error = %v, want runtime drain timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("close ignored gate deadline: %s", elapsed)
	}
	if snapshot := manager.Snapshot(); snapshot.Admission != string(runtimeAdmissionPaused) {
		t.Fatalf("timed out close snapshot = %#v", snapshot)
	}

	close(releaseRequest)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("long request did not finish after release")
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), time.Second)
	defer finishCancel()
	if err := manager.Close(finishCtx); err != nil {
		t.Fatalf("close after release: %v", err)
	}
	if snapshot := manager.Snapshot(); snapshot.Admission != string(runtimeAdmissionClosed) {
		t.Fatalf("closed snapshot = %#v", snapshot)
	}
}

func TestRuntimeManagerApplyHonorsGateDeadlineAndParksWorkers(t *testing.T) {
	tracker := NewTracker()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestDone := make(chan struct{})
	catalogStopped := make(chan struct{})
	oldRuntime := runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
		finish := tracker.Begin()
		defer finish()
		close(requestStarted)
		<-releaseRequest
	})
	oldRuntime.CatalogLifecycle = func(ctx context.Context) {
		<-ctx.Done()
		close(catalogStopped)
	}
	manager, err := NewRuntimeManager(context.Background(), tracker, RuntimeSpec{Profile: RuntimeProfileExternal}, oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-releaseRequest:
		default:
			close(releaseRequest)
		}
		closeRuntimeManager(t, manager)
	}()
	go func() {
		manager.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(requestDone)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("long request did not enter runtime")
	}

	var candidateDisposed atomic.Int64
	applyCtx, applyCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	err = manager.Apply(applyCtx, RuntimeSpec{Profile: RuntimeProfileExternal}, func(context.Context, *Tracker) (Runtime, error) {
		candidate := runtimeForTest(tracker, func(http.ResponseWriter, *http.Request) {})
		candidate.Dispose = func() { candidateDisposed.Add(1) }
		return candidate, nil
	})
	applyCancel()
	if !errors.Is(err, ErrRuntimeDrainTimeout) {
		t.Fatalf("apply error = %v, want runtime drain timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("apply ignored gate deadline: %s", elapsed)
	}
	if got := candidateDisposed.Load(); got != 1 {
		t.Fatalf("uncommitted candidate dispose calls = %d, want 1", got)
	}
	select {
	case <-catalogStopped:
	case <-time.After(time.Second):
		t.Fatal("timed out apply did not cancel old catalog lifecycle")
	}
	if snapshot := manager.Snapshot(); snapshot.Admission != string(runtimeAdmissionPaused) {
		t.Fatalf("timed out apply snapshot = %#v", snapshot)
	}

	close(releaseRequest)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("long request did not finish after release")
	}
	if err := manager.Apply(context.Background(), RuntimeSpec{Profile: RuntimeProfileExternal}, func(context.Context, *Tracker) (Runtime, error) {
		return runtimeForTest(tracker, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "recovered")
		}), nil
	}); err != nil {
		t.Fatalf("explicit apply after drain timeout: %v", err)
	}
	response := httptest.NewRecorder()
	manager.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusOK || response.Body.String() != "recovered" {
		t.Fatalf("recovered response = %d %q", response.Code, response.Body.String())
	}
}

func runtimeForTest(tracker *Tracker, handler http.HandlerFunc) Runtime {
	return Runtime{GeneralHandler: handler, InteractiveHandler: handler}
}

func waitForRuntime(t *testing.T, manager *RuntimeManager, timeout time.Duration, predicate func(RuntimeSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate != nil && predicate(manager.Snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if predicate == nil || !predicate(manager.Snapshot()) {
		t.Fatalf("timed out waiting for runtime state: %#v", manager.Snapshot())
	}
}

func closeRuntimeManager(t *testing.T, manager *RuntimeManager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Errorf("close runtime manager: %v", err)
	}
}
