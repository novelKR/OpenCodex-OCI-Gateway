package containerruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
)

const securityExecutable = "/usr/bin/security"

// SystemKeychain owns exactly two fixed generic-password services. Token
// creation uses security(1)'s stdin prompt form: no credential is ever placed
// in argv, relay.json, a receipt, or an error.
type SystemKeychain struct{ runner commandRunner }

func NewSystemKeychain() *SystemKeychain { return &SystemKeychain{runner: systemCommandRunner{}} }

func newSystemKeychainWithRunner(runner commandRunner) *SystemKeychain {
	return &SystemKeychain{runner: runner}
}

func (k *SystemKeychain) Load(ctx context.Context, account string) (Secrets, error) {
	if k == nil || k.runner == nil || !validKeychainAccount(account) {
		return Secrets{}, ErrCredential
	}
	api, err := k.loadOne(ctx, account, APIKeychainService)
	if err != nil {
		return Secrets{}, ErrCredential
	}
	admin, err := k.loadOne(ctx, account, AdminKeychainService)
	if err != nil {
		zeroBytes(api)
		return Secrets{}, ErrCredential
	}
	if !validSecret(api) || !validSecret(admin) || bytes.Equal(api, admin) {
		zeroBytes(api)
		zeroBytes(admin)
		return Secrets{}, ErrCredential
	}
	return Secrets{APIToken: api, AdminToken: admin}, nil
}

func (k *SystemKeychain) Ensure(ctx context.Context, account string) (Secrets, error) {
	if k == nil || k.runner == nil || !validKeychainAccount(account) {
		return Secrets{}, ErrCredential
	}
	api, err := k.loadOrCreate(ctx, account, APIKeychainService)
	if err != nil {
		return Secrets{}, ErrCredential
	}
	admin, err := k.loadOrCreate(ctx, account, AdminKeychainService)
	if err != nil {
		zeroBytes(api)
		return Secrets{}, ErrCredential
	}
	if !validSecret(api) || !validSecret(admin) || bytes.Equal(api, admin) {
		zeroBytes(api)
		zeroBytes(admin)
		return Secrets{}, ErrCredential
	}
	return Secrets{APIToken: api, AdminToken: admin}, nil
}

func (k *SystemKeychain) loadOrCreate(ctx context.Context, account, service string) ([]byte, error) {
	if value, err := k.loadOne(ctx, account, service); err == nil {
		return value, nil
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, ErrCredential
	}
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(token)))
	base64.RawURLEncoding.Encode(encoded, token)
	zeroBytes(token)
	input := append(append([]byte(nil), encoded...), '\n')
	output, err := k.runner.Run(ctx, securityExecutable, []string{
		"add-generic-password", "-a", account, "-s", service, "-w",
	}, bytes.NewReader(input), 4<<10)
	zeroBytes(input)
	zeroBytes(output.stdout)
	zeroBytes(output.stderr)
	if err != nil {
		zeroBytes(encoded)
		return nil, ErrCredential
	}
	// Read back the Keychain item. This detects a prompt/CLI behavior change
	// and binds the returned bytes to the persisted current-user account.
	readBack, err := k.loadOne(ctx, account, service)
	if err != nil || !bytes.Equal(readBack, encoded) {
		zeroBytes(encoded)
		zeroBytes(readBack)
		return nil, ErrCredential
	}
	zeroBytes(encoded)
	return readBack, nil
}

func (k *SystemKeychain) loadOne(ctx context.Context, account, service string) ([]byte, error) {
	output, err := k.runner.Run(ctx, securityExecutable, []string{
		"find-generic-password", "-a", account, "-s", service, "-w",
	}, nil, 4<<10)
	zeroBytes(output.stderr)
	if err != nil {
		zeroBytes(output.stdout)
		return nil, ErrCredential
	}
	value := bytes.TrimSpace(output.stdout)
	result := append([]byte(nil), value...)
	zeroBytes(output.stdout)
	if !validSecret(result) {
		zeroBytes(result)
		return nil, ErrCredential
	}
	return result, nil
}

func validKeychainAccount(account string) bool {
	if account == "" || len(account) > 256 || account[0] == '-' || strings.TrimSpace(account) != account {
		return false
	}
	for _, character := range account {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
