// Package credentials resolves the three data-plane secrets without ever
// persisting or logging their values in the relay configuration.
package credentials

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"syscall"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

const (
	CFClientIDName     = "CF_ACCESS_CLIENT_ID"
	CFClientSecretName = "CF_ACCESS_CLIENT_SECRET"
	GatewayKeyName     = "OPENCODEX_GATEWAY_API_KEY"

	CFClientIDService           = "opencodex-relay-cf-access-client-id"
	CFClientSecretService       = "opencodex-relay-cf-access-client-secret"
	GatewayKeyService           = "opencodex-relay-gateway-api-key"
	LocalOpenCodexAPIKeyService = "opencodex-relay-apple-container-api-auth-token"
)

type Values struct {
	CFClientID           string
	CFClientSecret       string
	GatewayKey           string
	LocalOpenCodexAPIKey string
}

func (v Values) Validate() error {
	return v.ValidateForProfile(config.RemoteAuthenticationCloudflareAccessAndGatewayKey)
}

func (v Values) ValidateForProfile(profile string) error {
	switch profile {
	case config.RemoteAuthenticationNone:
		return nil
	case config.RemoteAuthenticationGatewayAPIKey:
		if v.GatewayKey == "" {
			return errors.New("gateway API key is required")
		}
	case config.RemoteAuthenticationCloudflareAccessAndGatewayKey:
		if v.CFClientID == "" || v.CFClientSecret == "" || v.GatewayKey == "" {
			return errors.New("all Cloudflare and gateway credentials are required")
		}
	case config.LocalAuthenticationOpenCodexAPIKey:
		decoded, err := base64.RawURLEncoding.DecodeString(v.LocalOpenCodexAPIKey)
		if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != v.LocalOpenCodexAPIKey {
			return errors.New("local OpenCodex API key must be an unpadded base64url-encoded 32-byte value")
		}
	default:
		return errors.New("unknown authentication profile")
	}
	return nil
}

func Load(cfg config.CredentialsConfig) (Values, error) {
	return LoadContext(context.Background(), cfg)
}

// LoadContext bounds Keychain subprocess access to the caller's request or
// lifecycle operation. In particular, an Apple runtime reader/writer lease
// must never survive a canceled security(1) authorization prompt.
func LoadContext(ctx context.Context, cfg config.CredentialsConfig) (Values, error) {
	if ctx == nil {
		return Values{}, errors.New("credential context is required")
	}
	if err := ctx.Err(); err != nil {
		return Values{}, fmt.Errorf("credential access cancelled: %w", err)
	}
	profile := cfg.RemoteAuthenticationProfile()
	if profile == config.RemoteAuthenticationNone {
		return Values{}, nil
	}
	var values Values
	var err error
	switch cfg.Source {
	case config.CredentialsSourceKeychain:
		if runtime.GOOS != "darwin" {
			return Values{}, errors.New("keychain credentials are supported only on macOS")
		}
		if profile == config.LocalAuthenticationOpenCodexAPIKey {
			current, currentErr := user.Current()
			if currentErr != nil || current.Username == "" || (cfg.Account != "" && cfg.Account != current.Username) {
				return Values{}, errors.New("local OpenCodex API key account must be the current user")
			}
		}
		values, err = loadKeychain(ctx, cfg.Account, profile)
	case config.CredentialsSourceFile:
		values, err = loadFile(cfg.File)
	case config.CredentialsSourceNone:
		return Values{}, errors.New("credential source none requires authentication profile none")
	default:
		return Values{}, errors.New("unknown credential source")
	}
	if err != nil {
		return Values{}, err
	}
	if err := values.ValidateForProfile(profile); err != nil {
		return Values{}, err
	}
	return values, nil
}

func ResolveKeychainAccount(account string) (string, error) {
	if account != "" {
		return account, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve Keychain account: %w", err)
	}
	if current.Username == "" {
		return "", errors.New("Keychain account is empty")
	}
	return current.Username, nil
}

func loadKeychain(ctx context.Context, account, profile string) (Values, error) {
	account, err := ResolveKeychainAccount(account)
	if err != nil {
		return Values{}, err
	}
	read := func(service string) (string, error) {
		command := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w")
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("read Keychain item %q: %w", service, err)
		}
		value := strings.TrimSpace(string(output))
		if value == "" {
			return "", fmt.Errorf("Keychain item %q is empty", service)
		}
		return value, nil
	}
	values := Values{}
	if profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		values.CFClientID, err = read(CFClientIDService)
		if err != nil {
			return Values{}, err
		}
		values.CFClientSecret, err = read(CFClientSecretService)
		if err != nil {
			return Values{}, err
		}
	}
	if profile == config.RemoteAuthenticationGatewayAPIKey || profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		values.GatewayKey, err = read(GatewayKeyService)
		if err != nil {
			return Values{}, err
		}
	}
	if profile == config.LocalAuthenticationOpenCodexAPIKey {
		values.LocalOpenCodexAPIKey, err = read(LocalOpenCodexAPIKeyService)
		if err != nil {
			return Values{}, err
		}
	}
	return values, values.ValidateForProfile(profile)
}

func loadFile(path string) (Values, error) {
	if path == "" {
		return Values{}, errors.New("credential file path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Values{}, fmt.Errorf("inspect credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Values{}, errors.New("credential file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Values{}, errors.New("credential file must not be accessible to group or others")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return Values{}, errors.New("credential file must be owned by the current user")
	}
	file, err := os.Open(path)
	if err != nil {
		return Values{}, fmt.Errorf("open credential file: %w", err)
	}
	defer file.Close()
	allowed := map[string]*string{}
	values := Values{}
	allowed[CFClientIDName] = &values.CFClientID
	allowed[CFClientSecretName] = &values.CFClientSecret
	allowed[GatewayKeyName] = &values.GatewayKey
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return Values{}, errors.New("credential file contains a malformed line")
		}
		name := strings.TrimSpace(parts[0])
		target, ok := allowed[name]
		if !ok {
			return Values{}, fmt.Errorf("credential file contains unsupported variable %q", name)
		}
		value := strings.TrimSpace(parts[1])
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if value == "" {
			return Values{}, fmt.Errorf("credential %q is empty", name)
		}
		*target = value
	}
	if err := scanner.Err(); err != nil {
		return Values{}, fmt.Errorf("read credential file: %w", err)
	}
	return values, nil
}
