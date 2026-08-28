package legacymigration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeKeychain map[string][]byte

func (f fakeKeychain) Inspect(oldService, newService, _ string) (bool, bool, bool, error) {
	oldValue, oldExists := f[oldService]
	newValue, newExists := f[newService]
	return oldExists, newExists, oldExists && newExists && string(oldValue) == string(newValue), nil
}

func (f fakeKeychain) Fingerprint(service, _ string) (string, bool, error) {
	value, exists := f[service]
	if !exists {
		return "", false, nil
	}
	return digest(value), true, nil
}

func (f fakeKeychain) Copy(oldService, newService, _ string, expectedSHA256 string) (bool, error) {
	value, exists := f[oldService]
	if !exists {
		return false, nil
	}
	if digest(value) != expectedSHA256 {
		return false, &fakeError{"source changed"}
	}
	if current, exists := f[newService]; exists {
		if string(current) != string(value) {
			return false, errFakeConflict
		}
		return false, nil
	}
	f[newService] = append([]byte(nil), value...)
	return true, nil
}

func (f fakeKeychain) DeleteIfMatching(service, _ string, expectedSHA256 string) (bool, error) {
	value, exists := f[service]
	if !exists {
		return false, nil
	}
	if digest(value) != expectedSHA256 {
		return false, &fakeError{"destination changed"}
	}
	delete(f, service)
	return true, nil
}

type interruptingKeychain struct {
	fakeKeychain
	interrupted bool
}

func (f *interruptingKeychain) Copy(oldService, newService, account, expectedSHA256 string) (bool, error) {
	created, err := f.fakeKeychain.Copy(oldService, newService, account, expectedSHA256)
	if err != nil {
		return created, err
	}
	if created && !f.interrupted {
		f.interrupted = true
		return true, &fakeError{"interrupted after Keychain mutation"}
	}
	return created, nil
}

type changingBeforeDeleteKeychain struct {
	fakeKeychain
	service string
	changed bool
}

func (f *changingBeforeDeleteKeychain) DeleteIfMatching(service, account, expectedSHA256 string) (bool, error) {
	if service == f.service && !f.changed {
		f.changed = true
		f.fakeKeychain[service] = []byte("replacement-secret")
	}
	return f.fakeKeychain.DeleteIfMatching(service, account, expectedSHA256)
}

var errFakeConflict = &fakeError{"conflict"}

type fakeError struct{ message string }

func (e *fakeError) Error() string { return e.message }

func TestApplyAndRollbackPreserveLegacyStateAndHideSecrets(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	oldRoot := filepath.Join(home, ".config", old)
	codexRoot := filepath.Join(home, ".codex")
	mustWrite(t, filepath.Join(oldRoot, "relay.json"), `{"catalog":{"path":"`+filepath.Join(codexRoot, old+"-catalog.json")+`"}}`)
	mustWrite(t, filepath.Join(oldRoot, "relay.json.routing-state.json"), `{"bound_config_path":"`+filepath.Join(oldRoot, "relay.json")+`","phase":"relay_active"}`)
	mustWrite(t, filepath.Join(oldRoot, "relay.json.routing-initialized"), old+"-relay-routing-initialized-v1\n")
	mustWrite(t, filepath.Join(codexRoot, old+"-catalog.json"), `{"models":[]}`)
	codexPath := filepath.Join(codexRoot, "config.toml")
	mustWrite(t, codexPath, "# >>> "+old+"-relay managed begin >>>\nopenai_base_url = \"http://127.0.0.1:18180/v1\"\n# <<< "+old+"-relay managed end <<<\n")
	keychain := fakeKeychain{}
	for index, item := range services() {
		keychain[item.Old] = []byte("secret-value-" + string(rune('a'+index)))
	}

	options := Options{Home: home, CodexConfig: codexPath, Keychain: keychain}
	inspection, err := Run("inspect", options)
	if err != nil || inspection.Status != "ready" {
		t.Fatalf("inspect: result=%#v err=%v", inspection, err)
	}
	applied, err := Run("apply", options)
	if err != nil || applied.Status != "applied" {
		t.Fatalf("apply: result=%#v err=%v", applied, err)
	}
	newConfig := filepath.Join(home, ".config", newStem, "relay.json")
	content, err := os.ReadFile(newConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), old) || !strings.Contains(string(content), newStem) {
		t.Fatalf("config was not transformed: %s", content)
	}
	if _, err := os.Stat(filepath.Join(oldRoot, "relay.json")); err != nil {
		t.Fatalf("legacy config was not preserved: %v", err)
	}
	for _, item := range services() {
		if string(keychain[item.New]) != string(keychain[item.Old]) {
			t.Fatalf("Keychain item %s was not copied", item.Name)
		}
	}
	journalContent, err := os.ReadFile(applied.Journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range keychain {
		if strings.Contains(string(journalContent), string(secret)) {
			t.Fatal("migration journal contains a credential value")
		}
	}
	var migrationJournal journal
	if err := json.Unmarshal(journalContent, &migrationJournal); err != nil {
		t.Fatal(err)
	}
	for _, entry := range migrationJournal.Keychain {
		if !validDigest(entry.AfterSHA256) {
			t.Fatalf("Keychain journal witness is invalid: %#v", entry)
		}
	}

	rolledBack, err := Run("rollback", options)
	if err != nil || rolledBack.Status != "rolled_back" {
		t.Fatalf("rollback: result=%#v err=%v", rolledBack, err)
	}
	if _, err := os.Stat(newConfig); !os.IsNotExist(err) {
		t.Fatalf("new config survived rollback: %v", err)
	}
	codexContent, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codexContent), old+"-relay managed begin") {
		t.Fatal("Codex marker backup was not restored")
	}
	for _, item := range services() {
		if _, exists := keychain[item.New]; exists {
			t.Fatalf("new Keychain item %s survived rollback", item.Name)
		}
		if _, exists := keychain[item.Old]; !exists {
			t.Fatalf("legacy Keychain item %s was removed", item.Name)
		}
	}
}

