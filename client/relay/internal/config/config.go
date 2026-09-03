// Package config owns the non-secret, local compatibility-relay configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddress = "127.0.0.1:18180"

	UpstreamModeExternalGateway     = "external_gateway"
	UpstreamModeLocalOpenCodex      = "local_opencodex"
	UpstreamModeLocalAppleContainer = "local_apple_container"

	CredentialsSourceKeychain = "keychain"
	CredentialsSourceFile     = "file"
	CredentialsSourceNone     = "none"

	RemoteAuthenticationNone                          = "none"
	RemoteAuthenticationGatewayAPIKey                 = "gateway_api_key"
	RemoteAuthenticationCloudflareAccessAndGatewayKey = "cloudflare_access_and_gateway_api_key"
	LocalAuthenticationOpenCodexAPIKey                = "local_opencodex_api_key"

	ResponsesWebSocketModePassthrough           = "passthrough"
	ResponsesWebSocketModeHTTPFallback          = "http_fallback"
	ResponsesModelModeBoundedJSON               = "bounded_json"
	DefaultResponsesInteractiveListenPort       = 18182
	DefaultResponsesMaxClassifications          = 8
	DefaultResponsesMaxPendingRequests          = 24
	DefaultResponsesMaxPendingEncodedBytes      = int64(512 << 20)
	DefaultResponsesQueueTimeoutMS              = 60_000
	DefaultResponsesMaxGeneralUpstream          = 4
	DefaultResponsesInteractiveReservedUpstream = 1
	DefaultResponsesMaxConcurrentTransforms     = 2
	DefaultResponsesMaxOpenDeliveries           = 16

	CatalogOwnerRelay         = "relay"
	CatalogOwnerRemoteManager = "remote_manager"

	InstallationScopeProduction       = "production"
	InstallationScopeLocalDevelopment = "local_development"
	LocalDevelopmentListenAddress     = "127.0.0.1:18190"
	LocalDevelopmentInteractiveListen = "127.0.0.1:18192"
	LocalDevelopmentExternalCatalog   = "opencodex-relay-dev-external-catalog.json"
	LocalDevelopmentLocalCatalog      = "opencodex-relay-dev-local-catalog.json"
	LocalDevelopmentAppleCatalog      = "opencodex-relay-dev-apple-container-catalog.json"
	LocalAppleContainerCatalog        = "opencodex-relay-apple-container-catalog.json"

	// CatalogRestartPendingSuffix and CatalogPreviousSuffix name every
	// sidecar artifact a relay-owned catalog writer may create. Keeping these
	// names in config lets dual-profile validation protect the complete writer
	// namespace without importing the catalog package (which already imports
	// config).
	CatalogRestartPendingSuffix = ".restart-pending"
	CatalogPreviousSuffix       = ".previous"

	localOpenCodexIPv4URL  = "http://127.0.0.1:10100/v1"
	localOpenCodexIPv6URL  = "http://[::1]:10100/v1"
	localAppleContainerURL = "http://127.0.0.1:10210/v1"

	minResponsesPendingBytes = int64(32 << 20)
	maxResponsesPendingBytes = int64(16 << 30)
)

// Config deliberately contains no credential values. Credentials are resolved
// from the macOS Keychain or a Linux owner-only file at runtime.
type Config struct {
	// InstallationScope binds the non-secret config to one reviewed
	// distribution namespace. An absent value is the historical production
	// contract; the local-only development installer writes local_development.
	InstallationScope string                `json:"installation_scope,omitempty"`
	ListenAddress     string                `json:"listen_address"`
	UpstreamMode      string                `json:"upstream_mode,omitempty"`
	UpstreamBaseURL   string                `json:"upstream_base_url"`
	VoiceEnabled      bool                  `json:"voice_enabled"`
	Credentials       CredentialsConfig     `json:"credentials"`
	Responses         ResponsesConfig       `json:"responses,omitempty"`
	Catalog           CatalogConfig         `json:"catalog"`
	ConnectionProbe   ConnectionProbeConfig `json:"connection_probe,omitempty"`
	// LocalOpenCodex is an optional, macOS-only relay-owned profile. The
	// top-level fields remain the canonical external profile so existing Linux
	// local_opencodex + remote_manager installations retain their established
	// single-topology contract.
	LocalOpenCodex *LocalOpenCodexProfile `json:"local_opencodex,omitempty"`
	// LocalAppleContainer is an optional macOS Apple Silicon profile. Its
	// endpoint, credential source, authentication profile, and catalog
	// namespace are compiled-in invariants; only the current-user Keychain
	// account may be selected explicitly.
	LocalAppleContainer *LocalAppleContainerProfile `json:"local_apple_container,omitempty"`

	// localProfileRuntime marks an ephemeral Config returned by
	// LocalOpenCodexRuntimeConfig. It is deliberately not serialized: a saved
	// relay.json must never silently change its canonical external topology.
	localProfileRuntime        bool
	localAppleContainerRuntime bool
}

// Scope returns a canonical bounded installation scope without allowing a
// caller to infer or carry arbitrary user-supplied labels into routing state.
func (c Config) Scope() string {
	if c.InstallationScope == "" {
		return InstallationScopeProduction
	}
	return c.InstallationScope
}

