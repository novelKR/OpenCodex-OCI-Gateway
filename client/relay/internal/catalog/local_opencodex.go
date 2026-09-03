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
	"strings"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
)

// LocalOpenCodexAvailability is safe to include in the relayctl/MenuBar
// projection. It never contains a host, a raw response, or a transport error.
type LocalOpenCodexAvailability string

const (
	LocalOpenCodexReady       LocalOpenCodexAvailability = "ready"
	LocalOpenCodexUnavailable LocalOpenCodexAvailability = "unavailable"
	LocalOpenCodexForeign     LocalOpenCodexAvailability = "foreign"
	LocalOpenCodexInvalid     LocalOpenCodexAvailability = "invalid"
	LocalOpenCodexUnknown     LocalOpenCodexAvailability = "unknown"
)

var ErrLocalOpenCodexPreflight = errors.New("local OpenCodex preflight failed")

// localOpenCodexMaterializationTimeout bounds the synchronous catalog barrier
// used by a Desktop-safe Local profile apply.  Refresh makes one health and
// one models request, each with its own five-second transport cap, so this
// leaves a small amount of time for the owner-only atomic replacement without
// allowing an Apply to wait indefinitely.
const localOpenCodexMaterializationTimeout = 12 * time.Second

// LocalOpenCodexFetcher is deliberately separate from Fetcher and never
// consults an environment proxy. Native Local is credentialless; the Apple
// profile can obtain one API token only through its connection-bound lease
// and post-dial authorizer. Callers provide the policy-validated numeric
// loopback /v1 base URL.
type LocalOpenCodexFetcher struct {
	BaseURL               string
	CatalogPath           string
	ExpectedServicePort   int
	AuthenticationProfile string
	ConnectionLease       loopbackauth.LeaseAcquirer
	AuthorizeConnection   loopbackauth.Authorizer
	HTTPClient            *http.Client
}

// MaterializeLocalOpenCodexCatalog validates the fixed loopback OpenCodex
// identity and model response, then atomically replaces only the supplied
// Local-profile catalog.  It is the bounded synchronous counterpart to the
// resident Local catalog lifecycle: callers use it at the Desktop restart
// boundary so Codex is never pointed at an absent or unchecked catalog.
//
// This native helper deliberately constructs a no-proxy, no-credential,
// no-redirect client. It accepts only the policy-validated numeric loopback
// endpoint and does not expose any external gateway settings.
func MaterializeLocalOpenCodexCatalog(ctx context.Context, baseURL, catalogPath string) (Result, error) {
	return materializeLocalOpenCodexCatalog(ctx, LocalOpenCodexFetcher{
		BaseURL:     baseURL,
		CatalogPath: catalogPath,
	})
}

func materializeLocalOpenCodexCatalog(ctx context.Context, fetcher LocalOpenCodexFetcher) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, cancel := context.WithTimeout(ctx, localOpenCodexMaterializationTimeout)
	defer cancel()
	return fetcher.Refresh(bounded)
}

func (f LocalOpenCodexFetcher) Preflight(ctx context.Context) (LocalOpenCodexAvailability, error) {
	availability, _, err := f.preflightEntries(ctx)
	return availability, err
}

