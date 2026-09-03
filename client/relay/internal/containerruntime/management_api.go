package containerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
)

const (
	managementBaseURL          = "http://127.0.0.1:10210"
	managementRequestTimeout   = 10 * time.Second
	maximumManagementBodyBytes = 64 << 10
	maximumAuthorizationURL    = 4096
	maximumInstructions        = 4096
	maximumUserCode            = 256
	maximumFlowID              = 256
	adminTokenHeader           = "X-OpenCodex-API-Key"
)

var errManagementAPI = errors.New("OpenCodex management API request failed")

// HTTPManagementAPI is the version-pinned adapter over the management routes
// already exposed by OpenCodex. It never follows redirects, consults a proxy,
// or reuses a connection. The origin cannot be supplied by configuration.
type HTTPManagementAPI struct {
	client    *http.Client
	peerGuard func(context.Context) error
}

// These projections enumerate the complete v2.40.0 response surface for the
// routes consumed by the runtime adapter. Fields the Relay does not need stay
// as RawMessage so their values are neither interpreted nor coupled to UI
// behavior, while DisallowUnknownFields still makes an upstream contract drift
// fail closed until the pinned adapter is reviewed.
type genericOAuthStatusResponse struct {
	LoggedIn        *bool           `json:"loggedIn"`
	Email           json.RawMessage `json:"email"`
	Source          json.RawMessage `json:"source"`
	Error           string          `json:"error"`
	Done            *bool           `json:"done"`
	ActiveAccountID json.RawMessage `json:"activeAccountId"`
	Accounts        json.RawMessage `json:"accounts"`
}

type codexOAuthStatusResponse struct {
	Status                string          `json:"status"`
	StartedAt             json.RawMessage `json:"startedAt"`
	AccountID             json.RawMessage `json:"accountId"`
	Email                 json.RawMessage `json:"email"`
	Error                 json.RawMessage `json:"error"`
	Code                  json.RawMessage `json:"code"`
	NeedsReauth           json.RawMessage `json:"needsReauth"`
	CatalogRefreshPending json.RawMessage `json:"catalogRefreshPending"`
	DoneAt                json.RawMessage `json:"doneAt"`
}

var _ ManagementAPI = (*HTTPManagementAPI)(nil)

