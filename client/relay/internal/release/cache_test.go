package release

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskCheckCacheRoundTripUsesOwnerOnlyFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "cache")
	cache := NewDiskCheckCache(directory)
	entry := CheckCacheEntry{
		SchemaVersion: CheckSchemaVersion,
		RequestURL:    "https://api.github.com/repos/novelKR/OpenCodex-OCI-Gateway/releases?per_page=100&page=1",
		ETag:          `"etag"`,
		Body:          []byte(`[]`),
	}
	if err := cache.Save(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := cache.Load(context.Background(), entry.RequestURL)
	if err != nil || !found || loaded.ETag != entry.ETag || string(loaded.Body) != string(entry.Body) {
		t.Fatalf("loaded=%#v found=%v err=%v", loaded, found, err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode=%v err=%v", directoryInfo.Mode().Perm(), err)
	}
	fileInfo, err := os.Stat(filepath.Join(directory, releaseCheckCacheFile))
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode=%v err=%v", fileInfo.Mode().Perm(), err)
	}
}

func TestDiskCheckCacheRejectsSymlinkAndUnknownField(t *testing.T) {
	t.Run("symlink destination", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "target")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, releaseCheckCacheFile)); err != nil {
			t.Fatal(err)
		}
		cache := NewDiskCheckCache(directory)
		if _, _, err := cache.Load(context.Background(), "https://example.invalid"); err == nil {
			t.Fatal("symlink cache was accepted")
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		directory := t.TempDir()
		body := []byte(`{"schema_version":1,"request_url":"https://example.invalid","etag":"etag","body":[],"unknown":true}`)
		if err := os.WriteFile(filepath.Join(directory, releaseCheckCacheFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		cache := NewDiskCheckCache(directory)
		if _, _, err := cache.Load(context.Background(), "https://example.invalid"); err == nil {
			t.Fatal("cache with unknown field was accepted")
		}
	})
}

func TestReadPublicKeyFileRequiresAbsoluteRegularFile(t *testing.T) {
	if _, err := ReadPublicKeyFile("relative.pem"); err == nil {
		t.Fatal("relative public key path was accepted")
	}
	directory := t.TempDir()
	if _, err := ReadPublicKeyFile(directory); err == nil {
		t.Fatal("public key directory was accepted")
	}
}
