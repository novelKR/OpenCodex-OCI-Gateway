package localdev

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

func signedManifest(t *testing.T, manifest any) ([]byte, []byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload)) + "\n")
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return payload, signature, key
}

func validManifest() Manifest {
	return Manifest{
		Schema:       ManifestSchemaVersion,
		Distribution: Distribution,
		Version:      "1.2.3-dev.1",
		SourceCommit: strings.Repeat("a", 40),
		Artifacts: []Artifact{{
			OS: "darwin", Arch: "arm64", Component: ComponentMenuBarBundle, File: BundleFile,
			SHA256: strings.Repeat("b", 64), BundleID: BundleID,
		}},
		Documents: []Document{{File: NoticesFile, SHA256: strings.Repeat("c", 64)}},
	}
}

func TestVerifyAcceptsStrictLocalDevelopmentManifest(t *testing.T) {
	payload, signature, key := signedManifest(t, validManifest())
	got, err := Verify(payload, signature, key)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Distribution != Distribution || got.Artifacts[0].File != BundleFile {
		t.Fatalf("Verify() = %+v", got)
	}
}

func TestVerifyRejectsProductionMetadata(t *testing.T) {
	for name, extra := range map[string]any{
		"artifact URL": map[string]any{"url": "https://example.test/local.zip"},
		"Team ID":      map[string]any{"team_id": "EXAMPLETEAM"},
		"notarization": map[string]any{"notarization": "stapled"},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := map[string]any{
				"os": "darwin", "arch": "arm64", "component": ComponentMenuBarBundle,
				"file": BundleFile, "sha256": strings.Repeat("b", 64), "bundle_id": BundleID,
			}
			for key, value := range extra.(map[string]any) {
				artifact[key] = value
			}
			manifest := map[string]any{
				"schema": ManifestSchemaVersion, "distribution": Distribution, "version": "1.2.3",
				"source_commit": strings.Repeat("a", 40),
				"artifacts":     []map[string]any{artifact},
				"documents":     []map[string]any{{"file": NoticesFile, "sha256": strings.Repeat("c", 64)}},
			}
			payload, signature, key := signedManifest(t, manifest)
			if _, err := Verify(payload, signature, key); err == nil {
				t.Fatal("Verify() accepted production metadata")
			}
		})
	}
}

func TestVerifyRejectsWrongArtifactOrSignature(t *testing.T) {
	manifest := validManifest()
	manifest.Artifacts[0].BundleID = "com.example.invalid"
	payload, signature, key := signedManifest(t, manifest)
	if _, err := Verify(payload, signature, key); err == nil {
		t.Fatal("Verify() accepted wrong bundle identifier")
	}
	signature[0] = 'A'
	if _, err := Verify(payload, signature, key); err == nil {
		t.Fatal("Verify() accepted tampered signature")
	}
}
