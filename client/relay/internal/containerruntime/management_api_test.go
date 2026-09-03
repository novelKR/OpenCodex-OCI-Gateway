package containerruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type managementRoundTripFunc func(*http.Request) (*http.Response, error)

func (f managementRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPManagementAPIUsesHardenedFixedLoopbackClient(t *testing.T) {
	api := NewHTTPManagementAPI()
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
	index := 0
	api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if index >= len(requests) {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		expected := requests[index]
		index++
		if request.URL.Scheme != "http" || request.URL.Host != "127.0.0.1:10210" || request.Method != expected.method || request.URL.RequestURI() != expected.path {
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
	})}}

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
	if index != len(requests) {
		t.Fatalf("made %d requests, want %d", index, len(requests))
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
	index := 0
	api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		got := request.Method + " " + request.URL.RequestURI()
		if index >= len(paths) || got != paths[index] {
			t.Fatalf("request %d = %q, want %q", index, got, paths[index])
		}
		response := managementResponse(http.StatusOK, responses[index])
		index++
		return response, nil
	})}}
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
			api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(http.StatusOK, `{"ok":true,"cancelled":false}`), nil
			})}}
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
			api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(test.status, test.body), nil
			})}}
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
			api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return managementResponse(http.StatusOK, `{"status":"`+upstream+`"}`), nil
			})}}
			status, err := api.Status(context.Background(), token, flow)
			if err != nil || status != expected {
				t.Fatalf("status = %q, %v; want %q", status, err, expected)
			}
		})
	}
	api := &HTTPManagementAPI{client: &http.Client{Transport: managementRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return managementResponse(http.StatusOK, `{"status":"idle"}`), nil
	})}}
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

func testOAuthToken(fill byte) []byte {
	return []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)))
}

func equalJSONMaps(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
