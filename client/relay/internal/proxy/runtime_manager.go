package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/scheduler"
)

// RuntimeProfile identifies the immutable upstream runtime currently attached
// to the resident listener. It is deliberately independent of config and
// routing packages so the control plane can map its own durable state onto a
// runtime without making the proxy own that state.
type RuntimeProfile string

const (
	RuntimeProfileExternal            RuntimeProfile = "external"
	RuntimeProfileLocalOpenCodex      RuntimeProfile = "local_opencodex"
	RuntimeProfileLocalAppleContainer RuntimeProfile = "local_apple_container"
	RuntimeProfileNone                RuntimeProfile = "none"
)

// LocalAvailability is the bounded, non-secret result of a local OpenCodex
// identity/catalog preflight. Unknown input is normalized to Unknown; only
// Ready may admit a local runtime.
type LocalAvailability string

const (
	LocalAvailabilityReady       LocalAvailability = "ready"
	LocalAvailabilityUnavailable LocalAvailability = "unavailable"
	LocalAvailabilityForeign     LocalAvailability = "foreign"
	LocalAvailabilityInvalid     LocalAvailability = "invalid"
	LocalAvailabilityUnknown     LocalAvailability = "unknown"
)

const (
	defaultLocalProbeInterval = 5 * time.Second
	defaultLocalProbeTimeout  = 2 * time.Second
	runtimeGateRetryInterval  = 5 * time.Millisecond
)

var (
	ErrRuntimeManagerClosed       = errors.New("runtime manager is closed")
	ErrRuntimeDrainTimeout        = errors.New("runtime drain did not complete")
	ErrLocalOpenCodexUnavailable  = errors.New("local OpenCodex is unavailable")
	ErrRuntimeMaintenanceState    = errors.New("runtime maintenance state is invalid")
	ErrInvalidRuntime             = errors.New("runtime is invalid")
	ErrInvalidRuntimeProfile      = errors.New("runtime profile is invalid")
	ErrMissingLocalOpenCodexProbe = errors.New("local OpenCodex probe is required")
)

// LocalUnavailableError records only the bounded preflight classification.
// It intentionally carries neither an endpoint nor an underlying probe error.
type LocalUnavailableError struct {
	Availability LocalAvailability
}

func (e *LocalUnavailableError) Error() string {
	return fmt.Sprintf("%s: %s", ErrLocalOpenCodexUnavailable, e.Availability)
}

func (e *LocalUnavailableError) Unwrap() error { return ErrLocalOpenCodexUnavailable }

// LocalProbe is injected by the caller. It must make its own no-proxy,
// no-redirect, bounded identity/catalog checks and return only the bounded
// availability classification. The runtime manager never assumes a config
// shape or performs a raw TCP check.
type LocalProbe func(context.Context) (LocalAvailability, error)

// LocalProbeAllowed is an optional admission check owned by the embedding
// runtime.  It lets a routing watcher stop periodic loopback identity probes
// as soon as a profile enters applying/native/recovery, without coupling the
// proxy package to durable routing state.
type LocalProbeAllowed func() bool

// CatalogLifecycle is one immutable runtime's background catalog worker. It
// must return after ctx is canceled. The manager starts at most one lifecycle
// for the active runtime and waits for it to stop before the next one starts.
type CatalogLifecycle func(ctx context.Context)

// Runtime contains immutable handlers and background work for one upstream
// profile. Factory functions receive the manager's shared Tracker and should
// build their proxy.Server with that exact tracker.
type Runtime struct {
	GeneralHandler     http.Handler
	InteractiveHandler http.Handler
	CatalogLifecycle   CatalogLifecycle

	// Dispose is invoked after the runtime is no longer reachable by either
	// resident listener. It is for best-effort transport cleanup only; it must
	// not block or mutate durable routing/configuration state.
	Dispose func()
}

func (r Runtime) handlerForLane(lane scheduler.Lane) http.Handler {
	switch lane {
	case scheduler.LaneGeneral:
		return r.GeneralHandler
	case scheduler.LaneInteractive:
		return r.InteractiveHandler
	default:
		return nil
	}
}

