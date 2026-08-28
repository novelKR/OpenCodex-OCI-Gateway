// Package scheduler provides the process-wide capacity controls used by the
// relay's staged Responses normalization path. It owns no HTTP transport,
// request body, response body, retry, or routing policy.
package scheduler

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const MiB = int64(1024 * 1024)

var (
	ErrInvalidLimits        = errors.New("invalid scheduler limits")
	ErrInvalidLane          = errors.New("invalid scheduler lane")
	ErrQueueTimeout         = errors.New("scheduler capacity wait timed out")
	ErrPendingRequestClosed = errors.New("pending request is closed")
	ErrInvalidByteCount     = errors.New("invalid pending byte count")
	ErrPendingBytesTooLarge = errors.New("pending request bytes exceed scheduler limit")
)

// Lane identifies the listener that admitted a request. It is supplied by the
// relay's listener context and must never be inferred from client headers.
type Lane string

const (
	LaneGeneral     Lane = "general"
	LaneInteractive Lane = "interactive"
)

func (l Lane) valid() bool {
	return l == LaneGeneral || l == LaneInteractive
}

// Stage identifies the capacity boundary at which a wait occurred.
type Stage string

const (
	StageClassification Stage = "classification"
	StagePendingRequest Stage = "pending_request"
	StagePendingBytes   Stage = "pending_bytes"
	StageUpstream       Stage = "upstream"
	StageTransform      Stage = "transform"
	StageDelivery       Stage = "delivery"
)

// CapacityTimeoutError preserves the stage that exhausted its queue budget.
// Callers can use errors.Is(err, ErrQueueTimeout) for policy decisions.
type CapacityTimeoutError struct {
	Stage Stage
}

func (e *CapacityTimeoutError) Error() string {
	return fmt.Sprintf("%s: %v", e.Stage, ErrQueueTimeout)
}

func (e *CapacityTimeoutError) Unwrap() error { return ErrQueueTimeout }

// Limits configures independent capacity boundaries. QueueTimeout is also the
// total capacity-wait budget for a PendingRequest: admission, incremental byte
// reservations, and upstream acquisition share one absolute deadline.
type Limits struct {
	MaxClassifications          int
	MaxPendingRequests          int
	MaxPendingBytes             int64
	QueueTimeout                time.Duration
	MaxGeneralUpstream          int
	InteractiveReservedUpstream int
	MaxConcurrentTransforms     int
	MaxOpenDeliveries           int
}

func DefaultLimits() Limits {
	return Limits{
		MaxClassifications:          8,
		MaxPendingRequests:          24,
		MaxPendingBytes:             512 * MiB,
		QueueTimeout:                60 * time.Second,
		MaxGeneralUpstream:          4,
		InteractiveReservedUpstream: 1,
		MaxConcurrentTransforms:     2,
		MaxOpenDeliveries:           16,
	}
}

func (l Limits) validate() error {
	if l.MaxClassifications <= 0 || l.MaxPendingRequests <= 0 || l.MaxPendingBytes <= 0 ||
		l.QueueTimeout <= 0 || l.MaxGeneralUpstream <= 0 || l.InteractiveReservedUpstream <= 0 ||
		l.MaxConcurrentTransforms <= 0 || l.MaxOpenDeliveries <= 0 {
		return ErrInvalidLimits
	}
	return nil
}

// Snapshot is a content-free, point-in-time view suitable for health and
// metrics endpoints. Fields from independent stages can advance immediately
// after the snapshot is returned.
type Snapshot struct {
	ActiveClassifications  int
	WaitingClassifications int
	// PendingRequests includes admitted jobs whose request or response spool
	// ownership has been handed off and not yet released.
	PendingRequests            int
	PendingBytes               int64
	WaitingPendingRequests     int
	WaitingPendingByteRequests int
	WaitingPendingBytes        int64
	ActiveGeneralUpstream      int
	ActiveInteractiveUpstream  int
	ActiveReservedUpstream     int
	ActiveBorrowedInteractive  int
	WaitingGeneralUpstream     int
	WaitingInteractiveUpstream int
	ActiveTransforms           int
	WaitingTransforms          int
	ActiveDeliveries           int
	WaitingDeliveries          int
	CapacityRejections         uint64
}

