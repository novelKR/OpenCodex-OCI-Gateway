package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func testLimits() Limits {
	return Limits{
		MaxClassifications:          1,
		MaxPendingRequests:          8,
		MaxPendingBytes:             64,
		QueueTimeout:                500 * time.Millisecond,
		MaxGeneralUpstream:          2,
		InteractiveReservedUpstream: 1,
		MaxConcurrentTransforms:     1,
		MaxOpenDeliveries:           2,
	}
}

func newTestScheduler(t *testing.T, limits Limits) *Scheduler {
	t.Helper()
	scheduler, err := New(limits)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return scheduler
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestDefaultLimits(t *testing.T) {
	want := Limits{
		MaxClassifications:          8,
		MaxPendingRequests:          24,
		MaxPendingBytes:             512 * MiB,
		QueueTimeout:                60 * time.Second,
		MaxGeneralUpstream:          4,
		InteractiveReservedUpstream: 1,
		MaxConcurrentTransforms:     2,
		MaxOpenDeliveries:           16,
	}
	if got := DefaultLimits(); got != want {
		t.Fatalf("DefaultLimits() = %#v, want %#v", got, want)
	}
}

func TestNewRejectsInvalidLimits(t *testing.T) {
	fields := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"classifications", func(l *Limits) { l.MaxClassifications = 0 }},
		{"pending requests", func(l *Limits) { l.MaxPendingRequests = 0 }},
		{"pending bytes", func(l *Limits) { l.MaxPendingBytes = 0 }},
		{"queue timeout", func(l *Limits) { l.QueueTimeout = 0 }},
		{"general upstream", func(l *Limits) { l.MaxGeneralUpstream = 0 }},
		{"reserved upstream", func(l *Limits) { l.InteractiveReservedUpstream = 0 }},
		{"transforms", func(l *Limits) { l.MaxConcurrentTransforms = 0 }},
		{"deliveries", func(l *Limits) { l.MaxOpenDeliveries = 0 }},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			limits := testLimits()
			field.mutate(&limits)
			if _, err := New(limits); !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("New() error = %v, want ErrInvalidLimits", err)
			}
		})
	}
}

func TestClassificationCancellationAndExactOnceRelease(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	first, err := scheduler.AcquireClassification(context.Background())
	if err != nil {
		t.Fatalf("first AcquireClassification() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := scheduler.AcquireClassification(ctx)
		result <- err
	}()
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingClassifications == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued AcquireClassification() error = %v, want context.Canceled", err)
	}
	first.Release()
	first.Release()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.ActiveClassifications == 0 && snapshot.WaitingClassifications == 0
	})
	if got := scheduler.Snapshot().CapacityRejections; got != 0 {
		t.Fatalf("CapacityRejections = %d, want 0 for caller cancellation", got)
	}
}

func TestCapacityTimeoutRecordsStageAndRejection(t *testing.T) {
	limits := testLimits()
	limits.QueueTimeout = 30 * time.Millisecond
	scheduler := newTestScheduler(t, limits)
	first, err := scheduler.AcquireClassification(context.Background())
	if err != nil {
		t.Fatalf("first AcquireClassification() error = %v", err)
	}
	defer first.Release()
	_, err = scheduler.AcquireClassification(context.Background())
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("second AcquireClassification() error = %v, want ErrQueueTimeout", err)
	}
	var timeout *CapacityTimeoutError
	if !errors.As(err, &timeout) || timeout.Stage != StageClassification {
		t.Fatalf("timeout = %#v, want classification stage", timeout)
	}
	if got := scheduler.Snapshot().CapacityRejections; got != 1 {
		t.Fatalf("CapacityRejections = %d, want 1", got)
	}
}

