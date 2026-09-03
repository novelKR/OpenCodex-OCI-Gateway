package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

func TestLoadContextRejectsCancelledCredentialAccessBeforeIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadContext(ctx, config.CredentialsConfig{
		Source: config.CredentialsSourceNone,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled credential load error = %v", err)
	}
}

func TestLoadNoneDoesNotRequireOuterCredentials(t *testing.T) {
	values, err := Load(config.CredentialsConfig{Source: config.CredentialsSourceNone})
	if err != nil {
		t.Fatalf("load no credentials: %v", err)
	}
	if values != (Values{}) {
		t.Fatalf("none source returned credential values: %#v", values)
	}
}

func TestLoadFileAcceptsOnlyTheThreeRequiredValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	content := "CF_ACCESS_CLIENT_ID=id\nCF_ACCESS_CLIENT_SECRET='secret'\nOPENCODEX_GATEWAY_API_KEY=key\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := Load(config.CredentialsConfig{Source: "file", File: path})
	if err != nil {
		t.Fatal(err)
	}
	if values.CFClientID != "id" || values.CFClientSecret != "secret" || values.GatewayKey != "key" {
		t.Fatalf("values = %#v", values)
	}
}

func TestLoadFileValidatesOnlySelectedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte("OPENCODEX_GATEWAY_API_KEY=key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := Load(config.CredentialsConfig{
		Source:                config.CredentialsSourceFile,
		File:                  path,
		AuthenticationProfile: config.RemoteAuthenticationGatewayAPIKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if values.GatewayKey != "key" || values.CFClientID != "" || values.CFClientSecret != "" {
		t.Fatalf("profile loaded unexpected values: %#v", values)
	}
	if _, err := Load(config.CredentialsConfig{Source: config.CredentialsSourceFile, File: path}); err == nil {
		t.Fatal("legacy full profile accepted a gateway-key-only file")
	}
}

func TestLoadFileRejectsShellSyntaxAndUnsafePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte("export CF_ACCESS_CLIENT_ID=id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(config.CredentialsConfig{Source: "file", File: path}); err == nil {
		t.Fatal("shell syntax was accepted")
	}
	if err := os.WriteFile(path, []byte("CF_ACCESS_CLIENT_ID=id\nCF_ACCESS_CLIENT_SECRET=secret\nOPENCODEX_GATEWAY_API_KEY=key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(config.CredentialsConfig{Source: "file", File: path}); err == nil {
		t.Fatal("group-readable credential file was accepted")
	}
}
