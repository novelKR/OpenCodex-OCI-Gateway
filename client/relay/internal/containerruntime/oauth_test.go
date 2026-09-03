package containerruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeOAuthKeychain struct {
	secrets Secrets
	err     error
	loads   int
}

func (f *fakeOAuthKeychain) Load(context.Context, string) (Secrets, error) {
	f.loads++
	return Secrets{
		APIToken:   append([]byte(nil), f.secrets.APIToken...),
		AdminToken: append([]byte(nil), f.secrets.AdminToken...),
	}, f.err
}

func (f *fakeOAuthKeychain) Ensure(context.Context, string) (Secrets, error) {
	panic("OAuth operations must never create credentials")
}

type fakeManagementAPI struct {
	providers      []OAuthProvider
	startFlow      ManagementFlow
	status         OAuthStatus
	err            error
	lastAdminToken []byte
	lastFlow       ManagementFlow
	submitted      string
	starts         int
	statuses       int
	cancels        int
	requestHook    func()
}

func (f *fakeManagementAPI) capture(admin []byte, flow ManagementFlow) {
	if f.requestHook != nil {
		f.requestHook()
	}
	f.lastAdminToken = append(f.lastAdminToken[:0], admin...)
	f.lastFlow = flow
}

func (f *fakeManagementAPI) Providers(_ context.Context, admin []byte) ([]OAuthProvider, error) {
	f.capture(admin, ManagementFlow{})
	return append([]OAuthProvider(nil), f.providers...), f.err
}

func (f *fakeManagementAPI) Start(_ context.Context, admin []byte, provider string, kind OAuthKind) (ManagementFlow, error) {
	f.starts++
	f.capture(admin, ManagementFlow{Provider: provider, Kind: kind})
	return f.startFlow, f.err
}

func (f *fakeManagementAPI) Status(_ context.Context, admin []byte, flow ManagementFlow) (OAuthStatus, error) {
	f.statuses++
	f.capture(admin, flow)
	return f.status, f.err
}

func (f *fakeManagementAPI) Submit(_ context.Context, admin []byte, flow ManagementFlow, input string) error {
	f.capture(admin, flow)
	f.submitted = input
	return f.err
}

func (f *fakeManagementAPI) Cancel(_ context.Context, admin []byte, flow ManagementFlow) error {
	f.cancels++
	f.capture(admin, flow)
	return f.err
}

func TestOAuthSessionManagerPersistsOnlyOpaqueSessionIdentity(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{
		startFlow: ManagementFlow{
			Provider:         "xai",
			Kind:             OAuthKindGeneric,
			AuthorizationURL: "https://login.example/authorize?state=sensitive-state",
			Instructions:     "paste the result",
			UserCode:         "SAFE-USER-CODE",
		},
		status: OAuthStatusComplete,
	}
	keychain := validFakeOAuthKeychain()
	manager := newTestOAuthManager(t, root, api, keychain)
	manager.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	receipt, err := manager.Start(context.Background(), "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.OK || receipt.Status != OAuthStatusAwaitingUser || receipt.AuthorizationURL != api.startFlow.AuthorizationURL || receipt.UserCode != api.startFlow.UserCode {
		t.Fatalf("unexpected start receipt: %#v", receipt)
	}
	path := filepath.Join(root, oauthSessionDirectory, receipt.OperationID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"login.example", "sensitive-state", "paste the result", "SAFE-USER-CODE", string(keychain.secrets.APIToken), string(keychain.secrets.AdminToken), "authorization_url", "instructions", "user_code", "token", "state"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("durable OAuth session contains forbidden value %q: %s", forbidden, data)
		}
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record) != 5 || record["schema"] != float64(1) || record["operation_id"] != receipt.OperationID || record["provider"] != "xai" || record["kind"] != "generic" {
		t.Fatalf("unexpected durable session projection: %#v", record)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !safeSingleOwnerFile(info) {
		t.Fatalf("session file is not owner-only: %v, %#o", err, info.Mode().Perm())
	}

	// Reconstructing the manager simulates a new relayctl process. It can poll
	// the generic provider using only the opaque operation record.
	restarted := newTestOAuthManager(t, root, api, keychain)
	restarted.now = manager.now
	status, err := restarted.Status(context.Background(), receipt.OperationID)
	if err != nil || status.Status != OAuthStatusComplete {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal session was not removed: %v", err)
	}
	if string(api.lastAdminToken) != string(keychain.secrets.AdminToken) || string(api.lastAdminToken) == string(keychain.secrets.APIToken) {
		t.Fatal("management adapter did not receive only the Admin token")
	}
}