func (r Runtime) validate() error {
	if r.GeneralHandler == nil || r.InteractiveHandler == nil {
		return fmt.Errorf("%w: both listener handlers are required", ErrInvalidRuntime)
	}
	return nil
}

// RuntimeForServer adapts an immutable Server into a manager runtime. The
// caller remains responsible for constructing server with Manager.Tracker().
func RuntimeForServer(server *Server, lifecycle CatalogLifecycle) (Runtime, error) {
	if server == nil {
		return Runtime{}, fmt.Errorf("%w: server is nil", ErrInvalidRuntime)
	}
	return Runtime{
		GeneralHandler:     server.HandlerForLane(scheduler.LaneGeneral),
		InteractiveHandler: server.HandlerForLane(scheduler.LaneInteractive),
		CatalogLifecycle:   lifecycle,
		Dispose:            server.CloseIdleConnections,
	}, nil
}

// CloseIdleConnections discards only idle transport connections. Requests are
// drained by RuntimeManager before this is called, so it is safe for a retired
// immutable server and does not affect the stable resident listeners.
func (s *Server) CloseIdleConnections() {
	if s == nil || s.proxy == nil || s.proxy.Transport == nil {
		return
	}
	if transport, ok := s.proxy.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// RuntimeSpec describes runtime-only behavior for a profile. It carries no
// endpoint, credentials, catalog path, or persistent routing state.
type RuntimeSpec struct {
	Profile RuntimeProfile

	// StartParked is reserved for reconstructing an Apple runtime while a
	// durable runtime-maintenance witness is pending after process restart. It
	// installs only the health surface: no preflight, catalog worker, monitor,
	// or data-plane admission starts until the coordinator verifies the selected
	// container endpoint and calls ResumeMaintenance.
	StartParked bool

	// LocalProbe is required for the local profile. It is run before a local
	// profile becomes active and periodically while it remains active.
	LocalProbe         LocalProbe
	LocalProbeInterval time.Duration
	LocalProbeTimeout  time.Duration
	LocalProbeAllowed  LocalProbeAllowed
	// LocalAvailabilityObserver receives only the bounded preflight result.
	// It lets the active Server's health projection reflect a local loss
	// without coupling RuntimeManager to config or routing packages.
	LocalAvailabilityObserver func(LocalAvailability)
}

func (s RuntimeSpec) normalized() (RuntimeSpec, error) {
	if s.StartParked && s.Profile != RuntimeProfileLocalAppleContainer {
		return RuntimeSpec{}, fmt.Errorf("%w: only the Apple runtime may start parked", ErrInvalidRuntimeProfile)
	}
	switch s.Profile {
	case RuntimeProfileExternal, RuntimeProfileNone:
		return s, nil
	case RuntimeProfileLocalOpenCodex, RuntimeProfileLocalAppleContainer:
		if s.LocalProbe == nil {
			return RuntimeSpec{}, ErrMissingLocalOpenCodexProbe
		}
		if s.LocalProbeInterval <= 0 {
			s.LocalProbeInterval = defaultLocalProbeInterval
		}
		if s.LocalProbeTimeout <= 0 {
			s.LocalProbeTimeout = defaultLocalProbeTimeout
		}
		return s, nil
	default:
		return RuntimeSpec{}, fmt.Errorf("%w: %q", ErrInvalidRuntimeProfile, s.Profile)
	}
}

// RuntimeFactory builds one immutable runtime without starting its catalog
// lifecycle. The manager invokes it before it pauses the old runtime, so a
// construction failure never interrupts the active listener.
type RuntimeFactory func(ctx context.Context, tracker *Tracker) (Runtime, error)

type runtimeAdmission string

const (
	runtimeAdmissionActive           runtimeAdmission = "active"
	runtimeAdmissionTransitioning    runtimeAdmission = "transitioning"
	runtimeAdmissionLocalUnavailable runtimeAdmission = "local_unavailable"
	runtimeAdmissionPaused           runtimeAdmission = "paused"
	runtimeAdmissionClosed           runtimeAdmission = "closed"
)

// RuntimeSnapshot is the in-memory projection exposed to a controller/status
// adapter. It deliberately excludes URLs, credentials, catalog paths, and raw
// probe failures.
type RuntimeSnapshot struct {
	Generation        uint64
	Profile           RuntimeProfile
	Admission         string
	LocalAvailability LocalAvailability
	ActiveRequests    int64
	CatalogRunning    bool
}

type runtimeSlot struct {
	runtime    Runtime
	spec       RuntimeSpec
	generation uint64
	lifecycle  *managedLifecycle
	monitor    *managedMonitor
}

type managedLifecycle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startLifecycle(parent context.Context, lifecycle CatalogLifecycle) *managedLifecycle {
	if lifecycle == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	running := &managedLifecycle{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		lifecycle(ctx)
	}()
	return running
}

func (l *managedLifecycle) cancelAndWait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.cancel()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("catalog lifecycle: %w", ctx.Err())
	}
}

type managedMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *managedMonitor) cancelAndWait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.cancel()
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("local availability monitor: %w", ctx.Err())
	}
}

func (s *runtimeSlot) stop(ctx context.Context) error {
	s.cancelWorkers()
	if err := s.monitor.cancelAndWait(ctx); err != nil {
		return err
	}
	return s.lifecycle.cancelAndWait(ctx)
}

func (s *runtimeSlot) cancelWorkers() {
	if s == nil {
		return
	}
	// Signal both workers before waiting. This prevents a stuck/slow probe from
	// needlessly extending catalog egress after admission has been parked.
	if s.monitor != nil {
		s.monitor.cancel()
	}
	if s.lifecycle != nil {
		s.lifecycle.cancel()
	}
}

// RuntimeManager owns an atomic handler indirection while keeping the process
// listeners and one Tracker stable. It is intentionally not a routing-state
// writer: callers apply durable transactions first/afterward as appropriate
// and pass fully built immutable runtimes here.
type RuntimeManager struct {
	parent context.Context
	cancel context.CancelFunc

	tracker *Tracker

	// changes serializes lifecycle cancellation, tracker quiescence, and
	// replacement. gate excludes protocol requests while a replacement is
	// committed; it is distinct from Tracker so a stable handler can reject or
	// wait before it reaches a retired proxy server.
	changes sync.Mutex
	gate    sync.RWMutex

	mu                sync.Mutex
	current           *runtimeSlot
	generation        uint64
	admission         runtimeAdmission
	localAvailability LocalAvailability
	changed           chan struct{}

	maintenanceMu       sync.Mutex
	maintenancePrepared bool
	maintenanceSlot     *runtimeSlot
}

// NewRuntimeManager starts the supplied initial runtime behind stable
// handlers. A local initial runtime that fails its injected preflight remains
// installed solely for health, with local_unavailable admission and no
// lifecycle or monitor. This preserves an explicit External/Native recovery
// path without ever admitting the unready local data plane.
func NewRuntimeManager(ctx context.Context, tracker *Tracker, spec RuntimeSpec, initial Runtime) (*RuntimeManager, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tracker == nil {
		tracker = NewTracker()
	}
	var err error
	spec, err = spec.normalized()
	if err != nil {
		return nil, err
	}
	if spec.Profile != RuntimeProfileNone || initial.GeneralHandler != nil || initial.InteractiveHandler != nil {
		if err := initial.validate(); err != nil {
			return nil, err
		}
	}
	availability := LocalAvailabilityUnknown
	if isLocalRuntimeProfile(spec.Profile) && !spec.StartParked {
		availability = probeLocal(ctx, spec.LocalProbe)
		notifyLocalAvailability(spec, availability)
	}
	parent, cancel := context.WithCancel(ctx)
	manager := &RuntimeManager{
		parent:            parent,
		cancel:            cancel,
		tracker:           tracker,
		generation:        1,
		localAvailability: availability,
		changed:           make(chan struct{}),
	}
	if spec.Profile == RuntimeProfileNone {
		// Retain a fully constructed but never-admitted handler solely for the
		// loopback health endpoint. This lets a relay that starts while Native,
		// applying, recovery, or a lost Local profile is selected prove its
		// parked gate without opening any data-plane or catalog lifecycle.
		if initial.GeneralHandler != nil && initial.InteractiveHandler != nil {
			manager.current = &runtimeSlot{runtime: initial, spec: spec, generation: manager.generation}
		}
		manager.admission = runtimeAdmissionPaused
		return manager, nil
	}
	manager.current = &runtimeSlot{runtime: initial, spec: spec, generation: manager.generation}
	if spec.StartParked {
		manager.admission = runtimeAdmissionPaused
		return manager, nil
	}
	if isLocalRuntimeProfile(spec.Profile) && availability != LocalAvailabilityReady {
		// Keep the Local profile visible to the controller and retain the
		// supplied handlers for loopback health only. Starting either worker
		// here would resume local catalog/probe egress after a failed bootstrap
		// preflight, before an explicit recovery Apply has been accepted.
		manager.admission = runtimeAdmissionLocalUnavailable
		return manager, nil
	}
	manager.admission = runtimeAdmissionActive
	manager.startSlot(manager.current)
	return manager, nil
}

