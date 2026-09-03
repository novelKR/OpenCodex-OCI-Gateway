package routing

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Snapshot is safe to expose in a loopback health response. It deliberately
// contains no parse error text, path, credentials, or upstream information.
type Snapshot struct {
	State   State
	Legacy  bool
	Invalid bool
}

// DriftCheck is a read-only ownership check for the Codex artifacts bound in
// State. It returns no data to the health/UI surface; any failure simply parks
// the watcher as invalid.
type DriftCheck func(State) error

// RecoveryGate is an external durable recovery witness. Any error parks
// admission without exposing its details through health or status.
type RecoveryGate func() error

type WatcherOption func(*Watcher)

func WithDriftCheck(check DriftCheck) WatcherOption {
	return func(watcher *Watcher) { watcher.driftCheck = check }
}

func WithWatcherRecoveryGate(check RecoveryGate) WatcherOption {
	return func(watcher *Watcher) { watcher.recoveryGate = check }
}

func (s Snapshot) AllowsDataPlane() bool { return !s.Invalid && s.State.AllowsDataPlane() }
func (s Snapshot) AllowsCatalog() bool   { return !s.Invalid && s.State.AllowsCatalog() }

// Watcher polls the atomically-replaced state file. Polling avoids a native
// filesystem watcher dependency and works after editor/rename based writes.
type Watcher struct {
	store        *Store
	interval     time.Duration
	mu           sync.RWMutex
	snapshot     Snapshot
	driftCheck   DriftCheck
	recoveryGate RecoveryGate
}

func NewWatcher(store *Store, interval time.Duration, options ...WatcherOption) *Watcher {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	w := &Watcher{store: store, interval: interval}
	for _, option := range options {
		if option != nil {
			option(w)
		}
	}
	w.Refresh()
	return w
}

func (w *Watcher) Refresh() {
	if w == nil || w.store == nil {
		return
	}
	state, legacy, err := w.store.Read()
	snapshot := Snapshot{State: state, Legacy: legacy}
	// A transaction journal left beside a non-applying durable state means a
	// controller crashed between routing/config mutations. Do not infer which
	// side is authoritative; park the resident relay until explicit recovery.
	// During `applying`, the state itself already denies data-plane/catalog
	// traffic and the active controller needs the generation visible for its
	// acknowledgement wait.
	if err == nil {
		pending, transactionErr := w.store.HasPendingTransaction()
		if transactionErr != nil || (pending && (legacy || state.Phase != PhaseApplying)) {
			err = fmt.Errorf("routing transaction requires recovery")
		}
		maintenance, maintenanceFound, maintenanceErr := w.store.loadMaintenance()
		if maintenanceErr != nil || (maintenanceFound && (legacy || state.Phase != PhaseApplying || !maintenance.matchesState(state))) {
			err = fmt.Errorf("runtime maintenance requires recovery")
		}
		if pending && maintenanceFound {
			// The two transaction owners are mutually exclusive. Even if both
			// journals are individually well formed, their coexistence cannot
			// prove which runtime/config mutation is authoritative.
			err = fmt.Errorf("routing transaction owners conflict")
		}
		// An ordinary container-runtime route keeps its separate witness until
		// the lifecycle manager durably commits. A strict matching witness is
		// compatible with the finite routing states below; malformed, foreign,
		// or multiply-owned evidence parks admission.
		runtimeRouting, runtimeRoutingFound, runtimeRoutingErr := w.store.loadRuntimeRouting(state.BoundCodexConfigPath)
		if runtimeRoutingErr != nil {
			err = fmt.Errorf("container runtime routing witness requires recovery")
		} else if runtimeRoutingFound {
			controller := &Controller{store: w.store, journalPath: w.store.TransactionPath()}
			transaction, transactionFound, loadErr := controller.loadJournal()
			if loadErr != nil || maintenanceFound || !runtimeRouting.matchesState(state, transaction, transactionFound) {
				err = fmt.Errorf("container runtime routing witness conflicts with routing state")
			}
		}
		if err == nil && !legacy && w.driftCheck != nil && state.Phase != PhaseApplying && state.Phase != PhaseRecoveryRequired {
			if driftErr := w.driftCheck(state); driftErr != nil {
				err = fmt.Errorf("relay-managed Codex routing drift requires recovery")
			}
		}
		if err == nil && w.recoveryGate != nil {
			if gateErr := w.recoveryGate(); gateErr != nil {
				err = fmt.Errorf("external routing recovery gate is active")
			}
		}
	}
	if err != nil {
		recovery, recoveryErr := NewRecoveryState(w.store.configPath)
		if recoveryErr == nil {
			snapshot.State = recovery
		}
		snapshot.Legacy = false
		snapshot.Invalid = true
	}
	w.mu.Lock()
	w.snapshot = snapshot
	w.mu.Unlock()
}

func (w *Watcher) Run(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Refresh()
		}
	}
}

func (w *Watcher) Snapshot() Snapshot {
	if w == nil {
		return Snapshot{State: State{Phase: PhaseRelayActive}}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snapshot
}

func (w *Watcher) AllowsDataPlane() bool { return w.Snapshot().AllowsDataPlane() }
func (w *Watcher) AllowsCatalog() bool   { return w.Snapshot().AllowsCatalog() }
