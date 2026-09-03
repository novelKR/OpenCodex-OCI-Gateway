package containerruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type managementRoundTripFunc func(*http.Request) (*http.Response, error)

func (f managementRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPManagementAPIUsesHardenedFixedLoopbackClient(t *testing.T) {
	api := NewHTTPManagementAPI(func(context.Context) error { return nil })
	if api.client.Timeout != managementRequestTimeout || api.client.CheckRedirect == nil {
		t.Fatal("management client timeout or redirect policy is missing")
	}
	if err := api.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirects must be rejected, got %v", err)
	}
	transport, ok := api.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableKeepAlives || transport.DialContext == nil || transport.MaxIdleConns != 0 || transport.MaxIdleConnsPerHost != 0 {
		t.Fatalf("management transport is not hardened: %#v", api.client.Transport)
	}
	if managementBaseURL != "http://127.0.0.1:10210" {
		t.Fatalf("management origin must remain fixed, got %q", managementBaseURL)
	}
}

func TestHTTPManagementAPIGenericRoutesAndAdminHeader(t *testing.T) {
	token := testOAuthToken(2)
	type expectedRequest struct {
		method string
		path   string
		body   map[string]any
		result string
	}
	requests := []expectedRequest{
		{http.MethodGet, "/api/oauth/providers", nil, `{"providers":["xai","github-copilot"]}`},
		{http.MethodPost, "/api/oauth/login", map[string]any{"provider": "xai"}, `{"url":"https://login.example/authorize","instructions":"Open the browser","deviceCode":"ABCD-EFGH"}`},
		{http.MethodGet, "/api/oauth/status?provider=xai", nil, `{"loggedIn":false,"done":false}`},
		{http.MethodPost, "/api/oauth/login/code", map[string]any{"provider": "xai", "input": "https://localhost/callback?code=opaque"}, `{"ok":true}`},
		{http.MethodPost, "/api/oauth/login/cancel", map[string]any{"provider": "xai"}, `{"ok":true,"cancelled":true}`},
	}
	var index atomic.Int64
	api := newTestManagementAPI(t, managementRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestIndex := int(index.Add(1) - 1)
		if requestIndex >= len(requests) {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		expected := requests[requestIndex]
		if request.Host != "127.0.0.1:10210" || request.Method != expected.method || request.URL.RequestURI() != expected.path {
			t.Fatalf("request escaped fixed contract: %s %s", request.Method, request.URL)
		}
		if request.Header.Get(adminTokenHeader) != string(token) || request.Header.Get("Authorization") != "" {
			t.Fatal("request did not use only the admin token header")
		}
		if expected.body == nil {
			if request.Body != nil && request.Body != http.NoBody {
				data, _ := io.ReadAll(request.Body)
				if len(data) != 0 {
					t.Fatalf("unexpected request body %q", data)
				}
			}
		} else {
			var actual map[string]any
			if err := json.NewDecoder(request.Body).Decode(&actual); err != nil {
				t.Fatal(err)
			}
			if !equalJSONMaps(actual, expected.body) {
				t.Fatalf("body = %#v, want %#v", actual, expected.body)
			}
		}
		return managementResponse(http.StatusOK, expected.result), nil
	}))

	providers, err := api.Providers(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 3 || providers[0].ID != "github-copilot" || providers[1].ID != "xai" || providers[2].ID != "chatgpt" || providers[2].Kind != OAuthKindCodex {
		t.Fatalf("unexpected providers: %#v", providers)
	}
	flow, err := api.Start(context.Background(), token, "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	if flow.AuthorizationURL == "" || flow.UserCode != "ABCD-EFGH" || flow.UpstreamFlowID != "" {
		t.Fatalf("unexpected flow: %#v", flow)
	}
	status, err := api.Status(context.Background(), token, flow)
	if err != nil || status != OAuthStatusPending {
		t.Fatalf("status = %q, %v", status, err)
	}
	if err := api.Submit(context.Background(), token, flow, "https://localhost/callback?code=opaque"); err != nil {
		t.Fatal(err)
	}
	if err := api.Cancel(context.Background(), token, flow); err != nil {
		t.Fatal(err)
	}
	if index.Load() != int64(len(requests)) {
		t.Fatalf("made %d requests, want %d", index.Load(), len(requests))
	}
}

