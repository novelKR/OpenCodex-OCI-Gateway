package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTierADiscoversExactNPMInstallationFromAbsolutePATH(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(home, "node")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")

	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier:                DiscoveryTierA,
		HomeDir:             home,
		PathEnv:             filepath.Join(prefix, "bin"),
		GOOS:                "darwin",
		GOARCH:              "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != discoverySchemaVersion || len(result.Candidates) != 1 {
		t.Fatalf("discovery result = %#v", result)
	}
	candidate := result.Candidates[0]
	if candidate.PackageRoot != root || candidate.Version != "2.22.0" || candidate.RemovalCapability != RemovalCapabilityExactNPM ||
		candidate.RemovalAuthority != RemovalAuthorityAutomatic || candidate.ExecutableSHA256 == "" || candidate.Fingerprint == "" || candidate.RequiresElevation || !candidate.UserWritable {
		t.Fatalf("candidate = %#v", candidate)
	}
	if len(candidate.Launchers) != 2 || candidate.NodeExecutable == "" || candidate.NPMCLI == "" {
		t.Fatalf("candidate launchers/pair = %#v", candidate)
	}
}

func TestNormalizeDiscoveryOptionsDoesNotRequireDefaultHomebrewWhenDefaultsAreSkipped(t *testing.T) {
	home := t.TempDir()
	options, err := normalizeDiscoveryOptions(DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.HomebrewPrefix != "" {
		t.Fatalf("Homebrew prefix = %q, want empty when default prefixes are skipped", options.HomebrewPrefix)
	}
}

func TestDiscoveryDowngradesUserOwnedGroupWritableParent(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(home, "homebrew-like")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	if err := os.Chmod(prefix, 0o775); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Rejected != 0 {
		t.Fatalf("Homebrew-like discovery result = %#v", result)
	}
	candidate := result.Candidates[0]
	if candidate.PackageRoot != root || candidate.RemovalCapability != RemovalCapabilityManual || candidate.RemovalAuthority != RemovalAuthorityManual {
		t.Fatalf("Homebrew-like candidate = %#v", candidate)
	}
	if candidate.NativeRestoreCapability != NativeRestoreCapabilityVerifiedSnapshot ||
		candidate.NativeRestoreFingerprint == "" || candidate.nativeRestoreProof == nil {
		t.Fatalf("manual removal candidate did not retain independent native restore proof: %#v", candidate)
	}
	foundWarning := false
	for _, warning := range candidate.Warnings {
		if warning == "writable_parent_chain" {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("candidate warnings = %#v", candidate.Warnings)
	}
}

func TestHomebrewGuardedNPMDiscoveryPreservesSelectorAcrossProtection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "homebrew")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	if err := os.Chmod(prefix, 0o775); err != nil {
		t.Fatal(err)
	}
	options := DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		HomebrewPrefix: prefix, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	}
	before, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(before.Candidates) != 1 {
		t.Fatalf("guarded discovery = %#v, %v", before, err)
	}
	candidate := before.Candidates[0]
	if before.SchemaVersion != discoverySchemaVersion || candidate.PackageRoot != root || candidate.Manager != DiscoveryManagerHomebrew ||
		candidate.RemovalCapability != RemovalCapabilityHomebrewGuardedNPM ||
		candidate.RemovalAuthority != RemovalAuthorityAutomatic || !candidate.HomebrewGuardRequired ||
		candidate.TeardownCapability != TeardownCapabilityRelayPreserveV1 ||
		candidate.DataCapability != DataCapabilitySelectiveTrashV1 ||
		candidate.TeardownCompatibility != teardownCompatibilityCompatible ||
		!containsString(candidate.Warnings, "homebrew_guard_required") {
		t.Fatalf("guarded candidate = %#v", candidate)
	}
	if err := validateAutomaticRemovalCandidateStaticContext(context.Background(), candidate, os.Geteuid()); err != nil {
		t.Fatalf("read-only guarded proof = %v", err)
	}
	if err := validateAutomaticRemovalCandidate(candidate, os.Geteuid()); !errors.Is(err, ErrRemovalCandidateChanged) {
		t.Fatalf("unguarded mutation proof = %v", err)
	}
	process := &recordedRemovalProcess{}
	if _, err := (ExactNPMRunner{HomeDir: home, process: process}).Inventory(context.Background(), candidate); err != nil {
		t.Fatalf("guarded inventory = %v", err)
	}
	if len(process.calls) != 1 || process.calls[0].program == candidate.BunExecutable {
		t.Fatalf("guarded inventory did not use private snapshot: %#v", process.calls)
	}

	if err := os.Chmod(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(after.Candidates) != 1 {
		t.Fatalf("protected discovery = %#v, %v", after, err)
	}
	protected := after.Candidates[0]
	if protected.ID != candidate.ID || protected.Fingerprint != candidate.Fingerprint ||
		protected.RemovalCapability != RemovalCapabilityHomebrewGuardedNPM || !protected.HomebrewGuardRequired {
		t.Fatalf("guard changed selector: before=%#v after=%#v", candidate, protected)
	}
	if err := validateAutomaticRemovalCandidate(protected, os.Geteuid()); err != nil {
		t.Fatalf("protected mutation proof = %v", err)
	}
}

