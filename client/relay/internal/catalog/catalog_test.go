package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

func TestEntriesFromResponseAcceptsOpenAIDataAndFiltersHidden(t *testing.T) {
	raw := map[string]json.RawMessage{
		"data": json.RawMessage(`[{"id":"visible"},{"id":"hidden","visibility":"hide"}]`),
	}
	entries, err := entriesFromResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := filterVisible(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || modelID(filtered[0]) != "visible" {
		t.Fatalf("filtered entries = %#v", filtered)
	}
}

func TestFilterVisibleKeepsVisibleAccountScopedSpark(t *testing.T) {
	filtered, err := filterVisible([]map[string]any{
		{"slug": "gpt-5.3-codex-spark"},
		{"slug": "hidden", "visibility": "hide"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || modelID(filtered[0]) != "gpt-5.3-codex-spark" {
		t.Fatalf("filtered entries = %#v", filtered)
	}
}

func TestFilterVisibleRejectsDuplicateVisibleIDs(t *testing.T) {
	_, err := filterVisible([]map[string]any{{"slug": "a"}, {"id": "a"}})
	if err == nil {
		t.Fatal("duplicate identifier was accepted")
	}
}

func TestRefreshUsesV1ModelsAndWritesVisibleNormalizedCatalog(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("client_version") != "0.146.1" {
			t.Fatalf("client_version = %q", r.URL.Query().Get("client_version"))
		}
		if r.Header.Get("CF-Access-Client-Id") != "client-id" || r.Header.Get("X-OpenCodex-API-Key") != "gateway" {
			t.Fatal("catalog request did not carry relay admission headers")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"visible"},{"id":"hidden","visibility":"hide"}]}`))
	}))
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	cfg, err := config.NewDefault(server.URL+"/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = catalogPath
	result, err := (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway"}, nil
		},
		Version:    func(context.Context, string) (string, error) { return "0.146.1", nil },
		HTTPClient: server.Client(),
	}).Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Count != 1 || !Pending(catalogPath) {
		t.Fatalf("refresh result = %#v, pending = %t", result, Pending(catalogPath))
	}
	var persisted struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Models) != 1 || persisted.Models[0].ID != "visible" {
		t.Fatalf("persisted catalog = %s", data)
	}
}

func TestRefreshRejectsRedirectWithoutForwardingAdmissionCredentials(t *testing.T) {
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	var targetReached atomic.Bool
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetReached.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	target.Listener = targetListener
	target.Start()
	defer target.Close()

	sourceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != "client-id" ||
			r.Header.Get("CF-Access-Client-Secret") != "client-secret" ||
			r.Header.Get("X-OpenCodex-API-Key") != "gateway" {
			t.Error("source request did not carry admission credentials")
		}
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	source.Listener = sourceListener
	source.Start()
	defer source.Close()

	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	cfg, err := config.NewDefault(source.URL+"/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = catalogPath
	_, err = (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway"}, nil
		},
		Version:    func(context.Context, string) (string, error) { return "0.146.1", nil },
		HTTPClient: source.Client(),
	}).Refresh(context.Background())
	if err == nil {
		t.Fatal("authenticated catalog redirect was accepted")
	}
	if targetReached.Load() {
		t.Fatal("redirect target received the authenticated catalog request")
	}
	if _, statErr := os.Stat(catalogPath); !os.IsNotExist(statErr) {
		t.Fatalf("redirect response created catalog: %v", statErr)
	}
}

func TestProbeUsesOneAuthenticatedModelsRequestWithoutCatalogWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	var requests atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.URL.RawQuery != "" {
			t.Errorf("probe request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("CF-Access-Client-Id") != "client-id" ||
			r.Header.Get("CF-Access-Client-Secret") != "client-secret" ||
			r.Header.Get("X-OpenCodex-API-Key") != "gateway" {
			t.Error("probe request did not carry relay admission headers")
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.Listener = listener
	server.StartTLS()
	defer server.Close()

	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalogPath, []byte("existing catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewDefault(server.URL+"/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = catalogPath
	err = (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway"}, nil
		},
		HTTPClient: server.Client(),
	}).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("probe requests = %d, want exactly one", requests.Load())
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing catalog\n" || Pending(catalogPath) {
		t.Fatalf("probe modified catalog state: catalog=%q pending=%t", data, Pending(catalogPath))
	}
}

func TestProbeRejectsRedirectWithoutFollowingAdmissionHeaders(t *testing.T) {
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	var targetReached atomic.Bool
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetReached.Store(true)
	}))
	target.Listener = targetListener
	target.Start()
	defer target.Close()

	sourceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OpenCodex-API-Key") != "gateway" {
			t.Error("source did not receive admission header")
		}
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	source.Listener = sourceListener
	source.StartTLS()
	defer source.Close()

	cfg, err := config.NewDefault(source.URL+"/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	err = (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway"}, nil
		},
		HTTPClient: source.Client(),
	}).Probe(context.Background())
	if err == nil {
		t.Fatal("authenticated probe redirect was accepted")
	}
	if targetReached.Load() {
		t.Fatal("redirect target received authenticated probe")
	}
}

func TestProbeRejectsNonExternalTopologyBeforeCredentialLookup(t *testing.T) {
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamMode = config.UpstreamModeLocalOpenCodex
	cfg.UpstreamBaseURL = "http://127.0.0.1:10100/v1"
	cfg.Credentials.Source = config.CredentialsSourceNone
	cfg.Catalog.Owner = config.CatalogOwnerRemoteManager
	var loads atomic.Int64
	err = (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			loads.Add(1)
			return credentials.Values{}, nil
		},
	}).Probe(context.Background())
	if err == nil {
		t.Fatal("local topology was accepted for an external gateway probe")
	}
	if loads.Load() != 0 {
		t.Fatalf("non-external probe loaded credentials %d time(s)", loads.Load())
	}
}

func TestValidateUsesOneAuthenticatedVersionedRequestWithoutChangingCatalog(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte("existing catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = catalogPath
	requests := 0
	result, err := (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway"}, nil
		},
		Version: func(context.Context, string) (string, error) { return "0.146.1", nil },
		HTTPClient: &http.Client{Transport: localRoundTrip(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Method != http.MethodGet || request.URL.Path != "/v1/models" || request.URL.Query().Get("client_version") != "0.146.1" {
				t.Fatalf("validation request = %s %s", request.Method, request.URL.String())
			}
			if request.Header.Get("CF-Access-Client-Id") != "client-id" ||
				request.Header.Get("CF-Access-Client-Secret") != "client-secret" ||
				request.Header.Get("X-OpenCodex-API-Key") != "gateway" {
				t.Fatal("validation request omitted admission credentials")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"data":[{"id":"gpt-test"}]}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}).Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.Count != 1 || result.Changed || result.Hash == "" {
		t.Fatalf("validation result=%#v requests=%d", result, requests)
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil || string(data) != "existing catalog\n" || Pending(catalogPath) {
		t.Fatalf("validation changed catalog state: %q pending=%t err=%v", data, Pending(catalogPath), err)
	}
}

func TestValidateRejectsRedirectWithoutFollowingAdmissionHeaders(t *testing.T) {
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	_, err = (Fetcher{
		Config: cfg,
		Credentials: func() (credentials.Values, error) {
			return credentials.Values{CFClientID: "id", CFClientSecret: "secret", GatewayKey: "key"}, nil
		},
		Version: func(context.Context, string) (string, error) { return "0.146.1", nil },
		HTTPClient: &http.Client{Transport: localRoundTrip(func(*http.Request) (*http.Response, error) {
			requests++
			if requests > 1 {
				t.Fatal("validation followed an authenticated redirect")
			}
			return &http.Response{
				StatusCode: http.StatusFound,
				Body:       io.NopCloser(strings.NewReader("")),
				Header: http.Header{
					"Location": []string{"https://redirect.example.test/stolen"},
				},
			}, nil
		})},
	}).Validate(context.Background())
	if !errors.Is(err, ErrValidationCatalog) {
		t.Fatalf("Validate error = %v, want %v", err, ErrValidationCatalog)
	}
	if requests != 1 {
		t.Fatalf("validation requests = %d, want exactly one", requests)
	}
}

func TestValidateClassifiesAuthenticationTransportAndCatalogFailures(t *testing.T) {
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	validCredentials := func() (credentials.Values, error) {
		return credentials.Values{CFClientID: "id", CFClientSecret: "secret", GatewayKey: "key"}, nil
	}
	tests := []struct {
		name        string
		credentials func() (credentials.Values, error)
		response    func(*http.Request) (*http.Response, error)
		want        error
	}{
		{
			name:        "missing credentials",
			credentials: func() (credentials.Values, error) { return credentials.Values{}, errors.New("unavailable") },
			want:        ErrValidationAuthentication,
		},
		{
			name:        "forbidden",
			credentials: validCredentials,
			response:    validationResponse(http.StatusForbidden, `{}`),
			want:        ErrValidationAuthentication,
		},
		{
			name:        "transport",
			credentials: validCredentials,
			response:    func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") },
			want:        ErrValidationUnreachable,
		},
		{
			name:        "unexpected catalog status",
			credentials: validCredentials,
			response:    validationResponse(http.StatusBadGateway, `{}`),
			want:        ErrValidationCatalog,
		},
		{
			name:        "malformed catalog",
			credentials: validCredentials,
			response:    validationResponse(http.StatusOK, `{"data":`),
			want:        ErrValidationCatalog,
		},
		{
			name:        "oversized catalog",
			credentials: validCredentials,
			response:    validationResponse(http.StatusOK, `{"data":[{"id":"model","padding":"`+strings.Repeat("x", (8<<20)+1)+`"}]}`),
			want:        ErrValidationCatalog,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{}
			if test.response != nil {
				client.Transport = localRoundTrip(test.response)
			}
			_, err := (Fetcher{
				Config:      cfg,
				Credentials: test.credentials,
				Version:     func(context.Context, string) (string, error) { return "0.146.1", nil },
				HTTPClient:  client,
			}).Validate(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
		})
	}
}

func validationResponse(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
}
