package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releaseCheckCacheFile = "release-check-v1.json"

type DiskCheckCache struct {
	directory string
}

func NewDiskCheckCache(directory string) *DiskCheckCache {
	return &DiskCheckCache{directory: directory}
}

func ProductionCheckCache() (*DiskCheckCache, error) {
	root, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(root) {
		return nil, errors.New("user cache directory is unavailable")
	}
	return NewDiskCheckCache(filepath.Join(root, "OpenCodexRelay", "Updates")), nil
}

func (c *DiskCheckCache) Load(_ context.Context, requestURL string) (CheckCacheEntry, bool, error) {
	if err := c.prepareDirectory(); err != nil {
		return CheckCacheEntry{}, false, err
	}
	path := filepath.Join(c.directory, releaseCheckCacheFile)
	metadata, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return CheckCacheEntry{}, false, nil
	}
	if err != nil {
		return CheckCacheEntry{}, false, err
	}
	if !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Mode().Perm()&0o077 != 0 {
		return CheckCacheEntry{}, false, errors.New("release check cache is not an owner-only regular file")
	}
	if metadata.Size() < 2 || metadata.Size() > maximumGitHubResponseBytes+4096 {
		return CheckCacheEntry{}, false, errors.New("release check cache size is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return CheckCacheEntry{}, false, err
	}
	defer file.Close()
	body, oversized, err := readBounded(file, metadata.Size(), maximumGitHubResponseBytes+4096)
	if err != nil || oversized {
		return CheckCacheEntry{}, false, errors.New("release check cache cannot be read safely")
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return CheckCacheEntry{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var entry CheckCacheEntry
	if err := decoder.Decode(&entry); err != nil {
		return CheckCacheEntry{}, false, fmt.Errorf("decode release check cache: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return CheckCacheEntry{}, false, err
	}
	if entry.SchemaVersion != CheckSchemaVersion || entry.RequestURL != requestURL ||
		boundedETag(entry.ETag) == "" || len(entry.Body) == 0 || len(entry.Body) > maximumGitHubResponseBytes {
		return CheckCacheEntry{}, false, errors.New("release check cache schema is invalid")
	}
	return entry, true, nil
}

func (c *DiskCheckCache) Save(_ context.Context, entry CheckCacheEntry) error {
	if err := c.prepareDirectory(); err != nil {
		return err
	}
	if entry.SchemaVersion != CheckSchemaVersion || entry.RequestURL == "" ||
		boundedETag(entry.ETag) == "" || len(entry.Body) == 0 || len(entry.Body) > maximumGitHubResponseBytes {
		return errors.New("release check cache entry is invalid")
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(c.directory, ".release-check.*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		temporary.Close()
		if !complete {
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	destination := filepath.Join(c.directory, releaseCheckCacheFile)
	if existing, err := os.Lstat(destination); err == nil &&
		(!existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0) {
		return errors.New("release check cache destination is unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	complete = true
	return nil
}

func (c *DiskCheckCache) prepareDirectory() error {
	if !filepath.IsAbs(c.directory) {
		return errors.New("release check cache directory must be absolute")
	}
	if err := os.MkdirAll(c.directory, 0o700); err != nil {
		return err
	}
	metadata, err := os.Lstat(c.directory)
	if err != nil {
		return err
	}
	if !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Mode().Perm()&0o077 != 0 {
		return errors.New("release check cache directory is unsafe")
	}
	return nil
}

func ReadPublicKeyFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("release public key path must be absolute")
	}
	metadata, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() < 1 || metadata.Size() > 8192 {
		return nil, errors.New("release public key must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, oversized, err := readBounded(file, metadata.Size(), 8192)
	if err != nil || oversized {
		return nil, errors.New("release public key cannot be read safely")
	}
	if _, err := publicKeyID(data); err != nil {
		return nil, err
	}
	return data, nil
}

func NewProductionChecker(updaterVersion string) (*Checker, error) {
	cache, err := ProductionCheckCache()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if len(previous) >= 4 || !allowedProductionRedirect(request.URL) {
				return errors.New("release request redirect is not allowed")
			}
			return nil
		},
	}
	return NewChecker(CheckerConfig{
		HTTPClient:     client,
		Cache:          cache,
		APIBaseURL:     ProductionAPIBaseURL,
		Repository:     ProductionRepository,
		UpdaterVersion: updaterVersion,
		SystemVersion:  currentSystemVersion(),
		Now:            time.Now,
		Sleep:          sleepWithContext,
	})
}

func allowedProductionRedirect(target *url.URL) bool {
	if target == nil || target.Scheme != "https" || target.User != nil || target.Fragment != "" {
		return false
	}
	switch strings.ToLower(target.Hostname()) {
	case "api.github.com", "github.com", "objects.githubusercontent.com",
		"release-assets.githubusercontent.com", "github-releases.githubusercontent.com":
		return true
	default:
		return false
	}
}

func currentSystemVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil || len(output) > 64 {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(max(0, duration))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