type CredentialsConfig struct {
	// Source is "keychain" on macOS, "file" on Linux, or "none" for a
	// local OpenCodex upstream that does not cross the admission boundary.
	Source  string `json:"source"`
	File    string `json:"file,omitempty"`
	Account string `json:"account,omitempty"`
	// AuthenticationProfile selects exactly which stored values are read and
	// injected. An absent value is the historical Cloudflare + gateway-key
	// contract for keychain/file configs and none for credentials.source=none.
	AuthenticationProfile string `json:"authentication_profile,omitempty"`
	// AllowInsecurePrivateIP records the per-apply acknowledgement required
	// before Codex traffic may be sent over HTTP to a private IP literal.
	AllowInsecurePrivateIP bool `json:"allow_insecure_private_ip,omitempty"`
}

// RemoteAuthenticationProfile returns the backward-compatible canonical
// profile for this credential configuration.
func (c CredentialsConfig) RemoteAuthenticationProfile() string {
	if c.AuthenticationProfile != "" {
		return c.AuthenticationProfile
	}
	if c.Source == CredentialsSourceNone {
		return RemoteAuthenticationNone
	}
	return RemoteAuthenticationCloudflareAccessAndGatewayKey
}

// RequiredCredentialKinds returns bounded, non-secret identifiers suitable
// for control-plane inspection responses.
func RequiredCredentialKinds(profile string) ([]string, error) {
	switch profile {
	case RemoteAuthenticationNone:
		return []string{}, nil
	case RemoteAuthenticationGatewayAPIKey:
		return []string{"gateway_api_key"}, nil
	case RemoteAuthenticationCloudflareAccessAndGatewayKey:
		return []string{"cloudflare_access_client_id", "cloudflare_access_client_secret", "gateway_api_key"}, nil
	case LocalAuthenticationOpenCodexAPIKey:
		return []string{"local_opencodex_api_key"}, nil
	default:
		return nil, errors.New("authentication_profile is invalid")
	}
}

// ResponsesConfig controls the opt-in Responses protocol compatibility path.
// An absent configuration preserves the historical transparent proxy behavior.
type ResponsesConfig struct {
	WebSocketMode string                   `json:"websocket_mode,omitempty"`
	ModelModes    map[string]string        `json:"model_modes,omitempty"`
	Scheduler     ResponsesSchedulerConfig `json:"scheduler,omitempty"`
}

// ResponsesSchedulerConfig bounds request classification, queueing, upstream
// dispatch, transformations, and client delivery for Responses traffic. Zero
// values retain the backward-compatible defaults when a legacy config loads.
type ResponsesSchedulerConfig struct {
	InteractiveListenAddress    string `json:"interactive_listen_address,omitempty"`
	MaxClassifications          int    `json:"max_classifications,omitempty"`
	MaxPendingRequests          int    `json:"max_pending_requests,omitempty"`
	MaxPendingEncodedBytes      int64  `json:"max_pending_encoded_bytes,omitempty"`
	QueueTimeoutMS              int    `json:"queue_timeout_ms,omitempty"`
	MaxGeneralUpstream          int    `json:"max_general_upstream,omitempty"`
	InteractiveReservedUpstream int    `json:"interactive_reserved_upstream,omitempty"`
	MaxConcurrentTransforms     int    `json:"max_concurrent_transforms,omitempty"`
	MaxOpenDeliveries           int    `json:"max_open_deliveries,omitempty"`
}

// ModeForModel returns only an exact model policy, ignoring case. It does not
// trim, parse provider aliases, or inherit a policy across colon families.
func (c ResponsesConfig) ModeForModel(model string) (string, bool) {
	if mode, ok := c.ModelModes[model]; ok {
		return mode, true
	}
	for configured, mode := range c.ModelModes {
		if strings.EqualFold(configured, model) {
			return mode, true
		}
	}
	return "", false
}

type CatalogConfig struct {
	// Owner is "relay" unless a colocated Remote manager owns catalog refresh
	// and activation. The latter prevents competing writers to one Codex home.
	Owner           string `json:"owner,omitempty"`
	Path            string `json:"path"`
	RefreshInterval string `json:"refresh_interval"`
	// ManageAppServer is opt-in because a catalog belongs to one Codex home,
	// not every AppServer owned by the same OS user.
	ManageAppServer bool `json:"manage_app_server"`
	// AppServerHome must be the exact CODEX_HOME of a process that may be
	// restarted. A missing value deliberately disables automatic activation,
	// including for legacy configs created before this field existed.
	AppServerHome   string `json:"app_server_home,omitempty"`
	CodexExecutable string `json:"codex_executable"`
}

// LocalOpenCodexProfile is deliberately small. It contains no credential
// source, admission header, or proxy setting: local traffic always uses the
// fixed numeric loopback OpenCodex endpoint with credentials.source=none.
//
// CatalogPath is independent from Config.Catalog.Path so OpenCodex and the
// external gateway can never compete to write the same catalog file.
type LocalOpenCodexProfile struct {
	UpstreamBaseURL string `json:"upstream_base_url"`
	CatalogPath     string `json:"catalog_path"`
}

// LocalAppleContainerProfile stores only non-secret enrollment metadata. The
// endpoint and catalog basename are fixed so relay.json cannot repurpose this
// profile as arbitrary local egress or a competing catalog writer.
type LocalAppleContainerProfile struct {
	UpstreamBaseURL   string `json:"upstream_base_url"`
	CatalogPath       string `json:"catalog_path"`
	CredentialAccount string `json:"credential_account,omitempty"`
}