func TestPendingRequestAndIncrementalByteQuota(t *testing.T) {
	limits := testLimits()
	limits.MaxPendingRequests = 2
	limits.MaxPendingBytes = 10
	scheduler := newTestScheduler(t, limits)
	first, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("first AcquirePendingRequest() error = %v", err)
	}
	defer first.Close()
	if err := first.ReserveBytes(context.Background(), 8); err != nil {
		t.Fatalf("first ReserveBytes() error = %v", err)
	}
	second, err := scheduler.AcquirePendingRequest(context.Background(), LaneInteractive)
	if err != nil {
		t.Fatalf("second AcquirePendingRequest() error = %v", err)
	}
	defer second.Close()
	result := make(chan error, 1)
	go func() { result <- second.ReserveBytes(context.Background(), 5) }()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 2 && snapshot.PendingBytes == 8 &&
			snapshot.WaitingPendingByteRequests == 1 && snapshot.WaitingPendingBytes == 5
	})
	if err := first.ReleaseBytes(3); err != nil {
		t.Fatalf("ReleaseBytes() error = %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("queued ReserveBytes() error = %v", err)
	}
	if got := second.ReservedBytes(); got != 5 {
		t.Fatalf("second ReservedBytes() = %d, want 5", got)
	}
	snapshot := scheduler.Snapshot()
	if snapshot.PendingBytes != 10 {
		t.Fatalf("PendingBytes = %d, want 10", snapshot.PendingBytes)
	}
	if err := first.ReleaseBytes(6); !errors.Is(err, ErrInvalidByteCount) {
		t.Fatalf("underflow ReleaseBytes() error = %v, want ErrInvalidByteCount", err)
	}
	if err := second.ReserveBytes(context.Background(), 6); !errors.Is(err, ErrPendingBytesTooLarge) {
		t.Fatalf("oversize ReserveBytes() error = %v, want ErrPendingBytesTooLarge", err)
	}
	if err := second.ReserveBytes(context.Background(), -1); !errors.Is(err, ErrInvalidByteCount) {
		t.Fatalf("negative ReserveBytes() error = %v, want ErrInvalidByteCount", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second first.Close() error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

func TestPendingRequestQuotaTimeoutDoesNotLeak(t *testing.T) {
	limits := testLimits()
	limits.MaxPendingRequests = 1
	limits.QueueTimeout = 30 * time.Millisecond
	scheduler := newTestScheduler(t, limits)
	first, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("first AcquirePendingRequest() error = %v", err)
	}
	defer first.Close()
	_, err = scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("second AcquirePendingRequest() error = %v, want ErrQueueTimeout", err)
	}
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingPendingRequests == 0 })
	if got := scheduler.Snapshot().PendingRequests; got != 1 {
		t.Fatalf("PendingRequests = %d, want 1", got)
	}
}

func TestPendingByteWaitCancellationDoesNotLeak(t *testing.T) {
	limits := testLimits()
	limits.MaxPendingRequests = 2
	limits.MaxPendingBytes = 8
	scheduler := newTestScheduler(t, limits)
	first, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("first AcquirePendingRequest() error = %v", err)
	}
	defer first.Close()
	if err := first.ReserveBytes(context.Background(), 8); err != nil {
		t.Fatalf("first ReserveBytes() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	second, err := scheduler.AcquirePendingRequest(ctx, LaneGeneral)
	if err != nil {
		t.Fatalf("second AcquirePendingRequest() error = %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- second.ReserveBytes(ctx, 1) }()
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingPendingByteRequests == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("ReserveBytes() error = %v, want context.Canceled", err)
	}
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 1 && snapshot.PendingBytes == 8 &&
			snapshot.WaitingPendingByteRequests == 0
	})
	if got := scheduler.Snapshot().CapacityRejections; got != 0 {
		t.Fatalf("CapacityRejections = %d, want 0 for caller cancellation", got)
	}
}

type upstreamResult struct {
	lease *Lease
	err   error
}

func acquireUpstreamAsync(pending *PendingRequest) <-chan upstreamResult {
	result := make(chan upstreamResult, 1)
	go func() {
		lease, err := pending.AcquireUpstream(context.Background())
		result <- upstreamResult{lease: lease, err: err}
	}()
	return result
}

func receiveUpstream(t *testing.T, result <-chan upstreamResult) *Lease {
	t.Helper()
	select {
	case outcome := <-result:
		if outcome.err != nil {
			t.Fatalf("AcquireUpstream() error = %v", outcome.err)
		}
		return outcome.lease
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireUpstream() did not complete")
		return nil
	}
}

