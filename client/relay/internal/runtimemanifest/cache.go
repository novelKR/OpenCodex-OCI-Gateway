package runtimemanifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const runtimeCheckCacheFile = "runtime-release-check-v1.json"

type DiskCheckCache struct{ directory string }

func NewDiskCheckCache(directory string) *DiskCheckCache {
	return &DiskCheckCache{directory: directory}
}

func ProductionCheckCache() (*DiskCheckCache, error) {
	root, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(root) {
		return nil, errors.New("runtime release cache root is unavailable")
	}
	return NewDiskCheckCache(filepath.Join(root, "OpenCodexRelay", "RuntimeUpdates")), nil
}

func (c *DiskCheckCache) Load(_ context.Context, requestURL string) (CheckCacheEntry, bool, error) {
	if err := c.prepare(); err != nil {
		return CheckCacheEntry{}, false, err
	}
	path := filepath.Join(c.directory, runtimeCheckCacheFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return CheckCacheEntry{}, false, nil
	}
	if err != nil || !safeRuntimeCacheFile(info) || info.Size() < 2 || info.Size() > maximumGitHubResponseBytes+4096 {
		return CheckCacheEntry{}, false, errors.New("runtime release cache is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return CheckCacheEntry{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumGitHubResponseBytes+4097))
	if err != nil || len(data) > maximumGitHubResponseBytes+4096 || rejectDuplicateJSONKeys(data) != nil {
		return CheckCacheEntry{}, false, errors.New("runtime release cache is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var entry CheckCacheEntry
	if err := decoder.Decode(&entry); err != nil || requireEOF(decoder) != nil || entry.SchemaVersion != CheckSchemaVersion || entry.RequestURL != requestURL || boundedETag(entry.ETag) == "" || len(entry.Body) == 0 || len(entry.Body) > maximumGitHubResponseBytes {
		return CheckCacheEntry{}, false, errors.New("runtime release cache schema is invalid")
	}
	return entry, true, nil
}

func (c *DiskCheckCache) Save(_ context.Context, entry CheckCacheEntry) error {
	if err := c.prepare(); err != nil {
		return err
	}
	if entry.SchemaVersion != CheckSchemaVersion || entry.RequestURL == "" || boundedETag(entry.ETag) == "" || len(entry.Body) == 0 || len(entry.Body) > maximumGitHubResponseBytes {
		return errors.New("runtime release cache entry is invalid")
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(c.directory, ".runtime-release-cache.*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	destination := filepath.Join(c.directory, runtimeCheckCacheFile)
	if info, err := os.Lstat(destination); err == nil && !safeRuntimeCacheFile(info) {
		return errors.New("runtime release cache destination is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	complete = true
	return nil
}

func (c *DiskCheckCache) prepare() error {
	if !filepath.IsAbs(c.directory) {
		return errors.New("runtime release cache directory must be absolute")
	}
	if err := os.MkdirAll(c.directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(c.directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !runtimeCacheOwnedByCurrentUser(info) {
		return errors.New("runtime release cache directory is unsafe")
	}
	return nil
}

func safeRuntimeCacheFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm()&0o077 == 0 && runtimeCacheOwnedByCurrentUser(info)
}

func runtimeCacheOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