// ConnectionProbeConfig permits the macOS MenuBar installer to opt in to a
// deliberately low-frequency gateway reachability observation. It contains no
// endpoint, credentials, or timing knobs: the resident relay fixes those
// safety properties (10 minutes, a five second deadline, no redirects, and no
// retries). The runtime only schedules it on macOS for an external gateway.
//
// Keeping this object in relay.json lets the installer make the opt-in durable
// without giving the MenuBar app direct access to credentials or the catalog.
type ConnectionProbeConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencodex-relay", "relay.json"), nil
}

func DefaultCatalogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".codex", "opencodex-relay-catalog.json"), nil
}

// DefaultLocalOpenCodexCatalogPath returns a deliberately distinct catalog
// path for an enrolled local OpenCodex profile. The profile remains opt-in;
// callers must explicitly place this path in LocalOpenCodex before it can be
// selected.
func DefaultLocalOpenCodexCatalogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".codex", "opencodex-relay-local-catalog.json"), nil
}

// DefaultLocalAppleContainerCatalogPath returns the production catalog
// namespace reserved for the Apple Container runtime. It is intentionally
// distinct from both the external gateway and host-native OpenCodex paths.
func DefaultLocalAppleContainerCatalogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".codex", LocalAppleContainerCatalog), nil
}

// NewLocalOpenCodexProfile creates the safe default enrollment shape. It
// does not contact OpenCodex or alter relay.json; callers still need to write
// the returned profile through their explicit enrollment transaction.
func NewLocalOpenCodexProfile() (*LocalOpenCodexProfile, error) {
	catalogPath, err := DefaultLocalOpenCodexCatalogPath()
	if err != nil {
		return nil, err
	}
	return &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     catalogPath,
	}, nil
}

// NewLocalOpenCodexProfileForCodexConfig binds the relay-owned local catalog
// to the explicitly selected Codex home. It avoids silently placing a custom
// `--codex-config` enrollment under the default ~/.codex directory.
func NewLocalOpenCodexProfileForCodexConfig(codexConfigPath string) (*LocalOpenCodexProfile, error) {
	return NewLocalOpenCodexProfileForCodexConfigWithCatalogName(codexConfigPath, "opencodex-relay-local-catalog.json")
}

// NewLocalOpenCodexProfileForCodexConfigWithCatalogName binds one of the two
// compiled-in catalog namespaces to an explicit Codex config. The local-dev
// owner never shares the production local catalog writer path.
func NewLocalOpenCodexProfileForCodexConfigWithCatalogName(codexConfigPath, catalogName string) (*LocalOpenCodexProfile, error) {
	if codexConfigPath == "" || !filepath.IsAbs(codexConfigPath) || filepath.Clean(codexConfigPath) != codexConfigPath {
		return nil, errors.New("Codex config path must be a clean absolute path")
	}
	if catalogName != "opencodex-relay-local-catalog.json" && catalogName != LocalDevelopmentLocalCatalog {
		return nil, errors.New("local OpenCodex catalog filename is unsupported")
	}
	return &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     filepath.Join(filepath.Dir(codexConfigPath), catalogName),
	}, nil
}

// NewLocalAppleContainerProfile creates the compiled-in production profile.
// It contains no credential value and does not mutate relay.json.
func NewLocalAppleContainerProfile() (*LocalAppleContainerProfile, error) {
	catalogPath, err := DefaultLocalAppleContainerCatalogPath()
	if err != nil {
		return nil, err
	}
	return &LocalAppleContainerProfile{
		UpstreamBaseURL: localAppleContainerURL,
		CatalogPath:     catalogPath,
	}, nil
}

// NewLocalAppleContainerProfileForCodexConfig binds the fixed Apple catalog
// basename to the explicitly selected Codex home.
func NewLocalAppleContainerProfileForCodexConfig(codexConfigPath string) (*LocalAppleContainerProfile, error) {
	return NewLocalAppleContainerProfileForCodexConfigWithCatalogName(codexConfigPath, LocalAppleContainerCatalog)
}

// NewLocalAppleContainerProfileForCodexConfigWithCatalogName keeps production
// and local-development catalog namespaces disjoint.
func NewLocalAppleContainerProfileForCodexConfigWithCatalogName(codexConfigPath, catalogName string) (*LocalAppleContainerProfile, error) {
	if codexConfigPath == "" || !filepath.IsAbs(codexConfigPath) || filepath.Clean(codexConfigPath) != codexConfigPath {
		return nil, errors.New("Codex config path must be a clean absolute path")
	}
	if catalogName != LocalAppleContainerCatalog && catalogName != LocalDevelopmentAppleCatalog {
		return nil, errors.New("local Apple Container catalog filename is unsupported")
	}
	return &LocalAppleContainerProfile{
		UpstreamBaseURL: localAppleContainerURL,
		CatalogPath:     filepath.Join(filepath.Dir(codexConfigPath), catalogName),
	}, nil
}

// HasLocalOpenCodexProfile reports only durable enrollment, not listener
// reachability. Readiness is intentionally established by the bounded local
// preflight rather than config inspection or a TCP connect.
func (c Config) HasLocalOpenCodexProfile() bool { return c.LocalOpenCodex != nil }

