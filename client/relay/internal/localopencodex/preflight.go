// Package localopencodex contains the deliberately bounded identity and
// catalog-shape check used before the optional macOS local profile is chosen.
// It is not a reachability shortcut: a listening TCP port alone never proves
// that the service is OpenCodex or that it can supply a usable model catalog.
package localopencodex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
)

const (
	maxResponseBytes = 8 << 20
	requestTimeout   = 3 * time.Second
)

// Availability is safe to expose through relayctl status. It intentionally
// excludes URLs, response text, and transport errors.
type Availability string

const (
	AvailabilityReady       Availability = "ready"
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityForeign     Availability = "foreign"
	AvailabilityInvalid     Availability = "invalid"
	AvailabilityUnknown     Availability = "unknown"
)

// Result contains only the finite availability result and a count. Model IDs
// remain local to the validation routine and are never reflected in UI status.
type Result struct {
	Availability Availability
	ModelCount   int
}

// Target keeps the host transport endpoint distinct from the service identity
// reported by the guest. In particular, Apple Container publishes host port
// 10210 while the OpenCodex health payload must continue to identify guest
// service port 10100.
type Target struct {
	BaseURL               string
	ExpectedServicePort   int
	AuthenticationProfile string
	ConnectionLease       loopbackauth.LeaseAcquirer
	AuthorizeConnection   loopbackauth.Authorizer
}

// NativeTarget returns the credentialless host-native OpenCodex contract.
func NativeTarget(baseURL string) Target {
	return Target{
		BaseURL:               baseURL,
		ExpectedServicePort:   10100,
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}
}

// AppleContainerTarget returns the fixed authenticated Apple Container
// contract. The API key is used only for /v1/models; /healthz remains
// credentialless.
func AppleContainerTarget(lease loopbackauth.LeaseAcquirer, authorize loopbackauth.Authorizer) Target {
	return Target{
		BaseURL:               "http://127.0.0.1:10210/v1",
		ExpectedServicePort:   10100,
		AuthenticationProfile: config.LocalAuthenticationOpenCodexAPIKey,
		ConnectionLease:       lease,
		AuthorizeConnection:   authorize,
	}
}

func (r Result) Ready() bool { return r.Availability == AvailabilityReady }

// Preflight makes one identity request and one catalog request to the fixed
// numeric loopback endpoint. It never consults environment proxy settings,
// redirects, credentials, or caller-supplied headers. A fresh non-reusable
// transport makes net/http's idle-connection retry path unavailable.
func Preflight(ctx context.Context, upstreamBaseURL string) Result {
	return PreflightTarget(ctx, NativeTarget(upstreamBaseURL))
}

func preflight(ctx context.Context, upstreamBaseURL string, client *http.Client) Result {
	return preflightTarget(ctx, NativeTarget(upstreamBaseURL), client)
}

// PreflightTarget performs the bounded typed check for either the native or
// Apple Container local profile.
func PreflightTarget(ctx context.Context, target Target) Result {
	return preflightTarget(ctx, target, newHTTPClient())
}

func preflightTarget(ctx context.Context, target Target, client *http.Client) Result {
	if !validTarget(target) || client == nil {
		return Result{Availability: AvailabilityInvalid}
	}
	// Even test/injected clients must retain the protocol's no-redirect and
	// bounded-timeout contract. A copy retains a private test transport but
	// prevents a 30x response from turning one local identity check into a
	// second request to an arbitrary origin.
	hardened := *client
	hardened.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if hardened.Timeout <= 0 || hardened.Timeout > requestTimeout {
		hardened.Timeout = requestTimeout
	}
	healthURL, modelsURL, ok := endpoints(target.BaseURL)
	if !ok {
		return Result{Availability: AvailabilityInvalid}
	}
	if result := checkHealth(ctx, &hardened, healthURL, target.ExpectedServicePort); result != AvailabilityReady {
		return Result{Availability: result}
	}
	modelsClient := &hardened
	if target.AuthenticationProfile == config.LocalAuthenticationOpenCodexAPIKey {
		base, ok := hardened.Transport.(*http.Transport)
		if !ok {
			return Result{Availability: AvailabilityInvalid}
		}
		bound, err := loopbackauth.NewTransport(base, target.ConnectionLease, target.AuthorizeConnection)
		if err != nil {
			return Result{Availability: AvailabilityInvalid}
		}
		copy := hardened
		copy.Transport = bound
		modelsClient = &copy
	}
	count, availability := checkModels(ctx, modelsClient, modelsURL)
	return Result{Availability: availability, ModelCount: count}
}