// Scheduler coordinates every staged capacity boundary for one relay process.
// It is safe for concurrent use.
type Scheduler struct {
	limits         Limits
	classification *fifoLimiter
	pending        *pendingQuota
	upstream       *upstreamPool
	transform      *fifoLimiter
	delivery       *fifoLimiter
	rejections     atomic.Uint64
}

func New(limits Limits) (*Scheduler, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Scheduler{
		limits:         limits,
		classification: newFIFOLimiter(limits.MaxClassifications),
		pending:        newPendingQuota(limits.MaxPendingRequests, limits.MaxPendingBytes),
		upstream:       newUpstreamPool(limits.MaxGeneralUpstream, limits.InteractiveReservedUpstream),
		transform:      newFIFOLimiter(limits.MaxConcurrentTransforms),
		delivery:       newFIFOLimiter(limits.MaxOpenDeliveries),
	}, nil
}

func (s *Scheduler) Limits() Limits { return s.limits }

// Lease owns one stage permit. Release and Close are idempotent.
type Lease struct {
	stage       Stage
	lane        Lane
	wait        time.Duration
	releaseOnce sync.Once
	release     func()
}

// JobLease owns one request slot after classification. It lets callers carry
// the admission bound across an outbound request or normalized response
// without keeping a queue deadline or upstream permit alive.
type JobLease struct {
	releaseOnce sync.Once
	release     func()
}

func newJobLease(release func()) *JobLease {
	return &JobLease{release: release}
}