// HasLocalAppleContainerProfile reports durable enrollment only. Runtime
// health is established independently by the authenticated local preflight.
func (c Config) HasLocalAppleContainerProfile() bool { return c.LocalAppleContainer != nil }

// LocalOpenCodexRuntimeConfig derives the immutable runtime settings for the
// optional relay-owned local profile. It preserves the persisted external
// profile and forces all local safety invariants in one place.
func (c Config) LocalOpenCodexRuntimeConfig() (Config, error) {
	if c.LocalOpenCodex == nil {
		return Config{}, errors.New("local_opencodex profile is not enrolled")
	}
	if c.UpstreamMode != UpstreamModeExternalGateway {
		return Config{}, errors.New("local_opencodex profile requires external_gateway as the canonical relay topology")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	result := c
	result.UpstreamMode = UpstreamModeLocalOpenCodex
	result.UpstreamBaseURL = c.LocalOpenCodex.UpstreamBaseURL
	result.Credentials = CredentialsConfig{Source: CredentialsSourceNone}
	result.Catalog.Owner = CatalogOwnerRelay
	result.Catalog.Path = c.LocalOpenCodex.CatalogPath
	result.ConnectionProbe.Enabled = false
	// The canonical config may enroll both local backends. An immutable
	// runtime clone carries only the selected profile so validation cannot
	// reinterpret the other profile as a second active topology.
	result.LocalAppleContainer = nil
	result.localProfileRuntime = true
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

// LocalAppleContainerRuntimeConfig derives the immutable runtime settings for
// the Apple Container profile. The API token remains in Keychain and is
// selected through a dedicated authentication profile; the management token
// is intentionally outside this generic relay configuration and loader.
func (c Config) LocalAppleContainerRuntimeConfig() (Config, error) {
	if c.LocalAppleContainer == nil {
		return Config{}, errors.New("local_apple_container profile is not enrolled")
	}
	if c.UpstreamMode != UpstreamModeExternalGateway {
		return Config{}, errors.New("local_apple_container profile requires external_gateway as the canonical relay topology")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	result := c
	result.UpstreamMode = UpstreamModeLocalAppleContainer
	result.UpstreamBaseURL = c.LocalAppleContainer.UpstreamBaseURL
	result.Credentials = CredentialsConfig{
		Source:                CredentialsSourceKeychain,
		Account:               c.LocalAppleContainer.CredentialAccount,
		AuthenticationProfile: LocalAuthenticationOpenCodexAPIKey,
	}
	result.Catalog.Owner = CatalogOwnerRelay
	result.Catalog.Path = c.LocalAppleContainer.CatalogPath
	result.ConnectionProbe.Enabled = false
	result.LocalOpenCodex = nil
	result.localAppleContainerRuntime = true
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

// IsLocalOpenCodexBaseURL accepts only the two fixed numeric loopback
// endpoints used by the local profile. It intentionally rejects localhost,
// alternate ports, path variants, and TLS so a configuration cannot turn a
// local profile into an arbitrary egress target.
func IsLocalOpenCodexBaseURL(value string) bool {
	return value == localOpenCodexIPv4URL || value == localOpenCodexIPv6URL
}

// IsLocalAppleContainerBaseURL accepts only the fixed numeric host-side
// published endpoint. The guest identity port is validated separately by the
// typed local preflight and is deliberately not inferred from this URL.
func IsLocalAppleContainerBaseURL(value string) bool {
	return value == localAppleContainerURL
}

func DefaultCredentialFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencodex-relay", "credentials.env"), nil
}

func NewDefault(upstream string, credentialSource string) (Config, error) {
	catalogPath, err := DefaultCatalogPath()
	if err != nil {
		return Config{}, err
	}
	credentialFile, err := DefaultCredentialFile()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddress:   DefaultListenAddress,
		UpstreamMode:    UpstreamModeExternalGateway,
		UpstreamBaseURL: upstream,
		Credentials: CredentialsConfig{
			Source: credentialSource,
			File:   credentialFile,
		},
		Responses: ResponsesConfig{
			WebSocketMode: ResponsesWebSocketModePassthrough,
		},
		Catalog: CatalogConfig{
			Owner:           CatalogOwnerRelay,
			Path:            catalogPath,
			RefreshInterval: "10m",
			ManageAppServer: false,
			CodexExecutable: "codex",
		},
	}
	cfg.applyDefaults()
	return cfg, nil
}

// NormalizeExternalGatewayURL accepts only a gateway origin or its canonical
// /v1 base. HTTPS may use a hostname or IP literal. HTTP is deliberately
// limited to RFC1918 IPv4 and IPv6 ULA literals and never accepts loopback.
func NormalizeExternalGatewayURL(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("upstream_base_url is invalid")
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || parsed.RawPath != "" {
		return "", errors.New("upstream_base_url must be an absolute gateway URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", errors.New("upstream_base_url must use HTTPS or private-IP HTTP")
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", errors.New("upstream_base_url host is required")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("upstream_base_url port is invalid")
		}
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path != "" && path != "/v1" {
		return "", errors.New("upstream_base_url path must be empty or /v1")
	}
	if scheme == "http" {
		ip := net.ParseIP(hostname)
		if ip == nil || ip.IsLoopback() || !isAllowedPrivateGatewayIP(ip) {
			return "", errors.New("HTTP gateway host must be an RFC1918 IPv4 or IPv6 ULA literal")
		}
	}
	parsed.Scheme = scheme
	parsed.Path = "/v1"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func isAllowedPrivateGatewayIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4[0] == 10 ||
			(ipv4[0] == 172 && ipv4[1] >= 16 && ipv4[1] <= 31) ||
			(ipv4[0] == 192 && ipv4[1] == 168)
	}
	ipv6 := ip.To16()
	return ipv6 != nil && ipv6[0]&0xfe == 0xfc
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read relay config: %w", err)
	}
	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode relay config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Write(path string, cfg Config) error {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create relay config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode relay config: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".relay.json.")
	if err != nil {
		return fmt.Errorf("create temporary relay config: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary relay config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write relay config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync relay config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close relay config: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace relay config: %w", err)
	}
	if err := syncConfigDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync relay config directory: %w", err)
	}
	return nil
}