func NewHTTPManagementAPI(peerGuard func(context.Context) error) *HTTPManagementAPI {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		IdleConnTimeout:       0,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &HTTPManagementAPI{peerGuard: peerGuard, client: &http.Client{
		Timeout:   managementRequestTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (a *HTTPManagementAPI) Providers(ctx context.Context, adminToken []byte) ([]OAuthProvider, error) {
	var response struct {
		Providers []string `json:"providers"`
	}
	if err := a.request(ctx, adminToken, http.MethodGet, "/api/oauth/providers", nil, &response); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(response.Providers)+1)
	providers := make([]OAuthProvider, 0, len(response.Providers)+1)
	for _, raw := range response.Providers {
		provider := strings.TrimSpace(raw)
		if !validOAuthProvider(provider) || provider == "chatgpt" {
			return nil, errManagementAPI
		}
		if _, exists := seen[provider]; exists {
			return nil, errManagementAPI
		}
		seen[provider] = struct{}{}
		providers = append(providers, OAuthProvider{
			ID:   provider,
			Name: provider,
			Kind: OAuthKindGeneric,
		})
	}
	sort.Slice(providers, func(left, right int) bool { return providers[left].ID < providers[right].ID })
	providers = append(providers, OAuthProvider{
		ID:   "chatgpt",
		Name: "ChatGPT Codex",
		Kind: OAuthKindCodex,
	})
	return providers, nil
}

func (a *HTTPManagementAPI) Start(ctx context.Context, adminToken []byte, provider string, kind OAuthKind) (ManagementFlow, error) {
	if !validOAuthProvider(provider) || !validOAuthKind(kind) || kind == OAuthKindCodex && provider != "chatgpt" || kind == OAuthKindGeneric && provider == "chatgpt" {
		return ManagementFlow{}, ErrInvalidRequest
	}
	if kind == OAuthKindCodex {
		var response struct {
			OK           bool   `json:"ok"`
			FlowID       string `json:"flowId"`
			URL          string `json:"url"`
			Instructions string `json:"instructions"`
		}
		if err := a.request(ctx, adminToken, http.MethodPost, "/api/codex-auth/login", struct{}{}, &response); err != nil {
			return ManagementFlow{}, err
		}
		flow := ManagementFlow{
			Provider:         provider,
			Kind:             kind,
			UpstreamFlowID:   response.FlowID,
			AuthorizationURL: response.URL,
			Instructions:     response.Instructions,
		}
		if !response.OK || !validFlowID(flow.UpstreamFlowID) || validateFlowPresentation(flow) != nil {
			return ManagementFlow{}, errManagementAPI
		}
		return flow, nil
	}

	var response struct {
		URL          string `json:"url"`
		Instructions string `json:"instructions"`
		DeviceCode   string `json:"deviceCode"`
	}
	if err := a.request(ctx, adminToken, http.MethodPost, "/api/oauth/login", struct {
		Provider string `json:"provider"`
	}{Provider: provider}, &response); err != nil {
		return ManagementFlow{}, err
	}
	flow := ManagementFlow{
		Provider:         provider,
		Kind:             kind,
		AuthorizationURL: response.URL,
		Instructions:     response.Instructions,
		UserCode:         response.DeviceCode,
	}
	if validateFlowPresentation(flow) != nil {
		return ManagementFlow{}, errManagementAPI
	}
	return flow, nil
}

func (a *HTTPManagementAPI) Status(ctx context.Context, adminToken []byte, flow ManagementFlow) (OAuthStatus, error) {
	if validateManagementFlowIdentity(flow) != nil {
		return "", ErrInvalidRequest
	}
	if flow.Kind == OAuthKindCodex {
		var response codexOAuthStatusResponse
		path := "/api/codex-auth/login-status?flowId=" + url.QueryEscape(flow.UpstreamFlowID)
		if err := a.request(ctx, adminToken, http.MethodGet, path, nil, &response); err != nil {
			return "", err
		}
		switch response.Status {
		case "starting", "pending":
			return OAuthStatusPending, nil
		case "done":
			return OAuthStatusComplete, nil
		case "error", "expired":
			return OAuthStatusFailed, nil
		default:
			return "", errManagementAPI
		}
	}

	var response genericOAuthStatusResponse
	path := "/api/oauth/status?provider=" + url.QueryEscape(flow.Provider)
	if err := a.request(ctx, adminToken, http.MethodGet, path, nil, &response); err != nil {
		return "", err
	}
	if response.LoggedIn == nil || response.Done == nil || len(response.Error) > maximumInstructions || hasUnsafeText(response.Error) {
		return "", errManagementAPI
	}
	if *response.Done {
		if *response.LoggedIn {
			return OAuthStatusComplete, nil
		}
		return OAuthStatusFailed, nil
	}
	if response.Error != "" {
		return OAuthStatusFailed, nil
	}
	return OAuthStatusPending, nil
}

func (a *HTTPManagementAPI) Submit(ctx context.Context, adminToken []byte, flow ManagementFlow, input string) error {
	if validateManagementFlowIdentity(flow) != nil || input == "" || len(input) > MaximumOAuthInputBytes || hasUnsafeText(input) {
		return ErrInvalidRequest
	}
	var response struct {
		OK bool `json:"ok"`
	}
	var path string
	var body any
	if flow.Kind == OAuthKindCodex {
		path = "/api/codex-auth/login/code"
		body = struct {
			FlowID string `json:"flowId"`
			Input  string `json:"input"`
		}{FlowID: flow.UpstreamFlowID, Input: input}
	} else {
		path = "/api/oauth/login/code"
		body = struct {
			Provider string `json:"provider"`
			Input    string `json:"input"`
		}{Provider: flow.Provider, Input: input}
	}
	if err := a.request(ctx, adminToken, http.MethodPost, path, body, &response); err != nil {
		return err
	}
	if !response.OK {
		return errManagementAPI
	}
	return nil
}

func (a *HTTPManagementAPI) Cancel(ctx context.Context, adminToken []byte, flow ManagementFlow) error {
	if validateManagementFlowIdentity(flow) != nil {
		return ErrInvalidRequest
	}
	var response struct {
		OK        bool  `json:"ok"`
		Cancelled *bool `json:"cancelled"`
	}
	var path string
	var body any
	if flow.Kind == OAuthKindCodex {
		path = "/api/codex-auth/login/cancel"
		body = struct {
			FlowID string `json:"flowId"`
		}{FlowID: flow.UpstreamFlowID}
	} else {
		path = "/api/oauth/login/cancel"
		body = struct {
			Provider string `json:"provider"`
		}{Provider: flow.Provider}
	}
	if err := a.request(ctx, adminToken, http.MethodPost, path, body, &response); err != nil {
		return err
	}
	if !response.OK || response.Cancelled == nil || !*response.Cancelled {
		return errManagementAPI
	}
	return nil
}

func (a *HTTPManagementAPI) request(ctx context.Context, adminToken []byte, method, path string, body, destination any) error {
	if a == nil || a.client == nil || a.peerGuard == nil || !validSecret(adminToken) || (method != http.MethodGet && method != http.MethodPost) || !strings.HasPrefix(path, "/api/") {
		return ErrInvalidRequest
	}
	base, err := url.Parse(managementBaseURL)
	if err != nil {
		return errManagementAPI
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.User != nil || relative.Fragment != "" || !strings.HasPrefix(relative.Path, "/api/") {
		return ErrInvalidRequest
	}
	endpoint := base.ResolveReference(relative)
	if endpoint.Scheme != "http" || endpoint.Host != "127.0.0.1:10210" || endpoint.Path != relative.Path || !strings.HasPrefix(endpoint.Path, "/api/") {
		return ErrInvalidRequest
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil || len(payload) > MaximumOAuthInputBytes+1024 {
			zeroBytes(payload)
			return ErrInvalidRequest
		}
	}
	defer zeroBytes(payload)
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return errManagementAPI
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	baseTransport, ok := a.client.Transport.(*http.Transport)
	if !ok {
		return errManagementAPI
	}
	bound, err := loopbackauth.NewTransport(
		baseTransport,
		func(context.Context) (func() error, error) { return func() error { return nil }, nil },
		func(authorizeCtx context.Context) (loopbackauth.Authorization, error) {
			if err := a.peerGuard(authorizeCtx); err != nil {
				return loopbackauth.Authorization{}, err
			}
			return loopbackauth.Authorization{Token: append([]byte(nil), adminToken...)}, nil
		},
	)
	if err != nil {
		return errManagementAPI
	}
	client := *a.client
	client.Transport = bound
	response, err := client.Do(request)
	if err != nil {
		return errManagementAPI
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumManagementBodyBytes+1))
		return errManagementAPI
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		return errManagementAPI
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumManagementBodyBytes+1))
	if err != nil || len(data) == 0 || len(data) > maximumManagementBodyBytes {
		return errManagementAPI
	}
	defer zeroBytes(data)
	if err := decodeManagementProjection(data, destination); err != nil {
		return errManagementAPI
	}
	return nil
}

