package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

type countedHandoffExecutor struct {
	calls int
}

func (e *countedHandoffExecutor) ExecuteExpected(_ context.Context, _ string, _ string, _ handoff.Action, _ bool) (handoff.Record, error) {
	e.calls++
	return handoff.Record{}, nil
}

func TestParseHandoffActionAllowsOnlyRetainHandoffs(t *testing.T) {
	for _, test := range []struct {
		value string
		want  handoff.Action
		ok    bool
	}{
		{value: string(handoff.RetainProxyRemoveShim), want: handoff.RetainProxyRemoveShim, ok: true},
		{value: string(handoff.RetainProxyKeepShim), want: handoff.RetainProxyKeepShim, ok: true},
		{value: "uninstall"},
		{value: ""},
		{value: "retain_proxy"},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseHandoffAction(test.value)
			if test.ok {
				if err != nil || got != test.want {
					t.Fatalf("parseHandoffAction(%q) = %q, %v", test.value, got, err)
				}
				return
			}
			if !errors.Is(err, handoff.ErrInvalidAction) || got != "" {
				t.Fatalf("parseHandoffAction(%q) = %q, %v; want invalid", test.value, got, err)
			}
		})
	}
}

func TestRecoveryRequiredStopsHandoffBeforeExecutor(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	store, err := routing.Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := routing.NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = routing.BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = routing.PhaseRecoveryRequired
	executor := &countedHandoffExecutor{}
	_, err = executeHandoffAfterRoutingPreflight(
		context.Background(), store, state, codexPath,
		"/not/reached/ocx", strings.Repeat("a", 64),
		handoff.RetainProxyRemoveShim, executor,
	)
	if !errors.Is(err, routing.ErrRecoveryRequired) {
		t.Fatalf("handoff error = %v, want routing recovery", err)
	}
	if executor.calls != 0 {
		t.Fatalf("recovery state invoked OCX executor %d time(s)", executor.calls)
	}
	envelope := safeOperationError(err)
	if envelope.Error.Code != "routing_recovery_required" {
		t.Fatalf("handoff envelope = %#v", envelope)
	}
}

func TestRelayctlUsageOmitsLegacyHandoffUninstall(t *testing.T) {
	var output strings.Builder
	writeUsage(&output)
	usage := output.String()
	if !strings.Contains(usage, "--action retain_proxy_remove_shim|retain_proxy_keep_shim") {
		t.Fatalf("usage omits approved handoff actions: %q", usage)
	}
	if strings.Contains(usage, "retain_proxy_keep_shim|uninstall") {
		t.Fatalf("usage advertises legacy uninstall: %q", usage)
	}
	for _, required := range []string{
		"mode repair-native", "--expected-routing-generation N", "--confirm-local-development-native-repair",
		"mode inspect-native-repair", "mode inspect-native-repair-owner", "mode repair-native-routing", "--expected-owner local_relay|opencodex",
		"--installation-id ID", "--installation-fingerprint SHA256", "--native-restore-fingerprint SHA256",
		"--confirm-local-development-native-routing-repair",
	} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omits local-development native repair contract %q: %q", required, usage)
		}
	}
}

