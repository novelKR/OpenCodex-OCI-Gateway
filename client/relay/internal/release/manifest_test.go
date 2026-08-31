package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"testing"
)

func TestVerifyAndSelect(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":1,"artifacts":[{"os":"darwin","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest)))
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	verified, err := Verify(manifest, signature, key)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := verified.Select("darwin", "arm64")
	if err != nil || artifact.File != "relay" {
		t.Fatalf("artifact = %#v, err = %v", artifact, err)
	}
}

func TestVerifyRejectsModifiedManifest(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":1,"artifacts":[{"os":"linux","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest)))
	der, _ := x509.MarshalPKIXPublicKey(publicKey)
	key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if _, err := Verify(append(manifest, ' '), signature, key); err == nil {
		t.Fatal("modified manifest was accepted")
	}
}

func TestVerifyRevisionTwoRequiresThirdPartyNotices(t *testing.T) {
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":2,"artifacts":[{"os":"linux","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	signature, key := signManifestForTest(t, manifest)

	verified, err := Verify(manifest, signature, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Documents) != 1 || verified.Documents[0].File != ThirdPartyNoticesFile {
		t.Fatalf("documents = %#v", verified.Documents)
	}
}

func TestVerifyRevisionTwoRejectsInvalidDocuments(t *testing.T) {
	artifact := `"artifacts":[{"os":"linux","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
	tests := map[string]string{
		"missing":       `{"version":"1.2.3","compatibility_revision":2,` + artifact + `}`,
		"wrong file":    `{"version":"1.2.3","compatibility_revision":2,` + artifact + `,"documents":[{"file":"LICENSE.txt","url":"https://example.test/LICENSE.txt","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`,
		"duplicate":     `{"version":"1.2.3","compatibility_revision":2,` + artifact + `,"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`,
		"non-https URL": `{"version":"1.2.3","compatibility_revision":2,` + artifact + `,"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"http://example.test/THIRD_PARTY_NOTICES.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`,
		"invalid hash":  `{"version":"1.2.3","compatibility_revision":2,` + artifact + `,"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := []byte(raw)
			signature, key := signManifestForTest(t, manifest)
			if _, err := Verify(manifest, signature, key); err == nil {
				t.Fatal("invalid revision 2 manifest was accepted")
			}
		})
	}
}

func TestVerifyRejectsUnsupportedCompatibilityRevision(t *testing.T) {
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":3,"artifacts":[{"os":"linux","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	signature, key := signManifestForTest(t, manifest)
	if _, err := Verify(manifest, signature, key); err == nil {
		t.Fatal("unsupported compatibility revision was accepted")
	}
}

func TestVerifyRejectsLocalDevelopmentManifestSchema(t *testing.T) {
	// The local-only distribution is intentionally a different manifest
	// protocol. Even if an operator happens to reuse an Ed25519 key, its
	// source-only schema must not be mistaken for a downloadable production
	// release manifest.
	manifest := []byte(`{"schema":1,"distribution":"local_development","version":"1.2.3-dev.1","source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay Dev.app.zip","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","bundle_id":"io.github.novelkr.opencodex-relay.dev"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}`)
	signature, key := signManifestForTest(t, manifest)
	if _, err := Verify(manifest, signature, key); err == nil {
		t.Fatal("production release verifier accepted a local development manifest")
	}
}

func TestVerifyRevisionFourAdHocMacBundleAndLinuxHelpers(t *testing.T) {
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":4,"artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay.app.zip","url":"https://example.test/OpenCodexRelay.app.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bundle_id":"com.example.relay","signing_mode":"adhoc"},{"os":"linux","arch":"amd64","component":"relay","file":"opencodex-relay_linux_amd64","url":"https://example.test/opencodex-relay_linux_amd64","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"os":"linux","arch":"amd64","component":"relayctl","file":"opencodex-relayctl_linux_amd64","url":"https://example.test/opencodex-relayctl_linux_amd64","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},{"os":"linux","arch":"arm64","component":"relay","file":"opencodex-relay_linux_arm64","url":"https://example.test/opencodex-relay_linux_arm64","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},{"os":"linux","arch":"arm64","component":"relayctl","file":"opencodex-relayctl_linux_arm64","url":"https://example.test/opencodex-relayctl_linux_arm64","sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}]}`)
	signature, key := signManifestForTest(t, manifest)

	verified, err := Verify(manifest, signature, key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := verified.SelectComponent("darwin", "arm64", ComponentMacOSMenuBarBundle)
	if err != nil || bundle.File != "OpenCodexRelay.app.zip" {
		t.Fatalf("bundle = %#v, err = %v", bundle, err)
	}
	if _, err := verified.Select("darwin", "arm64"); err == nil {
		t.Fatal("v4 legacy selection was accepted")
	}
}

func TestVerifyRevisionFourRejectsDarwinRawHelper(t *testing.T) {
	manifest := []byte(`{"version":"1.2.3","compatibility_revision":4,"artifacts":[{"os":"darwin","arch":"arm64","component":"relay","file":"opencodex-relay_darwin_arm64","url":"https://example.test/opencodex-relay_darwin_arm64","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	signature, key := signManifestForTest(t, manifest)
	if _, err := Verify(manifest, signature, key); err == nil {
		t.Fatal("darwin raw helper was accepted in revision 4")
	}
}

func TestVerifyRevisionFourRejectsUnknownAndAppleSigningFields(t *testing.T) {
	base := `{"version":"1.2.3","compatibility_revision":4,"artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay.app.zip","url":"https://example.test/OpenCodexRelay.app.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bundle_id":"com.example.relay"%s},{"os":"linux","arch":"amd64","component":"relay","file":"relay-amd64","url":"https://example.test/relay-amd64","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"os":"linux","arch":"amd64","component":"relayctl","file":"relayctl-amd64","url":"https://example.test/relayctl-amd64","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},{"os":"linux","arch":"arm64","component":"relay","file":"relay-arm64","url":"https://example.test/relay-arm64","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},{"os":"linux","arch":"arm64","component":"relayctl","file":"relayctl-arm64","url":"https://example.test/relayctl-arm64","sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}]}`
	for name, field := range map[string]string{
		"team id":       `,"signing_mode":"adhoc","team_id":"ABCDE12345"`,
		"unknown field": `,"signing_mode":"adhoc","unexpected":true`,
		"missing mode":  ``,
	} {
		t.Run(name, func(t *testing.T) {
			manifest := []byte(fmt.Sprintf(base, field))
			signature, key := signManifestForTest(t, manifest)
			if _, err := Verify(manifest, signature, key); err == nil {
				t.Fatal("invalid revision 4 manifest was accepted")
			}
		})
	}
}

func TestVerifyRevisionFiveUpdaterMetadata(t *testing.T) {
	manifest := []byte(`{"version":"1.2.4-rc.1","compatibility_revision":5,"artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay.app.zip","url":"https://example.test/OpenCodexRelay.app.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bundle_id":"com.example.relay","signing_mode":"adhoc","minimum_macos_version":"26.0","integration_protocol":1,"helper_protocol":1},{"os":"linux","arch":"amd64","component":"relay","file":"relay-amd64","url":"https://example.test/relay-amd64","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"os":"linux","arch":"amd64","component":"relayctl","file":"relayctl-amd64","url":"https://example.test/relayctl-amd64","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},{"os":"linux","arch":"arm64","component":"relay","file":"relay-arm64","url":"https://example.test/relay-arm64","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},{"os":"linux","arch":"arm64","component":"relayctl","file":"relayctl-arm64","url":"https://example.test/relayctl-arm64","sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}],"channel":"preview","minimum_updater_version":"1.2.3","trust_key_id":"1111111111111111111111111111111111111111111111111111111111111111"}`)
	signature, key := signManifestForTest(t, manifest)

	verified, err := Verify(manifest, signature, key)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Channel != ChannelPreview || verified.MinimumUpdaterVersion != "1.2.3" {
		t.Fatalf("verified updater manifest = %#v", verified)
	}
	artifact, err := verified.SelectComponent("darwin", "arm64", ComponentMacOSMenuBarBundle)
	if err != nil || artifact.IntegrationProtocol != 1 || artifact.HelperProtocol != 1 {
		t.Fatalf("macOS updater artifact = %#v, err = %v", artifact, err)
	}
}

func TestVerifyRevisionFiveRejectsInvalidUpdaterMetadata(t *testing.T) {
	base := `{"version":"1.2.4-rc.1","compatibility_revision":5,"artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay.app.zip","url":"https://example.test/OpenCodexRelay.app.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bundle_id":"com.example.relay","signing_mode":"adhoc","minimum_macos_version":"26.0","integration_protocol":1,"helper_protocol":1},{"os":"linux","arch":"amd64","component":"relay","file":"relay-amd64","url":"https://example.test/relay-amd64","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"os":"linux","arch":"amd64","component":"relayctl","file":"relayctl-amd64","url":"https://example.test/relayctl-amd64","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},{"os":"linux","arch":"arm64","component":"relay","file":"relay-arm64","url":"https://example.test/relay-arm64","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},{"os":"linux","arch":"arm64","component":"relayctl","file":"relayctl-arm64","url":"https://example.test/relayctl-arm64","sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}],%s}`
	tests := map[string]string{
		"channel mismatch":        `"channel":"stable","minimum_updater_version":"1.2.3","trust_key_id":"1111111111111111111111111111111111111111111111111111111111111111"`,
		"invalid minimum updater": `"channel":"preview","minimum_updater_version":"1.2.03","trust_key_id":"1111111111111111111111111111111111111111111111111111111111111111"`,
		"invalid trust key":       `"channel":"preview","minimum_updater_version":"1.2.3","trust_key_id":"AAAA"`,
		"unknown field":           `"channel":"preview","minimum_updater_version":"1.2.3","trust_key_id":"1111111111111111111111111111111111111111111111111111111111111111","unexpected":true`,
	}
	for name, fields := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := []byte(fmt.Sprintf(base, fields))
			signature, key := signManifestForTest(t, manifest)
			if _, err := Verify(manifest, signature, key); err == nil {
				t.Fatal("invalid updater manifest was accepted")
			}
		})
	}
}

func TestVerifyRevisionFiveRequiresMacOSCompatibilityMetadata(t *testing.T) {
	manifest := []byte(`{"version":"1.2.4-rc.1","compatibility_revision":5,"artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"OpenCodexRelay.app.zip","url":"https://example.test/OpenCodexRelay.app.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bundle_id":"com.example.relay","signing_mode":"adhoc","minimum_macos_version":"26.0","helper_protocol":1},{"os":"linux","arch":"amd64","component":"relay","file":"relay-amd64","url":"https://example.test/relay-amd64","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"os":"linux","arch":"amd64","component":"relayctl","file":"relayctl-amd64","url":"https://example.test/relayctl-amd64","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},{"os":"linux","arch":"arm64","component":"relay","file":"relay-arm64","url":"https://example.test/relay-arm64","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},{"os":"linux","arch":"arm64","component":"relayctl","file":"relayctl-arm64","url":"https://example.test/relayctl-arm64","sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"documents":[{"file":"THIRD_PARTY_NOTICES.md","url":"https://example.test/THIRD_PARTY_NOTICES.md","sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}],"channel":"preview","minimum_updater_version":"1.2.3","trust_key_id":"1111111111111111111111111111111111111111111111111111111111111111"}`)
	signature, key := signManifestForTest(t, manifest)
	if _, err := Verify(manifest, signature, key); err == nil {
		t.Fatal("revision 5 manifest without integration protocol was accepted")
	}
}

func TestVerifyRejectsDuplicateJSONKeys(t *testing.T) {
	manifest := []byte(`{"version":"1.2.3","version":"1.2.4","compatibility_revision":1,"artifacts":[{"os":"linux","arch":"arm64","file":"relay","url":"https://example.test/relay","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	signature, key := signManifestForTest(t, manifest)
	if _, err := Verify(manifest, signature, key); err == nil {
		t.Fatal("duplicate manifest key was accepted")
	}
}

func signManifestForTest(t *testing.T, manifest []byte) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