func TestInteractiveReservedPermitAndConditionalBorrowing(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	generalLeases := make([]*Lease, 0, 2)
	for range 2 {
		pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
		if err != nil {
			t.Fatalf("AcquirePendingRequest(general) error = %v", err)
		}
		lease, err := pending.AcquireUpstream(context.Background())
		if err != nil {
			t.Fatalf("AcquireUpstream(general) error = %v", err)
		}
		generalLeases = append(generalLeases, lease)
	}

	generalPending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("queued general admission error = %v", err)
	}
	generalResult := acquireUpstreamAsync(generalPending)
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingGeneralUpstream == 1 })

	interactivePending, err := scheduler.AcquirePendingRequest(context.Background(), LaneInteractive)
	if err != nil {
		t.Fatalf("interactive admission error = %v", err)
	}
	interactiveReserved, err := interactivePending.AcquireUpstream(context.Background())
	if err != nil {
		t.Fatalf("interactive reserved AcquireUpstream() error = %v", err)
	}
	if interactiveReserved.Lane() != LaneInteractive {
		t.Fatalf("interactive lease lane = %q, want %q", interactiveReserved.Lane(), LaneInteractive)
	}

	borrowPending, err := scheduler.AcquirePendingRequest(context.Background(), LaneInteractive)
	if err != nil {
		t.Fatalf("borrow admission error = %v", err)
	}
	borrowResult := acquireUpstreamAsync(borrowPending)
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.WaitingGeneralUpstream == 1 && snapshot.WaitingInteractiveUpstream == 1
	})

	generalLeases[0].Release()
	queuedGeneral := receiveUpstream(t, generalResult)
	select {
	case outcome := <-borrowResult:
		if outcome.lease != nil {
			outcome.lease.Release()
		}
		t.Fatalf("interactive borrowed while a general waiter had priority: %v", outcome.err)
	case <-time.After(20 * time.Millisecond):
	}

	generalLeases[1].Release()
	borrowed := receiveUpstream(t, borrowResult)
	snapshot := scheduler.Snapshot()
	if snapshot.ActiveGeneralUpstream != 1 || snapshot.ActiveInteractiveUpstream != 2 ||
		snapshot.ActiveReservedUpstream != 1 || snapshot.ActiveBorrowedInteractive != 1 {
		t.Fatalf("unexpected upstream snapshot: %#v", snapshot)
	}

	queuedGeneral.Release()
	interactiveReserved.Release()
	borrowed.Release()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.ActiveGeneralUpstream == 0 && snapshot.ActiveInteractiveUpstream == 0
	})
}

func TestPendingDeadlineCoversAdmissionThroughUpstreamWait(t *testing.T) {
	limits := testLimits()
	limits.MaxGeneralUpstream = 1
	limits.QueueTimeout = 160 * time.Millisecond
	scheduler := newTestScheduler(t, limits)
	holderPending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("holder admission error = %v", err)
	}
	holder, err := holderPending.AcquireUpstream(context.Background())
	if err != nil {
		t.Fatalf("holder upstream error = %v", err)
	}
	defer holder.Release()

	pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("pending admission error = %v", err)
	}
	if err := pending.ReserveBytes(context.Background(), 4); err != nil {
		t.Fatalf("ReserveBytes() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	_, err = pending.AcquireUpstream(context.Background())
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("AcquireUpstream() error = %v, want ErrQueueTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 120*time.Millisecond {
		t.Fatalf("upstream wait = %v, want shared remaining deadline", elapsed)
	}
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0 && snapshot.WaitingGeneralUpstream == 0
	})
}

func TestUpstreamCancellationReleasesWaiterAndPendingQuota(t *testing.T) {
	limits := testLimits()
	limits.MaxGeneralUpstream = 1
	scheduler := newTestScheduler(t, limits)
	holderPending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("holder admission error = %v", err)
	}
	holder, err := holderPending.AcquireUpstream(context.Background())
	if err != nil {
		t.Fatalf("holder upstream error = %v", err)
	}
	defer holder.Release()

	ctx, cancel := context.WithCancel(context.Background())
	pending, err := scheduler.AcquirePendingRequest(ctx, LaneGeneral)
	if err != nil {
		t.Fatalf("queued admission error = %v", err)
	}
	if err := pending.ReserveBytes(ctx, 8); err != nil {
		t.Fatalf("ReserveBytes() error = %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := pending.AcquireUpstream(ctx)
		result <- err
	}()
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingGeneralUpstream == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireUpstream() error = %v, want context.Canceled", err)
	}
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0 &&
			snapshot.WaitingGeneralUpstream == 0 && snapshot.ActiveGeneralUpstream == 1
	})
	if got := scheduler.Snapshot().CapacityRejections; got != 0 {
		t.Fatalf("CapacityRejections = %d, want 0 for caller cancellation", got)
	}
}