// Tracker is the sole request tracker intended for every immutable server
// created for this manager. Keeping it stable allows an apply controller to
// observe a truthful zero-request drain across profile generations.
func (m *RuntimeManager) Tracker() *Tracker {
	if m == nil {
		return nil
	}
	return m.tracker
}

// Handler returns the stable general-listener handler.
func (m *RuntimeManager) Handler() http.Handler {
	return m.HandlerForLane(scheduler.LaneGeneral)
}

// HandlerForLane returns a stable handler suitable for a long-lived
// http.Server. The returned handler never captures a particular upstream
// runtime; each request resolves the current immutable runtime after admission
// succeeds.
func (m *RuntimeManager) HandlerForLane(lane scheduler.Lane) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.serveHTTP(lane, w, r)
	})
}

func (m *RuntimeManager) serveHTTP(lane scheduler.Lane, w http.ResponseWriter, r *http.Request) {
	if lane != scheduler.LaneGeneral && lane != scheduler.LaneInteractive {
		writeError(w, http.StatusInternalServerError, "invalid_listener_lane")
		return
	}
	for {
		admission, current, changed := m.snapshotForHandler()
		// Health remains available through a parked runtime. It has no
		// data-plane credential lookup and is required for the controller to
		// acknowledge a native/recovery/applying state while the stable listener
		// PID remains alive.
		if r.URL.Path == "/__relay/healthz" && current != nil {
			handler := current.runtime.handlerForLane(lane)
			if handler != nil {
				handler.ServeHTTP(w, r)
				return
			}
		}
		switch admission {
		case runtimeAdmissionActive:
			m.gate.RLock()
			admission, current, _ = m.snapshotForHandler()
			if admission != runtimeAdmissionActive || current == nil {
				m.gate.RUnlock()
				continue
			}
			handler := current.runtime.handlerForLane(lane)
			if handler == nil {
				m.gate.RUnlock()
				writeError(w, http.StatusServiceUnavailable, "upstream_unavailable")
				return
			}
			handler.ServeHTTP(w, r)
			m.gate.RUnlock()
			return
		case runtimeAdmissionTransitioning:
			// A transition deliberately pauses new admission rather than sending
			// a request through a runtime that is about to be retired. Waiting
			// preserves the existing response contract for a successful switch.
			select {
			case <-changed:
				continue
			case <-r.Context().Done():
				// A request that timed out while waiting for a drain/swap must
				// still receive the relay's bounded 503 shape when its writer is
				// usable. Returning without a response can otherwise look like a
				// successful empty HTTP response to in-process callers.
				writeTypedError(w, http.StatusServiceUnavailable, "service_unavailable", "routing_switch_in_progress")
				return
			}
		case runtimeAdmissionLocalUnavailable:
			writeTypedError(w, http.StatusServiceUnavailable, "service_unavailable", "local_opencodex_unavailable")
			return
		case runtimeAdmissionPaused, runtimeAdmissionClosed:
			// Native, recovery, and start-parked states keep the listener only
			// for health/control acknowledgement.  They must not be mistaken for
			// a generic upstream outage or trigger a client retry toward a
			// different backend.
			writeTypedError(w, http.StatusServiceUnavailable, "service_unavailable", "relay_native_mode")
			return
		default:
			writeError(w, http.StatusServiceUnavailable, "upstream_unavailable")
			return
		}
	}
}