func TestInspectBlocksConflictingDestination(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	mustWrite(t, filepath.Join(home, ".config", old, "relay.json"), old)
	mustWrite(t, filepath.Join(home, ".config", newStem, "relay.json"), "foreign")
	result, err := Run("inspect", Options{Home: home, Keychain: fakeKeychain{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" {
		t.Fatalf("status=%q, want blocked", result.Status)
	}
	if _, err := Run("apply", Options{Home: home, Keychain: fakeKeychain{}}); err == nil {
		t.Fatal("apply accepted a conflicting destination")
	}
}

func TestRollbackRefusesModifiedMigratedArtifact(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	mustWrite(t, filepath.Join(home, ".config", old, "relay.json"), `{"name":"`+old+`"}`)
	options := Options{Home: home, Keychain: fakeKeychain{}}
	result, err := Run("apply", options)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".config", newStem, "relay.json"), "modified")
	if _, err := Run("rollback", options); err == nil || !strings.Contains(err.Error(), "modified artifact") {
		t.Fatalf("rollback error=%v", err)
	}
	var record journal
	content, err := os.ReadFile(result.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatal(err)
	}
	if record.Phase != "applied" {
		t.Fatalf("journal phase=%q", record.Phase)
	}
}

func TestInterruptedKeychainCopyCanRollbackWithoutDeletingLegacy(t *testing.T) {
	home := t.TempDir()
	item := services()[0]
	keys := &interruptingKeychain{fakeKeychain: fakeKeychain{
		item.Old: []byte("secret-value"),
	}}
	options := Options{Home: home, Keychain: keys}
	if _, err := Run("apply", options); err == nil {
		t.Fatal("apply unexpectedly survived an interrupted Keychain copy")
	}
	_, journalPath := paths(options)
	record, err := readJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != "recovery_required" || len(record.Keychain) != 1 || !record.Keychain[0].Created {
		t.Fatalf("journal does not own the interrupted Keychain mutation: %#v", record)
	}
	if _, err := Run("rollback", options); err != nil {
		t.Fatal(err)
	}
	if _, exists := keys.fakeKeychain[item.New]; exists {
		t.Fatal("new Keychain item survived interrupted rollback")
	}
	if _, exists := keys.fakeKeychain[item.Old]; !exists {
		t.Fatal("legacy Keychain item was removed")
	}
}