func (l *JobLease) Release() {
	if l == nil {
		return
	}
	l.releaseOnce.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

func (l *JobLease) Close() error {
	l.Release()
	return nil
}

func newLease(stage Stage, lane Lane, wait time.Duration, release func()) *Lease {
	return &Lease{stage: stage, lane: lane, wait: wait, release: release}
}

func (l *Lease) Stage() Stage {
	if l == nil {
		return ""
	}
	return l.stage
}

func (l *Lease) Lane() Lane {
	if l == nil {
		return ""
	}
	return l.lane
}

func (l *Lease) WaitDuration() time.Duration {
	if l == nil {
		return 0
	}
	return l.wait
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.releaseOnce.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

func (l *Lease) Close() error {
	l.Release()
	return nil
}

func (s *Scheduler) AcquireClassification(ctx context.Context) (*Lease, error) {
	lease, err := s.classification.acquire(ctx, time.Now().Add(s.limits.QueueTimeout), StageClassification)
	s.recordRejection(err)
	return lease, err
}

// AcquirePendingRequest reserves one request slot and starts its shared queue
// deadline. Callers should defer Close immediately. Known Content-Length can be
// reserved once; chunked bodies can reserve bytes before accepting each chunk.
func (s *Scheduler) AcquirePendingRequest(ctx context.Context, lane Lane) (*PendingRequest, error) {
	if !lane.valid() {
		return nil, ErrInvalidLane
	}
	deadline := time.Now().Add(s.limits.QueueTimeout)
	if err := s.pending.acquireRequest(ctx, deadline); err != nil {
		s.recordRejection(err)
		return nil, err
	}
	lifecycle, cancel := context.WithCancel(ctx)
	pending := &PendingRequest{
		scheduler: s,
		lane:      lane,
		deadline:  deadline,
		lifecycle: lifecycle,
		cancel:    cancel,
	}
	go func() {
		<-lifecycle.Done()
		pending.Close()
	}()
	return pending, nil
}

func (s *Scheduler) AcquireTransform(ctx context.Context) (*Lease, error) {
	lease, err := s.transform.acquire(ctx, time.Now().Add(s.limits.QueueTimeout), StageTransform)
	s.recordRejection(err)
	return lease, err
}

func (s *Scheduler) AcquireDelivery(ctx context.Context) (*Lease, error) {
	lease, err := s.delivery.acquire(ctx, time.Now().Add(s.limits.QueueTimeout), StageDelivery)
	s.recordRejection(err)
	return lease, err
}

func (s *Scheduler) recordRejection(err error) {
	if errors.Is(err, ErrQueueTimeout) {
		s.rejections.Add(1)
	}
}

func (s *Scheduler) Snapshot() Snapshot {
	classificationActive, classificationWaiting := s.classification.snapshot()
	pendingRequests, pendingBytes, waitingRequests, waitingByteRequests, waitingBytes := s.pending.snapshot()
	generalActive, interactiveActive, reservedActive, borrowedInteractive, generalWaiting, interactiveWaiting := s.upstream.snapshot()
	transformActive, transformWaiting := s.transform.snapshot()
	deliveryActive, deliveryWaiting := s.delivery.snapshot()
	return Snapshot{
		ActiveClassifications:      classificationActive,
		WaitingClassifications:     classificationWaiting,
		PendingRequests:            pendingRequests,
		PendingBytes:               pendingBytes,
		WaitingPendingRequests:     waitingRequests,
		WaitingPendingByteRequests: waitingByteRequests,
		WaitingPendingBytes:        waitingBytes,
		ActiveGeneralUpstream:      generalActive,
		ActiveInteractiveUpstream:  interactiveActive,
		ActiveReservedUpstream:     reservedActive,
		ActiveBorrowedInteractive:  borrowedInteractive,
		WaitingGeneralUpstream:     generalWaiting,
		WaitingInteractiveUpstream: interactiveWaiting,
		ActiveTransforms:           transformActive,
		WaitingTransforms:          transformWaiting,
		ActiveDeliveries:           deliveryActive,
		WaitingDeliveries:          deliveryWaiting,
		CapacityRejections:         s.rejections.Load(),
	}
}

// PendingRequest owns one pending-request quota and all bytes reserved for it.
// It can either release those quotas directly or transfer them to a JobLease
// that remains live across the outbound or downstream body lifetime. Close is
// safe to call concurrently and more than once.
type PendingRequest struct {
	scheduler *Scheduler
	lane      Lane
	deadline  time.Time
	lifecycle context.Context
	cancel    context.CancelFunc

	opMu sync.Mutex
	mu   sync.Mutex

	closed        bool
	reservedBytes int64
}

func (p *PendingRequest) Lane() Lane {
	if p == nil {
		return ""
	}
	return p.lane
}

func (p *PendingRequest) ReservedBytes() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reservedBytes
}

// ReserveBytes adds to this request's aggregate encoded-body reservation. A
// single request can never reserve more than the process-wide byte limit.
func (p *PendingRequest) ReserveBytes(ctx context.Context, count int64) error {
	if p == nil {
		return ErrPendingRequestClosed
	}
	if count < 0 {
		return ErrInvalidByteCount
	}
	if count == 0 {
		return p.openError()
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPendingRequestClosed
	}
	if count > p.scheduler.limits.MaxPendingBytes-p.reservedBytes {
		p.mu.Unlock()
		return ErrPendingBytesTooLarge
	}
	p.mu.Unlock()

	if err := p.scheduler.pending.reserveBytes(ctx, p.lifecycle, p.deadline, count); err != nil {
		p.scheduler.recordRejection(err)
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.scheduler.pending.releaseBytes(count)
		return ErrPendingRequestClosed
	}
	p.reservedBytes += count
	p.mu.Unlock()
	return nil
}

// ReleaseBytes returns part of a reservation before the request leaves the
// pending stage. It rejects underflow rather than silently corrupting metrics.
func (p *PendingRequest) ReleaseBytes(count int64) error {
	if p == nil {
		return ErrPendingRequestClosed
	}
	if count < 0 {
		return ErrInvalidByteCount
	}
	if count == 0 {
		return p.openError()
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPendingRequestClosed
	}
	if count > p.reservedBytes {
		p.mu.Unlock()
		return ErrInvalidByteCount
	}
	p.reservedBytes -= count
	p.mu.Unlock()
	p.scheduler.pending.releaseBytes(count)
	return nil
}

// AcquireUpstream waits for a lane-appropriate upstream permit. The pending
// request and byte quotas are always released before this method returns.
func (p *PendingRequest) AcquireUpstream(ctx context.Context) (*Lease, error) {
	if p == nil {
		return nil, ErrPendingRequestClosed
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if err := p.openError(); err != nil {
		return nil, err
	}
	lease, err := p.scheduler.upstream.acquire(ctx, p.lifecycle, p.deadline, p.lane)
	p.finish()
	p.scheduler.recordRejection(err)
	return lease, err
}

// HandoffPassthrough transfers the request slot and its encoded-byte
// reservation to a JobLease. The caller must release the job when the outbound
// body reaches EOF or Close.
func (p *PendingRequest) HandoffPassthrough() (*JobLease, error) {
	if p == nil {
		return nil, ErrPendingRequestClosed
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	return p.handoff(true)
}

// AcquireUpstreamJob acquires the lane-specific upstream permit and transfers
// the request slot to an end-to-end JobLease. Encoded request bytes leave the
// pending quota at dispatch because the upstream permit bounds the rewritten
// body from that point forward.
func (p *PendingRequest) AcquireUpstreamJob(ctx context.Context) (*Lease, *JobLease, error) {
	if p == nil {
		return nil, nil, ErrPendingRequestClosed
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if err := p.openError(); err != nil {
		return nil, nil, err
	}
	lease, err := p.scheduler.upstream.acquire(ctx, p.lifecycle, p.deadline, p.lane)
	if err != nil {
		p.finish()
		p.scheduler.recordRejection(err)
		return nil, nil, err
	}
	job, err := p.handoff(false)
	if err != nil {
		lease.Release()
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, nil, contextErr
		}
		if lifecycleErr := p.lifecycle.Err(); lifecycleErr != nil {
			return nil, nil, lifecycleErr
		}
		return nil, nil, err
	}
	return lease, job, nil
}

func (p *PendingRequest) handoff(retainBytes bool) (*JobLease, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPendingRequestClosed
	}
	p.closed = true
	reserved := p.reservedBytes
	p.reservedBytes = 0
	p.mu.Unlock()
	p.cancel()

	retained := reserved
	if !retainBytes {
		p.scheduler.pending.releaseBytes(reserved)
		retained = 0
	}
	return newJobLease(func() {
		p.scheduler.pending.releaseRequest(retained)
	}), nil
}

func (p *PendingRequest) openError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrPendingRequestClosed
	}
	return nil
}