func (m *RuntimeManager) snapshotForHandler() (runtimeAdmission, *runtimeSlot, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admission, m.current, m.changed
}

// Snapshot returns a bounded in-memory view for a status adapter.
func (m *RuntimeManager) Snapshot() RuntimeSnapshot {
	if m == nil {
		return RuntimeSnapshot{Admission: string(runtimeAdmissionClosed), LocalAvailability: LocalAvailabilityUnknown}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	profile := RuntimeProfileNone
	catalogRunning := false
	if m.current != nil {
		profile = m.current.spec.Profile
		if m.current.lifecycle != nil {
			select {
			case <-m.current.lifecycle.done:
			default:
				catalogRunning = true
			}
		}
	}
	return RuntimeSnapshot{
		Generation:        m.generation,
		Profile:           profile,
		Admission:         string(m.admission),
		LocalAvailability: m.localAvailability,
		ActiveRequests:    m.tracker.Active(),
		CatalogRunning:    catalogRunning,
	}
}

// Apply builds and atomically activates a new immutable runtime without
// replacing resident listeners or Tracker. A local profile is preflighted
// after construction but before the old runtime is paused; an unavailable
// local backend therefore leaves the current runtime untouched.
func (m *RuntimeManager) Apply(ctx context.Context, spec RuntimeSpec, factory RuntimeFactory) error {
	if m == nil {
		return ErrRuntimeManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	spec, err = spec.normalized()
	if err != nil {
		return err
	}
	if spec.StartParked {
		return fmt.Errorf("%w: parked startup is constructor-only", ErrInvalidRuntimeProfile)
	}

	var candidate Runtime
	if spec.Profile != RuntimeProfileNone {
		if factory == nil {
			return fmt.Errorf("%w: runtime factory is required", ErrInvalidRuntime)
		}
		candidate, err = factory(ctx, m.tracker)
		if err != nil {
			return fmt.Errorf("build runtime: %w", err)
		}
		if err := candidate.validate(); err != nil {
			if candidate.Dispose != nil {
				candidate.Dispose()
			}
			return err
		}
	}
	committed := false
	defer func() {
		if !committed && candidate.Dispose != nil {
			candidate.Dispose()
		}
	}()

	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if m.maintenancePrepared {
		return ErrRuntimeMaintenanceState
	}
	m.changes.Lock()
	defer m.changes.Unlock()
	if m.isClosed() {
		return ErrRuntimeManagerClosed
	}

	availability := m.localAvailabilitySnapshot()
	if isLocalRuntimeProfile(spec.Profile) {
		availability = probeLocal(ctx, spec.LocalProbe)
		m.ObserveLocalAvailability(availability)
		notifyLocalAvailability(spec, availability)
		if availability != LocalAvailabilityReady {
			return &LocalUnavailableError{Availability: availability}
		}
	}

	old := m.beginTransition()
	old.cancelWorkers()
	if err := m.lockGate(ctx); err != nil {
		m.parkAfterFailedDrain(old)
		return err
	}
	defer m.gate.Unlock()
	if err := m.quiesceAndStop(ctx, old); err != nil {
		// Catalog cancellation was requested but not confirmed. Re-admitting
		// the retired runtime could overlap catalog writers, so retain the old
		// slot only as a fail-closed parked runtime until an explicit recovery.
		m.parkAfterFailedDrain(old)
		return err
	}

	if spec.Profile == RuntimeProfileNone {
		m.commitPaused(old)
		committed = true
		return nil
	}
	if old != nil && old.runtime.Dispose != nil {
		old.runtime.Dispose()
	}

	m.commitRuntime(candidate, spec, availability)
	committed = true
	return nil
}

// lockGate waits for handlers that entered the stable read-side gate before a
// transition began. Unlike sync.RWMutex.Lock, it observes the caller's
// deadline so server shutdown cannot remain blocked behind an SSE or WebSocket
// forever. Admission was already switched to transitioning before this is
// called, so new handlers cannot keep the writer starved.
func (m *RuntimeManager) lockGate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %v", ErrRuntimeDrainTimeout, err)
		}
		if m.gate.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrRuntimeDrainTimeout, ctx.Err())
		case <-time.After(runtimeGateRetryInterval):
		}
	}
}