func (c Config) RefreshEvery() (time.Duration, error) {
	value, err := time.ParseDuration(c.Catalog.RefreshInterval)
	if err != nil {
		return 0, fmt.Errorf("parse catalog refresh interval: %w", err)
	}
	if value < time.Minute {
		return 0, errors.New("catalog refresh interval must be at least one minute")
	}
	return value, nil
}

func (c Config) Validate() error {
	switch c.Scope() {
	case InstallationScopeProduction:
	case InstallationScopeLocalDevelopment:
		if c.ListenAddress != LocalDevelopmentListenAddress {
			return fmt.Errorf("local_development listen_address must be %s", LocalDevelopmentListenAddress)
		}
	default:
		return errors.New("installation_scope must be production or local_development")
	}
	if c.ListenAddress == "" {
		return errors.New("listen_address is required")
	}
	primaryHost, primaryPort, err := parseLoopbackListenAddress("listen_address", c.ListenAddress)
	if err != nil {
		return err
	}
	upstreamMode := c.UpstreamMode
	if upstreamMode == "" {
		upstreamMode = UpstreamModeExternalGateway
	}
	switch upstreamMode {
	case UpstreamModeExternalGateway:
		normalized, err := NormalizeExternalGatewayURL(c.UpstreamBaseURL)
		if err != nil || normalized != c.UpstreamBaseURL {
			return errors.New("upstream_base_url must be a canonical external gateway /v1 URL")
		}
		profile := c.Credentials.RemoteAuthenticationProfile()
		if _, err := RequiredCredentialKinds(profile); err != nil {
			return err
		}
		if profile == LocalAuthenticationOpenCodexAPIKey {
			return errors.New("local_opencodex_api_key is reserved for local_apple_container")
		}
		if profile == RemoteAuthenticationNone {
			if c.Credentials.Source != CredentialsSourceNone && c.Credentials.Source != CredentialsSourceKeychain && c.Credentials.Source != CredentialsSourceFile {
				return errors.New("credentials.source is invalid")
			}
		} else if c.Credentials.Source != CredentialsSourceKeychain && c.Credentials.Source != CredentialsSourceFile {
			return errors.New("credentials.source must be keychain or file for authenticated external_gateway")
		}
		if strings.HasPrefix(normalized, "http://") {
			if profile == RemoteAuthenticationCloudflareAccessAndGatewayKey {
				return errors.New("Cloudflare Access credentials require HTTPS")
			}
			if !c.Credentials.AllowInsecurePrivateIP {
				return errors.New("private-IP HTTP gateway requires explicit acknowledgement")
			}
		} else if c.Credentials.AllowInsecurePrivateIP {
			return errors.New("allow_insecure_private_ip is valid only for private-IP HTTP")
		}
	case UpstreamModeLocalOpenCodex:
		if !IsLocalOpenCodexBaseURL(c.UpstreamBaseURL) {
			return fmt.Errorf("upstream_base_url must be %s or %s for local_opencodex", localOpenCodexIPv4URL, localOpenCodexIPv6URL)
		}
		if c.Credentials.Source != CredentialsSourceNone {
			return errors.New("credentials.source must be none for local_opencodex")
		}
		if c.Credentials.RemoteAuthenticationProfile() != RemoteAuthenticationNone || c.Credentials.AllowInsecurePrivateIP {
			return errors.New("local_opencodex must not configure remote authentication")
		}
	case UpstreamModeLocalAppleContainer:
		if !c.localAppleContainerRuntime || c.LocalAppleContainer == nil {
			return errors.New("local_apple_container is a derived runtime profile")
		}
		if !IsLocalAppleContainerBaseURL(c.UpstreamBaseURL) {
			return fmt.Errorf("upstream_base_url must be %s for local_apple_container", localAppleContainerURL)
		}
		if c.Credentials.Source != CredentialsSourceKeychain || c.Credentials.File != "" {
			return errors.New("credentials.source must be keychain for local_apple_container")
		}
		if c.Credentials.RemoteAuthenticationProfile() != LocalAuthenticationOpenCodexAPIKey || c.Credentials.AllowInsecurePrivateIP {
			return errors.New("local_apple_container requires the fixed local API authentication profile")
		}
	default:
		return errors.New("upstream_mode must be external_gateway, local_opencodex, or a derived local_apple_container profile")
	}
	if c.Credentials.Source == CredentialsSourceFile && c.Credentials.File == "" {
		return errors.New("credentials.file is required for file credentials")
	}
	webSocketMode := c.Responses.WebSocketMode
	if webSocketMode == "" {
		webSocketMode = ResponsesWebSocketModePassthrough
	}
	if webSocketMode != ResponsesWebSocketModePassthrough && webSocketMode != ResponsesWebSocketModeHTTPFallback {
		return errors.New("responses.websocket_mode must be passthrough or http_fallback")
	}
	seenModels := make([]string, 0, len(c.Responses.ModelModes))
	for model, mode := range c.Responses.ModelModes {
		if model == "" {
			return errors.New("responses.model_modes keys must not be empty")
		}
		if strings.TrimSpace(model) != model {
			return fmt.Errorf("responses.model_modes key %q must not contain surrounding whitespace", model)
		}
		for _, previous := range seenModels {
			if strings.EqualFold(previous, model) {
				return fmt.Errorf("responses.model_modes keys %q and %q must be unique case-insensitively", previous, model)
			}
		}
		seenModels = append(seenModels, model)
		if mode != ResponsesModelModeBoundedJSON {
			return fmt.Errorf("responses.model_modes[%q] must be bounded_json", model)
		}
	}
	if len(c.Responses.ModelModes) > 0 && webSocketMode != ResponsesWebSocketModeHTTPFallback {
		return errors.New("responses.websocket_mode must be http_fallback when model_modes are configured")
	}
	scheduler := c.Responses.Scheduler
	scheduler.applyDefaults(c.ListenAddress)
	if c.Scope() == InstallationScopeLocalDevelopment && scheduler.InteractiveListenAddress != LocalDevelopmentInteractiveListen {
		return fmt.Errorf("local_development interactive listener must be %s", LocalDevelopmentInteractiveListen)
	}
	interactiveHost, interactivePort, err := parseLoopbackListenAddress(
		"responses.scheduler.interactive_listen_address",
		scheduler.InteractiveListenAddress,
	)
	if err != nil {
		return err
	}
	if primaryHost == interactiveHost && primaryPort == interactivePort {
		return errors.New("responses.scheduler.interactive_listen_address must differ from listen_address")
	}
	if err := validateIntRange("responses.scheduler.max_classifications", scheduler.MaxClassifications, 1, 64); err != nil {
		return err
	}
	if err := validateIntRange("responses.scheduler.max_pending_requests", scheduler.MaxPendingRequests, 1, 256); err != nil {
		return err
	}
	if scheduler.MaxPendingEncodedBytes < minResponsesPendingBytes || scheduler.MaxPendingEncodedBytes > maxResponsesPendingBytes {
		return fmt.Errorf(
			"responses.scheduler.max_pending_encoded_bytes must be between %d and %d",
			minResponsesPendingBytes,
			maxResponsesPendingBytes,
		)
	}
	if err := validateIntRange("responses.scheduler.queue_timeout_ms", scheduler.QueueTimeoutMS, 1_000, 300_000); err != nil {
		return err
	}
	if err := validateIntRange("responses.scheduler.max_general_upstream", scheduler.MaxGeneralUpstream, 1, 64); err != nil {
		return err
	}
	if err := validateIntRange("responses.scheduler.interactive_reserved_upstream", scheduler.InteractiveReservedUpstream, 1, 16); err != nil {
		return err
	}
	if err := validateIntRange("responses.scheduler.max_concurrent_transforms", scheduler.MaxConcurrentTransforms, 1, 16); err != nil {
		return err
	}
	if err := validateIntRange("responses.scheduler.max_open_deliveries", scheduler.MaxOpenDeliveries, 1, 256); err != nil {
		return err
	}
	catalogOwner := c.Catalog.Owner
	if catalogOwner == "" {
		catalogOwner = CatalogOwnerRelay
	}
	if catalogOwner != CatalogOwnerRelay && catalogOwner != CatalogOwnerRemoteManager {
		return errors.New("catalog.owner must be relay or remote_manager")
	}
	if upstreamMode == UpstreamModeLocalOpenCodex && catalogOwner != CatalogOwnerRemoteManager && !(c.localProfileRuntime && catalogOwner == CatalogOwnerRelay) {
		return errors.New("catalog.owner must be remote_manager for legacy local_opencodex")
	}
	if upstreamMode == UpstreamModeLocalAppleContainer && catalogOwner != CatalogOwnerRelay {
		return errors.New("catalog.owner must be relay for local_apple_container")
	}
	if upstreamMode == UpstreamModeExternalGateway && catalogOwner != CatalogOwnerRelay {
		return errors.New("catalog.owner must be relay for external_gateway")
	}
	if c.ConnectionProbe.Enabled && upstreamMode != UpstreamModeExternalGateway {
		return errors.New("connection_probe.enabled requires external_gateway")
	}
	if c.Catalog.Path == "" {
		return errors.New("catalog.path is required")
	}
	if c.Scope() == InstallationScopeLocalDevelopment {
		if !filepath.IsAbs(c.Catalog.Path) || filepath.Clean(c.Catalog.Path) != c.Catalog.Path {
			return errors.New("local_development catalog.path must be a clean absolute path")
		}
		expectedCatalogName := LocalDevelopmentExternalCatalog
		if c.localProfileRuntime {
			expectedCatalogName = LocalDevelopmentLocalCatalog
		} else if c.localAppleContainerRuntime {
			expectedCatalogName = LocalDevelopmentAppleCatalog
		}
		if filepath.Base(c.Catalog.Path) != expectedCatalogName {
			return fmt.Errorf("local_development catalog.path must end in %s", expectedCatalogName)
		}
		if !c.localProfileRuntime && c.LocalOpenCodex != nil && filepath.Base(c.LocalOpenCodex.CatalogPath) != LocalDevelopmentLocalCatalog {
			return fmt.Errorf("local_development local_opencodex.catalog_path must end in %s", LocalDevelopmentLocalCatalog)
		}
		if !c.localAppleContainerRuntime && c.LocalAppleContainer != nil && filepath.Base(c.LocalAppleContainer.CatalogPath) != LocalDevelopmentAppleCatalog {
			return fmt.Errorf("local_development local_apple_container.catalog_path must end in %s", LocalDevelopmentAppleCatalog)
		}
	}
	if c.Catalog.AppServerHome != "" {
		if !filepath.IsAbs(c.Catalog.AppServerHome) || filepath.Clean(c.Catalog.AppServerHome) != c.Catalog.AppServerHome {
			return errors.New("catalog.app_server_home must be a clean absolute path")
		}
	}
	if c.Catalog.CodexExecutable == "" {
		return errors.New("catalog.codex_executable is required")
	}
	if err := c.validateLocalOpenCodexProfile(upstreamMode); err != nil {
		return err
	}
	if err := c.validateLocalAppleContainerProfile(upstreamMode); err != nil {
		return err
	}
	if !c.localProfileRuntime && !c.localAppleContainerRuntime {
		paths := []string{c.Catalog.Path}
		if c.LocalOpenCodex != nil {
			paths = append(paths, c.LocalOpenCodex.CatalogPath)
		}
		if c.LocalAppleContainer != nil {
			paths = append(paths, c.LocalAppleContainer.CatalogPath)
		}
		if err := validateCatalogArtifactSeparation(paths...); err != nil {
			return err
		}
	}
	_, err = c.RefreshEvery()
	return err
}