func TestTransformQueueIsFIFO(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	first, err := scheduler.AcquireTransform(context.Background())
	if err != nil {
		t.Fatalf("first AcquireTransform() error = %v", err)
	}
	type result struct {
		id    int
		lease *Lease
		err   error
	}
	results := make(chan result, 2)
	start := func(id int) {
		go func() {
			lease, err := scheduler.AcquireTransform(context.Background())
			results <- result{id: id, lease: lease, err: err}
		}()
	}
	start(1)
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingTransforms == 1 })
	start(2)
	waitFor(t, func() bool { return scheduler.Snapshot().WaitingTransforms == 2 })
	first.Release()
	one := <-results
	if one.err != nil || one.id != 1 {
		t.Fatalf("first queued result = %#v, want id 1", one)
	}
	one.lease.Release()
	two := <-results
	if two.err != nil || two.id != 2 {
		t.Fatalf("second queued result = %#v, want id 2", two)
	}
	two.lease.Release()
}

func TestDeliveryAndPendingReleaseAreConcurrentExactOnce(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	delivery, err := scheduler.AcquireDelivery(context.Background())
	if err != nil {
		t.Fatalf("AcquireDelivery() error = %v", err)
	}
	pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("AcquirePendingRequest() error = %v", err)
	}
	if err := pending.ReserveBytes(context.Background(), 32); err != nil {
		t.Fatalf("ReserveBytes() error = %v", err)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Add(2)
		go func() {
			defer group.Done()
			delivery.Release()
		}()
		go func() {
			defer group.Done()
			_ = pending.Close()
		}()
	}
	group.Wait()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.ActiveDeliveries == 0 && snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

func TestPassthroughJobHandoffRetainsQuotaUntilRelease(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("AcquirePendingRequest() error = %v", err)
	}
	if err := pending.ReserveBytes(context.Background(), 32); err != nil {
		t.Fatalf("ReserveBytes() error = %v", err)
	}
	job, err := pending.HandoffPassthrough()
	if err != nil {
		t.Fatalf("HandoffPassthrough() error = %v", err)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close() after handoff error = %v", err)
	}
	snapshot := scheduler.Snapshot()
	if snapshot.PendingRequests != 1 || snapshot.PendingBytes != 32 {
		t.Fatalf("handoff snapshot = %#v, want one retained request and 32 bytes", snapshot)
	}
	job.Release()
	job.Release()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

func TestAcquireUpstreamJobRetainsRequestButReleasesEncodedBytes(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("AcquirePendingRequest() error = %v", err)
	}
	if err := pending.ReserveBytes(context.Background(), 32); err != nil {
		t.Fatalf("ReserveBytes() error = %v", err)
	}
	upstream, job, err := pending.AcquireUpstreamJob(context.Background())
	if err != nil {
		t.Fatalf("AcquireUpstreamJob() error = %v", err)
	}
	snapshot := scheduler.Snapshot()
	if snapshot.PendingRequests != 1 || snapshot.PendingBytes != 0 || snapshot.ActiveGeneralUpstream != 1 {
		t.Fatalf("dispatched job snapshot = %#v", snapshot)
	}
	upstream.Release()
	if got := scheduler.Snapshot().PendingRequests; got != 1 {
		t.Fatalf("PendingRequests after upstream release = %d, want 1", got)
	}
	job.Release()
	waitFor(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0 && snapshot.ActiveGeneralUpstream == 0
	})
}

func TestInvalidLaneAndClosedPendingRequest(t *testing.T) {
	scheduler := newTestScheduler(t, testLimits())
	if _, err := scheduler.AcquirePendingRequest(context.Background(), Lane("client-header")); !errors.Is(err, ErrInvalidLane) {
		t.Fatalf("AcquirePendingRequest(invalid lane) error = %v, want ErrInvalidLane", err)
	}
	pending, err := scheduler.AcquirePendingRequest(context.Background(), LaneGeneral)
	if err != nil {
		t.Fatalf("AcquirePendingRequest() error = %v", err)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := pending.AcquireUpstream(context.Background()); !errors.Is(err, ErrPendingRequestClosed) {
		t.Fatalf("AcquireUpstream() error = %v, want ErrPendingRequestClosed", err)
	}
	if err := pending.ReserveBytes(context.Background(), 1); !errors.Is(err, ErrPendingRequestClosed) {
		t.Fatalf("ReserveBytes() error = %v, want ErrPendingRequestClosed", err)
	}
}
