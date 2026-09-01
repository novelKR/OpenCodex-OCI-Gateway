package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/release"
)

func TestRelayctlUsageDocumentsFixedReleaseCheckContract(t *testing.T) {
	var output strings.Builder
	writeUsage(&output)
	usage := output.String()
	for _, required := range []string{
		"release check", "--channel stable|preview", "--current-version VERSION",
		"release stage", "--release-id ID", "--tag TAG", "--expected-manifest-sha256 SHA256",
		"--public-key ABSOLUTE_PATH", "--json",
	} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omits %q: %q", required, usage)
		}
	}
	for _, forbidden := range []string{"--repository", "--api-url", "--api-base-url"} {
		if strings.Contains(usage, forbidden) {
			t.Fatalf("usage exposes production trust boundary %q: %q", forbidden, usage)
		}
	}
}

func TestReleaseStageLifecycleGateRejectsDurableRecoveryArtifacts(t *testing.T) {
	home := t.TempDir()
	if err := requireReleaseStageLifecycleClean(home); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"Library/Application Support/OpenCodexRelay/application-relocation.json",
		"Library/Application Support/OpenCodexRelay/integration-journal.json",
		".config/opencodex-relay/relay.json.routing-transaction.json",
		".config/opencodex-relay/relay.json.open-codex-removal.json",
		"Library/Application Support/OpenCodexRelayLifecycle/standalone-native.open-codex-removal.json",
	} {
		t.Run(relative, func(t *testing.T) {
			path := filepath.Join(home, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := requireReleaseStageLifecycleClean(home); !errors.Is(err, release.ErrStageUnsafeFilesystem) {
				t.Fatalf("error = %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