func TestHomebrewGuardedNPMRejectsWorldWritablePrefix(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "homebrew")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	if err := os.Chmod(prefix, 0o777); err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		HomebrewPrefix: prefix, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if candidate.RemovalAuthority == RemovalAuthorityAutomatic ||
			candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM {
			t.Fatalf("world-writable candidate gained authority: %#v", candidate)
		}
	}
}

func TestHomebrewGuardedNPMRejectsExtendedACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ACL fixture")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "homebrew")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	if err := os.Chmod(prefix, 0o775); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/chmod", "+a", "everyone allow write", prefix).CombinedOutput(); err != nil {
		t.Fatalf("add ACL: %v (%s)", err, output)
	}
	defer exec.Command("/bin/chmod", "-a#", "0", prefix).Run()

	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		HomebrewPrefix: prefix, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("ACL discovery = %#v, %v", result, err)
	}
	candidate := result.Candidates[0]
	if candidate.RemovalCapability != RemovalCapabilityManual || candidate.RemovalAuthority != RemovalAuthorityManual ||
		candidate.HomebrewGuardRequired || !containsString(candidate.Warnings, "extended_acl") {
		t.Fatalf("ACL candidate gained guarded authority: %#v", candidate)
	}
}

func TestHomebrewGuardedNPMRejectsExtendedACLInExecutionTrees(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ACL fixture")
	}
	for _, test := range []struct {
		name   string
		target func(prefix, root string) string
	}{
		{
			name: "package dependency",
			target: func(_, root string) string {
				return filepath.Join(root, "node_modules", "transitive", "lib")
			},
		},
		{
			name: "npm dependency",
			target: func(prefix, _ string) string {
				return filepath.Join(prefix, "lib", "node_modules", "npm", "node_modules", "transitive")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Join(home, "homebrew")
			root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
			if err := os.Chmod(prefix, 0o775); err != nil {
				t.Fatal(err)
			}
			target := test.target(prefix, root)
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "fixture.js"), []byte("module.exports = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("/bin/chmod", "+a", "everyone allow write", target).CombinedOutput(); err != nil {
				t.Fatalf("add ACL: %v (%s)", err, output)
			}
			defer exec.Command("/bin/chmod", "-N", target).Run()

			result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
				HomebrewPrefix: prefix, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
			})
			if err != nil || len(result.Candidates) != 1 {
				t.Fatalf("ACL discovery = %#v, %v", result, err)
			}
			candidate := result.Candidates[0]
			if candidate.RemovalCapability != RemovalCapabilityManual || candidate.RemovalAuthority != RemovalAuthorityManual ||
				candidate.HomebrewGuardRequired || !containsString(candidate.Warnings, "extended_acl") {
				t.Fatalf("execution-tree ACL candidate gained guarded authority: %#v", candidate)
			}
		})
	}
}

