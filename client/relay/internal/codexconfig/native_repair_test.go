package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectNativeRepairForOwnerClassifiesOnlyKnownOwners(t *testing.T) {
	owner := LocalDevelopmentOwner
	tests := []struct {
		name        string
		content     string
		profile     string
		wantKind    NativeRepairKind
		wantBase    bool
		wantCatalog bool
		wantReason  string
	}{
		{name: "clean", content: "model = \"gpt-5.6-sol\"\n", wantKind: NativeRepairStateOnly, wantReason: "native_routing_clean"},
		{
			name:     "partial current relay block",
			content:  owner.BeginMarker + "\r\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\r\n" + owner.EndMarker + "\r\nmodel = \"gpt-5.6-sol\"\r\n",
			wantKind: NativeRepairLocalRelay, wantBase: true, wantReason: "local_relay_owned",
		},
		{
			name:     "current relay profile only",
			content:  "model = \"gpt-5.6-sol\"\n",
			profile:  owner.InteractiveProfileMarker + "\nopenai_base_url = \"http://127.0.0.1:18182/v1\"\nmodel_catalog_json = \"/tmp/opencodex-relay-dev-catalog.json\"\n",
			wantKind: NativeRepairLocalRelay, wantReason: "local_relay_owned",
		},
		{
			name:     "opencodex pair",
			content:  "# Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\nmodel_catalog_json = \"/home/test/.codex/opencodex-catalog.json\"\nmodel = \"gpt-5.6-sol\"\n",
			wantKind: NativeRepairOpenCodex, wantBase: true, wantCatalog: true, wantReason: "opencodex_owned",
		},
		{
			name:     "opencodex base only",
			content:  "# Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\n",
			wantKind: NativeRepairOpenCodex, wantBase: true, wantReason: "opencodex_owned",
		},
		{
			name:     "opencodex catalog only",
			content:  "model_catalog_json = \"/home/test/.codex/opencodex-catalog.json\"\n",
			wantKind: NativeRepairOpenCodex, wantCatalog: true, wantReason: "opencodex_owned",
		},
		{
			name:     "legacy opencodex provider",
			content:  "model_provider = \"opencodex\"\nmodel_catalog_json = \"/home/test/.codex/opencodex-catalog.json\"\n",
			wantKind: NativeRepairOpenCodex, wantCatalog: true, wantReason: "opencodex_owned",
		},
		{
			name:     "marker substring is not ownership",
			content:  "# user note mentions Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantReason: "unmanaged_routing_override",
		},
		{
			name:     "arbitrary base",
			content:  "openai_base_url = \"https://example.test/v1\"\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantReason: "unmanaged_routing_override",
		},
		{
			name:     "arbitrary catalog",
			content:  "model_catalog_json = \"/home/test/custom.json\"\n",
			wantKind: NativeRepairUnavailable, wantCatalog: true, wantReason: "unmanaged_routing_override",
		},
		{
			name:     "duplicate owned and user base",
			content:  "# Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\nopenai_base_url = \"https://example.test/v1\"\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantReason: "mixed_routing_owners",
		},
		{
			name:     "relay and opencodex mixed",
			content:  owner.BeginMarker + "\nmodel_catalog_json = \"/tmp/relay.json\"\n" + owner.EndMarker + "\n# Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantCatalog: true, wantReason: "mixed_routing_owners",
		},
		{
			name:     "incomplete relay marker",
			content:  owner.BeginMarker + "\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantReason: "relay_marker_incomplete",
		},
		{
			name:     "foreign owner",
			content:  ProductionOwner.BeginMarker + "\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\n" + ProductionOwner.EndMarker + "\n",
			wantKind: NativeRepairUnavailable, wantBase: true, wantReason: "foreign_relay_owner",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.profile != "" {
				if err := os.WriteFile(InteractiveProfilePathForOwner(path, owner), []byte(test.profile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := InspectNativeRepairForOwner(path, owner)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.wantKind || got.OpenAIBaseURL != test.wantBase || got.ModelCatalog != test.wantCatalog || got.Reason != test.wantReason {
				t.Fatalf("inspection = %#v, want kind=%q base=%t catalog=%t reason=%q", got, test.wantKind, test.wantBase, test.wantCatalog, test.wantReason)
			}
		})
	}
}

func TestNativeRepairInspectionWitnessDetectsConfigAndProfileChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	owner := LocalDevelopmentOwner
	managed := owner.BeginMarker + "\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\n" + owner.EndMarker + "\n"
	if err := os.WriteFile(path, []byte(managed), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectNativeRepairForOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(managed+"model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateNativeRepairInspection(path, owner, initial); err == nil {
		t.Fatal("config change passed witness revalidation")
	}

	if err := os.WriteFile(path, []byte(managed), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err = InspectNativeRepairForOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	profile := owner.InteractiveProfileMarker + "\nopenai_base_url = \"http://127.0.0.1:18182/v1\"\nmodel_catalog_json = \"/tmp/catalog.json\"\n"
	if err := os.WriteFile(InteractiveProfilePathForOwner(path, owner), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateNativeRepairInspection(path, owner, initial); err == nil {
		t.Fatal("profile creation passed witness revalidation")
	}
}

func TestNativeRepairBoundaryRevisionBindsConfigAndProfileWitnesses(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectNativeRepairForOwner(path, ProductionOwner)
	if err != nil {
		t.Fatal(err)
	}
	initialRevision, err := NativeRepairBoundaryRevision(path, ProductionOwner, initial)
	if err != nil || len(initialRevision) != 64 {
		t.Fatalf("initial revision=%q err=%v", initialRevision, err)
	}
	if err := os.WriteFile(path, []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := InspectNativeRepairForOwner(path, ProductionOwner)
	if err != nil {
		t.Fatal(err)
	}
	changedRevision, err := NativeRepairBoundaryRevision(path, ProductionOwner, changed)
	if err != nil || changedRevision == initialRevision {
		t.Fatalf("changed revision=%q initial=%q err=%v", changedRevision, initialRevision, err)
	}
	profile := InteractiveProfilePathForOwner(path, ProductionOwner)
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("profile = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withProfile, err := InspectNativeRepairForOwner(path, ProductionOwner)
	if err != nil {
		t.Fatal(err)
	}
	profileRevision, err := NativeRepairBoundaryRevision(path, ProductionOwner, withProfile)
	if err != nil || profileRevision == changedRevision {
		t.Fatalf("profile revision=%q changed=%q err=%v", profileRevision, changedRevision, err)
	}
}

func TestCreateNativeRepairBackupPreservesExactBytesAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "model = \"gpt-5.6-sol\"\r\n[tools]\r\nweb_search = true\r\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := CreateNativeRepairBackup(path)
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	matches, err := filepath.Glob(path + ".pre-opencodex-relay-native-repair-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("matches=%v err=%v", matches, err)
	}
	payload, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != original {
		t.Fatalf("backup changed bytes: %q", payload)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o", info.Mode().Perm())
	}
	if strings.Contains(matches[0], "gpt-5.6-sol") {
		t.Fatal("backup name exposed config content")
	}
}
