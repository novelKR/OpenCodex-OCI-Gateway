package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnablePreservesTablesAndDisableRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.6-sol\"\n\n[features]\nfast_mode = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Enable(path, "http://127.0.0.1:18180/v1", "/tmp/catalog.json"); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(path)
	if !strings.Contains(string(updated), BeginMarker) || strings.Index(string(updated), BeginMarker) > strings.Index(string(updated), "[features]") {
		t.Fatalf("managed block was not inserted at root: %s", updated)
	}
	if err := Disable(path); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != original {
		t.Fatalf("restored config differs:\n%s", restored)
	}
}

func TestEnableRejectsUnmanagedBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("openai_base_url = \"https://example.test/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Enable(path, "http://127.0.0.1:18180/v1", "/tmp/catalog.json"); err == nil {
		t.Fatal("unmanaged base URL was overwritten")
	}
}

func TestEnableRejectsExternalRootProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model_provider = \"pw_opencodex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Enable(path, "http://127.0.0.1:18180/v1", "/tmp/catalog.json"); err == nil {
		t.Fatal("external model provider was accepted")
	}
}

func TestEnableWithInteractiveProfileCreatesOnlyRoutingOverrides(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "model = \"gpt-5.6-sol\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnableWithInteractiveProfile(
		path,
		"http://127.0.0.1:18180/v1",
		"http://127.0.0.1:18182/v1",
		"/tmp/catalog.json",
	); err != nil {
		t.Fatal(err)
	}
	profilePath := InteractiveProfilePath(path)
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	want := InteractiveProfileMarker + "\n" +
		"openai_base_url = \"http://127.0.0.1:18182/v1\"\n" +
		"model_catalog_json = \"/tmp/catalog.json\"\n"
	if string(profile) != want {
		t.Fatalf("profile = %q, want %q", profile, want)
	}
	info, err := os.Stat(profilePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %v, err = %v", info.Mode().Perm(), err)
	}
	for _, forbidden := range []string{"model =", "reasoning", "agents.", "max_concurrent_threads"} {
		if strings.Contains(string(profile), forbidden) {
			t.Fatalf("profile contains forbidden setting %q: %s", forbidden, profile)
		}
	}
	if err := DisableWithInteractiveProfile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile remains after disable: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(restored), `model = "gpt-5.6-sol"`) || strings.Contains(string(restored), BeginMarker) || strings.Contains(string(restored), "openai_base_url") {
		t.Fatalf("restored config = %q, err = %v", restored, err)
	}
}

func TestInteractiveProfileRejectsUnmanagedAndSymlinkFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "unmanaged", setup: func(path string) error {
			return os.WriteFile(path, []byte("openai_base_url = \"http://example.test\"\n"), 0o600)
		}},
		{name: "symlink", setup: func(path string) error {
			target := filepath.Join(filepath.Dir(path), "target.toml")
			if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			configPath := filepath.Join(directory, "config.toml")
			original := "model = \"fixture\"\n"
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.setup(InteractiveProfilePath(configPath)); err != nil {
				t.Fatal(err)
			}
			if err := EnableWithInteractiveProfile(configPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", "/tmp/catalog.json"); err == nil {
				t.Fatal("unsafe interactive profile was overwritten")
			}
			unchanged, err := os.ReadFile(configPath)
			if err != nil || string(unchanged) != original {
				t.Fatalf("base config changed on preflight failure: %q, %v", unchanged, err)
			}
		})
	}
}

func TestInspectRoutingDistinguishesManagedArtifactsFromNativeConfiguration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.ManagedRoot || inspection.InteractiveProfileExists || inspection.InteractiveProfileManaged ||
		inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider {
		t.Fatalf("native inspection = %#v, want no managed routing", inspection)
	}

	if err := EnableWithInteractiveProfile(path, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", "/tmp/catalog.json"); err != nil {
		t.Fatal(err)
	}
	inspection, err = InspectRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.ManagedRoot || !inspection.InteractiveProfileExists || !inspection.InteractiveProfileManaged {
		t.Fatalf("relay inspection = %#v, want both managed artifacts", inspection)
	}
}