func TestHomebrewGuardedNPMRejectsExecutionTreeACLAddedAfterDiscovery(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ACL fixture")
	}
	for _, tree := range []string{"package", "npm"} {
		t.Run(tree, func(t *testing.T) {
			restoreRegistries := preserveTeardownRegistries(t)
			defer restoreRegistries()
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Join(home, "homebrew")
			root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
			if err := os.Chmod(prefix, 0o775); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "node_modules", "transitive")
			if tree == "npm" {
				target = filepath.Join(prefix, "lib", "node_modules", "npm", "node_modules", "transitive")
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "fixture.js"), []byte("module.exports = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tree == "package" {
				modulePayload, err := os.ReadFile(filepath.Join(root, "src", "cli", "index.ts"))
				if err != nil {
					t.Fatal(err)
				}
				moduleDigest := sha256.Sum256(modulePayload)
				adapterID := "acl_fixture_" + tree
				profile := reviewedTestTeardownProfile(t, root, teardownAdapterProfile{
					packageName: OpenCodexPackageName,
					version:     "2.22.0",
					adapterID:   adapterID,
					requiredModules: map[string]string{
						"src/cli/index.ts": hex.EncodeToString(moduleDigest[:]),
					},
				})
				teardownAdapterProfiles = append(teardownAdapterProfiles, profile)
				teardownAdapterImplementations = append(teardownAdapterImplementations, teardownAdapterImplementation{
					adapterID: adapterID, entrypoint: "fixture.ts",
					sources: map[string][]byte{"fixture.ts": []byte("process.exit(1)\n")},
				})
			}
			result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
				HomebrewPrefix: prefix, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
			})
			if err != nil || len(result.Candidates) != 1 {
				t.Fatalf("guarded discovery = %#v, %v", result, err)
			}
			candidate := result.Candidates[0]
			if candidate.RemovalCapability != RemovalCapabilityHomebrewGuardedNPM {
				t.Fatalf("candidate = %#v", candidate)
			}
			if output, err := exec.Command("/bin/chmod", "+a", "everyone allow write", target).CombinedOutput(); err != nil {
				t.Fatalf("add ACL: %v (%s)", err, output)
			}
			defer exec.Command("/bin/chmod", "-N", target).Run()
			if err := validateAutomaticRemovalCandidateStaticContext(context.Background(), candidate, os.Geteuid()); !errors.Is(err, ErrRemovalCandidateChanged) {
				t.Fatalf("post-discovery ACL validation = %v", err)
			}
		})
	}
}

func TestNativeRestoreProofDoesNotChangeRemovalIdentity(t *testing.T) {
	_, _, candidate := nativeRestoreDiscoveryCandidate(t, false)
	withoutRestore := candidate
	withoutRestore.NativeRestoreCapability = ""
	withoutRestore.NativeRestoreFingerprint = ""
	withoutRestore.nativeRestoreProof = nil

	withProofMerged := mergeInstallations([]NPMInstallation{candidate})
	withoutProofMerged := mergeInstallations([]NPMInstallation{withoutRestore})
	if withProofMerged.ID != withoutProofMerged.ID || withProofMerged.Fingerprint != withoutProofMerged.Fingerprint {
		t.Fatalf(
			"restore proof changed removal identity: with=%s/%s without=%s/%s",
			withProofMerged.ID, withProofMerged.Fingerprint,
			withoutProofMerged.ID, withoutProofMerged.Fingerprint,
		)
	}
}

func TestDiscoveryRemovalAuthorityFailsClosedForIncompleteCoverage(t *testing.T) {
	candidate := NPMInstallation{
		Tier:              DiscoveryTierA,
		Manager:           DiscoveryManagerNPM,
		RemovalCapability: RemovalCapabilityExactNPM,
		UserWritable:      true,
		NodeExecutable:    "/node", NodeSHA256: strings.Repeat("a", 64),
		NPMCLI: "/npm", NPMCLISHA256: strings.Repeat("b", 64),
		CLIEntry: "/cli", CLIEntrySHA256: strings.Repeat("c", 64),
		BunExecutable: "/bun", BunSHA256: strings.Repeat("d", 64),
		PackageTreeSHA256: strings.Repeat("e", 64), NPMTreeSHA256: strings.Repeat("f", 64),
		TeardownCapability:    TeardownCapabilityRelayPreserveV1,
		DataCapability:        DataCapabilityPreserveOnly,
		TeardownCompatibility: teardownCompatibilityCompatible,
		TeardownAdapterID:     "test_preserve_v1",
	}
	for _, test := range []struct {
		name   string
		result DiscoveryResult
	}{
		{name: "complete", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}}},
		{name: "truncated", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}, Truncated: true}},
		{name: "rejected", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}, Rejected: 1}},
		{name: "refused root", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}, Coverage: []DiscoveryCoverage{{State: "refused"}}}},
		{name: "truncated root", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}, Coverage: []DiscoveryCoverage{{State: "truncated"}}}},
		{name: "unknown root", result: DiscoveryResult{Candidates: []NPMInstallation{candidate}, Coverage: []DiscoveryCoverage{{State: "future_state"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			applyDiscoveryRemovalAuthority(&test.result)
			got := test.result.Candidates[0].RemovalAuthority
			want := RemovalAuthorityManual
			if test.name == "complete" {
				want = RemovalAuthorityAutomatic
			}
			if got != want {
				t.Fatalf("authority = %q, want %q for %#v", got, want, test.result)
			}
		})
	}
}

