package catalog

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

type localRoundTrip func(*http.Request) (*http.Response, error)

func (f localRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestLocalOpenCodexRefreshUsesVerifiedLoopbackWithoutCredentialsOrProxy(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "local-catalog.json")
	var healthCalls, modelsCalls atomic.Int64
	fetcher := LocalOpenCodexFetcher{
		BaseURL:     "http://127.0.0.1:10100/v1",
		CatalogPath: catalogPath,
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "" || request.Header.Get("CF-Access-Client-Id") != "" || request.Header.Get("CF-Access-Client-Secret") != "" || request.Header.Get("X-OpenCodex-API-Key") != "" {
				t.Fatalf("local catalog request sent credentials: %#v", request.Header)
			}
			switch request.URL.Path {
			case "/healthz":
				healthCalls.Add(1)
				return localJSON(http.StatusOK, `{"service":"opencodex","status":"ok","port":10100}`), nil
			case "/v1/models":
				modelsCalls.Add(1)
				return localJSON(http.StatusOK, `{"data":[{"id":"local-visible"},{"id":"hidden","visibility":"hide"}]}`), nil
			default:
				t.Fatalf("local request path = %q", request.URL.Path)
				return nil, nil
			}
		})},
	}
	result, err := fetcher.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Count != 1 || healthCalls.Load() != 1 || modelsCalls.Load() != 1 || !Pending(catalogPath) {
		t.Fatalf("result=%#v health=%d models=%d pending=%t", result, healthCalls.Load(), modelsCalls.Load(), Pending(catalogPath))
	}
	payload, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "hidden") || !strings.Contains(string(payload), "local-visible") {
		t.Fatalf("local catalog payload = %s", payload)
	}
}

func TestAppleContainerCatalogUsesFixedAPIAuthenticationAndGuestIdentityPort(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "apple-catalog.json")
	apiToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	loads := 0
	fetcher := LocalOpenCodexFetcher{
		BaseURL:               "http://127.0.0.1:10210/v1",
		CatalogPath:           catalogPath,
		ExpectedServicePort:   10100,
		AuthenticationProfile: config.LocalAuthenticationOpenCodexAPIKey,
		Credentials: func() (credentials.Values, error) {
			loads++
			return credentials.Values{LocalOpenCodexAPIKey: apiToken}, nil
		},
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "127.0.0.1:10210" || request.Header.Get("CF-Access-Client-Id") != "" || request.Header.Get("X-OpenCodex-Relay") != "" {
				t.Fatalf("unsafe Apple catalog request: url=%s headers=%#v", request.URL, request.Header)
			}
			switch request.URL.Path {
			case "/healthz":
				if request.Header.Get("X-OpenCodex-API-Key") != "" {
					t.Fatalf("Apple health request sent API key: %#v", request.Header)
				}
				return localJSON(http.StatusOK, `{"service":"opencodex","status":"ok","port":10100}`), nil
			case "/v1/models":
				if request.Header.Get("X-OpenCodex-API-Key") != apiToken {
					t.Fatalf("Apple models key = %q", request.Header.Get("X-OpenCodex-API-Key"))
				}
				return localJSON(http.StatusOK, `{"data":[{"id":"apple-visible"}]}`), nil
			default:
				t.Fatalf("Apple catalog path = %q", request.URL.Path)
				return nil, nil
			}
		})},
	}
	result, err := fetcher.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Count != 1 || loads != 1 || !Pending(catalogPath) {
		t.Fatalf("Apple catalog result=%#v loads=%d pending=%t", result, loads, Pending(catalogPath))
	}
}

func TestMaterializeLocalOpenCodexCatalogUsesBoundedNoCredentialFetcher(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "local-catalog.json")
	var calls atomic.Int64
	var requestDeadline time.Time
	fetcher := LocalOpenCodexFetcher{
		BaseURL:     "http://127.0.0.1:10100/v1",
		CatalogPath: catalogPath,
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			deadline, ok := request.Context().Deadline()
			if !ok {
				t.Fatal("synchronous materialization request had no deadline")
			}
			requestDeadline = deadline
			if request.Header.Get("Authorization") != "" || request.Header.Get("CF-Access-Client-Id") != "" || request.Header.Get("CF-Access-Client-Secret") != "" || request.Header.Get("X-OpenCodex-API-Key") != "" {
				t.Fatalf("materialization request sent credentials: %#v", request.Header)
			}
			switch request.URL.Path {
			case "/healthz":
				return localJSON(http.StatusOK, `{"service":"opencodex","status":"ok","port":10100}`), nil
			case "/v1/models":
				return localJSON(http.StatusOK, `{"data":[{"id":"local-visible"}]}`), nil
			default:
				t.Fatalf("materialization request path = %q", request.URL.Path)
				return nil, nil
			}
		})},
	}
	result, err := materializeLocalOpenCodexCatalog(context.Background(), fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Count != 1 || calls.Load() != 2 || !Pending(catalogPath) {
		t.Fatalf("result=%#v calls=%d pending=%t", result, calls.Load(), Pending(catalogPath))
	}
	if remaining := time.Until(requestDeadline); remaining <= 0 || remaining > localOpenCodexMaterializationTimeout {
		t.Fatalf("materialization deadline remaining = %s, want (0, %s]", remaining, localOpenCodexMaterializationTimeout)
	}
}

func TestLocalOpenCodexPreflightRejectsForeignIdentityRedirectAndInvalidEndpoint(t *testing.T) {
	foreign := LocalOpenCodexFetcher{
		BaseURL: "http://127.0.0.1:10100/v1",
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			return localJSON(http.StatusOK, `{"service":"other","status":"ok","port":10100}`), nil
		})},
	}
	availability, err := foreign.Preflight(context.Background())
	if err == nil || availability != LocalOpenCodexForeign {
		t.Fatalf("foreign preflight availability=%s err=%v", availability, err)
	}

	var calls atomic.Int64
	redirect := LocalOpenCodexFetcher{
		BaseURL: "http://127.0.0.1:10100/v1",
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := localJSON(http.StatusFound, "")
			response.Header.Set("Location", "http://127.0.0.1:10100/other")
			return response, nil
		})},
	}
	availability, err = redirect.Preflight(context.Background())
	if err == nil || availability != LocalOpenCodexForeign || calls.Load() != 1 {
		t.Fatalf("redirect preflight availability=%s err=%v calls=%d", availability, err, calls.Load())
	}

	invalid := LocalOpenCodexFetcher{BaseURL: "http://localhost:10100/v1"}
	availability, err = invalid.Preflight(context.Background())
	if err == nil || availability != LocalOpenCodexInvalid {
		t.Fatalf("invalid endpoint availability=%s err=%v", availability, err)
	}
}

func localJSON(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
