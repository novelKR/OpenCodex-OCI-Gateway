package responses

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var ErrResponseReadTimeout = errors.New("Responses upstream read timeout")

type TimeoutPhase string

const (
	TimeoutFirstByte  TimeoutPhase = "first_byte"
	TimeoutInterChunk TimeoutPhase = "inter_chunk"
	TimeoutTotal      TimeoutPhase = "total"
)

type ReadTimeouts struct {
	FirstByte  time.Duration
	InterChunk time.Duration
	Total      time.Duration
}

func DefaultReadTimeouts() ReadTimeouts {
	return ReadTimeouts{
		FirstByte:  180 * time.Second,
		InterChunk: 30 * time.Second,
		Total:      180 * time.Second,
	}
}

func (t ReadTimeouts) validate() error {
	if t.FirstByte <= 0 || t.InterChunk <= 0 || t.Total <= 0 {
		return fmt.Errorf("%w: read timeouts must be positive", ErrInvalidLimits)
	}
	return nil
}

type ReadTimeoutError struct {
	Phase TimeoutPhase
}

func (e *ReadTimeoutError) Error() string {
	return fmt.Sprintf("Responses upstream %s timeout", e.Phase)
}

func (e *ReadTimeoutError) Unwrap() error { return ErrResponseReadTimeout }

// TimedReadCloser exposes cleanup completion separately from Close. Close is
// intentionally non-blocking for the client-facing error path; CleanupDone is
// closed only after the source Close and every in-flight source Read return.
type TimedReadCloser interface {
	io.ReadCloser
	CleanupDone() <-chan struct{}
}

type timedReadCloser struct {
	ctx           context.Context
	source        io.ReadCloser
	timeouts      ReadTimeouts
	started       time.Time
	lastChunk     time.Time
	sawBytes      bool
	closed        chan struct{}
	cleanupDone   chan struct{}
	closeOnce     sync.Once
	readMu        sync.Mutex
	stateMu       sync.Mutex
	activeReads   int
	sourceClosed  bool
	cleanupClosed bool
}

// NewTimedReadCloser adds bounded body-read timing to one already-issued
// upstream response. It owns no transport and adds no request replay. Timeout
// or cancellation closes source so an in-flight network read can unwind.
func NewTimedReadCloser(ctx context.Context, source io.ReadCloser, timeouts ReadTimeouts) (TimedReadCloser, error) {
	if source == nil {
		return nil, fmt.Errorf("nil Responses upstream body")
	}
	if err := timeouts.validate(); err != nil {
		return nil, err
	}
	now := time.Now()
	return &timedReadCloser{
		ctx:         ctx,
		source:      source,
		timeouts:    timeouts,
		started:     now,
		lastChunk:   now,
		closed:      make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}, nil
}

type readResult struct {
	data []byte
	err  error
}

func (r *timedReadCloser) Read(destination []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if len(destination) == 0 {
		return 0, nil
	}
	select {
	case <-r.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	phase, deadline := r.deadline()
	if !time.Now().Before(deadline) {
		_ = r.Close()
		return 0, &ReadTimeoutError{Phase: phase}
	}
	result := make(chan readResult, 1)
	buffer := make([]byte, len(destination))
	r.beginSourceRead()
	go func() {
		defer r.finishSourceRead()
		count, err := r.source.Read(buffer)
		result <- readResult{data: buffer[:count], err: err}
	}()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case outcome := <-result:
		count := copy(destination, outcome.data)
		if count > 0 {
			r.sawBytes = true
			r.lastChunk = time.Now()
		}
		return count, outcome.err
	case <-r.ctx.Done():
		_ = r.Close()
		return 0, r.ctx.Err()
	case <-timer.C:
		_ = r.Close()
		return 0, &ReadTimeoutError{Phase: phase}
	}
}

func (r *timedReadCloser) deadline() (TimeoutPhase, time.Time) {
	phase := TimeoutFirstByte
	deadline := r.started.Add(r.timeouts.FirstByte)
	if r.sawBytes {
		phase = TimeoutInterChunk
		deadline = r.lastChunk.Add(r.timeouts.InterChunk)
	}
	total := r.started.Add(r.timeouts.Total)
	if total.Before(deadline) {
		return TimeoutTotal, total
	}
	return phase, deadline
}

func (r *timedReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		// A hostile or broken upstream body can itself block forever in Close.
		// Cancellation must still return to the Native Codex client, so observe
		// source cleanup asynchronously and never wait before emitting 502/504.
		go func() {
			_ = r.source.Close()
			r.finishSourceClose()
		}()
	})
	return nil
}

func (r *timedReadCloser) CleanupDone() <-chan struct{} { return r.cleanupDone }

func (r *timedReadCloser) beginSourceRead() {
	r.stateMu.Lock()
	r.activeReads++
	r.stateMu.Unlock()
}

func (r *timedReadCloser) finishSourceRead() {
	r.stateMu.Lock()
	if r.activeReads > 0 {
		r.activeReads--
	}
	r.closeCleanupDoneLocked()
	r.stateMu.Unlock()
}

func (r *timedReadCloser) finishSourceClose() {
	r.stateMu.Lock()
	r.sourceClosed = true
	r.closeCleanupDoneLocked()
	r.stateMu.Unlock()
}

func (r *timedReadCloser) closeCleanupDoneLocked() {
	if r.sourceClosed && r.activeReads == 0 && !r.cleanupClosed {
		r.cleanupClosed = true
		close(r.cleanupDone)
	}
}