func TestRemovalAuthorityProjectionContainsOnlyOpaqueSelectorAndAuthority(t *testing.T) {
	candidate := NPMInstallation{
		ID: "0123456789abcdef01234567", Fingerprint: strings.Repeat("a", 64),
		RemovalAuthority: RemovalAuthorityAutomatic, PackageRoot: "/private/package",
	}
	payload, err := json.Marshal(candidate.AuthorityProjection())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["installation_id"] != candidate.ID ||
		fields["installation_fingerprint"] != candidate.Fingerprint || fields["authority"] != string(RemovalAuthorityAutomatic) {
		t.Fatalf("authority projection = %s", payload)
	}
}

func TestSanitizedAuthorityProjectionRequiresOneExactSelector(t *testing.T) {
	base := NPMInstallation{
		ID:                "0123456789abcdef01234567",
		Fingerprint:       strings.Repeat("a", 64),
		Tier:              DiscoveryTierB,
		Manager:           DiscoveryManagerNPM,
		RemovalCapability: RemovalCapabilityExactNPM,
		RemovalAuthority:  RemovalAuthorityManual,
		UserWritable:      true,
		NodeExecutable:    "/node", NodeSHA256: strings.Repeat("1", 64),
		NPMCLI: "/npm", NPMCLISHA256: strings.Repeat("2", 64),
		CLIEntry: "/cli", CLIEntrySHA256: strings.Repeat("3", 64),
		BunExecutable: "/bun", BunSHA256: strings.Repeat("4", 64),
		PackageTreeSHA256: strings.Repeat("5", 64), NPMTreeSHA256: strings.Repeat("6", 64),
		TeardownCapability:    TeardownCapabilityRelayPreserveV1,
		DataCapability:        DataCapabilityPreserveOnly,
		TeardownCompatibility: teardownCompatibilityCompatible,
		TeardownAdapterID:     "test_preserve_v1",
	}
	for _, test := range []struct {
		name      string
		authority DiscoveryResult
		want      RemovalAuthority
	}{
		{
			name: "exact",
			authority: DiscoveryResult{
				SchemaVersion: discoverySchemaVersion,
				Candidates: []NPMInstallation{func() NPMInstallation {
					candidate := base
					candidate.RemovalAuthority = RemovalAuthorityAutomatic
					return candidate
				}()},
			},
			want: RemovalAuthorityAutomatic,
		},
		{
			name: "id only",
			authority: DiscoveryResult{
				SchemaVersion: discoverySchemaVersion,
				Candidates: []NPMInstallation{func() NPMInstallation {
					candidate := base
					candidate.Fingerprint = strings.Repeat("b", 64)
					candidate.RemovalAuthority = RemovalAuthorityAutomatic
					return candidate
				}()},
			},
			want: RemovalAuthorityManual,
		},
		{
			name: "fingerprint only",
			authority: DiscoveryResult{
				SchemaVersion: discoverySchemaVersion,
				Candidates: []NPMInstallation{func() NPMInstallation {
					candidate := base
					candidate.ID = "fedcba9876543210fedcba98"
					candidate.RemovalAuthority = RemovalAuthorityAutomatic
					return candidate
				}()},
			},
			want: RemovalAuthorityManual,
		},
		{
			name: "truncated",
			authority: DiscoveryResult{
				SchemaVersion: discoverySchemaVersion,
				Truncated:     true,
				Candidates:    []NPMInstallation{base},
			},
			want: RemovalAuthorityManual,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			display := DiscoveryResult{
				SchemaVersion: discoverySchemaVersion,
				Candidates:    []NPMInstallation{base},
			}
			beforeFingerprint := display.Candidates[0].Fingerprint
			beforeCapability := display.Candidates[0].RemovalCapability
			projectSanitizedRemovalAuthority(&display, test.authority)
			if display.Candidates[0].RemovalAuthority != test.want ||
				display.Candidates[0].Fingerprint != beforeFingerprint ||
				display.Candidates[0].RemovalCapability != beforeCapability {
				t.Fatalf("projected candidate=%#v want authority=%q", display.Candidates[0], test.want)
			}
		})
	}
}