func validTarget(target Target) bool {
	if target.ExpectedServicePort != 10100 {
		return false
	}
	switch target.AuthenticationProfile {
	case config.RemoteAuthenticationNone:
		return config.IsLocalOpenCodexBaseURL(target.BaseURL) && target.ConnectionLease == nil && target.AuthorizeConnection == nil
	case config.LocalAuthenticationOpenCodexAPIKey:
		return config.IsLocalAppleContainerBaseURL(target.BaseURL) && target.ConnectionLease != nil && target.AuthorizeConnection != nil
	default:
		return false
	}
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		MaxConnsPerHost:       1,
		IdleConnTimeout:       0,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func endpoints(upstreamBaseURL string) (healthURL, modelsURL string, ok bool) {
	endpoint, err := url.Parse(upstreamBaseURL)
	if err != nil {
		return "", "", false
	}
	endpoint.Path = "/healthz"
	endpoint.RawQuery = ""
	healthURL = endpoint.String()
	endpoint.Path = "/v1/models"
	modelsURL = endpoint.String()
	return healthURL, modelsURL, true
}

func checkHealth(ctx context.Context, client *http.Client, endpoint string, expectedServicePort int) Availability {
	payload, status, ok := getJSON(ctx, client, endpoint)
	if !ok {
		return status
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		return AvailabilityInvalid
	}
	service, ok := stringField(document, "service")
	if !ok {
		return AvailabilityInvalid
	}
	healthStatus, ok := stringField(document, "status")
	if !ok {
		return AvailabilityInvalid
	}
	port, ok := intField(document, "port")
	if !ok {
		return AvailabilityInvalid
	}
	if service != "opencodex" || healthStatus != "ok" || port != expectedServicePort {
		return AvailabilityForeign
	}
	return AvailabilityReady
}

func checkModels(ctx context.Context, client *http.Client, endpoint string) (int, Availability) {
	payload, status, ok := getJSON(ctx, client, endpoint)
	if !ok {
		return 0, status
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		return 0, AvailabilityInvalid
	}
	entriesPayload, found := document["models"]
	if !found {
		entriesPayload = document["data"]
	}
	if len(entriesPayload) == 0 {
		return 0, AvailabilityInvalid
	}
	var entries []map[string]any
	if err := json.Unmarshal(entriesPayload, &entries); err != nil {
		return 0, AvailabilityInvalid
	}
	seen := make(map[string]struct{}, len(entries))
	visible := 0
	for _, entry := range entries {
		if visibility, _ := entry["visibility"].(string); strings.EqualFold(visibility, "hide") {
			continue
		}
		identifier := modelIdentifier(entry)
		if identifier == "" {
			return 0, AvailabilityInvalid
		}
		if _, exists := seen[identifier]; exists {
			return 0, AvailabilityInvalid
		}
		seen[identifier] = struct{}{}
		visible++
	}
	if visible == 0 {
		return 0, AvailabilityInvalid
	}
	return visible, AvailabilityReady
}

func modelIdentifier(entry map[string]any) string {
	for _, key := range []string{"slug", "id"} {
		if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringField(document map[string]json.RawMessage, key string) (string, bool) {
	payload, found := document[key]
	if !found {
		return "", false
	}
	var value string
	if err := json.Unmarshal(payload, &value); err != nil {
		return "", false
	}
	return value, true
}

func intField(document map[string]json.RawMessage, key string) (int, bool) {
	payload, found := document[key]
	if !found {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(payload, &value); err != nil {
		return 0, false
	}
	return value, true
}

func getJSON(ctx context.Context, client *http.Client, endpoint string) ([]byte, Availability, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, AvailabilityInvalid, false
	}
	request.Close = true
	request.Header.Set("Accept", "application/json")
	request.Header.Del("CF-Access-Client-Id")
	request.Header.Del("CF-Access-Client-Secret")
	request.Header.Del("Cf-Access-Jwt-Assertion")
	request.Header.Del("X-OpenCodex-API-Key")
	request.Header.Del("X-OpenCodex-Relay")
	response, err := client.Do(request)
	if err != nil || response == nil {
		return nil, AvailabilityUnavailable, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
			return nil, AvailabilityInvalid, false
		}
		return nil, AvailabilityUnavailable, false
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return nil, AvailabilityInvalid, false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, AvailabilityInvalid, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, AvailabilityInvalid, false
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, AvailabilityInvalid, false
	}
	return payload, AvailabilityReady, true
}