func TestInspectRoutingReportsForeignRoutingWithoutReturningItsValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("openai_base_url = \"https://example.test/v1\"\nmodel_catalog_json = \"/foreign-catalog.json\"\nmodel_provider = \"other\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.UnmanagedOpenAIBaseURL || !inspection.UnmanagedModelCatalog || !inspection.UnmanagedModelProvider {
		t.Fatalf("foreign routing was not identified: %#v", inspection)
	}
}

func TestPreflightCodexConfigPathAllowsOnlyMissingOrRegularNonSymlinkLeaves(t *testing.T) {
	directory := t.TempDir()
	if err := PreflightCodexConfigPath(""); err == nil {
		t.Fatal("empty Codex config path was accepted")
	}
	missing := filepath.Join(directory, "missing.toml")
	if err := PreflightCodexConfigPath(missing); err != nil {
		t.Fatalf("missing Codex config was rejected: %v", err)
	}

	regular := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(regular, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreflightCodexConfigPath(regular); err != nil {
		t.Fatalf("regular Codex config was rejected: %v", err)
	}

	if err := PreflightCodexConfigPath(directory); err == nil {
		t.Fatal("Codex config directory was accepted")
	}
	symlink := filepath.Join(directory, "config-link.toml")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if err := PreflightCodexConfigPath(symlink); err == nil {
		t.Fatal("Codex config symlink was accepted")
	}
}

func TestCodexConfigSymlinkIsRejectedWithoutTouchingTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.toml")
	path := filepath.Join(directory, "config.toml")
	original := "model = \"gpt\"\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := Enable(path, "http://127.0.0.1:18180/v1", "/tmp/catalog.json"); err == nil {
		t.Fatal("symlink Codex config was accepted")
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != original {
		t.Fatalf("target changed = %q, err = %v", actual, err)
	}
}

func TestInspectRoutingRejectsAmbiguousManagedArtifacts(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{
			name: "incomplete root block",
			setup: func(path string) error {
				return os.WriteFile(path, []byte(BeginMarker+"\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\n"), 0o600)
			},
		},
		{
			name: "unmanaged interactive profile",
			setup: func(path string) error {
				if err := os.WriteFile(path, []byte("model = \"gpt\"\n"), 0o600); err != nil {
					return err
				}
				return os.WriteFile(InteractiveProfilePath(path), []byte("openai_base_url = \"https://example.test/v1\"\n"), 0o600)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := test.setup(path); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectRouting(path); err == nil {
				t.Fatal("ambiguous routing artifact was accepted")
			}
		})
	}
}

func TestPreflightEnableWithInteractiveProfileRejectsUnmanagedRootBeforeWrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "openai_base_url = \"https://example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreflightEnableWithInteractiveProfile(path); err == nil {
		t.Fatal("unmanaged root routing was accepted")
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != original {
		t.Fatalf("preflight changed config = %q, err = %v", actual, err)
	}
}

func TestDisableDoesNotCreateANewNativeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Disable(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("disable created native config: %v", err)
	}
}

func TestMigrateLegacyOnlyRemovesKnownRootAssignmentsAndCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model_provider = \"pw_opencodex\"\nmodel_catalog_json = \"/old/catalog.json\"\nmodel = \"gpt\"\n\n[model_providers.pw_opencodex]\nbase_url = \"https://example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := MigrateLegacy(path)
	if err != nil {
		t.Fatal(err)
	}
	backupContent, err := os.ReadFile(backup)
	if err != nil || string(backupContent) != original {
		t.Fatalf("legacy backup = %q, err = %v", backupContent, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "model_provider =") || strings.Contains(string(updated), "model_catalog_json =") {
		t.Fatalf("legacy root assignments remained: %s", updated)
	}
	if !strings.Contains(string(updated), "[model_providers.pw_opencodex]") {
		t.Fatalf("provider table was unexpectedly removed: %s", updated)
	}
}

