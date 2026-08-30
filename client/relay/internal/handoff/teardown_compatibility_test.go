package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Test fixtures use a deliberately tiny package tree. Registering its exact
// module digest only in the test binary keeps production compatibility strict
// while exercising the extensible package/version/adapter registry.
func init() {
	teardownAdapterImplementations = append(
		teardownAdapterImplementations,
		teardownAdapterImplementation{
			adapterID:  "test_preserve_v1",
			entrypoint: "relay-preserve-v1.ts",
			sources: map[string][]byte{
				"relay-preserve-v1.ts":      []byte("process.exit(1)\n"),
				"relay-preserve-v1-shim.ts": []byte("export {}\n"),
			},
		},
	)
	teardownAdapterProfiles = append(teardownAdapterProfiles, teardownAdapterProfile{
		packageName:           OpenCodexPackageName,
		version:               "2.22.0",
		artifactVariant:       "fixture_" + runtime.GOOS + "_" + runtime.GOARCH + "_v1",
		goos:                  runtime.GOOS,
		goarch:                runtime.GOARCH,
		registryIntegrity:     "sha512-YQ==",
		reviewedClosureSHA256: "6b4fb950c9f49ba0ade703fb6c9b1e6aabad1ced93096cf2c1987d3ef19fa613",
		adapterID:             "test_preserve_v1",
		requiredModules: map[string]string{
			"src/cli/index.ts": "a9022484f0390413999afb5b91590ad681ab0721fa1cad590c41bb705b02ec6d",
		},
	})
}

func TestProductionTeardownRegistryUsesExactStableProfiles(t *testing.T) {
	wantVersions := []string{
		"2.22.0", "2.23.0", "2.24.0", "2.24.1", "2.24.2", "2.25.0", "2.26.0",
		"2.27.0", "2.28.0", "2.29.0", "2.31.0", "2.32.0", "2.32.1", "2.33.0",
	}
	if got := productionTeardownVersions(); !reflect.DeepEqual(got, wantVersions) {
		t.Fatalf("production versions = %#v, want %#v", got, wantVersions)
	}

	implementations := productionTeardownAdapterImplementations()
	if len(implementations) != len(wantVersions) {
		t.Fatalf("production implementations = %d, want %d", len(implementations), len(wantVersions))
	}
	seenImplementations := make(map[string]bool, len(implementations))
	for _, implementation := range implementations {
		if !validTeardownAdapterImplementation(implementation) || seenImplementations[implementation.adapterID] {
			t.Fatalf("invalid or duplicate implementation: %#v", implementation)
		}
		seenImplementations[implementation.adapterID] = true
	}

	profilesByVersion := make(map[string]int, len(wantVersions))
	seenVariants := make(map[string]bool, len(teardownAdapterProfiles))
	productionProfileCount := 0
	for _, profile := range teardownAdapterProfiles {
		if !strings.HasPrefix(profile.artifactVariant, "npm_") {
			continue
		}
		productionProfileCount++
		if !validTeardownProfile(profile) || seenVariants[profile.artifactVariant] {
			t.Fatalf("invalid or duplicate profile: %#v", profile)
		}
		seenVariants[profile.artifactVariant] = true
		profilesByVersion[profile.version]++
		wantAdapterID := teardownAdapterIDForVersion(profile.version)
		if profile.adapterID != wantAdapterID || !seenImplementations[profile.adapterID] {
			t.Fatalf("profile %s adapter = %q, want %q", profile.artifactVariant, profile.adapterID, wantAdapterID)
		}
	}
	if productionProfileCount != len(wantVersions)+2 {
		t.Fatalf("production profiles = %d, want %d", productionProfileCount, len(wantVersions)+2)
	}
	for _, version := range wantVersions {
		wantCount := 1
		if version == "2.22.0" {
			wantCount = 3
		}
		if profilesByVersion[version] != wantCount {
			t.Fatalf("profiles for %s = %d, want %d", version, profilesByVersion[version], wantCount)
		}
	}
	var v3 *teardownAdapterProfile
	for index := range teardownAdapterProfiles {
		if teardownAdapterProfiles[index].artifactVariant == "npm_2_22_0_darwin_arm64_v3" {
			v3 = &teardownAdapterProfiles[index]
			break
		}
	}
	if v3 == nil || v3.reviewedClosureSHA256 != "f5f80363694b4d3dce4375acc93097ffc8f3ae1cd3b03df8450e3c3fd26b09f4" {
		t.Fatalf("2.22.0 v3 profile = %#v", v3)
	}
	if got := teardownAdapterIDForVersion("2.33.0"); got != "opencodex_npm_2_33_0_preserve_v1" {
		t.Fatalf("2.33 adapter id = %q", got)
	}
	for _, unsupported := range []string{"2.30.0", "2.33.0-preview.1", "2.34.0", "2.35.0", "2.36.0"} {
		if profilesByVersion[unsupported] != 0 {
			t.Fatalf("unsupported version %s was registered", unsupported)
		}
	}
}

