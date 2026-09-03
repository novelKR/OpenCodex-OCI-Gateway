// Package catalog materializes the OpenCodex model response as the startup
// catalog that native Codex CLI, AppServer, and Desktop consume.
package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

type Result struct {
	Changed bool
	Count   int
	Hash    string
}

var (
	ErrValidationAuthentication = errors.New("gateway authentication failed")
	ErrValidationUnreachable    = errors.New("gateway is unreachable")
	ErrValidationCatalog        = errors.New("gateway catalog is invalid")
)

type Fetcher struct {
	Config      config.Config
	Credentials func() (credentials.Values, error)
	HTTPClient  *http.Client
	Version     func(context.Context, string) (string, error)
}

// Probe performs one authenticated, read-only reachability observation of the
// external gateway. Unlike Refresh it never reads or writes the materialized
// catalog, restart marker, or AppServer state. Callers own the schedule and
// must keep it disabled unless a local UI has explicitly opted in.
//
// The client deliberately disables connection reuse and redirect following so
// this helper does not turn one scheduled observation into a replay of device
// admission headers against a different origin.
func (f Fetcher) Probe(ctx context.Context) error {
	if f.Credentials == nil {
		return errors.New("catalog credential loader is required")
	}
	if f.Config.UpstreamMode != config.UpstreamModeExternalGateway {
		return errors.New("connection probe requires external_gateway")
	}
	values, err := f.Credentials()
	if err != nil {
		return err
	}
	profile := f.Config.Credentials.RemoteAuthenticationProfile()
	if err := values.ValidateForProfile(profile); err != nil {
		return err
	}
	endpoint, err := url.Parse(f.Config.UpstreamBaseURL)
	if err != nil {
		return err
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/models"
	endpoint.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	applyCredentialHeaders(request.Header, profile, values)
	client := f.probeHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("probe gateway models: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("probe gateway models: upstream returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (f Fetcher) probeHTTPClient() http.Client {
	client := http.Client{Timeout: 5 * time.Second}
	if f.HTTPClient != nil {
		client = *f.HTTPClient
		// A caller-supplied test client can carry a private test CA. Preserve
		// that transport while making the planned five second cap strict.
		client.Timeout = 5 * time.Second
	}
	// net/http may transparently retry an idempotent request that was written
	// on an idle reused connection. A diagnostic probe must make exactly one
	// attempt with admission headers, so give it a fresh non-reusable transport.
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok {
		clone := transport.Clone()
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		clone.MaxIdleConns = 0
		clone.MaxIdleConnsPerHost = 0
		client.Transport = clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// Validate performs the same authenticated Codex catalog request and parser
// used by Refresh without reading or writing any catalog artifact. It is a
// single-attempt, five-second UI preflight: admission credentials are never
// replayed through redirects or an idle reused connection.
func (f Fetcher) Validate(ctx context.Context) (Result, error) {
	if f.Credentials == nil {
		return Result{}, ErrValidationAuthentication
	}
	if f.Config.UpstreamMode != config.UpstreamModeExternalGateway {
		return Result{}, ErrValidationCatalog
	}
	version := f.Version
	if version == nil {
		version = CodexVersion
	}
	clientVersion, err := version(ctx, f.Config.Catalog.CodexExecutable)
	if err != nil {
		return Result{}, fmt.Errorf("%w: client version", ErrValidationCatalog)
	}
	values, err := f.Credentials()
	profile := f.Config.Credentials.RemoteAuthenticationProfile()
	if err != nil || values.ValidateForProfile(profile) != nil {
		return Result{}, ErrValidationAuthentication
	}
	request, err := catalogRequest(ctx, f.Config.UpstreamBaseURL, clientVersion, profile, values)
	if err != nil {
		return Result{}, fmt.Errorf("%w: request", ErrValidationCatalog)
	}
	client := f.probeHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return Result{}, ErrValidationUnreachable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return Result{}, ErrValidationAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, ErrValidationCatalog
	}
	_, count, hashValue, err := decodeCatalog(response.Body)
	if err != nil {
		return Result{}, fmt.Errorf("%w: response", ErrValidationCatalog)
	}
	return Result{Count: count, Hash: hashValue}, nil
}

func (f Fetcher) Refresh(ctx context.Context) (Result, error) {
	if f.Credentials == nil {
		return Result{}, errors.New("catalog credential loader is required")
	}
	version := f.Version
	if version == nil {
		version = CodexVersion
	}
	client := http.Client{Timeout: 60 * time.Second}
	if f.HTTPClient != nil {
		client = *f.HTTPClient
	}
	// The request carries device admission credentials that must never leave
	// the configured upstream origin. Catalog endpoints are canonical, so any
	// redirect is an error rather than a reason to replay those headers.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	clientVersion, err := version(ctx, f.Config.Catalog.CodexExecutable)
	if err != nil {
		return Result{}, err
	}
	values, err := f.Credentials()
	if err != nil {
		return Result{}, err
	}
	profile := f.Config.Credentials.RemoteAuthenticationProfile()
	if err := values.ValidateForProfile(profile); err != nil {
		return Result{}, err
	}
	request, err := catalogRequest(ctx, f.Config.UpstreamBaseURL, clientVersion, profile, values)
	if err != nil {
		return Result{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("fetch model catalog: upstream returned HTTP %d", response.StatusCode)
	}
	payload, count, hashValue, err := decodeCatalog(response.Body)
	if err != nil {
		return Result{}, err
	}
	result := Result{Count: count, Hash: hashValue}
	previous, err := os.ReadFile(f.Config.Catalog.Path)
	if err == nil && bytes.Equal(previous, payload) {
		return result, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("read current catalog: %w", err)
	}
	if err := atomicWrite(f.Config.Catalog.Path, payload); err != nil {
		return Result{}, err
	}
	if err := markPending(f.Config.Catalog.Path); err != nil {
		return Result{}, err
	}
	result.Changed = true
	return result, nil
}

func catalogRequest(ctx context.Context, upstreamBaseURL, clientVersion, profile string, values credentials.Values) (*http.Request, error) {
	endpoint, err := url.Parse(upstreamBaseURL)
	if err != nil {
		return nil, err
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/models"
	query := endpoint.Query()
	query.Set("client_version", clientVersion)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	applyCredentialHeaders(request.Header, profile, values)
	return request, nil
}

func applyCredentialHeaders(header http.Header, profile string, values credentials.Values) {
	header.Del("CF-Access-Client-Id")
	header.Del("CF-Access-Client-Secret")
	header.Del("Cf-Access-Jwt-Assertion")
	header.Del("X-OpenCodex-API-Key")
	header.Del("X-OpenCodex-Relay")
	if profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		header.Set("CF-Access-Client-Id", values.CFClientID)
		header.Set("CF-Access-Client-Secret", values.CFClientSecret)
	}
	if profile == config.RemoteAuthenticationGatewayAPIKey || profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		header.Set("X-OpenCodex-API-Key", values.GatewayKey)
	}
	if profile == config.LocalAuthenticationOpenCodexAPIKey {
		header.Set("X-OpenCodex-API-Key", values.LocalOpenCodexAPIKey)
	}
}

func decodeCatalog(reader io.Reader) ([]byte, int, string, error) {
	limited := &io.LimitedReader{R: reader, N: 8<<20 + 1}
	decoder := json.NewDecoder(limited)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, 0, "", fmt.Errorf("decode model catalog: %w", err)
	}
	if limited.N == 0 {
		return nil, 0, "", errors.New("model catalog exceeds 8 MiB")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, 0, "", errors.New("model catalog contains multiple JSON values")
		}
		return nil, 0, "", fmt.Errorf("decode trailing model catalog data: %w", err)
	}
	entries, err := entriesFromResponse(raw)
	if err != nil {
		return nil, 0, "", err
	}
	filtered, err := filterVisible(entries)
	if err != nil {
		return nil, 0, "", err
	}
	payload, err := json.MarshalIndent(map[string]any{"models": filtered}, "", "  ")
	if err != nil {
		return nil, 0, "", err
	}
	payload = append(payload, '\n')
	hash := sha256.Sum256(payload)
	return payload, len(filtered), hex.EncodeToString(hash[:]), nil
}

func CodexVersion(ctx context.Context, executable string) (string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read Codex version: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", errors.New("Codex version output is empty")
	}
	version := strings.TrimPrefix(fields[len(fields)-1], "v")
	if !semverLike(version) {
		return "", errors.New("Codex version is not an explicit semver")
	}
	return version, nil
}

func PendingPath(catalogPath string) string { return catalogPath + config.CatalogRestartPendingSuffix }

func ClearPending(catalogPath string) error {
	if err := os.Remove(PendingPath(catalogPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear catalog restart marker: %w", err)
	}
	return nil
}

func Pending(catalogPath string) bool {
	info, err := os.Lstat(PendingPath(catalogPath))
	return err == nil && info.Mode().IsRegular()
}

func entriesFromResponse(raw map[string]json.RawMessage) ([]map[string]any, error) {
	encoded, ok := raw["models"]
	if !ok {
		encoded = raw["data"]
	}
	if len(encoded) == 0 {
		return nil, errors.New("model response contains neither models nor data")
	}
	var values []map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, errors.New("model response entries must be an array of objects")
	}
	return values, nil
}

func filterVisible(entries []map[string]any) ([]map[string]any, error) {
	seen := make(map[string]struct{}, len(entries))
	filtered := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if visibility, _ := entry["visibility"].(string); strings.EqualFold(visibility, "hide") {
			continue
		}
		id := modelID(entry)
		if id == "" {
			return nil, errors.New("model catalog entry is missing slug or id")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("model catalog contains duplicate identifier %q", id)
		}
		seen[id] = struct{}{}
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		return nil, errors.New("model catalog has no visible models")
	}
	return filtered, nil
}

func modelID(entry map[string]any) string {
	for _, key := range []string{"slug", "id"} {
		if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func atomicWrite(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create catalog directory: %w", err)
	}
	if previous, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+config.CatalogPreviousSuffix, previous, 0o600); err != nil {
			return fmt.Errorf("backup previous catalog: %w", err)
		}
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".catalog.")
	if err != nil {
		return fmt.Errorf("create temporary catalog: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary catalog: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return fmt.Errorf("write catalog: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close catalog: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace catalog: %w", err)
	}
	return nil
}

func markPending(catalogPath string) error {
	return os.WriteFile(PendingPath(catalogPath), []byte("pending\n"), 0o600)
}

func semverLike(value string) bool {
	parts := strings.SplitN(value, "-", 2)
	base := strings.Split(parts[0], ".")
	if len(base) != 3 {
		return false
	}
	for _, part := range base {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