func TestMigrateLegacyAcceptsObservedRemoteProviderWithoutRemovingItsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model_provider = \"pw_opencodex_remote\"\nmodel_catalog_json = \"/old/catalog.json\"\n\n[model_providers.pw_opencodex_remote]\nbase_url = \"https://example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := MigrateLegacy(path)
	if err != nil {
		t.Fatal(err)
	}
	backupContent, err := os.ReadFile(backup)
	if err != nil || string(backupContent) != original {
		t.Fatalf("legacy backup = %q, err = %v", backupContent, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "model_provider =") || strings.Contains(string(updated), "model_catalog_json =") {
		t.Fatalf("legacy root assignments remained: %s", updated)
	}
	if !strings.Contains(string(updated), "[model_providers.pw_opencodex_remote]") {
		t.Fatalf("provider table was unexpectedly removed: %s", updated)
	}
}

func TestMigrateLegacyAcceptsOnlyTheKnownDirectLoopbackBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "openai_base_url = \"http://127.0.0.1:10100/v1\"\nmodel_catalog_json = \"/old/catalog.json\"\nmodel = \"gpt\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacy(path); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "openai_base_url =") || strings.Contains(string(updated), "model_catalog_json =") {
		t.Fatalf("legacy loopback assignments remained: %s", updated)
	}
	if !strings.Contains(string(updated), "model = \"gpt\"") {
		t.Fatalf("user root assignment was unexpectedly removed: %s", updated)
	}

	unsupported := filepath.Join(t.TempDir(), "unsupported.toml")
	if err := os.WriteFile(unsupported, []byte("openai_base_url = \"https://example.test/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacy(unsupported); err == nil {
		t.Fatal("an arbitrary OpenAI base URL was accepted as legacy routing")
	}
}

func TestEnableWithLegacyMigrationCreatesBackupAndManagedProfiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "model_provider = \"opencodex\"\nmodel_catalog_json = \"/old/catalog.json\"\nmodel = \"gpt\"\n\n[model_providers.opencodex]\nbase_url = \"https://legacy.example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := EnableWithLegacyMigrationForOwner(
		path,
		ProductionOwner,
		"http://127.0.0.1:18180/v1",
		"http://127.0.0.1:18182/v1",
		filepath.Join(directory, "catalog.json"),
	)
	if err != nil || backup == "" {
		t.Fatalf("enable legacy migration backup=%q err=%v", backup, err)
	}
	backupContent, err := os.ReadFile(backup)
	if err != nil || string(backupContent) != original {
		t.Fatalf("backup content=%q err=%v", backupContent, err)
	}
	if info, statErr := os.Stat(backup); statErr != nil {
		t.Fatalf("backup stat=%v", statErr)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%v", info.Mode().Perm())
	}
	updated, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(updated), "model_provider =") ||
		!strings.Contains(string(updated), BeginMarker) ||
		!strings.Contains(string(updated), "[model_providers.opencodex]") {
		t.Fatalf("migrated config=%q err=%v", updated, err)
	}
	if err := ValidateManagedRouting(
		path,
		"http://127.0.0.1:18180/v1",
		"http://127.0.0.1:18182/v1",
		filepath.Join(directory, "catalog.json"),
	); err != nil {
		t.Fatalf("managed routing validation=%v", err)
	}
}

func TestLegacyMigrationPreflightRejectsUnknownOverridesWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "openai_base_url = \"https://unknown.example.test/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreflightLegacyMigrationWithInteractiveProfileForOwner(path, ProductionOwner); err == nil {
		t.Fatal("unknown override passed migration preflight")
	}
	observed, err := os.ReadFile(path)
	if err != nil || string(observed) != original {
		t.Fatalf("preflight mutated config=%q err=%v", observed, err)
	}
}

func TestLegacyMigrationPlanDistinguishesCleanNativeFromRecognizedLegacy(t *testing.T) {
	for _, test := range []struct {
		name              string
		content           string
		requiresMigration bool
	}{
		{
			name:              "clean native",
			content:           "model = \"gpt-5.6-sol\"\n",
			requiresMigration: false,
		},
		{
			name:              "recognized legacy",
			content:           "model_provider = \"opencodex\"\nmodel_catalog_json = \"/old/catalog.json\"\n\n[model_providers.opencodex]\nbase_url = \"https://legacy.example.test/v1\"\n",
			requiresMigration: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			preflight, err := PlanLegacyMigrationWithInteractiveProfileForOwner(path, ProductionOwner)
			if err != nil {
				t.Fatal(err)
			}
			if preflight.RequiresMigration != test.requiresMigration {
				t.Fatalf("requires migration=%t, want %t", preflight.RequiresMigration, test.requiresMigration)
			}
			observed, err := os.ReadFile(path)
			if err != nil || string(observed) != test.content {
				t.Fatalf("preflight mutated config=%q err=%v", observed, err)
			}
		})
	}
}