func TestTeardownRegistrySelectsExactFutureProfile(t *testing.T) {
	restore := preserveTeardownRegistries(t)
	defer restore()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("reviewed future module\n")
	if err := os.WriteFile(filepath.Join(root, "module.ts"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "transitive.ts"), []byte("reviewed transitive module\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target-a.ts"), []byte("target a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target-b.ts"), []byte("target b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-a.ts", filepath.Join(root, "selected.ts")); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	profile := reviewedTestTeardownProfile(t, root, teardownAdapterProfile{
		packageName: OpenCodexPackageName,
		version:     "9.9.9",
		adapterID:   "future_preserve_v2",
		requiredModules: map[string]string{
			"module.ts": hex.EncodeToString(digest[:]),
		},
	})
	teardownAdapterProfiles = append(teardownAdapterProfiles, profile)
	teardownAdapterImplementations = append(
		teardownAdapterImplementations,
		teardownAdapterImplementation{
			adapterID:  profile.adapterID,
			entrypoint: "fixture.ts",
			sources:    map[string][]byte{"fixture.ts": []byte("process.exit(1)\n")},
		},
	)

	capability, data, reason, adapterID, proof := inspectTeardownCompatibility(
		context.Background(),
		root,
		npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityRelayPreserveV1 ||
		data != DataCapabilitySelectiveTrashV1 ||
		reason != teardownCompatibilityCompatible ||
		adapterID != profile.adapterID ||
		proof == nil ||
		!proof.valid() {
		t.Fatalf("future compatibility = %q %q %q %q %#v", capability, data, reason, adapterID, proof)
	}
	selected, implementation, ok := teardownProfileForCandidate(
		profile.adapterID,
		profile.artifactVariant,
		OpenCodexPackageName,
		profile.version,
	)
	if !ok || selected.version != profile.version || len(implementation.sources) == 0 {
		t.Fatalf("future adapter selection = %#v %#v %v", selected, implementation, ok)
	}

	if err := os.WriteFile(filepath.Join(root, "module.ts"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, adapterID, proof = inspectTeardownCompatibility(
		context.Background(),
		root,
		npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone ||
		reason != teardownCompatibilityModuleChanged ||
		adapterID != "" ||
		proof != nil {
		t.Fatalf("changed module compatibility = %q %q %q %#v", capability, reason, adapterID, proof)
	}

	if err := os.WriteFile(filepath.Join(root, "module.ts"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "transitive.ts"), []byte("tampered transitive module\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, adapterID, proof = inspectTeardownCompatibility(
		context.Background(),
		root,
		npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone ||
		reason != teardownCompatibilityClosureChanged ||
		adapterID != "" ||
		proof != nil {
		t.Fatalf("changed closure compatibility = %q %q %q %#v", capability, reason, adapterID, proof)
	}

	if err := os.WriteFile(filepath.Join(root, "transitive.ts"), []byte("reviewed transitive module\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "transitive.ts"), 0o700); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, _, _ = inspectTeardownCompatibility(
		context.Background(), root, npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone || reason != teardownCompatibilityClosureChanged {
		t.Fatalf("changed executable mode compatibility = %q %q", capability, reason)
	}
	if err := os.Chmod(filepath.Join(root, "transitive.ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "selected.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-b.ts", filepath.Join(root, "selected.ts")); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, _, _ = inspectTeardownCompatibility(
		context.Background(), root, npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone || reason != teardownCompatibilityClosureChanged {
		t.Fatalf("changed symlink compatibility = %q %q", capability, reason)
	}

	if err := os.Remove(filepath.Join(root, "selected.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-a.ts", filepath.Join(root, "selected.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unexpected.ts"), []byte("unreviewed file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, _, _ = inspectTeardownCompatibility(
		context.Background(), root, npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone || reason != teardownCompatibilityClosureChanged {
		t.Fatalf("added file compatibility = %q %q", capability, reason)
	}

	if err := os.Remove(filepath.Join(root, "unexpected.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "target-b.ts")); err != nil {
		t.Fatal(err)
	}
	capability, _, reason, _, _ = inspectTeardownCompatibility(
		context.Background(), root, npmPackageManifest{Name: OpenCodexPackageName, Version: profile.version},
	)
	if capability != TeardownCapabilityNone || reason != teardownCompatibilityClosureChanged {
		t.Fatalf("deleted file compatibility = %q %q", capability, reason)
	}
}

func TestTeardownRegistryRejectsAmbiguousProfilesAndAdapters(t *testing.T) {
	restore := preserveTeardownRegistries(t)
	defer restore()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("same reviewed module\n")
	if err := os.WriteFile(filepath.Join(root, "module.ts"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	moduleDigest := hex.EncodeToString(digest[:])
	for _, adapterID := range []string{"future_a", "future_b"} {
		teardownAdapterProfiles = append(teardownAdapterProfiles, reviewedTestTeardownProfile(t, root, teardownAdapterProfile{
			packageName: OpenCodexPackageName,
			version:     "9.9.8",
			adapterID:   adapterID,
			requiredModules: map[string]string{
				"module.ts": moduleDigest,
			},
		}))
		teardownAdapterImplementations = append(
			teardownAdapterImplementations,
			teardownAdapterImplementation{
				adapterID:  adapterID,
				entrypoint: "fixture.ts",
				sources:    map[string][]byte{"fixture.ts": []byte("process.exit(1)\n")},
			},
		)
	}
	capability, _, reason, adapterID, proof := inspectTeardownCompatibility(
		context.Background(),
		root,
		npmPackageManifest{Name: OpenCodexPackageName, Version: "9.9.8"},
	)
	if capability != TeardownCapabilityNone ||
		reason != teardownCompatibilityConflict ||
		adapterID != "" ||
		proof != nil {
		t.Fatalf("ambiguous compatibility = %q %q %q %#v", capability, reason, adapterID, proof)
	}

	teardownAdapterImplementations = append(
		teardownAdapterImplementations,
		teardownAdapterImplementation{
			adapterID:  "future_a",
			entrypoint: "duplicate.ts",
			sources:    map[string][]byte{"duplicate.ts": []byte("duplicate\n")},
		},
	)
	if _, ok := teardownAdapterImplementationForID("future_a"); ok {
		t.Fatal("duplicate adapter implementation was accepted")
	}
}

func reviewedTestTeardownProfile(t *testing.T, root string, profile teardownAdapterProfile) teardownAdapterProfile {
	t.Helper()
	digest, err := stableReviewedPackageClosureDigest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	profile.artifactVariant = profile.adapterID + "_" + runtime.GOOS + "_" + runtime.GOARCH
	profile.goos = runtime.GOOS
	profile.goarch = runtime.GOARCH
	profile.registryIntegrity = "sha512-YQ=="
	profile.reviewedClosureSHA256 = digest
	return profile
}

func preserveTeardownRegistries(t *testing.T) func() {
	t.Helper()
	profiles := append([]teardownAdapterProfile(nil), teardownAdapterProfiles...)
	implementations := append(
		[]teardownAdapterImplementation(nil),
		teardownAdapterImplementations...,
	)
	return func() {
		teardownAdapterProfiles = profiles
		teardownAdapterImplementations = implementations
	}
}
