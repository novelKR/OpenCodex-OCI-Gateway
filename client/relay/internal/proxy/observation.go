package proxy

import (
	"sync"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

// ConnectionState is a deliberately small, non-secret observation projection
// for the local control plane. It never carries an endpoint, error text,
// credential source, or timestamp.
type ConnectionState string

const (
	ConnectionUnknown       ConnectionState = "unknown"
	ConnectionReachable     ConnectionState = "reachable"
	ConnectionUnreachable   ConnectionState = "unreachable"
	ConnectionNotApplicable ConnectionState = "not_applicable"
)

// CatalogLifecycleState reflects the resident worker's real gate state, not
// merely the desired routing state. `paused` is published only after any
// in-flight refresh has returned following cancellation.
type CatalogLifecycleState string

const (
	CatalogLifecycleUnknown CatalogLifecycleState = "unknown"
	CatalogLifecycleRunning CatalogLifecycleState = "running"
	CatalogLifecyclePaused  CatalogLifecycleState = "paused"
)

// ProbeState tells the control-plane UI whether the optional macOS-only
// observer is active. It does not reveal a target or the outcome itself.
type ProbeState string

const (
	ProbeDisabled      ProbeState = "disabled"
	ProbeEnabled       ProbeState = "enabled"
	ProbeNotApplicable ProbeState = "not_applicable"
)

type ConnectionSnapshot struct {
	RemoteGateway    ConnectionState
	CatalogLifecycle CatalogLifecycleState
	Probe            ProbeState
	LocalOpenCodex   LocalAvailability
}

// ConnectionObservation is an in-memory projection only. Restarting the
// resident relay intentionally returns the remote status to unknown rather
// than persisting traffic observations beside routing state.
type ConnectionObservation struct {
	externalGateway bool

	mu                 sync.RWMutex
	remoteGateway      ConnectionState
	catalogLifecycle   CatalogLifecycleState
	probe              ProbeState
	localOpenCodex     LocalAvailability
	lastCatalogSuccess time.Time
}

func NewConnectionObservation(upstreamMode string) *ConnectionObservation {
	remoteGateway := ConnectionUnknown
	probe := ProbeDisabled
	if upstreamMode != config.UpstreamModeExternalGateway {
		remoteGateway = ConnectionNotApplicable
		probe = ProbeNotApplicable
	}
	return &ConnectionObservation{
		externalGateway:  upstreamMode == config.UpstreamModeExternalGateway,
		remoteGateway:    remoteGateway,
		catalogLifecycle: CatalogLifecycleUnknown,
		localOpenCodex:   LocalAvailabilityUnknown,
		probe:            probe,
	}
}

func (o *ConnectionObservation) Snapshot() ConnectionSnapshot {
	if o == nil {
		return ConnectionSnapshot{
			RemoteGateway:    ConnectionUnknown,
			CatalogLifecycle: CatalogLifecycleUnknown,
			Probe:            ProbeDisabled,
		}
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return ConnectionSnapshot{
		RemoteGateway:    o.remoteGateway,
		CatalogLifecycle: o.catalogLifecycle,
		Probe:            o.probe,
		LocalOpenCodex:   o.localOpenCodex,
	}
}

// SetLocalOpenCodex records only a finite local identity/catalog result. It
// is intentionally not an endpoint health dump and is safe for the redacted
// relayctl status projection.
func (o *ConnectionObservation) SetLocalOpenCodex(value LocalAvailability) {
	if o == nil {
		return
	}
	value = normalizeLocalAvailability(value)
	o.mu.Lock()
	o.localOpenCodex = value
	o.mu.Unlock()
}

func (o *ConnectionObservation) RecordGatewayReachable() {
	if o == nil || !o.externalGateway {
		return
	}
	o.mu.Lock()
	o.remoteGateway = ConnectionReachable
	o.mu.Unlock()
}

func (o *ConnectionObservation) RecordGatewayUnreachable() {
	if o == nil || !o.externalGateway {
		return
	}
	o.mu.Lock()
	o.remoteGateway = ConnectionUnreachable
	o.mu.Unlock()
}

func (o *ConnectionObservation) RecordCatalogRefreshSuccess(now time.Time) {
	if o == nil || !o.externalGateway {
		return
	}
	o.mu.Lock()
	o.remoteGateway = ConnectionReachable
	o.lastCatalogSuccess = now
	o.mu.Unlock()
}

// HasRecentCatalogSuccess allows the scheduled diagnostic probe to reuse a
// successful catalog observation instead of producing duplicate `/models`
// traffic. A failed catalog refresh is deliberately not treated as gateway
// unreachable: failures can arise after a valid response is parsed locally.
func (o *ConnectionObservation) HasRecentCatalogSuccess(now time.Time, within time.Duration) bool {
	if o == nil || !o.externalGateway || within <= 0 {
		return false
	}
	o.mu.RLock()
	last := o.lastCatalogSuccess
	o.mu.RUnlock()
	return !last.IsZero() && now.Sub(last) >= 0 && now.Sub(last) < within
}

func (o *ConnectionObservation) SetCatalogLifecycle(state CatalogLifecycleState) {
	if o == nil {
		return
	}
	switch state {
	case CatalogLifecycleUnknown, CatalogLifecycleRunning, CatalogLifecyclePaused:
	default:
		return
	}
	o.mu.Lock()
	o.catalogLifecycle = state
	o.mu.Unlock()
}

func (o *ConnectionObservation) SetProbeEnabled(enabled bool) {
	if o == nil || !o.externalGateway {
		return
	}
	o.mu.Lock()
	if enabled {
		o.probe = ProbeEnabled
	} else {
		o.probe = ProbeDisabled
	}
	o.mu.Unlock()
}
