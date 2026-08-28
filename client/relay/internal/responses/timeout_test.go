package responses

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestDefaultReadTimeoutsMatchRelayPolicy(t *testing.T) {
	timeouts := DefaultReadTimeouts()
	if timeouts.FirstByte != 180*time.Second || timeouts.InterChunk != 30*time.Second || timeouts.Total != 180*time.Second {
		t.Fatalf("default timeouts = %+v", timeouts)
	}
}

func TestTimedReadCloserReportsFirstByteTimeout(t *testing.T) {
	source := newControlledReader()
	reader, err := NewTimedReadCloser(context.Background(), source, ReadTimeouts{
		FirstByte:  20 * time.Millisecond,
		InterChunk: time.Second,
		Total:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, err = reader.Read(make([]byte, 8))
	assertTimeoutPhase(t, err, TimeoutFirstByte)
	assertReaderClosed(t, source.closed)
}

func TestTimedReadCloserReportsInterChunkTimeout(t *testing.T) {
	source := newControlledReader()
	source.send([]byte("first"), nil)
	reader, err := NewTimedReadCloser(context.Background(), source, ReadTimeouts{
		FirstByte:  time.Second,
		InterChunk: 20 * time.Millisecond,
		Total:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if count, err := reader.Read(make([]byte, 8)); count != 5 || err != nil {
		t.Fatalf("first read = %d, %v", count, err)
	}
	_, err = reader.Read(make([]byte, 8))
	assertTimeoutPhase(t, err, TimeoutInterChunk)
}

func TestTimedReadCloserReportsTotalTimeout(t *testing.T) {
	source := newControlledReader()
	source.send([]byte("first"), nil)
	reader, err := NewTimedReadCloser(context.Background(), source, ReadTimeouts{
		FirstByte:  time.Second,
		InterChunk: time.Second,
		Total:      20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Read(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	_, err = reader.Read(make([]byte, 8))
	assertTimeoutPhase(t, err, TimeoutTotal)
}

func TestTimedReadCloserHonorsContextCancellation(t *testing.T) {
	source := newControlledReader()
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := NewTimedReadCloser(ctx, source, ReadTimeouts{
		FirstByte: time.Second, InterChunk: time.Second, Total: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cancel()
	if _, err := reader.Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	assertReaderClosed(t, source.closed)
}

func TestTimedReadCloserDoesNotWaitForABlockedClose(t *testing.T) {
	source := &blockedCloseReader{readStarted: make(chan struct{}), releaseRead: make(chan struct{}), releaseClose: make(chan struct{})}
	reader, err := NewTimedReadCloser(context.Background(), source, ReadTimeouts{
		FirstByte: 20 * time.Millisecond, InterChunk: time.Second, Total: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = reader.Read(make([]byte, 8))
	assertTimeoutPhase(t, err, TimeoutFirstByte)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("timeout waited for blocked Close: %s", elapsed)
	}
	select {
	case <-reader.CleanupDone():
		t.Fatal("cleanup completed while source Read and Close were still blocked")
	default:
	}
	close(source.releaseRead)
	select {
	case <-reader.CleanupDone():
		t.Fatal("cleanup completed before source Close returned")
	default:
	}
	close(source.releaseClose)
	select {
	case <-reader.CleanupDone():
	case <-time.After(time.Second):
		t.Fatal("cleanup did not complete after source Read and Close returned")
	}
}

func assertTimeoutPhase(t *testing.T, err error, phase TimeoutPhase) {
	t.Helper()
	if !errors.Is(err, ErrResponseReadTimeout) {
		t.Fatalf("error = %v, want timeout", err)
	}
	var timeout *ReadTimeoutError
	if !errors.As(err, &timeout) || timeout.Phase != phase {
		t.Fatalf("timeout = %#v, want phase %q", timeout, phase)
	}
}

func assertReaderClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("source was not closed")
	}
}

type controlledRead struct {
	data []byte
	err  error
}

type controlledReader struct {
	reads     chan controlledRead
	closed    chan struct{}
	closeOnce sync.Once
}

func newControlledReader() *controlledReader {
	return &controlledReader{reads: make(chan controlledRead, 4), closed: make(chan struct{})}
}

func (r *controlledReader) send(data []byte, err error) {
	r.reads <- controlledRead{data: data, err: err}
}

func (r *controlledReader) Read(destination []byte) (int, error) {
	select {
	case value := <-r.reads:
		return copy(destination, value.data), value.err
	case <-r.closed:
		return 0, io.ErrClosedPipe
	}
}

func (r *controlledReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

type blockedCloseReader struct {
	readStarted  chan struct{}
	releaseRead  chan struct{}
	releaseClose chan struct{}
	readOnce     sync.Once
}

func (r *blockedCloseReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.readStarted) })
	<-r.releaseRead
	return 0, io.ErrClosedPipe
}

func (r *blockedCloseReader) Close() error {
	<-r.releaseClose
	return nil
}