func (m *RuntimeManager) quiesceAndStop(ctx context.Context, old *runtimeSlot) error {
	for {
		var stopErr error
		if m.tracker.TryQuiesce(func() {
			if old != nil {
				stopErr = old.stop(ctx)
			}
		}) {
			if stopErr != nil {
				return fmt.Errorf("%w: %v", ErrRuntimeDrainTimeout, stopErr)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrRuntimeDrainTimeout, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (m *RuntimeManager) beginTransition() *runtimeSlot {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.current
	m.setAdmissionLocked(runtimeAdmissionTransitioning)
	return old
}

func (m *RuntimeManager) parkAfterFailedDrain(old *runtimeSlot) {
	m.mu.Lock()
	shouldCancel := m.current == old
	if shouldCancel {
		m.setAdmissionLocked(runtimeAdmissionPaused)
	}
	m.mu.Unlock()
	if shouldCancel {
		// A transition that cannot acquire the gate or finish its drain must
		// never leave a retired catalog/monitor running behind its parked
		// health-only handler.
		old.cancelWorkers()
	}
}

func (m *RuntimeManager) commitPaused(old *runtimeSlot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Retain the retired handler exclusively for loopback health. serveHTTP
	// keeps all data-plane paths behind runtimeAdmissionPaused, so a static
	// upstream cannot be reached after Native is applied. Keeping it avoids a
	// listener restart solely to make the health acknowledgement observable.
	m.current = old
	m.generation++
	m.setAdmissionLocked(runtimeAdmissionPaused)
}

func (m *RuntimeManager) commitRuntime(runtime Runtime, spec RuntimeSpec, availability LocalAvailability) {
	m.mu.Lock()
	m.generation++
	slot := &runtimeSlot{runtime: runtime, spec: spec, generation: m.generation}
	m.current = slot
	if isLocalRuntimeProfile(spec.Profile) {
		m.localAvailability = LocalAvailabilityReady
	} else {
		m.localAvailability = normalizeLocalAvailability(availability)
	}
	m.mu.Unlock()

	m.startSlot(slot)

	m.mu.Lock()
	// A monitor can only mark this slot unavailable after startSlot. It must
	// never override a newer profile or turn a parked local loss back on.
	if m.current == slot && m.admission == runtimeAdmissionTransitioning {
		m.setAdmissionLocked(runtimeAdmissionActive)
	}
	m.mu.Unlock()
}

func (m *RuntimeManager) startSlot(slot *runtimeSlot) {
	if slot == nil {
		return
	}
	slot.lifecycle = startLifecycle(m.parent, slot.runtime.CatalogLifecycle)
	if isLocalRuntimeProfile(slot.spec.Profile) {
		slot.monitor = m.startLocalMonitor(slot)
	}
}

func (m *RuntimeManager) startLocalMonitor(slot *runtimeSlot) *managedMonitor {
	ctx, cancel := context.WithCancel(m.parent)
	monitor := &managedMonitor{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(monitor.done)
		ticker := time.NewTicker(slot.spec.LocalProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if slot.spec.LocalProbeAllowed != nil && !slot.spec.LocalProbeAllowed() {
				// The durable routing state has already parked or is applying.
				// Do not issue a new local health/models request after that
				// boundary; explicit Apply owns any future monitor lifecycle.
				return
			}
			probeCtx, probeCancel := context.WithTimeout(ctx, slot.spec.LocalProbeTimeout)
			availability := probeLocal(probeCtx, slot.spec.LocalProbe)
			probeCancel()
			if ctx.Err() != nil {
				return
			}
			m.observeLocalAvailabilityForSlot(slot, availability)
			if availability != LocalAvailabilityReady {
				return
			}
		}
	}()
	return monitor
}

// ObserveLocalAvailability records an externally obtained bounded local
// readiness result. If the current active runtime is local, any non-ready
// result parks admission immediately and cancels its catalog lifecycle. A
// later Ready observation is recorded but never automatically re-admits the
// parked runtime; recovery requires an explicit Apply.
func (m *RuntimeManager) ObserveLocalAvailability(availability LocalAvailability) {
	if m == nil {
		return
	}
	availability = normalizeLocalAvailability(availability)
	m.mu.Lock()
	current := m.current
	m.localAvailability = availability
	var observer RuntimeSpec
	if current != nil {
		observer = current.spec
	}
	cancelCatalog := current != nil && isLocalRuntimeProfile(current.spec.Profile) && availability != LocalAvailabilityReady && current.lifecycle != nil
	if current != nil && isLocalRuntimeProfile(current.spec.Profile) && availability != LocalAvailabilityReady && m.admission == runtimeAdmissionActive {
		m.setAdmissionLocked(runtimeAdmissionLocalUnavailable)
	}
	m.mu.Unlock()
	notifyLocalAvailability(observer, availability)
	if cancelCatalog {
		current.lifecycle.cancel()
	}
}

func (m *RuntimeManager) observeLocalAvailabilityForSlot(slot *runtimeSlot, availability LocalAvailability) {
	availability = normalizeLocalAvailability(availability)
	m.mu.Lock()
	if m.current != slot || !isLocalRuntimeProfile(slot.spec.Profile) {
		m.mu.Unlock()
		return
	}
	m.localAvailability = availability
	cancelCatalog := availability != LocalAvailabilityReady && slot.lifecycle != nil
	if availability != LocalAvailabilityReady && m.admission == runtimeAdmissionActive {
		m.setAdmissionLocked(runtimeAdmissionLocalUnavailable)
	}
	m.mu.Unlock()
	notifyLocalAvailability(slot.spec, availability)
	if cancelCatalog {
		slot.lifecycle.cancel()
	}
}

func (m *RuntimeManager) localAvailabilitySnapshot() LocalAvailability {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.localAvailability
}

func (m *RuntimeManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admission == runtimeAdmissionClosed
}

func (m *RuntimeManager) setAdmissionLocked(admission runtimeAdmission) {
	if m.admission == admission {
		return
	}
	m.admission = admission
	close(m.changed)
	m.changed = make(chan struct{})
}

// PrepareMaintenance parks both stable listeners, stops the Apple profile's
// catalog/probe workers, and drains the shared request tracker. Admission stays
// transitioning across the external container replacement, while the Go locks
// themselves are released between control requests. This lets process shutdown
// remain bounded and lets an explicit recovery request resume a runtime that
// was reconstructed parked after a crash.
func (m *RuntimeManager) PrepareMaintenance(ctx context.Context) error {
	if m == nil {
		return ErrRuntimeManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if m.maintenancePrepared {
		return nil
	}

	m.changes.Lock()
	defer m.changes.Unlock()
	m.mu.Lock()
	current := m.current
	eligible := current != nil && current.spec.Profile == RuntimeProfileLocalAppleContainer &&
		(m.admission == runtimeAdmissionActive || m.admission == runtimeAdmissionPaused)
	m.mu.Unlock()
	if !eligible {
		return ErrRuntimeMaintenanceState
	}

	old := m.beginTransition()
	old.cancelWorkers()
	if err := m.lockGate(ctx); err != nil {
		m.parkAfterFailedDrain(old)
		return err
	}
	defer m.gate.Unlock()
	if err := m.quiesceAndStop(ctx, old); err != nil {
		m.parkAfterFailedDrain(old)
		return err
	}
	m.maintenancePrepared = true
	m.maintenanceSlot = old
	return nil
}

// VerifyMaintenance discards idle connections retained by the previous
// container instance and runs the Apple profile's authenticated local probe
// while admission remains parked. It does not restart catalog work or admit
// traffic; the routing journal must be committed first.
func (m *RuntimeManager) VerifyMaintenance(ctx context.Context) error {
	if m == nil {
		return ErrRuntimeManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if !m.maintenancePrepared || m.maintenanceSlot == nil ||
		m.maintenanceSlot.spec.Profile != RuntimeProfileLocalAppleContainer {
		return ErrRuntimeMaintenanceState
	}
	slot := m.maintenanceSlot
	if slot.runtime.Dispose != nil {
		slot.runtime.Dispose()
	}
	availability := probeLocal(ctx, slot.spec.LocalProbe)
	if availability != LocalAvailabilityReady {
		m.mu.Lock()
		m.localAvailability = availability
		m.mu.Unlock()
		notifyLocalAvailability(slot.spec, availability)
		return &LocalUnavailableError{Availability: availability}
	}
	m.mu.Lock()
	m.localAvailability = LocalAvailabilityReady
	m.mu.Unlock()
	notifyLocalAvailability(slot.spec, LocalAvailabilityReady)
	return nil
}

// ResumeMaintenance is deliberately no-fail: MaintenanceCoordinator invokes
// it only after endpoint verification, the final routing state, and journal
// removal are durable. A call without a live lease is a safe no-op.
func (m *RuntimeManager) ResumeMaintenance() {
	if m == nil {
		return
	}
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if !m.maintenancePrepared || m.maintenanceSlot == nil {
		return
	}
	slot := m.maintenanceSlot
	m.startSlot(slot)
	m.mu.Lock()
	if m.current == slot {
		m.localAvailability = LocalAvailabilityReady
		m.setAdmissionLocked(runtimeAdmissionActive)
	}
	m.mu.Unlock()
	m.maintenancePrepared = false
	m.maintenanceSlot = nil
}

// Close stops the active lifecycle and prevents further handler admission. It
// does not close any resident listener; the embedding process still owns that
// lifecycle. A timed-out drain remains fail-closed.
func (m *RuntimeManager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	m.changes.Lock()
	defer m.changes.Unlock()
	if m.isClosed() {
		return nil
	}
	old := m.beginTransition()
	// Cancel the manager parent before waiting for the protocol gate. A
	// long-lived SSE/WebSocket may retain that gate until shutdown's deadline,
	// but catalog and probe workers must stop as soon as Close starts.
	m.cancel()
	if err := m.lockGate(ctx); err != nil {
		m.parkAfterFailedDrain(old)
		return err
	}
	defer m.gate.Unlock()
	if err := m.quiesceAndStop(ctx, old); err != nil {
		m.parkAfterFailedDrain(old)
		return err
	}
	if old != nil && old.runtime.Dispose != nil {
		old.runtime.Dispose()
	}
	m.mu.Lock()
	m.current = nil
	m.setAdmissionLocked(runtimeAdmissionClosed)
	m.mu.Unlock()
	m.maintenancePrepared = false
	m.maintenanceSlot = nil
	return nil
}

func probeLocal(ctx context.Context, probe LocalProbe) LocalAvailability {
	if probe == nil {
		return LocalAvailabilityUnknown
	}
	availability, err := probe(ctx)
	if err != nil {
		return LocalAvailabilityUnknown
	}
	return normalizeLocalAvailability(availability)
}

func normalizeLocalAvailability(availability LocalAvailability) LocalAvailability {
	switch availability {
	case LocalAvailabilityReady, LocalAvailabilityUnavailable, LocalAvailabilityForeign, LocalAvailabilityInvalid, LocalAvailabilityUnknown:
		return availability
	default:
		return LocalAvailabilityUnknown
	}
}

func notifyLocalAvailability(spec RuntimeSpec, availability LocalAvailability) {
	if spec.LocalAvailabilityObserver != nil {
		spec.LocalAvailabilityObserver(normalizeLocalAvailability(availability))
	}
}

func isLocalRuntimeProfile(profile RuntimeProfile) bool {
	return profile == RuntimeProfileLocalOpenCodex || profile == RuntimeProfileLocalAppleContainer
}