func (c Config) validateLocalOpenCodexProfile(upstreamMode string) error {
	if c.localProfileRuntime {
		// The profile was already validated when its durable external config was
		// loaded. This ephemeral clone must be local + relay-owned and is never
		// written back as relay.json.
		if c.LocalOpenCodex == nil || upstreamMode != UpstreamModeLocalOpenCodex || c.Catalog.Owner != CatalogOwnerRelay {
			return errors.New("invalid local_opencodex runtime profile")
		}
		return nil
	}
	if c.LocalOpenCodex == nil {
		return nil
	}
	if upstreamMode != UpstreamModeExternalGateway {
		return errors.New("local_opencodex profile requires external_gateway as the canonical relay topology")
	}
	if !IsLocalOpenCodexBaseURL(c.LocalOpenCodex.UpstreamBaseURL) {
		return fmt.Errorf("local_opencodex.upstream_base_url must be %s or %s", localOpenCodexIPv4URL, localOpenCodexIPv6URL)
	}
	path := c.LocalOpenCodex.CatalogPath
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("local_opencodex.catalog_path must be a clean absolute path")
	}
	if c.Catalog.Path == "" || !filepath.IsAbs(c.Catalog.Path) || filepath.Clean(c.Catalog.Path) != c.Catalog.Path {
		return errors.New("catalog.path must be a clean absolute path when local_opencodex is configured")
	}
	return nil
}

