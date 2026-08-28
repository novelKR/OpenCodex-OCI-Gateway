package localopencodex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
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
