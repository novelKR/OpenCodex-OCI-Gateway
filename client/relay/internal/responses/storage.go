package responses

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

type storage struct {
	memory  []byte
	file    *os.File
	size    int64
	spilled bool
}

func readStorage(ctx context.Context, source io.Reader, limit, threshold int64, limitError error) (*storage, error) {
	writer := &storageWriter{limit: limit, threshold: threshold, limitError: limitError}
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			writer.close()
			return nil, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := writer.Write(buffer[:count]); err != nil {
				writer.close()
				return nil, err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			writer.close()
			return nil, readErr
		}
		if count == 0 {
			writer.close()
			return nil, io.ErrNoProgress
		}
	}
	return writer.finish(), nil
}

type storageWriter struct {
	limit      int64
	threshold  int64
	limitError error
	memory     bytes.Buffer
	file       *os.File
	size       int64
	spilled    bool
}

func (w *storageWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.size {
		return 0, fmt.Errorf("%w: maximum %d bytes", w.limitError, w.limit)
	}
	if w.file == nil && w.size+int64(len(data)) > w.threshold {
		if err := w.spill(); err != nil {
			return 0, err
		}
	}
	var (
		count int
		err   error
	)
	if w.file != nil {
		count, err = w.file.Write(data)
	} else {
		count, err = w.memory.Write(data)
	}
	w.size += int64(count)
	return count, err
}

func (w *storageWriter) spill() error {
	file, err := os.CreateTemp("", ".opencodex-relay-responses-")
	if err != nil {
		return fmt.Errorf("create request spool: %w", err)
	}
	name := file.Name()
	fail := func(cause error) error {
		_ = file.Close()
		_ = os.Remove(name)
		return cause
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("protect request spool: %w", err))
	}
	// The relay targets POSIX hosts. Unlink immediately so request content has
	// no directory entry and disappears even if the process exits abruptly.
	if err := os.Remove(name); err != nil {
		return fail(fmt.Errorf("unlink request spool: %w", err))
	}
	if _, err := file.Write(w.memory.Bytes()); err != nil {
		return fail(fmt.Errorf("seed request spool: %w", err))
	}
	w.memory.Reset()
	w.file = file
	w.spilled = true
	return nil
}

func (w *storageWriter) finish() *storage {
	if w.file != nil {
		return &storage{file: w.file, size: w.size, spilled: true}
	}
	return &storage{memory: bytes.Clone(w.memory.Bytes()), size: w.size}
}

func (w *storageWriter) close() {
	if w.file != nil {
		_ = w.file.Close()
	}
}

func (s *storage) reader() (io.Reader, error) {
	if s.file == nil {
		return bytes.NewReader(s.memory), nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return s.file, nil
}

func (s *storage) takeReader() (io.ReadCloser, error) {
	if s.file == nil {
		reader := newMemoryReadCloser(s.memory)
		s.memory = nil
		return reader, nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	file := s.file
	s.file = nil
	return file, nil
}

// memoryReadCloser owns the transferred in-memory spool until the final byte
// is read or Close is called. Dropping the slice before returning from either
// boundary ensures an outer quota/job release cannot make new work admissible
// while the old request bytes are still retained by the response lifecycle.
type memoryReadCloser struct {
	mu        sync.Mutex
	data      []byte
	offset    int
	exhausted bool
	closed    bool
}

func newMemoryReadCloser(data []byte) *memoryReadCloser {
	return &memoryReadCloser{data: data, exhausted: len(data) == 0}
}

func (r *memoryReadCloser) Read(destination []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.exhausted {
		return 0, io.EOF
	}
	if len(destination) == 0 {
		return 0, nil
	}
	count := copy(destination, r.data[r.offset:])
	r.offset += count
	if r.offset == len(r.data) {
		r.releaseBackingLocked()
		r.exhausted = true
	}
	return count, nil
}

func (r *memoryReadCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseBackingLocked()
	r.exhausted = true
	r.closed = true
	return nil
}

func (r *memoryReadCloser) retainedBytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

func (r *memoryReadCloser) releaseBackingLocked() {
	r.data = nil
	r.offset = 0
}

func (s *storage) close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func copyStorageRange(ctx context.Context, destination io.Writer, source *storage, offset, length int64) error {
	reader, err := source.reader()
	if err != nil {
		return err
	}
	if offset > 0 {
		if seeker, ok := reader.(io.Seeker); ok {
			if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
				return err
			}
		} else if _, err := io.CopyN(io.Discard, reader, offset); err != nil {
			return err
		}
	}
	buffer := make([]byte, 32*1024)
	remaining := length
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining
		}
		count, readErr := io.ReadFull(reader, buffer[:readSize])
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
			remaining -= int64(count)
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}