func TestSanitizedRemovalDiscoveryOptionsRejectsBroadAndAmbientAuthorityInputs(t *testing.T) {
	options := SanitizedRemovalDiscoveryOptions(DiscoveryOptions{
		Tier:              DiscoveryTierC,
		RelayConfigPath:   "/tmp/relay.json",
		HomeDir:           "/tmp/home",
		PathEnv:           "/tmp/untrusted/bin",
		BroadScanApproved: true,
		BroadRoots:        []string{"/tmp/untrusted"},
		Getenv: func(string) string {
			return "/tmp/untrusted-manager"
		},
	})
	if options.Tier != DiscoveryTierB || options.RelayConfigPath != "/tmp/relay.json" || options.HomeDir != "/tmp/home" ||
		options.PathEnv != "" || options.BroadScanApproved || options.BroadRoots != nil || options.Getenv("NVM_DIR") != "" {
		t.Fatalf("sanitized authority options = %#v", options)
	}
}

func TestDiscoveryRejectsWrongPackageEscapingBinAndWritableParent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{
			name: "wrong package",
			mutate: func(t *testing.T, prefix, root string) {
				writeManifest(t, root, "lookalike-opencodex", "bin/ocx.mjs")
			},
		},
		{
			name: "escaping bin",
			mutate: func(t *testing.T, prefix, root string) {
				writeManifest(t, root, OpenCodexPackageName, "../outside.mjs")
			},
		},
		{
			name: "writable parent",
			mutate: func(t *testing.T, prefix, root string) {
				if err := os.Chmod(prefix, 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			prefix := filepath.Join(home, "node")
			root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "bin/ocx.mjs")
			test.mutate(t, prefix, root)
			result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
				SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Candidates) != 0 {
				t.Fatalf("unsafe candidate accepted: %#v", result.Candidates)
			}
		})
	}
}

func TestTierBFindsNVMVersionsAndDeduplicatesLaunchers(t *testing.T) {
	home := t.TempDir()
	nvmRoot := filepath.Join(home, ".nvm")
	prefix := filepath.Join(nvmRoot, "versions", "node", "v22.0.0")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "bin/ocx.mjs")
	getenv := func(name string) string {
		if name == "NVM_DIR" {
			return nvmRoot
		}
		return ""
	}
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierB, HomeDir: home, PathEnv: "/does/not/exist", GOOS: "darwin", GOARCH: "arm64", SkipDefaultPrefixes: true, Getenv: getenv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("Tier B candidates = %#v", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.PackageRoot != root || candidate.Manager != DiscoveryManagerNVM || candidate.Tier != DiscoveryTierB || len(candidate.Launchers) != 2 {
		t.Fatalf("NVM candidate = %#v", candidate)
	}
}

func TestTierCRequiresConsentAndSkipsSymlinkDirectories(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("broad discovery requires Darwin local-volume validation")
	}
	home := t.TempDir()
	outside := t.TempDir()
	outsidePrefix := filepath.Join(outside, "node")
	writeNPMInstallation(t, outsidePrefix, OpenCodexPackageName, "bin/ocx.mjs")
	if err := os.Symlink(outside, filepath.Join(home, "linked-outside")); err != nil {
		t.Fatal(err)
	}

	options := DiscoveryOptions{
		Tier: DiscoveryTierC, HomeDir: home, PathEnv: "/does/not/exist", GOOS: "darwin", GOARCH: "arm64",
		BroadRoots: []string{home}, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	}
	if _, err := DiscoverNPMInstallations(context.Background(), options); !errors.Is(err, ErrBroadScanConsentRequired) {
		t.Fatalf("unapproved Tier C error = %v", err)
	}
	options.BroadScanApproved = true
	result, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 || !result.BroadScanApproved {
		t.Fatalf("Tier C followed a directory symlink: %#v", result)
	}

	insidePrefix := filepath.Join(home, "custom", "node")
	insideRoot := writeNPMInstallation(t, insidePrefix, OpenCodexPackageName, "bin/ocx.mjs")
	result, err = DiscoverNPMInstallations(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].PackageRoot != insideRoot || result.Candidates[0].Tier != DiscoveryTierC {
		t.Fatalf("approved Tier C result = %#v", result)
	}
}