func (c Config) validateLocalAppleContainerProfile(upstreamMode string) error {
	if c.localAppleContainerRuntime {
		if c.LocalAppleContainer == nil || upstreamMode != UpstreamModeLocalAppleContainer || c.Catalog.Owner != CatalogOwnerRelay {
			return errors.New("invalid local_apple_container runtime profile")
		}
		return nil
	}
	if c.LocalAppleContainer == nil {
		return nil
	}
	if upstreamMode != UpstreamModeExternalGateway {
		return errors.New("local_apple_container profile requires external_gateway as the canonical relay topology")
	}
	if !IsLocalAppleContainerBaseURL(c.LocalAppleContainer.UpstreamBaseURL) {
		return fmt.Errorf("local_apple_container.upstream_base_url must be %s", localAppleContainerURL)
	}
	path := c.LocalAppleContainer.CatalogPath
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("local_apple_container.catalog_path must be a clean absolute path")
	}
	expectedName := LocalAppleContainerCatalog
	if c.Scope() == InstallationScopeLocalDevelopment {
		expectedName = LocalDevelopmentAppleCatalog
	}
	if filepath.Base(path) != expectedName {
		return fmt.Errorf("local_apple_container.catalog_path must end in %s", expectedName)
	}
	if strings.TrimSpace(c.LocalAppleContainer.CredentialAccount) != c.LocalAppleContainer.CredentialAccount {
		return errors.New("local_apple_container.credential_account must not contain surrounding whitespace")
	}
	return nil
}