func TestValidateManagedRoutingRejectsCatalogOrInteractiveProfileDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	general := "http://127.0.0.1:18180/v1"
	interactive := "http://127.0.0.1:18182/v1"
	catalog := filepath.Join(filepath.Dir(path), "external-catalog.json")
	if err := EnableWithInteractiveProfile(path, general, interactive, catalog); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedRouting(path, general, interactive, catalog); err != nil {
		t.Fatalf("managed routing did not validate: %v", err)
	}
	if err := ValidateManagedRouting(path, general, interactive, filepath.Join(filepath.Dir(path), "local-catalog.json")); err == nil {
		t.Fatal("catalog drift was accepted")
	}
	if err := os.WriteFile(InteractiveProfilePath(path), []byte(InteractiveProfileMarker+"\nopenai_base_url = \"http://127.0.0.1:18182/v1\"\nmodel_catalog_json = \"/foreign.json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedRouting(path, general, interactive, catalog); err == nil {
		t.Fatal("interactive profile drift was accepted")
	}
}

func TestValidateNativeRoutingRejectsManagedOrForeignArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := ValidateNativeRouting(path); err != nil {
		t.Fatalf("empty native routing did not validate: %v", err)
	}
	if err := os.WriteFile(path, []byte("openai_base_url = \"https://foreign.example/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeRouting(path); err == nil {
		t.Fatal("foreign native override was accepted")
	}
}

func TestRoutingValidatorsTreatAnUnmanagedModelCatalogAsForeign(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	general := "http://127.0.0.1:18180/v1"
	interactive := "http://127.0.0.1:18182/v1"
	catalog := filepath.Join(filepath.Dir(path), "relay-catalog.json")
	if err := EnableWithInteractiveProfile(path, general, interactive, catalog); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	foreignManaged := strings.Replace(string(managed), EndMarker, EndMarker+"\nmodel_catalog_json = \"/foreign-catalog.json\"", 1)
	if err := os.WriteFile(path, []byte(foreignManaged), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectRouting(path)
	if err != nil || !inspection.UnmanagedModelCatalog {
		t.Fatalf("unmanaged catalog inspection = %#v, err=%v", inspection, err)
	}
	if err := ValidateManagedRouting(path, general, interactive, catalog); err == nil {
		t.Fatal("managed routing accepted an unmanaged catalog override")
	}
	if err := ValidateExpectedRoutingOwnership(path, true); err == nil {
		t.Fatal("managed handoff ownership accepted an unmanaged catalog override")
	}

	if err := os.Remove(InteractiveProfilePath(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model_catalog_json = \"/foreign-catalog.json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeRouting(path); err == nil {
		t.Fatal("native routing accepted an unmanaged catalog override")
	}
	if err := ValidateExpectedRoutingOwnership(path, false); err == nil {
		t.Fatal("native handoff ownership accepted an unmanaged catalog override")
	}
}

func TestValidateExpectedRoutingOwnershipRequiresTheObservedOwnershipShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := ValidateExpectedRoutingOwnership(path, false); err != nil {
		t.Fatalf("empty native routing ownership: %v", err)
	}
	if err := ValidateExpectedRoutingOwnership(path, true); err == nil {
		t.Fatal("missing relay artifacts were accepted as managed")
	}

	general := "http://127.0.0.1:18180/v1"
	interactive := "http://127.0.0.1:18182/v1"
	catalog := filepath.Join(filepath.Dir(path), "catalog.json")
	if err := EnableWithInteractiveProfile(path, general, interactive, catalog); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExpectedRoutingOwnership(path, true); err != nil {
		t.Fatalf("managed routing ownership: %v", err)
	}
	if err := ValidateExpectedRoutingOwnership(path, false); err == nil {
		t.Fatal("managed relay artifacts were accepted as native")
	}
}