func TestTierCBoundsEnumeration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("broad discovery requires Darwin local-volume validation")
	}
	home := t.TempDir()
	for _, name := range []string{"one", "two", "three", "four"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierC, HomeDir: home, PathEnv: "/does/not/exist", GOOS: "darwin", GOARCH: "arm64",
		BroadScanApproved: true, BroadRoots: []string{home}, MaxEntries: 2, MaxDepth: 10, SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatalf("bounded scan did not report truncation: %#v", result)
	}
}

func writeNPMInstallation(t *testing.T, prefix, packageName, binTarget string) string {
	t.Helper()
	root := packageRootForPrefix(prefix)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "bun", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "lib", "node_modules", "npm", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, packageName, binTarget)
	writeExecutable(t, filepath.Join(root, "bin", "ocx.mjs"))
	if err := os.WriteFile(filepath.Join(root, "src", "cli", "index.ts"), []byte("console.log('fixture')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "node_modules", "bun", "bin", "bun.exe"))
	writeExecutable(t, filepath.Join(prefix, "bin", "node"))
	npmCLI := filepath.Join(prefix, "lib", "node_modules", "npm", "bin", "npm-cli.js")
	writeExecutable(t, npmCLI)
	for _, pair := range [][2]string{
		{filepath.Join(root, "bin", "ocx.mjs"), filepath.Join(prefix, "bin", "ocx")},
		{filepath.Join(root, "bin", "ocx.mjs"), filepath.Join(prefix, "bin", "opencodex")},
		{npmCLI, filepath.Join(prefix, "bin", "npm")},
	} {
		if err := os.Symlink(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolvedRoot
}

func writeManifest(t *testing.T, root, packageName, binTarget string) {
	t.Helper()
	payload := []byte(`{"name":"` + packageName + `","version":"2.22.0","bin":{"ocx":"` + binTarget + `","opencodex":"` + binTarget + `"}}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "package.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestTierBRefusesHostileManagerRootsWithoutScanning(t *testing.T) {
	home := t.TempDir()
	for _, hostile := range []string{"/", home} {
		t.Run(filepath.Base(hostile), func(t *testing.T) {
			result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierB, HomeDir: home, PathEnv: "/does/not/exist", GOOS: "darwin", GOARCH: "arm64",
				SkipDefaultPrefixes: true,
				Getenv: func(name string) string {
					if name == "NVM_DIR" {
						return hostile
					}
					return ""
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Truncated || len(result.Candidates) != 0 {
				t.Fatalf("hostile manager root result = %#v", result)
			}
			foundRefusal := false
			for _, coverage := range result.Coverage {
				if coverage.Source == "nvm" && coverage.Root == hostile && coverage.State == "refused" {
					foundRefusal = true
				}
			}
			if !foundRefusal {
				t.Fatalf("hostile manager root was not explicitly refused: %#v", result.Coverage)
			}
		})
	}
}

func TestTierBRefusesManagerRootWithSymlinkAncestor(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	realRoot := filepath.Join(outside, "container", "nvm")
	prefix := filepath.Join(realRoot, "versions", "node", "v22.0.0")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "bin/ocx.mjs")
	if err := os.Symlink(filepath.Join(outside, "container"), filepath.Join(home, "linked")); err != nil {
		t.Fatal(err)
	}
	requestedRoot := filepath.Join(home, "linked", "nvm")
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierB, HomeDir: home, PathEnv: "/does/not/exist", GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true,
		Getenv: func(name string) string {
			if name == "NVM_DIR" {
				return requestedRoot
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 || result.Truncated {
		t.Fatalf("symlink-ancestor manager root result = %#v", result)
	}
	for _, coverage := range result.Coverage {
		if coverage.Source == "nvm" && coverage.Root == requestedRoot && coverage.State == "refused" {
			return
		}
	}
	t.Fatalf("symlink-ancestor manager root was not refused: %#v", result.Coverage)
}

func TestDiscoveryScanDoesNotCrossNestedDeviceBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	normalPrefix := filepath.Join(root, "normal", "node")
	normalPackage := writeNPMInstallation(t, normalPrefix, OpenCodexPackageName, "bin/ocx.mjs")
	mountedPrefix := filepath.Join(root, "mounted", "node")
	writeNPMInstallation(t, mountedPrefix, OpenCodexPackageName, "bin/ocx.mjs")

	collector := discoveryCollector{
		options: DiscoveryOptions{MaxEntries: defaultDiscoveryMaxEntries, MaxDepth: defaultDiscoveryMaxDepth},
		deviceID: func(info os.FileInfo) (uint64, bool) {
			if info == nil {
				return 0, false
			}
			if info.Name() == "mounted" {
				return 2, true
			}
			return 1, true
		},
	}
	if err := collector.scanRoot(context.Background(), root, "nvm", DiscoveryManagerNVM, root, DiscoveryTierB, "trusted"); err != nil {
		t.Fatal(err)
	}
	if collector.truncated || len(collector.seeds) != 1 || collector.seeds[0].packageRoot != normalPackage {
		t.Fatalf("nested-device scan seeds=%#v truncated=%t", collector.seeds, collector.truncated)
	}
}

func TestDiscoveryFingerprintBindsAbsentToForeignFixedLauncherTransition(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home := t.TempDir()
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	ocxLauncher := filepath.Join(prefix, "bin", "ocx")
	if err := os.Remove(ocxLauncher); err != nil {
		t.Fatal(err)
	}
	options := DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	}
	before, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(before.Candidates) != 1 {
		t.Fatalf("absent-launcher discovery = %#v, %v", before, err)
	}
	if before.Candidates[0].RemovalCapability != RemovalCapabilityExactNPM {
		t.Fatalf("an absent redundant launcher should remain exactly removable: %#v", before.Candidates[0])
	}
	writeExecutable(t, ocxLauncher)
	after, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(after.Candidates) != 1 {
		t.Fatalf("foreign-launcher discovery = %#v, %v", after, err)
	}
	candidate := after.Candidates[0]
	if candidate.RemovalCapability != RemovalCapabilityManual || before.Candidates[0].Fingerprint == candidate.Fingerprint || before.Candidates[0].ID == candidate.ID {
		t.Fatalf("foreign launcher did not revoke stale automatic authority: before=%#v after=%#v", before.Candidates[0], candidate)
	}
	if containsString(candidate.Launchers, ocxLauncher) || !containsString(candidate.Warnings, "launcher_mismatch_ocx") {
		t.Fatalf("foreign launcher gained removal authority: %#v", candidate)
	}
}

func TestDiscoveryExistingForeignFixedLauncherIsManualOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home := t.TempDir()
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	foreign := filepath.Join(prefix, "bin", "opencodex")
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, foreign)
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("foreign-launcher discovery = %#v, %v", result, err)
	}
	candidate := result.Candidates[0]
	if candidate.RemovalCapability != RemovalCapabilityManual || containsString(candidate.Launchers, foreign) ||
		!containsString(candidate.Warnings, "launcher_mismatch_opencodex") {
		t.Fatalf("existing foreign launcher was accepted: %#v", candidate)
	}
	if err := validateAutomaticRemovalCandidate(candidate, os.Geteuid()); !errors.Is(err, ErrRemovalManualOnly) {
		t.Fatalf("foreign launcher automatic validation error = %v", err)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestMergedExternalLauncherEvidenceIsMonotonicAndSelectorBound(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "node")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	base, err := validateNPMInstallation(context.Background(), discoverySeed{
		packageRoot: root, source: "fixed", manager: DiscoveryManagerNPM, tier: DiscoveryTierA, confidence: "high",
	})
	if err != nil || base.RemovalCapability != RemovalCapabilityExactNPM {
		t.Fatalf("base candidate = %#v, %v", base, err)
	}
	externalDirectory := filepath.Join(home, "external-bin")
	if err := os.MkdirAll(externalDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	externalLauncher := filepath.Join(externalDirectory, "ocx")
	if err := os.Symlink(base.Executable, externalLauncher); err != nil {
		t.Fatal(err)
	}
	external, err := validateNPMInstallation(context.Background(), discoverySeed{
		packageRoot: root, requiredLauncher: externalLauncher, source: "path", manager: DiscoveryManagerNPM,
		tier: DiscoveryTierA, confidence: "high",
	})
	if err != nil || external.RemovalCapability != RemovalCapabilityManual {
		t.Fatalf("external candidate = %#v, %v", external, err)
	}

	merged := mergeInstallations([]NPMInstallation{base, external})
	reduced := mergeInstallations([]NPMInstallation{base})
	if merged.RemovalCapability != RemovalCapabilityManual || !containsString(merged.Warnings, "external_launcher_requires_manual_removal") ||
		merged.Fingerprint == reduced.Fingerprint || merged.ID == reduced.ID {
		t.Fatalf("merged selector did not bind manual evidence: merged=%#v reduced=%#v", merged, reduced)
	}
	if err := validateAutomaticRemovalCandidate(merged, os.Geteuid()); !errors.Is(err, ErrRemovalManualOnly) {
		t.Fatalf("merged external launcher validation error = %v", err)
	}
}

func TestEnrollmentHardLinkLauncherSurvivesPATHFreeRediscoveryAsManualEvidence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nvmRoot := filepath.Join(home, ".nvm")
	prefix := filepath.Join(nvmRoot, "versions", "node", "v22.0.0")
	root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	externalDirectory := filepath.Join(home, "approved-bin")
	if err := os.MkdirAll(externalDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	externalLauncher := filepath.Join(externalDirectory, "ocx")
	if err := os.Link(filepath.Join(root, "bin", "ocx.mjs"), externalLauncher); err != nil {
		t.Fatal(err)
	}
	resolvedLauncher, launcherFingerprint, err := VerifyExecutable(externalLauncher)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "relay.json")
	if err := WriteRecord(configPath, Record{
		Schema: schemaVersion, Executable: resolvedLauncher, Fingerprint: launcherFingerprint, Action: RetainProxyKeepShim,
	}); err != nil {
		t.Fatal(err)
	}
	options := DiscoveryOptions{
		Tier: DiscoveryTierB, RelayConfigPath: configPath, HomeDir: home, PathEnv: "/does/not/exist",
		GOOS: "darwin", GOARCH: "arm64", SkipDefaultPrefixes: true,
		Getenv: func(name string) string {
			if name == "NVM_DIR" {
				return nvmRoot
			}
			return ""
		},
	}
	result, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || result.Truncated || len(result.Candidates) != 1 {
		t.Fatalf("PATH-free hard-link discovery=%#v err=%v", result, err)
	}
	candidate := result.Candidates[0]
	if candidate.PackageRoot != root || candidate.RemovalCapability != RemovalCapabilityManual ||
		!containsString(candidate.Launchers, externalLauncher) ||
		!containsString(candidate.Warnings, "external_launcher_requires_manual_removal") {
		t.Fatalf("hard-link evidence was not selector-bound: %#v", candidate)
	}
	resolver := DiscoveryRemovalResolver{Options: options, EffectiveUID: func() int { return os.Geteuid() }}
	if _, err := resolver.Resolve(context.Background(), NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint}); !errors.Is(err, ErrRemovalManualOnly) {
		t.Fatalf("hard-link candidate automatic removal error=%v", err)
	}

	options.RelayConfigPath = ""
	withoutEnrollment, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(withoutEnrollment.Candidates) != 1 {
		t.Fatalf("baseline discovery=%#v err=%v", withoutEnrollment, err)
	}
	if withoutEnrollment.Candidates[0].ID == candidate.ID || withoutEnrollment.Candidates[0].Fingerprint == candidate.Fingerprint {
		t.Fatalf("hard-link evidence did not alter aggregate selector: with=%#v without=%#v", candidate, withoutEnrollment.Candidates[0])
	}
}