func TestOAuthSessionManagerRejectsAliasedGenericProviderFlow(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider: "xai", Kind: OAuthKindGeneric, AuthorizationURL: "https://login.example/start",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	if _, err := manager.Start(context.Background(), "xai", OAuthKindGeneric); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), "xai", OAuthKindGeneric); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("second generic flow error = %v", err)
	}
	if api.starts != 1 {
		t.Fatalf("aliased generic flow reached upstream %d times", api.starts)
	}
}

func TestOAuthSessionManagerConstructionDoesNotCreateRuntimeState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime-not-yet-enrolled")
	if _, err := NewOAuthSessionManager(
		root,
		&fakeManagementAPI{},
		validFakeOAuthKeychain(),
		"test-account",
		func(context.Context) (func() error, error) { return func() error { return nil }, nil },
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only service construction created runtime state: %v", err)
	}
}

func TestOAuthSessionManagerKeepsCodexFlowContractSeparate(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider:         "chatgpt",
		Kind:             OAuthKindCodex,
		UpstreamFlowID:   "flow-opaque-123",
		AuthorizationURL: "https://auth.example/codex",
		Instructions:     "Sign in",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	receipt, err := manager.Start(context.Background(), "chatgpt", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, oauthSessionDirectory, receipt.OperationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"upstream_flow_id":"flow-opaque-123"`) || strings.Contains(string(data), "auth.example") || strings.Contains(string(data), "Sign in") {
		t.Fatalf("Codex session did not preserve only opaque flow identity: %s", data)
	}
	result, err := manager.Submit(context.Background(), OAuthSubmitRequest{
		OperationID: receipt.OperationID,
		Input: strings.NewReader(
			`{"schema_version":1,"redirect_url":"https://localhost/callback?code=manual"}`,
		),
	})
	if err != nil || result.Status != OAuthStatusPending {
		t.Fatalf("submit = %#v, %v", result, err)
	}
	if api.submitted != "https://localhost/callback?code=manual" || api.lastFlow.UpstreamFlowID != "flow-opaque-123" || api.lastFlow.Kind != OAuthKindCodex {
		t.Fatalf("submit used wrong contract: flow=%#v input=%q", api.lastFlow, api.submitted)
	}
	after, err := os.ReadFile(filepath.Join(root, oauthSessionDirectory, receipt.OperationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "manual") || strings.Contains(string(after), "callback") {
		t.Fatalf("submitted redirect was persisted: %s", after)
	}
	cancelled, err := manager.Cancel(context.Background(), receipt.OperationID)
	if err != nil || cancelled.Status != OAuthStatusCancelled || api.cancels != 1 {
		t.Fatalf("cancel = %#v, %v, calls=%d", cancelled, err, api.cancels)
	}
}

func TestOAuthSessionManagerRejectsOversizedInputBeforeAPI(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider: "xai", Kind: OAuthKindGeneric, AuthorizationURL: "https://login.example/start",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	receipt, err := manager.Start(context.Background(), "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Submit(context.Background(), OAuthSubmitRequest{
		OperationID: receipt.OperationID,
		Input:       io.LimitReader(strings.NewReader(strings.Repeat("x", MaximumOAuthInputBytes+2)), MaximumOAuthInputBytes+2),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized input error = %v", err)
	}
	if api.submitted != "" {
		t.Fatal("oversized input reached management API")
	}
}

func TestOAuthSessionManagerRejectsAmbiguousOrNonStrictInputEnvelope(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider: "xai", Kind: OAuthKindGeneric, AuthorizationURL: "https://login.example/start",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	receipt, err := manager.Start(context.Background(), "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{
		`{"schema_version":1}`,
		`{"schema_version":1,"redirect_url":"https://localhost/callback","code":"opaque"}`,
		`{"schema_version":1,"code":"opaque","unknown":true}`,
		`{"schema_version":1,"code":"first","code":"second"}`,
		`{"schema_version":1,"code":"opaque"}{}`,
		`{"schema_version":1,"code":" opaque "}`,
	}
	for _, input := range inputs {
		api.submitted = ""
		_, err := manager.Submit(context.Background(), OAuthSubmitRequest{
			OperationID: receipt.OperationID,
			Input:       strings.NewReader(input),
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("input %q error = %v", input, err)
		}
		if api.submitted != "" {
			t.Fatalf("invalid input %q reached management API", input)
		}
	}
}

func TestOAuthSessionManagerExpiresWithoutPollingOrLeaking(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider: "xai", Kind: OAuthKindGeneric, AuthorizationURL: "https://login.example/start",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	startedAt := time.Unix(1_700_000_000, 0)
	manager.now = func() time.Time { return startedAt }
	receipt, err := manager.Start(context.Background(), "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return startedAt.Add(time.Duration(maximumOAuthAgeSeconds+1) * time.Second) }
	status, err := manager.Status(context.Background(), receipt.OperationID)
	if err != nil || status.Status != OAuthStatusFailed {
		t.Fatalf("expired status = %#v, %v", status, err)
	}
	if api.statuses != 0 {
		t.Fatal("expired operation was sent upstream")
	}
	if _, err := os.Lstat(filepath.Join(root, oauthSessionDirectory, receipt.OperationID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired operation remained durable: %v", err)
	}
}

func TestOAuthSessionManagerRejectsHardLinkedSession(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{startFlow: ManagementFlow{
		Provider: "xai", Kind: OAuthKindGeneric, AuthorizationURL: "https://login.example/start",
	}}
	manager := newTestOAuthManager(t, root, api, validFakeOAuthKeychain())
	receipt, err := manager.Start(context.Background(), "xai", OAuthKindGeneric)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, oauthSessionDirectory, receipt.OperationID+".json")
	if err := os.Link(path, filepath.Join(root, "session-hardlink")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := manager.Status(context.Background(), receipt.OperationID); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("hard-linked session error = %v", err)
	}
}

func TestOAuthSessionManagerRejectsEqualOrInvalidCredentials(t *testing.T) {
	token := testOAuthToken(9)
	api := &fakeManagementAPI{providers: []OAuthProvider{{ID: "chatgpt", Name: "ChatGPT Codex", Kind: OAuthKindCodex}}}
	manager := newTestOAuthManager(t, t.TempDir(), api, &fakeOAuthKeychain{secrets: Secrets{APIToken: append([]byte(nil), token...), AdminToken: append([]byte(nil), token...)}})
	if _, err := manager.Providers(context.Background()); !errors.Is(err, ErrCredential) {
		t.Fatalf("equal credentials error = %v", err)
	}
	if api.lastAdminToken != nil {
		t.Fatal("invalid credentials reached management API")
	}
}

func TestOAuthSessionManagerRejectsLiveRuntimeDriftBeforeKeychainOrAPI(t *testing.T) {
	root := t.TempDir()
	api := &fakeManagementAPI{providers: []OAuthProvider{{ID: "chatgpt", Name: "ChatGPT Codex", Kind: OAuthKindCodex}}}
	keychain := validFakeOAuthKeychain()
	guardCalls := 0
	manager, err := NewOAuthSessionManager(
		root,
		api,
		keychain,
		"test-account",
		func(context.Context) (func() error, error) {
			guardCalls++
			return nil, ErrRecoveryRequired
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Providers(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("runtime drift error = %v", err)
	}
	if guardCalls != 1 || keychain.loads != 0 || api.lastAdminToken != nil {
		t.Fatalf("drift crossed credential/API boundary: guards=%d loads=%d token=%q", guardCalls, keychain.loads, api.lastAdminToken)
	}
}

func TestOAuthSessionManagerHoldsRuntimeLeaseThroughManagementRequest(t *testing.T) {
	held := false
	released := 0
	api := &fakeManagementAPI{
		providers: []OAuthProvider{{ID: "chatgpt", Name: "ChatGPT Codex", Kind: OAuthKindCodex}},
		requestHook: func() {
			if !held {
				t.Fatal("management request ran outside runtime lease")
			}
		},
	}
	manager, err := NewOAuthSessionManager(
		t.TempDir(),
		api,
		validFakeOAuthKeychain(),
		"test-account",
		func(context.Context) (func() error, error) {
			if held {
				t.Fatal("runtime lease was acquired twice")
			}
			held = true
			return func() error {
				held = false
				released++
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Providers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if held || released != 1 {
		t.Fatalf("runtime lease held=%t released=%d", held, released)
	}
}

func newTestOAuthManager(t *testing.T, root string, api ManagementAPI, keychain Keychain) *OAuthSessionManager {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewOAuthSessionManager(
		root,
		api,
		keychain,
		"test-account",
		func(context.Context) (func() error, error) { return func() error { return nil }, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func validFakeOAuthKeychain() *fakeOAuthKeychain {
	return &fakeOAuthKeychain{secrets: Secrets{APIToken: testOAuthToken(7), AdminToken: testOAuthToken(8)}}
}
