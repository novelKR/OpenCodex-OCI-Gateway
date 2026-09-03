package localopencodex

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestPreflightAcceptsOpenCodexIdentityAndVisibleModels(t *testing.T) {
	var requests []*http.Request
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Clone(request.Context()))
		switch request.URL.Path {
		case "/healthz":
			return jsonResponse(http.StatusOK, `{"service":"opencodex","status":"ok","port":10100}`), nil
		case "/v1/models":
			return jsonResponse(http.StatusOK, `{"data":[{"id":"gpt-local"},{"id":"hidden","visibility":"hide"}]}`), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}
	result := preflight(context.Background(), "http://127.0.0.1:10100/v1", client)
	if !result.Ready() || result.ModelCount != 1 {
		t.Fatalf("preflight result = %#v", result)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.Header.Get("Authorization") != "" || request.Header.Get("CF-Access-Client-Id") != "" || request.Header.Get("X-OpenCodex-API-Key") != "" {
			t.Fatalf("local preflight sent admission credentials: %#v", request.Header)
		}
		if request.Header.Get("Accept") != "application/json" || !request.Close {
			t.Fatalf("local preflight request is not bounded/no-reuse: %#v close=%t", request.Header, request.Close)
		}
	}
}

func TestAppleContainerPreflightSeparatesHostAndGuestPortsAndUsesOnlyAPIKeyForModels(t *testing.T) {
	apiToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Host != "127.0.0.1:10210" {
			t.Fatalf("Apple preflight host = %q", request.Host)
		}
		if request.Header.Get("CF-Access-Client-Id") != "" || request.Header.Get("CF-Access-Client-Secret") != "" || request.Header.Get("X-OpenCodex-Relay") != "" {
			t.Fatalf("Apple preflight leaked unrelated admission headers: %#v", request.Header)
		}
		switch request.URL.Path {
		case "/healthz":
			if request.Header.Get("X-OpenCodex-API-Key") != "" {
				t.Fatalf("health request was authenticated: %#v", request.Header)
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(response, `{"service":"opencodex","status":"ok","port":10100}`)
		case "/v1/models":
			if request.Header.Get("X-OpenCodex-API-Key") != apiToken {
				t.Fatalf("models API key = %q", request.Header.Get("X-OpenCodex-API-Key"))
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(response, `{"data":[{"id":"apple-local"}]}`)
		default:
			t.Fatalf("unexpected Apple preflight path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", server.Listener.Addr().String())
	}}
	client := &http.Client{Transport: transport}
	var leaseCalls, authorizeCalls atomic.Int64
	target := AppleContainerTarget(
		func(context.Context) (func() error, error) {
			leaseCalls.Add(1)
			return func() error { return nil }, nil
		},
		func(context.Context) (loopbackauth.Authorization, error) {
			authorizeCalls.Add(1)
			return loopbackauth.Authorization{Token: []byte(apiToken)}, nil
		},
	)
	result := preflightTarget(context.Background(), target, client)
	if !result.Ready() || result.ModelCount != 1 || calls.Load() != 2 {
		t.Fatalf("Apple preflight = %#v calls=%d", result, calls.Load())
	}
	if leaseCalls.Load() != 1 || authorizeCalls.Load() != 1 {
		t.Fatalf("Apple binding lease=%d authorize=%d", leaseCalls.Load(), authorizeCalls.Load())
	}

	invalid := AppleContainerTarget(nil, target.AuthorizeConnection)
	if result := preflightTarget(context.Background(), invalid, client); result.Availability != AvailabilityInvalid || calls.Load() != 2 {
		t.Fatalf("invalid Apple token preflight = %#v calls=%d", result, calls.Load())
	}
}

func TestPreflightClassifiesUnavailableForeignAndInvalid(t *testing.T) {
	tests := []struct {
		name   string
		health string
		models string
		code   int
		want   Availability
	}{
		{name: "unavailable health", code: http.StatusServiceUnavailable, want: AvailabilityUnavailable},
		{name: "foreign identity", health: `{"service":"something-else","status":"ok","port":10100}`, code: http.StatusOK, want: AvailabilityForeign},
		{name: "foreign status", health: `{"service":"opencodex","status":"wrong","port":10100}`, code: http.StatusOK, want: AvailabilityForeign},
		{name: "foreign port", health: `{"service":"opencodex","status":"ok","port":10101}`, code: http.StatusOK, want: AvailabilityForeign},
		{name: "invalid health", health: `{"service":false}`, code: http.StatusOK, want: AvailabilityInvalid},
		{name: "invalid catalog", health: `{"service":"opencodex","status":"ok","port":10100}`, models: `{"data":{"id":"not-an-array"}}`, code: http.StatusOK, want: AvailabilityInvalid},
		{name: "duplicate catalog", health: `{"service":"opencodex","status":"ok","port":10100}`, models: `{"data":[{"id":"same"},{"slug":"same"}]}`, code: http.StatusOK, want: AvailabilityInvalid},
		{name: "no visible catalog", health: `{"service":"opencodex","status":"ok","port":10100}`, models: `{"data":[{"id":"hidden","visibility":"hide"}]}`, code: http.StatusOK, want: AvailabilityInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/healthz" {
					return jsonResponse(test.code, test.health), nil
				}
				return jsonResponse(test.code, test.models), nil
			})}
			result := preflight(context.Background(), "http://127.0.0.1:10100/v1", client)
			if result.Availability != test.want {
				t.Fatalf("availability = %q, want %q", result.Availability, test.want)
			}
		})
	}
}

func TestPreflightRejectsRedirectAndOversizedCatalogWithoutASecondOrigin(t *testing.T) {
	redirects := 0
	redirectClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		redirects++
		response := jsonResponse(http.StatusFound, ``)
		response.Header.Set("Location", "http://127.0.0.1:10100/other")
		return response, nil
	})}
	if result := preflight(context.Background(), "http://127.0.0.1:10100/v1", redirectClient); result.Availability != AvailabilityInvalid {
		t.Fatalf("redirect availability = %q", result.Availability)
	}
	if redirects != 1 {
		t.Fatalf("redirect request count = %d, want 1", redirects)
	}

	oversized := bytes.Repeat([]byte("x"), maxResponseBytes+1)
	oversizedClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/healthz" {
			return jsonResponse(http.StatusOK, `{"service":"opencodex","status":"ok","port":10100}`), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(oversized))}, nil
	})}
	if result := preflight(context.Background(), "http://127.0.0.1:10100/v1", oversizedClient); result.Availability != AvailabilityInvalid {
		t.Fatalf("oversized catalog availability = %q", result.Availability)
	}
}

func TestPreflightRejectsNonCanonicalEndpointAndDefaultClientDisablesProxyReuseAndRedirects(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:10100/v1",
		"http://127.0.0.1:10101/v1",
		"https://127.0.0.1:10100/v1",
	} {
		if result := Preflight(context.Background(), endpoint); result.Availability != AvailabilityInvalid {
			t.Errorf("endpoint %q availability = %q, want invalid", endpoint, result.Availability)
		}
	}
	if !config.IsLocalOpenCodexBaseURL("http://[::1]:10100/v1") {
		t.Fatal("canonical IPv6 endpoint was rejected")
	}
	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableKeepAlives || transport.MaxIdleConns != 0 || transport.MaxIdleConnsPerHost != 0 {
		t.Fatalf("unsafe local preflight transport: %#v", transport)
	}
	if client.CheckRedirect == nil {
		t.Fatal("local preflight follows redirects")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy error = %v", err)
	}
	if client.Timeout != requestTimeout {
		t.Fatalf("unexpected local preflight timeout %s", client.Timeout)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