func TestHTTPManagementAPICodexRoutesRemainSeparate(t *testing.T) {
	token := testOAuthToken(3)
	paths := []string{
		"POST /api/codex-auth/login",
		"GET /api/codex-auth/login-status?flowId=flow-opaque",
		"POST /api/codex-auth/login/code",
		"POST /api/codex-auth/login/cancel",
	}
	responses := []string{
		`{"ok":true,"flowId":"flow-opaque","url":"https://auth.openai.example/start","instructions":"Sign in"}`,
		`{"status":"done","accountId":"masked"}`,
		`{"ok":true}`,
		`{"ok":true,"cancelled":true}`,
	}
	var index atomic.Int64
	api := newTestManagementAPI(t, managementRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestIndex := int(index.Add(1) - 1)
		got := request.Method + " " + request.URL.RequestURI()
		if requestIndex >= len(paths) || got != paths[requestIndex] {
			if requestIndex >= len(paths) {
				t.Fatalf("unexpected request %d = %q", requestIndex, got)
			}
			t.Fatalf("request %d = %q, want %q", requestIndex, got, paths[requestIndex])
		}
		response := managementResponse(http.StatusOK, responses[requestIndex])
		return response, nil
	}))
	flow, err := api.Start(context.Background(), token, "chatgpt", OAuthKindCodex)
	if err != nil || flow.UpstreamFlowID != "flow-opaque" {
		t.Fatalf("flow = %#v, %v", flow, err)
	}
	status, err := api.Status(context.Background(), token, flow)
	if err != nil || status != OAuthStatusComplete {
		t.Fatalf("status = %q, %v", status, err)
	}
	if err := api.Submit(context.Background(), token, flow, "manual-code"); err != nil {
		t.Fatal(err)
	}
	if err := api.Cancel(context.Background(), token, flow); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPManagementAPICancelRequiresPositiveUpstreamAcknowledgement(t *testing.T) {
	token := testOAuthToken(9)
	flows := []ManagementFlow{
		{Provider: "xai", Kind: OAuthKindGeneric},
		{Provider: "chatgpt", Kind: OAuthKindCodex, UpstreamFlowID: "flow-opaque"},
	}
	for _, flow := range flows {
		t.Run(string(flow.Kind), func(t *testing.T) {
			api := newTestManagementAPI(t, managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(http.StatusOK, `{"ok":true,"cancelled":false}`), nil
			}))
			if err := api.Cancel(context.Background(), token, flow); !errors.Is(err, errManagementAPI) {
				t.Fatalf("false cancellation acknowledgement error=%v", err)
			}
		})
	}
}

func TestHTTPManagementAPIRejectsUnsafeOrUnboundedResponsesWithoutLeakingBody(t *testing.T) {
	token := testOAuthToken(4)
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"http error", http.StatusConflict, `{"error":"secret-code-literal"}`},
		{"duplicate key", http.StatusOK, `{"providers":["xai"],"providers":["kimi"]}`},
		{"unknown field", http.StatusOK, `{"providers":["xai"],"futureContract":true}`},
		{"trailing data", http.StatusOK, `{"providers":["xai"]}{}`},
		{"javascript authorization URL", http.StatusOK, `{"url":"javascript:alert(1)","instructions":"click"}`},
		{"oversized", http.StatusOK, `{"providers":["` + strings.Repeat("a", maximumManagementBodyBytes) + `"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newTestManagementAPI(t, managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(test.status, test.body), nil
			}))
			var err error
			if test.name == "javascript authorization URL" {
				_, err = api.Start(context.Background(), token, "xai", OAuthKindGeneric)
			} else {
				_, err = api.Providers(context.Background(), token)
			}
			if err == nil {
				t.Fatal("expected rejection")
			}
			if strings.Contains(err.Error(), "secret-code-literal") || len(err.Error()) > 128 {
				t.Fatalf("error leaked response data: %q", err)
			}
		})
	}
}

func TestHTTPManagementAPIStatusMappingFailsClosed(t *testing.T) {
	token := testOAuthToken(5)
	flow := ManagementFlow{Provider: "chatgpt", Kind: OAuthKindCodex, UpstreamFlowID: "flow-1"}
	for upstream, expected := range map[string]OAuthStatus{
		"starting": OAuthStatusPending,
		"pending":  OAuthStatusPending,
		"done":     OAuthStatusComplete,
		"error":    OAuthStatusFailed,
		"expired":  OAuthStatusFailed,
	} {
		t.Run(upstream, func(t *testing.T) {
			api := newTestManagementAPI(t, managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(http.StatusOK, `{"status":"`+upstream+`"}`), nil
			}))
			status, err := api.Status(context.Background(), token, flow)
			if err != nil || status != expected {
				t.Fatalf("status = %q, %v; want %q", status, err, expected)
			}
		})
	}
	api := newTestManagementAPI(t, managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return managementResponse(http.StatusOK, `{"status":"idle"}`), nil
	}))
	if _, err := api.Status(context.Background(), token, flow); err == nil {
		t.Fatal("unknown/idle Codex flow must fail closed")
	}
}

func managementResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newTestManagementAPI(t *testing.T, handler managementRoundTripFunc) *HTTPManagementAPI {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstream, err := handler(request)
		if err != nil || upstream == nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer upstream.Body.Close()
		for name, values := range upstream.Header {
			for _, value := range values {
				response.Header().Add(name, value)
			}
		}
		response.WriteHeader(upstream.StatusCode)
		_, _ = io.Copy(response, upstream.Body)
	}))
	t.Cleanup(server.Close)
	api := NewHTTPManagementAPI(func(context.Context) error { return nil })
	transport := api.client.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", server.Listener.Addr().String())
	}
	api.client.Transport = transport
	return api
}

func testOAuthToken(fill byte) []byte {
	return []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)))
}

func equalJSONMaps(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