func (p *PendingRequest) finish() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	reserved := p.reservedBytes
	p.reservedBytes = 0
	p.mu.Unlock()
	p.cancel()
	p.scheduler.pending.releaseRequest(reserved)
}

func (p *PendingRequest) Close() error {
	if p == nil {
		return nil
	}
	p.finish()
	return nil
}

type fifoLimiter struct {
	mu       sync.Mutex
	capacity int
	active   int
	waiters  list.List
}

type limiterWaiter struct {
	ready   chan struct{}
	element *list.Element
	granted bool
}

func newFIFOLimiter(capacity int) *fifoLimiter {
	return &fifoLimiter{capacity: capacity}
}

func (l *fifoLimiter) acquire(ctx context.Context, deadline time.Time, stage Stage) (*Lease, error) {
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !started.Before(deadline) {
		return nil, &CapacityTimeoutError{Stage: stage}
	}
	waiter := &limiterWaiter{ready: make(chan struct{})}
	l.mu.Lock()
	if l.active < l.capacity && l.waiters.Len() == 0 {
		l.active++
		l.mu.Unlock()
		return newLease(stage, "", time.Since(started), l.release), nil
	}
	waiter.element = l.waiters.PushBack(waiter)
	l.mu.Unlock()

	if err := waitUntil(ctx, nil, deadline, waiter.ready, stage); err != nil {
		l.mu.Lock()
		if waiter.granted {
			l.active--
			l.dispatchLocked()
		} else if waiter.element != nil {
			l.waiters.Remove(waiter.element)
			waiter.element = nil
		}
		l.mu.Unlock()
		return nil, err
	}
	return newLease(stage, "", time.Since(started), l.release), nil
}

func (l *fifoLimiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.dispatchLocked()
	l.mu.Unlock()
}

func (l *fifoLimiter) dispatchLocked() {
	for l.active < l.capacity && l.waiters.Len() > 0 {
		element := l.waiters.Front()
		waiter := element.Value.(*limiterWaiter)
		l.waiters.Remove(element)
		waiter.element = nil
		waiter.granted = true
		l.active++
		close(waiter.ready)
	}
}

func (l *fifoLimiter) snapshot() (active, waiting int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.waiters.Len()
}

type quotaWaiter struct {
	ready   chan struct{}
	element *list.Element
	bytes   int64
	granted bool
}

type pendingQuota struct {
	mu sync.Mutex

	maxRequests int
	maxBytes    int64
	requests    int
	bytes       int64

	requestWaiters list.List
	byteWaiters    list.List
}

func newPendingQuota(maxRequests int, maxBytes int64) *pendingQuota {
	return &pendingQuota{maxRequests: maxRequests, maxBytes: maxBytes}
}

