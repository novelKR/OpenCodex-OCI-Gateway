package containerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	oauthSessionSchema       = 1
	oauthSessionDirectory    = "oauth"
	maximumOAuthSessionFiles = 64
)

type oauthSession struct {
	Schema         int       `json:"schema"`
	OperationID    string    `json:"operation_id"`
	Provider       string    `json:"provider"`
	Kind           OAuthKind `json:"kind"`
	UpstreamFlowID string    `json:"upstream_flow_id,omitempty"`
	CreatedAtUnix  int64     `json:"created_at_unix"`
}

// OAuthSessionManager owns the short-lived mapping between a Relay operation
// id and an upstream OpenCodex login flow. Its durable records intentionally do
// not contain authorization URLs, callback state, submitted codes, user codes,
// tokens, instructions, or upstream response bodies.
type OAuthSessionManager struct {
	store    *oauthSessionStore
	api      ManagementAPI
	keychain Keychain
	lease    RuntimeCredentialLease
	account  string
	now      func() time.Time
	mu       sync.Mutex
}

// RuntimeCredentialLease proves the exact signed Apple container is still
// running and serializes the complete Admin-token HTTP request with lifecycle
// mutations that can release and rebind its fixed loopback port.
type RuntimeCredentialLease func(context.Context) (func() error, error)

func NewOAuthSessionManager(root string, api ManagementAPI, keychain Keychain, account string, lease RuntimeCredentialLease) (*OAuthSessionManager, error) {
	if api == nil || keychain == nil || lease == nil || !validKeychainAccount(account) {
		return nil, ErrInvalidRequest
	}
	store, err := newOAuthSessionStore(root)
	if err != nil {
		return nil, err
	}
	return &OAuthSessionManager{
		store:    store,
		api:      api,
		keychain: keychain,
		lease:    lease,
		account:  account,
		now:      time.Now,
	}, nil
}

