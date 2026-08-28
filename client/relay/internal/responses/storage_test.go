package responses

import (
	"bytes"
	"io"
	"os"
	"testing"
)

type retainedMemoryInspector interface {
	retainedBytes() int
}

func TestMemoryStorageReaderReleasesBackingAtEOFAndClose(t *testing.T) {
	t.Run("EOF", func(t *testing.T) {
		payload := bytes.Repeat([]byte("m"), 4096)
		stored := &storage{memory: bytes.Clone(payload), size: int64(len(payload))}
		reader, err := stored.takeReader()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		inspector, ok := reader.(retainedMemoryInspector)
		if !ok {
			t.Fatalf("memory reader %T does not expose backing ownership", reader)
		}
		if got := inspector.retainedBytes(); got != len(payload) {
			t.Fatalf("initial retained bytes = %d, want %d", got, len(payload))
		}
		read, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read, payload) {
			t.Fatal("memory reader changed payload bytes")
		}
		if got := inspector.retainedBytes(); got != 0 {
			t.Fatalf("retained bytes after EOF = %d, want 0", got)
		}
	})

	t.Run("Close", func(t *testing.T) {
		payload := bytes.Repeat([]byte("c"), 4096)
		stored := &storage{memory: bytes.Clone(payload), size: int64(len(payload))}
		reader, err := stored.takeReader()
		if err != nil {
			t.Fatal(err)
		}
		inspector, ok := reader.(retainedMemoryInspector)
		if !ok {
			t.Fatalf("memory reader %T does not expose backing ownership", reader)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if got := inspector.retainedBytes(); got != 0 {
			t.Fatalf("retained bytes after Close = %d, want 0", got)
		}
	})
}

func TestFileStorageTakeReaderPreservesFileSpool(t *testing.T) {
	writer := &storageWriter{limit: 1024, threshold: 1, limitError: ErrEncodedBodyTooLarge}
	payload := []byte("file-backed payload")
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	stored := writer.finish()
	if !stored.spilled {
		t.Fatal("fixture did not spill to an anonymous file")
	}
	reader, err := stored.takeReader()
	if err != nil {
		t.Fatal(err)
	}
	file, ok := reader.(*os.File)
	if !ok {
		t.Fatalf("file-backed reader = %T, want *os.File", reader)
	}
	read, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, payload) {
		t.Fatalf("file-backed payload = %q", read)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