func decodeManagementProjection(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maximumManagementBodyBytes || destination == nil {
		return errManagementAPI
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return errManagementAPI
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errManagementAPI
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errManagementAPI
	}
	return nil
}

func validateManagementFlowIdentity(flow ManagementFlow) error {
	if !validOAuthProvider(flow.Provider) || !validOAuthKind(flow.Kind) || flow.Kind == OAuthKindCodex && (flow.Provider != "chatgpt" || !validFlowID(flow.UpstreamFlowID)) || flow.Kind == OAuthKindGeneric && (flow.Provider == "chatgpt" || flow.UpstreamFlowID != "") {
		return ErrInvalidRequest
	}
	return nil
}

func validateFlowPresentation(flow ManagementFlow) error {
	if flow.AuthorizationURL == "" && flow.Instructions == "" && flow.UserCode == "" {
		return errManagementAPI
	}
	if len(flow.AuthorizationURL) > maximumAuthorizationURL || len(flow.Instructions) > maximumInstructions || len(flow.UserCode) > maximumUserCode || hasUnsafeText(flow.Instructions) || hasUnsafeText(flow.UserCode) {
		return errManagementAPI
	}
	if flow.AuthorizationURL != "" {
		parsed, err := url.Parse(flow.AuthorizationURL)
		if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Fragment != "" || parsed.Scheme != "https" && parsed.Scheme != "http" || hasUnsafeText(flow.AuthorizationURL) {
			return errManagementAPI
		}
	}
	return nil
}

func validOAuthProvider(provider string) bool {
	if len(provider) == 0 || len(provider) > 64 || provider != strings.ToLower(provider) {
		return false
	}
	for index, character := range provider {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_') {
			continue
		}
		return false
	}
	return true
}

func validOAuthKind(kind OAuthKind) bool { return kind == OAuthKindGeneric || kind == OAuthKindCodex }

func validFlowID(flowID string) bool {
	return len(flowID) > 0 && len(flowID) <= maximumFlowID && !hasUnsafeText(flowID)
}

func hasUnsafeText(value string) bool {
	for _, character := range value {
		if character == '\x00' || character == '\r' || character == '\n' || character == '\u007f' {
			return true
		}
	}
	return false
}