func (m *OAuthSessionManager) Providers(ctx context.Context) (OAuthProvidersReceipt, error) {
	if m == nil {
		return OAuthProvidersReceipt{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	admin, release, err := m.loadAdminToken(ctx)
	if err != nil {
		return OAuthProvidersReceipt{}, err
	}
	defer zeroBytes(admin)
	defer release()
	providers, err := m.api.Providers(ctx, admin)
	if err != nil {
		return OAuthProvidersReceipt{}, err
	}
	if err := validateProviders(providers); err != nil {
		return OAuthProvidersReceipt{}, err
	}
	return OAuthProvidersReceipt{SchemaVersion: SchemaVersion, OK: true, Providers: providers}, nil
}

func (m *OAuthSessionManager) Start(ctx context.Context, provider string, kind OAuthKind) (OAuthReceipt, error) {
	if m == nil || !validOAuthProvider(provider) || !validOAuthKind(kind) || kind == OAuthKindCodex && provider != "chatgpt" || kind == OAuthKindGeneric && provider == "chatgpt" {
		return OAuthReceipt{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	if err := m.store.cleanupExpired(now); err != nil {
		return OAuthReceipt{}, err
	}
	if kind == OAuthKindGeneric {
		active, err := m.store.hasActiveGenericProvider(provider)
		if err != nil {
			return OAuthReceipt{}, err
		}
		if active {
			return OAuthReceipt{}, ErrStateChanged
		}
	}
	admin, release, err := m.loadAdminToken(ctx)
	if err != nil {
		return OAuthReceipt{}, err
	}
	defer zeroBytes(admin)
	defer release()
	flow, err := m.api.Start(ctx, admin, provider, kind)
	if err != nil {
		return OAuthReceipt{}, err
	}
	if validateManagementFlowIdentity(flow) != nil || flow.Provider != provider || flow.Kind != kind || validateFlowPresentation(flow) != nil {
		return OAuthReceipt{}, errManagementAPI
	}
	operationID, err := randomHex(32)
	if err != nil {
		return OAuthReceipt{}, err
	}
	session := oauthSession{
		Schema:         oauthSessionSchema,
		OperationID:    operationID,
		Provider:       provider,
		Kind:           kind,
		UpstreamFlowID: flow.UpstreamFlowID,
		CreatedAtUnix:  now.Unix(),
	}
	if err := m.store.save(session); err != nil {
		// The caller cannot subsequently cancel a flow without a durable
		// operation id, so close it best-effort before returning the store error.
		_ = m.api.Cancel(ctx, admin, flow)
		return OAuthReceipt{}, err
	}
	status := OAuthStatusPending
	if flow.AuthorizationURL != "" || flow.Instructions != "" || flow.UserCode != "" {
		status = OAuthStatusAwaitingUser
	}
	return OAuthReceipt{
		SchemaVersion:    SchemaVersion,
		OK:               true,
		OperationID:      operationID,
		Provider:         provider,
		Kind:             kind,
		Status:           status,
		AuthorizationURL: flow.AuthorizationURL,
		Instructions:     flow.Instructions,
		UserCode:         flow.UserCode,
	}, nil
}

func (m *OAuthSessionManager) Status(ctx context.Context, operationID string) (OAuthReceipt, error) {
	if m == nil || !isSHA256(operationID) {
		return OAuthReceipt{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.store.load(operationID)
	if err != nil {
		return OAuthReceipt{}, err
	}
	if m.sessionExpired(session) {
		_ = m.store.remove(operationID)
		return receiptForSession(session, OAuthStatusFailed), nil
	}
	admin, release, err := m.loadAdminToken(ctx)
	if err != nil {
		return OAuthReceipt{}, err
	}
	defer zeroBytes(admin)
	defer release()
	status, err := m.api.Status(ctx, admin, flowForSession(session))
	if err != nil {
		return OAuthReceipt{}, err
	}
	if !validOAuthStatus(status) || status == OAuthStatusCancelled || status == OAuthStatusAwaitingUser {
		return OAuthReceipt{}, errManagementAPI
	}
	if terminalOAuthStatus(status) {
		if err := m.store.remove(operationID); err != nil {
			return OAuthReceipt{}, err
		}
	}
	return receiptForSession(session, status), nil
}

func (m *OAuthSessionManager) Submit(ctx context.Context, request OAuthSubmitRequest) (OAuthReceipt, error) {
	if m == nil || !isSHA256(request.OperationID) || request.Input == nil {
		return OAuthReceipt{}, ErrInvalidRequest
	}
	input, err := readOAuthInput(request.Input)
	if err != nil {
		return OAuthReceipt{}, err
	}
	defer zeroBytes(input)
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.store.load(request.OperationID)
	if err != nil {
		return OAuthReceipt{}, err
	}
	if m.sessionExpired(session) {
		_ = m.store.remove(request.OperationID)
		return OAuthReceipt{}, ErrInvalidRequest
	}
	admin, release, err := m.loadAdminToken(ctx)
	if err != nil {
		return OAuthReceipt{}, err
	}
	defer zeroBytes(admin)
	defer release()
	if err := m.api.Submit(ctx, admin, flowForSession(session), string(input)); err != nil {
		return OAuthReceipt{}, err
	}
	return receiptForSession(session, OAuthStatusPending), nil
}

func (m *OAuthSessionManager) Cancel(ctx context.Context, operationID string) (OAuthReceipt, error) {
	if m == nil || !isSHA256(operationID) {
		return OAuthReceipt{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.store.load(operationID)
	if err != nil {
		return OAuthReceipt{}, err
	}
	admin, release, err := m.loadAdminToken(ctx)
	if err != nil {
		return OAuthReceipt{}, err
	}
	defer zeroBytes(admin)
	defer release()
	if err := m.api.Cancel(ctx, admin, flowForSession(session)); err != nil {
		return OAuthReceipt{}, err
	}
	if err := m.store.remove(operationID); err != nil {
		return OAuthReceipt{}, err
	}
	return receiptForSession(session, OAuthStatusCancelled), nil
}

func (m *OAuthSessionManager) loadAdminToken(ctx context.Context) ([]byte, func() error, error) {
	if ctx == nil || m.lease == nil {
		return nil, nil, ErrRecoveryRequired
	}
	release, err := m.lease(ctx)
	if err != nil || release == nil {
		return nil, nil, ErrRecoveryRequired
	}
	secrets, err := m.keychain.Load(ctx, m.account)
	if err != nil {
		zeroBytes(secrets.APIToken)
		zeroBytes(secrets.AdminToken)
		_ = release()
		return nil, nil, ErrCredential
	}
	defer zeroBytes(secrets.APIToken)
	defer zeroBytes(secrets.AdminToken)
	if !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) || bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		_ = release()
		return nil, nil, ErrCredential
	}
	return append([]byte(nil), secrets.AdminToken...), release, nil
}

func (m *OAuthSessionManager) sessionExpired(session oauthSession) bool {
	now := m.now().UTC().Unix()
	return session.CreatedAtUnix > now+5 || now-session.CreatedAtUnix > maximumOAuthAgeSeconds
}

func receiptForSession(session oauthSession, status OAuthStatus) OAuthReceipt {
	return OAuthReceipt{
		SchemaVersion: SchemaVersion,
		OK:            true,
		OperationID:   session.OperationID,
		Provider:      session.Provider,
		Kind:          session.Kind,
		Status:        status,
	}
}

func flowForSession(session oauthSession) ManagementFlow {
	return ManagementFlow{Provider: session.Provider, Kind: session.Kind, UpstreamFlowID: session.UpstreamFlowID}
}

func readOAuthInput(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaximumOAuthInputBytes+1))
	if err != nil || len(data) == 0 || len(data) > MaximumOAuthInputBytes {
		zeroBytes(data)
		return nil, ErrInvalidRequest
	}
	defer zeroBytes(data)
	if rejectDuplicateJSONKeys(data) != nil {
		return nil, ErrInvalidRequest
	}
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		RedirectURL   string `json:"redirect_url,omitempty"`
		Code          string `json:"code,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		return nil, ErrInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidRequest
	}
	hasRedirect := envelope.RedirectURL != ""
	hasCode := envelope.Code != ""
	if envelope.SchemaVersion != 1 || hasRedirect == hasCode {
		return nil, ErrInvalidRequest
	}
	value := envelope.Code
	if hasRedirect {
		value = envelope.RedirectURL
	}
	result := []byte(value)
	if len(result) == 0 || len(result) > MaximumOAuthSubmissionBytes || strings.TrimSpace(value) != value ||
		bytes.IndexByte(result, 0) >= 0 || bytes.IndexByte(result, '\r') >= 0 ||
		bytes.IndexByte(result, '\n') >= 0 || bytes.IndexByte(result, 0x7f) >= 0 {
		zeroBytes(result)
		return nil, ErrInvalidRequest
	}
	return result, nil
}

func validateProviders(providers []OAuthProvider) error {
	if len(providers) == 0 || len(providers) > 64 {
		return errManagementAPI
	}
	seen := make(map[string]struct{}, len(providers))
	codex := 0
	for _, provider := range providers {
		if !validOAuthProvider(provider.ID) || provider.Name == "" || len(provider.Name) > 128 || hasUnsafeText(provider.Name) || !validOAuthKind(provider.Kind) {
			return errManagementAPI
		}
		if _, exists := seen[provider.ID+"\x00"+string(provider.Kind)]; exists {
			return errManagementAPI
		}
		seen[provider.ID+"\x00"+string(provider.Kind)] = struct{}{}
		if provider.Kind == OAuthKindCodex {
			codex++
			if provider.ID != "chatgpt" {
				return errManagementAPI
			}
		} else if provider.ID == "chatgpt" {
			return errManagementAPI
		}
	}
	if codex != 1 {
		return errManagementAPI
	}
	return nil
}

func validOAuthStatus(status OAuthStatus) bool {
	return status == OAuthStatusPending || status == OAuthStatusAwaitingUser || status == OAuthStatusComplete || status == OAuthStatusCancelled || status == OAuthStatusFailed
}

func terminalOAuthStatus(status OAuthStatus) bool {
	return status == OAuthStatusComplete || status == OAuthStatusCancelled || status == OAuthStatusFailed
}

type oauthSessionStore struct {
	state *stateStore
	dir   string
}

func newOAuthSessionStore(root string) (*oauthSessionStore, error) {
	state, err := newStateStore(root)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, oauthSessionDirectory)
	return &oauthSessionStore{state: state, dir: directory}, nil
}

func (s *oauthSessionStore) prepare() error {
	if s == nil || s.state == nil {
		return ErrUnsafeState
	}
	if err := s.state.prepareRoot(); err != nil {
		return err
	}
	return s.state.prepareDirectory(s.dir)
}

func (s *oauthSessionStore) save(session oauthSession) error {
	if validateOAuthSession(session) != nil {
		return ErrUnsafeState
	}
	path := s.path(session.OperationID)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeState
	}
	return s.state.writeJSON(path, session)
}

func (s *oauthSessionStore) load(operationID string) (oauthSession, error) {
	if !isSHA256(operationID) {
		return oauthSession{}, ErrInvalidRequest
	}
	path := s.path(operationID)
	data, found, err := s.state.readOptional(path, maximumStateBytes)
	if err != nil || !found {
		return oauthSession{}, ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil || !safeSingleOwnerFile(info) {
		return oauthSession{}, ErrUnsafeState
	}
	var session oauthSession
	if decodeStrict(data, &session) != nil || validateOAuthSession(session) != nil || session.OperationID != operationID {
		return oauthSession{}, ErrUnsafeState
	}
	return session, nil
}

func (s *oauthSessionStore) remove(operationID string) error {
	if !isSHA256(operationID) {
		return ErrInvalidRequest
	}
	path := s.path(operationID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !safeSingleOwnerFile(info) {
		return ErrUnsafeState
	}
	return os.Remove(path)
}

func (s *oauthSessionStore) cleanupExpired(now time.Time) error {
	if err := s.prepare(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil || len(entries) > maximumOAuthSessionFiles*2 {
		return ErrUnsafeState
	}
	active := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			return ErrUnsafeState
		}
		operationID := strings.TrimSuffix(name, ".json")
		session, err := s.load(operationID)
		if err != nil {
			return err
		}
		if session.CreatedAtUnix > now.Unix()+5 || now.Unix()-session.CreatedAtUnix > maximumOAuthAgeSeconds {
			if err := s.remove(operationID); err != nil {
				return err
			}
			continue
		}
		active++
	}
	if active >= maximumOAuthSessionFiles {
		return ErrUnsafeState
	}
	return nil
}

func (s *oauthSessionStore) hasActiveGenericProvider(provider string) (bool, error) {
	if !validOAuthProvider(provider) {
		return false, ErrInvalidRequest
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil || len(entries) > maximumOAuthSessionFiles*2 {
		return false, ErrUnsafeState
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			return false, ErrUnsafeState
		}
		session, err := s.load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return false, err
		}
		if session.Kind == OAuthKindGeneric && session.Provider == provider {
			return true, nil
		}
	}
	return false, nil
}

func (s *oauthSessionStore) path(operationID string) string {
	return filepath.Join(s.dir, operationID+".json")
}

func validateOAuthSession(session oauthSession) error {
	if session.Schema != oauthSessionSchema || !isSHA256(session.OperationID) || !validOAuthProvider(session.Provider) || !validOAuthKind(session.Kind) || session.CreatedAtUnix <= 0 {
		return ErrUnsafeState
	}
	if session.Kind == OAuthKindCodex {
		if session.Provider != "chatgpt" || !validFlowID(session.UpstreamFlowID) {
			return ErrUnsafeState
		}
	} else if session.Provider == "chatgpt" || session.UpstreamFlowID != "" {
		return ErrUnsafeState
	}
	return nil
}

func safeSingleOwnerFile(info os.FileInfo) bool {
	if !safeOwnerFile(info) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