// preflightEntries keeps the two identity checks and the catalog shape check
// together. Refresh reuses the validated model list rather than issuing a
// second request: local preflight is a read-only capability check, not a
// reason to duplicate an authenticated-native request.
func (f LocalOpenCodexFetcher) preflightEntries(ctx context.Context) (LocalOpenCodexAvailability, []map[string]any, error) {
	endpoint, err := f.endpoint()
	if err != nil {
		return LocalOpenCodexInvalid, nil, fmt.Errorf("%w: endpoint", ErrLocalOpenCodexPreflight)
	}
	client := f.client()
	health := *endpoint
	health.Path = "/healthz"
	health.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		return LocalOpenCodexInvalid, nil, fmt.Errorf("%w: health request", ErrLocalOpenCodexPreflight)
	}
	response, err := client.Do(request)
	if err != nil {
		return LocalOpenCodexUnavailable, nil, fmt.Errorf("%w: health unavailable", ErrLocalOpenCodexPreflight)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LocalOpenCodexForeign, nil, fmt.Errorf("%w: health identity", ErrLocalOpenCodexPreflight)
	}
	var identity struct {
		Service string `json:"service"`
		Status  string `json:"status"`
		Port    int    `json:"port"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&identity); err != nil || identity.Service != "opencodex" || identity.Status != "ok" || identity.Port != f.expectedServicePort() {
		return LocalOpenCodexForeign, nil, fmt.Errorf("%w: health identity", ErrLocalOpenCodexPreflight)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return LocalOpenCodexForeign, nil, fmt.Errorf("%w: health identity", ErrLocalOpenCodexPreflight)
	}
	entries, err := f.fetchEntries(ctx, client, endpoint)
	if err != nil {
		return localAvailabilityFor(err), nil, err
	}
	return LocalOpenCodexReady, entries, nil
}

// Refresh verifies both local identity and models before replacing only the
// selected Local profile's catalog path. The External catalog path is never
// read or written by this method.
func (f LocalOpenCodexFetcher) Refresh(ctx context.Context) (Result, error) {
	if f.CatalogPath == "" {
		return Result{}, fmt.Errorf("%w: catalog path", ErrLocalOpenCodexPreflight)
	}
	availability, entries, err := f.preflightEntries(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s", err, availability)
	}
	filtered, err := filterVisible(entries)
	if err != nil {
		return Result{}, fmt.Errorf("%w: models", ErrLocalOpenCodexPreflight)
	}
	if len(filtered) == 0 {
		return Result{}, fmt.Errorf("%w: models empty", ErrLocalOpenCodexPreflight)
	}
	payload, err := json.MarshalIndent(map[string]any{"models": filtered}, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode local catalog: %w", err)
	}
	payload = append(payload, '\n')
	hash := sha256.Sum256(payload)
	result := Result{Count: len(filtered), Hash: hex.EncodeToString(hash[:])}
	previous, err := os.ReadFile(f.CatalogPath)
	if err == nil && bytes.Equal(previous, payload) {
		return result, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("read local catalog: %w", err)
	}
	if err := atomicWrite(f.CatalogPath, payload); err != nil {
		return Result{}, err
	}
	if err := markPending(f.CatalogPath); err != nil {
		return Result{}, err
	}
	result.Changed = true
	return result, nil
}

func (f LocalOpenCodexFetcher) fetchEntries(ctx context.Context, client http.Client, endpoint *url.URL) ([]map[string]any, error) {
	models := *endpoint
	models.Path = strings.TrimRight(models.Path, "/") + "/models"
	models.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, models.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: models request", ErrLocalOpenCodexPreflight)
	}
	profile := f.authenticationProfile()
	if profile == config.LocalAuthenticationOpenCodexAPIKey {
		base, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("%w: models authentication", ErrLocalOpenCodexPreflight)
		}
		bound, bindErr := loopbackauth.NewTransport(base, f.ConnectionLease, f.AuthorizeConnection)
		if bindErr != nil {
			return nil, fmt.Errorf("%w: models authentication", ErrLocalOpenCodexPreflight)
		}
		client.Transport = bound
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: models unavailable", ErrLocalOpenCodexPreflight)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: models identity", ErrLocalOpenCodexPreflight)
	}
	limited := &io.LimitedReader{R: response.Body, N: 8<<20 + 1}
	decoder := json.NewDecoder(limited)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || limited.N == 0 {
		return nil, fmt.Errorf("%w: models invalid", ErrLocalOpenCodexPreflight)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("%w: models invalid", ErrLocalOpenCodexPreflight)
	}
	entries, err := entriesFromResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: models invalid", ErrLocalOpenCodexPreflight)
	}
	return entries, nil
}

func (f LocalOpenCodexFetcher) endpoint() (*url.URL, error) {
	profile := f.authenticationProfile()
	validBaseURL := profile == config.RemoteAuthenticationNone && config.IsLocalOpenCodexBaseURL(f.BaseURL)
	if profile == config.LocalAuthenticationOpenCodexAPIKey {
		validBaseURL = config.IsLocalAppleContainerBaseURL(f.BaseURL) && f.ConnectionLease != nil && f.AuthorizeConnection != nil
	} else if f.ConnectionLease != nil || f.AuthorizeConnection != nil {
		validBaseURL = false
	}
	if !validBaseURL || f.expectedServicePort() != 10100 {
		return nil, errors.New("invalid local OpenCodex endpoint")
	}
	endpoint, err := url.Parse(f.BaseURL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasSuffix(strings.TrimRight(endpoint.Path, "/"), "/v1") {
		return nil, errors.New("invalid local OpenCodex endpoint")
	}
	return endpoint, nil
}

func (f LocalOpenCodexFetcher) authenticationProfile() string {
	if f.AuthenticationProfile == "" {
		return config.RemoteAuthenticationNone
	}
	return f.AuthenticationProfile
}

func (f LocalOpenCodexFetcher) expectedServicePort() int {
	if f.ExpectedServicePort == 0 {
		return 10100
	}
	return f.ExpectedServicePort
}

func (f LocalOpenCodexFetcher) client() http.Client {
	client := http.Client{Timeout: 5 * time.Second}
	if f.HTTPClient != nil {
		client = *f.HTTPClient
		client.Timeout = 5 * time.Second
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok {
		clone := transport.Clone()
		clone.Proxy = nil
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		clone.MaxIdleConns = 0
		clone.MaxIdleConnsPerHost = 0
		client.Transport = clone
	} else if f.HTTPClient == nil {
		client.Transport = &http.Transport{Proxy: nil, DisableKeepAlives: true}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func localAvailabilityFor(err error) LocalOpenCodexAvailability {
	if err == nil {
		return LocalOpenCodexReady
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "unavailable"):
		return LocalOpenCodexUnavailable
	case strings.Contains(text, "identity"):
		return LocalOpenCodexForeign
	default:
		return LocalOpenCodexInvalid
	}
}