func TestValidateHandoffResultOwnershipAcceptsOnlyNativeOrExactRelayProfiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(t *testing.T, codexPath string, cfg config.Config)
		want   handoffCodexOwnership
		reject bool
	}{
		{
			name: "native",
			setup: func(_ *testing.T, _ string, _ config.Config) {
				// A missing Codex configuration is native; handoff must not create it.
			},
			want: handoffCodexOwnershipNative,
		},
		{
			name: "exact external relay profile",
			setup: func(t *testing.T, codexPath string, cfg config.Config) {
				t.Helper()
				if err := codexconfig.EnableWithInteractiveProfile(
					codexPath,
					"http://"+cfg.ListenAddress+"/v1",
					"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
					cfg.Catalog.Path,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: handoffCodexOwnershipExternal,
		},
		{
			name: "exact local relay profile",
			setup: func(t *testing.T, codexPath string, cfg config.Config) {
				t.Helper()
				local, err := cfg.LocalOpenCodexRuntimeConfig()
				if err != nil {
					t.Fatal(err)
				}
				if err := codexconfig.EnableWithInteractiveProfile(
					codexPath,
					"http://"+local.ListenAddress+"/v1",
					"http://"+local.Responses.Scheduler.InteractiveListenAddress+"/v1",
					local.Catalog.Path,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: handoffCodexOwnershipLocalOpenCodex,
		},
		{
			name: "foreign OpenCodex shim routing",
			setup: func(t *testing.T, codexPath string, _ config.Config) {
				t.Helper()
				if err := os.WriteFile(codexPath, []byte("openai_base_url = \"http://127.0.0.1:10100/v1\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			reject: true,
		},
		{
			name: "foreign root model catalog",
			setup: func(t *testing.T, codexPath string, _ config.Config) {
				t.Helper()
				if err := os.WriteFile(codexPath, []byte("model_catalog_json = \"/foreign-catalog.json\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			reject: true,
		},
		{
			name: "marker profile with catalog drift",
			setup: func(t *testing.T, codexPath string, cfg config.Config) {
				t.Helper()
				if err := codexconfig.EnableWithInteractiveProfile(
					codexPath,
					"http://"+cfg.ListenAddress+"/v1",
					"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
					filepath.Join(filepath.Dir(codexPath), "unexpected-catalog.json"),
				); err != nil {
					t.Fatal(err)
				}
			},
			reject: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			codexPath := filepath.Join(directory, "config.toml")
			cfg := handoffOwnershipTestConfig(t, directory)
			test.setup(t, codexPath, cfg)

			before, err := os.ReadFile(codexPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			got, err := validateHandoffResultOwnership(codexPath, codexconfig.ProductionOwner, cfg)
			if test.reject {
				if err == nil || got != handoffCodexOwnershipUnknown {
					t.Fatalf("ownership = %v, %v; want rejected", got, err)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("ownership = %v, %v; want %v", got, err, test.want)
			}
			after, readErr := os.ReadFile(codexPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if string(after) != string(before) {
				t.Fatal("post-handoff ownership validation rewrote Codex TOML")
			}
		})
	}
}

func TestPreflightHandoffCodexConfigAcceptsMissingOrRegularLeavesOnly(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.toml")
	if err := preflightHandoffCodexConfig(missing); err != nil {
		t.Fatalf("missing handoff config was rejected: %v", err)
	}

	regular := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(regular, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preflightHandoffCodexConfig(regular); err != nil {
		t.Fatalf("regular handoff config was rejected: %v", err)
	}
	if err := preflightHandoffCodexConfig(directory); err == nil {
		t.Fatal("handoff config directory was accepted")
	}

	symlink := filepath.Join(directory, "config-link.toml")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if err := preflightHandoffCodexConfig(symlink); err == nil {
		t.Fatal("handoff config symlink was accepted")
	}
}

func TestExecuteHandoffRejectsUnsafeCodexLeafBeforeOpenCodexInvocation(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(regular, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "config-link.toml")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []struct {
		name string
		path string
	}{
		{name: "directory", path: directory},
		{name: "symlink", path: symlink},
	} {
		t.Run(unsafe.name, func(t *testing.T) {
			executor := &countedHandoffExecutor{}
			if _, err := executeHandoff(context.Background(), unsafe.path, "/owner-only/ocx", "fingerprint", handoff.RetainProxyKeepShim, executor); !errors.Is(err, errHandoffCodexConfigPreflight) {
				t.Fatalf("unsafe Codex config error = %v, want Codex leaf preflight", err)
			}
			if executor.calls != 0 {
				t.Fatalf("unsafe Codex config invoked OpenCodex %d times", executor.calls)
			}
		})
	}

	executor := &countedHandoffExecutor{}
	if _, err := executeHandoff(context.Background(), regular, "/owner-only/ocx", "fingerprint", handoff.RetainProxyKeepShim, executor); err != nil {
		t.Fatalf("regular Codex config did not reach handoff execution: %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("regular Codex config invoked OpenCodex %d times, want 1", executor.calls)
	}
}

func TestHandoffOwnershipMustMatchDurableAppliedBackend(t *testing.T) {
	for _, test := range []struct {
		ownership handoffCodexOwnership
		backend   routing.Backend
		want      bool
	}{
		{handoffCodexOwnershipNative, routing.BackendNone, true},
		{handoffCodexOwnershipExternal, routing.BackendExternal, true},
		{handoffCodexOwnershipLocalOpenCodex, routing.BackendLocalOpenCodex, true},
		{handoffCodexOwnershipNative, routing.BackendExternal, false},
		{handoffCodexOwnershipExternal, routing.BackendLocalOpenCodex, false},
		{handoffCodexOwnershipUnknown, routing.BackendNone, false},
	} {
		if got := handoffOwnershipMatchesAppliedBackend(test.ownership, test.backend); got != test.want {
			t.Fatalf("matches(%v, %v) = %t, want %t", test.ownership, test.backend, got, test.want)
		}
	}
}

func handoffOwnershipTestConfig(t *testing.T, directory string) config.Config {
	t.Helper()
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceKeychain)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "127.0.0.1:18180"
	cfg.Responses.Scheduler.InteractiveListenAddress = "127.0.0.1:18182"
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	cfg.LocalOpenCodex = &config.LocalOpenCodexProfile{
		UpstreamBaseURL: "http://127.0.0.1:10100/v1",
		CatalogPath:     filepath.Join(directory, "local-catalog.json"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}