func TestRollbackRefusesModifiedMigratedKeychainItemBeforeDeletingAnything(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	mustWrite(t, filepath.Join(home, ".config", old, "relay.json"), `{"name":"`+old+`"}`)
	keys := fakeKeychain{}
	for index, service := range services() {
		keys[service.Old] = []byte("secret-value-" + string(rune('a'+index)))
	}
	options := Options{Home: home, Keychain: keys}
	result, err := Run("apply", options)
	if err != nil {
		t.Fatal(err)
	}
	modified := services()[len(services())-1]
	keys[modified.New] = []byte("replacement-secret")

	if _, err := Run("rollback", options); err == nil || !strings.Contains(err.Error(), "modified Keychain item") {
		t.Fatalf("rollback error=%v", err)
	}
	for _, service := range services() {
		if _, exists := keys[service.New]; !exists {
			t.Fatalf("Keychain item %s was deleted before rollback preflight completed", service.Name)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", newStem, "relay.json")); err != nil {
		t.Fatalf("migrated artifact changed after refused rollback: %v", err)
	}
	record, err := readJournal(result.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != "applied" {
		t.Fatalf("journal phase=%q, want applied", record.Phase)
	}
}

func TestInterruptedKeychainCopyRollbackRefusesModifiedDestination(t *testing.T) {
	home := t.TempDir()
	item := services()[0]
	keys := &interruptingKeychain{fakeKeychain: fakeKeychain{
		item.Old: []byte("secret-value"),
	}}
	options := Options{Home: home, Keychain: keys}
	if _, err := Run("apply", options); err == nil {
		t.Fatal("apply unexpectedly survived an interrupted Keychain copy")
	}
	keys.fakeKeychain[item.New] = []byte("replacement-secret")

	if _, err := Run("rollback", options); err == nil || !strings.Contains(err.Error(), "modified Keychain item") {
		t.Fatalf("rollback error=%v", err)
	}
	if string(keys.fakeKeychain[item.New]) != "replacement-secret" {
		t.Fatal("modified destination was not preserved")
	}
}

func TestInterruptedKeychainCopyRollbackAllowsAbsentDestination(t *testing.T) {
	home := t.TempDir()
	item := services()[0]
	keys := &interruptingKeychain{fakeKeychain: fakeKeychain{
		item.Old: []byte("secret-value"),
	}}
	options := Options{Home: home, Keychain: keys}
	if _, err := Run("apply", options); err == nil {
		t.Fatal("apply unexpectedly survived an interrupted Keychain copy")
	}
	delete(keys.fakeKeychain, item.New)

	if _, err := Run("rollback", options); err != nil {
		t.Fatal(err)
	}
	if _, exists := keys.fakeKeychain[item.Old]; !exists {
		t.Fatal("legacy Keychain item was removed")
	}
}

func TestRollbackRechecksKeychainWitnessImmediatelyBeforeDelete(t *testing.T) {
	home := t.TempDir()
	item := services()[0]
	keys := &changingBeforeDeleteKeychain{fakeKeychain: fakeKeychain{
		item.Old: []byte("secret-value"),
	}, service: item.New}
	options := Options{Home: home, Keychain: keys}
	if _, err := Run("apply", options); err != nil {
		t.Fatal(err)
	}

	if _, err := Run("rollback", options); err == nil || !strings.Contains(err.Error(), "destination changed") {
		t.Fatalf("rollback error=%v", err)
	}
	if string(keys.fakeKeychain[item.New]) != "replacement-secret" {
		t.Fatal("credential changed after preflight was deleted")
	}
}

func TestWitnesslessCreatedKeychainJournalFailsClosed(t *testing.T) {
	home := t.TempDir()
	options := Options{Home: home, Keychain: fakeKeychain{}}
	_, journalPath := paths(options)
	record := journal{
		SchemaVersion: 1,
		Phase:         "recovery_required",
		Keychain: []journalKeychain{{
			Name: "cf_access_client_id", Service: services()[0].New, Created: true,
		}},
	}
	if err := writeJournal(journalPath, &record); err != nil {
		t.Fatal(err)
	}

	if _, err := Run("rollback", options); err == nil || !strings.Contains(err.Error(), "witness") {
		t.Fatalf("rollback error=%v", err)
	}
}

func TestPreexistingEqualKeychainDestinationIsNotJournalOwned(t *testing.T) {
	home := t.TempDir()
	item := services()[0]
	keys := fakeKeychain{
		item.Old: []byte("secret-value"),
		item.New: []byte("secret-value"),
	}
	options := Options{Home: home, Keychain: keys}
	result, err := Run("apply", options)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readJournal(result.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Keychain) != 0 {
		t.Fatalf("pre-existing destination was journaled as owned: %#v", record.Keychain)
	}
	if _, err := Run("rollback", options); err != nil {
		t.Fatal(err)
	}
	if string(keys[item.New]) != "secret-value" {
		t.Fatal("pre-existing equal destination was deleted")
	}
}

func TestCodexMigrationChangesOnlyTheMarkerOwnedBlock(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	userOwned := "user_note = \"" + old + "-relay must remain\"\n"
	managed := "# >>> " + old + "-relay managed begin >>>\n" +
		"model_catalog_json = \"" + filepath.Join(home, ".codex", old+"-catalog.json") + "\"\n" +
		"# <<< " + old + "-relay managed end <<<\n"
	mustWrite(t, codexPath, userOwned+managed)

	if _, err := Run("apply", Options{Home: home, CodexConfig: codexPath, Keychain: fakeKeychain{}}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), userOwned) {
		t.Fatalf("user-owned content was changed: %s", content)
	}
	if strings.Contains(string(content), "# >>> "+old+"-relay managed begin >>>") ||
		!strings.Contains(string(content), "# >>> "+newStem+" managed begin >>>") {
		t.Fatalf("managed marker block was not migrated: %s", content)
	}
}

func TestInspectRejectsIncompleteCodexMarkerBlock(t *testing.T) {
	home := t.TempDir()
	old := legacyStem()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	mustWrite(t, codexPath, "# >>> "+old+"-relay managed begin >>>\n")

	if _, err := Run("inspect", Options{Home: home, CodexConfig: codexPath, Keychain: fakeKeychain{}}); err == nil ||
		!strings.Contains(err.Error(), "incomplete or duplicated") {
		t.Fatalf("inspect error=%v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