// validateCatalogArtifactSeparation rejects both direct catalog aliases and
// aliases among writer-owned sidecars. A Local profile uses a separate writer
// from the canonical External profile, so sharing even a restart marker or
// previous-catalog backup would violate the single-writer contract.
//
// This is intentionally conservative for case-only spellings. We do not
// probe or mutate the filesystem to discover its case behavior, so EqualFold
// prevents an unsafe dual profile on the common case-insensitive volume while
// merely rejecting an otherwise unusual case-sensitive spelling.
func validateCatalogArtifactSeparation(paths ...string) error {
	for firstIndex, firstPath := range paths {
		firstArtifacts := catalogArtifactPaths(firstPath)
		for secondIndex := firstIndex + 1; secondIndex < len(paths); secondIndex++ {
			secondArtifacts := catalogArtifactPaths(paths[secondIndex])
			for _, firstArtifact := range firstArtifacts {
				canonicalFirst, err := canonicalCatalogArtifactPath(firstArtifact)
				if err != nil {
					return errors.New("catalog artifact path cannot be resolved safely")
				}
				for _, secondArtifact := range secondArtifacts {
					canonicalSecond, err := canonicalCatalogArtifactPath(secondArtifact)
					if err != nil {
						return errors.New("catalog artifact path cannot be resolved safely")
					}
					if strings.EqualFold(canonicalFirst, canonicalSecond) {
						return errors.New("external, local_opencodex, and local_apple_container catalog artifacts must be distinct")
					}
					if same, err := sameExistingFile(firstArtifact, secondArtifact); err != nil {
						return errors.New("catalog artifact path cannot be inspected safely")
					} else if same {
						return errors.New("external, local_opencodex, and local_apple_container catalog artifacts must be distinct")
					}
				}
			}
		}
	}
	return nil
}

func catalogArtifactPaths(path string) []string {
	return []string{
		path,
		path + CatalogRestartPendingSuffix,
		path + CatalogPreviousSuffix,
	}
}

// canonicalCatalogArtifactPath resolves every existing portion of an
// absolute clean path. A missing leaf is permitted because a catalog may not
// be materialized yet, but a broken symlink or inaccessible existing ancestor
// is rejected rather than being treated as a lexical path.
func canonicalCatalogArtifactPath(path string) (string, error) {
	current := path
	var missingSuffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missingSuffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missingSuffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", os.ErrNotExist
		}
		missingSuffix = append(missingSuffix, filepath.Base(current))
		current = parent
	}
}

func sameExistingFile(firstPath, secondPath string) (bool, error) {
	first, err := os.Stat(firstPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	second, err := os.Stat(secondPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(first, second), nil
}

func parseLoopbackListenAddress(field string, address string) (string, int, error) {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("invalid %s: %w", field, err)
	}
	if host != "127.0.0.1" && host != "::1" {
		return "", 0, fmt.Errorf("%s must bind a numeric loopback address", field)
	}
	if rawPort == "" || strings.IndexFunc(rawPort, func(character rune) bool {
		return character < '0' || character > '9'
	}) >= 0 {
		return "", 0, fmt.Errorf("%s port must be numeric", field)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65_535 {
		return "", 0, fmt.Errorf("%s port must be between 1 and 65535", field)
	}
	return host, port, nil
}

func validateIntRange(field string, value int, minimum int, maximum int) error {
	if value < minimum || value > maximum {
		return fmt.Errorf("%s must be between %d and %d", field, minimum, maximum)
	}
	return nil
}

func (c *ResponsesSchedulerConfig) applyDefaults(primaryListenAddress string) {
	if c.InteractiveListenAddress == "" {
		if host, _, err := net.SplitHostPort(primaryListenAddress); err == nil && (host == "127.0.0.1" || host == "::1") {
			c.InteractiveListenAddress = net.JoinHostPort(host, strconv.Itoa(DefaultResponsesInteractiveListenPort))
		}
	}
	if c.MaxClassifications == 0 {
		c.MaxClassifications = DefaultResponsesMaxClassifications
	}
	if c.MaxPendingRequests == 0 {
		c.MaxPendingRequests = DefaultResponsesMaxPendingRequests
	}
	if c.MaxPendingEncodedBytes == 0 {
		c.MaxPendingEncodedBytes = DefaultResponsesMaxPendingEncodedBytes
	}
	if c.QueueTimeoutMS == 0 {
		c.QueueTimeoutMS = DefaultResponsesQueueTimeoutMS
	}
	if c.MaxGeneralUpstream == 0 {
		c.MaxGeneralUpstream = DefaultResponsesMaxGeneralUpstream
	}
	if c.InteractiveReservedUpstream == 0 {
		c.InteractiveReservedUpstream = DefaultResponsesInteractiveReservedUpstream
	}
	if c.MaxConcurrentTransforms == 0 {
		c.MaxConcurrentTransforms = DefaultResponsesMaxConcurrentTransforms
	}
	if c.MaxOpenDeliveries == 0 {
		c.MaxOpenDeliveries = DefaultResponsesMaxOpenDeliveries
	}
}

func (c *Config) applyDefaults() {
	if c.UpstreamMode == "" {
		c.UpstreamMode = UpstreamModeExternalGateway
	}
	if c.Responses.WebSocketMode == "" {
		c.Responses.WebSocketMode = ResponsesWebSocketModePassthrough
	}
	c.Responses.Scheduler.applyDefaults(c.ListenAddress)
	if c.Catalog.Owner == "" {
		c.Catalog.Owner = CatalogOwnerRelay
	}
}