func (q *pendingQuota) acquireRequest(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return &CapacityTimeoutError{Stage: StagePendingRequest}
	}
	waiter := &quotaWaiter{ready: make(chan struct{})}
	q.mu.Lock()
	if q.requests < q.maxRequests && q.requestWaiters.Len() == 0 {
		q.requests++
		q.mu.Unlock()
		return nil
	}
	waiter.element = q.requestWaiters.PushBack(waiter)
	q.mu.Unlock()
	if err := waitUntil(ctx, nil, deadline, waiter.ready, StagePendingRequest); err != nil {
		q.mu.Lock()
		if waiter.granted {
			q.requests--
			q.dispatchLocked()
		} else if waiter.element != nil {
			q.requestWaiters.Remove(waiter.element)
			waiter.element = nil
		}
		q.mu.Unlock()
		return err
	}
	return nil
}

func (q *pendingQuota) reserveBytes(ctx, lifecycle context.Context, deadline time.Time, count int64) error {
	if count > q.maxBytes {
		return ErrPendingBytesTooLarge
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lifecycle.Err(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return &CapacityTimeoutError{Stage: StagePendingBytes}
	}
	waiter := &quotaWaiter{ready: make(chan struct{}), bytes: count}
	q.mu.Lock()
	if count <= q.maxBytes-q.bytes && q.byteWaiters.Len() == 0 {
		q.bytes += count
		q.mu.Unlock()
		return nil
	}
	waiter.element = q.byteWaiters.PushBack(waiter)
	q.mu.Unlock()
	if err := waitUntil(ctx, lifecycle, deadline, waiter.ready, StagePendingBytes); err != nil {
		q.mu.Lock()
		if waiter.granted {
			q.bytes -= count
			q.dispatchLocked()
		} else if waiter.element != nil {
			q.byteWaiters.Remove(waiter.element)
			waiter.element = nil
		}
		q.mu.Unlock()
		return err
	}
	return nil
}

func (q *pendingQuota) releaseBytes(count int64) {
	q.mu.Lock()
	q.bytes -= count
	if q.bytes < 0 {
		q.bytes = 0
	}
	q.dispatchLocked()
	q.mu.Unlock()
}

func (q *pendingQuota) releaseRequest(reservedBytes int64) {
	q.mu.Lock()
	if q.requests > 0 {
		q.requests--
	}
	q.bytes -= reservedBytes
	if q.bytes < 0 {
		q.bytes = 0
	}
	q.dispatchLocked()
	q.mu.Unlock()
}

func (q *pendingQuota) dispatchLocked() {
	for q.requests < q.maxRequests && q.requestWaiters.Len() > 0 {
		element := q.requestWaiters.Front()
		waiter := element.Value.(*quotaWaiter)
		q.requestWaiters.Remove(element)
		waiter.element = nil
		waiter.granted = true
		q.requests++
		close(waiter.ready)
	}
	for q.byteWaiters.Len() > 0 {
		element := q.byteWaiters.Front()
		waiter := element.Value.(*quotaWaiter)
		if waiter.bytes > q.maxBytes-q.bytes {
			break
		}
		q.byteWaiters.Remove(element)
		waiter.element = nil
		waiter.granted = true
		q.bytes += waiter.bytes
		close(waiter.ready)
	}
}

func (q *pendingQuota) snapshot() (requests int, bytes int64, waitingRequests, waitingByteRequests int, waitingBytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for element := q.byteWaiters.Front(); element != nil; element = element.Next() {
		waitingBytes += element.Value.(*quotaWaiter).bytes
	}
	return q.requests, q.bytes, q.requestWaiters.Len(), q.byteWaiters.Len(), waitingBytes
}

type permitKind uint8

const (
	permitGeneral permitKind = iota
	permitReserved
)

type upstreamWaiter struct {
	ready   chan struct{}
	element *list.Element
	lane    Lane
	permit  permitKind
	granted bool
}

type upstreamPool struct {
	mu sync.Mutex

	maxGeneral  int
	maxReserved int
	usedGeneral int
	usedReserve int

	activeGeneralLane     int
	activeInteractiveLane int
	borrowedInteractive   int

	generalWaiters     list.List
	interactiveWaiters list.List
}

func newUpstreamPool(maxGeneral, maxReserved int) *upstreamPool {
	return &upstreamPool{maxGeneral: maxGeneral, maxReserved: maxReserved}
}

func (p *upstreamPool) acquire(ctx, lifecycle context.Context, deadline time.Time, lane Lane) (*Lease, error) {
	started := time.Now()
	if !lane.valid() {
		return nil, ErrInvalidLane
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := lifecycle.Err(); err != nil {
		return nil, err
	}
	if !started.Before(deadline) {
		return nil, &CapacityTimeoutError{Stage: StageUpstream}
	}
	waiter := &upstreamWaiter{ready: make(chan struct{}), lane: lane}
	p.mu.Lock()
	if lane == LaneGeneral {
		waiter.element = p.generalWaiters.PushBack(waiter)
	} else {
		waiter.element = p.interactiveWaiters.PushBack(waiter)
	}
	p.dispatchLocked()
	p.mu.Unlock()

	if err := waitUntil(ctx, lifecycle, deadline, waiter.ready, StageUpstream); err != nil {
		p.mu.Lock()
		if waiter.granted {
			p.releaseGrantLocked(waiter.lane, waiter.permit)
			p.dispatchLocked()
		} else if waiter.element != nil {
			if waiter.lane == LaneGeneral {
				p.generalWaiters.Remove(waiter.element)
			} else {
				p.interactiveWaiters.Remove(waiter.element)
			}
			waiter.element = nil
			p.dispatchLocked()
		}
		p.mu.Unlock()
		return nil, err
	}
	return newLease(StageUpstream, lane, time.Since(started), func() {
		p.release(waiter.lane, waiter.permit)
	}), nil
}

func (p *upstreamPool) release(lane Lane, permit permitKind) {
	p.mu.Lock()
	p.releaseGrantLocked(lane, permit)
	p.dispatchLocked()
	p.mu.Unlock()
}

func (p *upstreamPool) releaseGrantLocked(lane Lane, permit permitKind) {
	if permit == permitReserved {
		if p.usedReserve > 0 {
			p.usedReserve--
		}
	} else {
		if p.usedGeneral > 0 {
			p.usedGeneral--
		}
		if lane == LaneInteractive && p.borrowedInteractive > 0 {
			p.borrowedInteractive--
		}
	}
	if lane == LaneGeneral {
		if p.activeGeneralLane > 0 {
			p.activeGeneralLane--
		}
	} else if p.activeInteractiveLane > 0 {
		p.activeInteractiveLane--
	}
}

func (p *upstreamPool) dispatchLocked() {
	for p.usedReserve < p.maxReserved && p.interactiveWaiters.Len() > 0 {
		p.grantLocked(p.interactiveWaiters.Front(), permitReserved)
	}
	for p.usedGeneral < p.maxGeneral {
		if p.generalWaiters.Len() > 0 {
			p.grantLocked(p.generalWaiters.Front(), permitGeneral)
			continue
		}
		if p.interactiveWaiters.Len() > 0 {
			p.grantLocked(p.interactiveWaiters.Front(), permitGeneral)
			continue
		}
		break
	}
}

func (p *upstreamPool) grantLocked(element *list.Element, permit permitKind) {
	waiter := element.Value.(*upstreamWaiter)
	if waiter.lane == LaneGeneral {
		p.generalWaiters.Remove(element)
		p.activeGeneralLane++
	} else {
		p.interactiveWaiters.Remove(element)
		p.activeInteractiveLane++
	}
	waiter.element = nil
	waiter.permit = permit
	waiter.granted = true
	if permit == permitReserved {
		p.usedReserve++
	} else {
		p.usedGeneral++
		if waiter.lane == LaneInteractive {
			p.borrowedInteractive++
		}
	}
	close(waiter.ready)
}

func (p *upstreamPool) snapshot() (generalActive, interactiveActive, reservedActive, borrowedInteractive, generalWaiting, interactiveWaiting int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeGeneralLane, p.activeInteractiveLane, p.usedReserve, p.borrowedInteractive,
		p.generalWaiters.Len(), p.interactiveWaiters.Len()
}

func waitUntil(ctx, lifecycle context.Context, deadline time.Time, ready <-chan struct{}, stage Stage) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return &CapacityTimeoutError{Stage: stage}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	var lifecycleDone <-chan struct{}
	if lifecycle != nil {
		lifecycleDone = lifecycle.Done()
	}
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-lifecycleDone:
		return lifecycle.Err()
	case <-timer.C:
		return &CapacityTimeoutError{Stage: stage}
	}
}
