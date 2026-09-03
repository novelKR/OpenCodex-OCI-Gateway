package runtimemanifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

type manifestFixture struct {
	manifest   Manifest
	publicKey  []byte
	privateKey ed25519.PrivateKey
}

func newManifestFixture(t *testing.T) manifestFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyID := sha256.Sum256(der)
	return manifestFixture{
		publicKey:  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		privateKey: privateKey,
		manifest: Manifest{
			Schema:          1,
			ArtifactKind:    ArtifactKind,
			ArtifactVersion: "2.40.0-r1",
			ReleaseSequence: 7,
			Channel:         StableChannel,
			Source: Source{
				Repository:         ProductionSourceRepo,
				Revision:           strings.Repeat("a", 40),
				UpstreamLockSHA256: strings.Repeat("b", 64),
			},
			Upstream: Upstream{
				Repository:   ProductionUpstreamRepo,
				ReleaseID:    381148440,
				ReleaseTag:   "v2.40.0",
				Version:      "2.40.0",
				Revision:     strings.Repeat("c", 40),
				NPMPackage:   NPMPackage,
				NPMIntegrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
			},
			Image: Image{
				Repository:  ProductionImageRepository,
				IndexDigest: "sha256:" + strings.Repeat("d", 64),
				Platforms: []Platform{
					{OS: "linux", Arch: "amd64", Digest: "sha256:" + strings.Repeat("e", 64)},
					{OS: "linux", Arch: "arm64", Digest: "sha256:" + strings.Repeat("f", 64)},
				},
			},
			Compatibility: Compatibility{
				MinimumRelayVersion:   "0.3.9",
				MinimumMacOS:          "26.0",
				MinimumAppleContainer: "1.3.1",
				ManagementAPIRevision: 1,
				SecretDelivery:        SecretDeliveryUDSV1,
				StateFormatRevision:   1,
			},
			Canary: Canary{
				SourceRevision:     strings.Repeat("a", 40),
				WorkflowRunID:      "12345",
				WorkflowRunAttempt: 1,
				Result:             "passed",
			},
			TrustKeyID: hex.EncodeToString(keyID[:]),
		},
	}
}

func (fixture manifestFixture) signed(t *testing.T) ([]byte, []byte) {
	t.Helper()
	manifest, err := json.Marshal(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(fixture.privateKey, manifest)
	return manifest, []byte(base64.StdEncoding.EncodeToString(signature))
}

func TestVerifyAcceptsStrictRuntimeManifest(t *testing.T) {
	fixture := newManifestFixture(t)
	manifest, signature := fixture.signed(t)
	verified, err := Verify(manifest, signature, fixture.publicKey, VerifyOptions{
		HighestSeenSequence:   6,
		RelayVersion:          "0.3.9",
		MacOSVersion:          "26.5.1",
		AppleContainerVersion: "1.3.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.ArtifactVersion != "2.40.0-r1" || verified.ReleaseSequence != 7 {
		t.Fatalf("verified = %#v", verified)
	}
	if digest, ok := verified.ARM64Digest(); !ok || digest != "sha256:"+strings.Repeat("f", 64) {
		t.Fatalf("arm64 digest = %q, %t", digest, ok)
	}
}

func TestVerifyRejectsDuplicateUnknownTrailingAndOversize(t *testing.T) {
	fixture := newManifestFixture(t)
	for name, body := range map[string][]byte{
		"duplicate": []byte(`{"schema":1,"schema":1}`),
		"unknown":   []byte(`{"unexpected":true}`),
		"trailing":  []byte(`{} {}`),
		"oversize":  make([]byte, MaximumManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			signature := base64.StdEncoding.EncodeToString(ed25519.Sign(fixture.privateKey, body))
			if _, err := Verify(body, []byte(signature), fixture.publicKey, VerifyOptions{}); err == nil {
				t.Fatal("unsafe manifest was accepted")
			}
		})
	}
}

func TestVerifyAuthenticatesBeforeDecoding(t *testing.T) {
	fixture := newManifestFixture(t)
	body := []byte(`{"schema":1,"schema":1}`)
	invalidSignature := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	_, err := Verify(body, []byte(invalidSignature), fixture.publicKey, VerifyOptions{})
	if err == nil || err.Error() != "runtime manifest signature is invalid" {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyRejectsTrustSequencePlatformAndCompatibilityDrift(t *testing.T) {
	tests := map[string]func(*Manifest){
		"trust key": func(m *Manifest) { m.TrustKeyID = strings.Repeat("0", 64) },
		"platform":  func(m *Manifest) { m.Image.Platforms[1].Arch = "amd64" },
		"kind":      func(m *Manifest) { m.ArtifactKind = "relay" },
		"version":   func(m *Manifest) { m.Upstream.Version = "2.39.0" },
		"secret":    func(m *Manifest) { m.Compatibility.SecretDelivery = "env-file" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newManifestFixture(t)
			mutate(&fixture.manifest)
			body, signature := fixture.signed(t)
			if _, err := Verify(body, signature, fixture.publicKey, VerifyOptions{}); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
	fixture := newManifestFixture(t)
	body, signature := fixture.signed(t)
	if _, err := Verify(body, signature, fixture.publicKey, VerifyOptions{HighestSeenSequence: 8}); err == nil {
		t.Fatal("sequence rollback was accepted")
	}
}

func TestVerifyRequiresCanonicalOrderedDistinctPlatforms(t *testing.T) {
	tests := map[string]func(*Manifest){
		"order": func(m *Manifest) {
			m.Image.Platforms[0], m.Image.Platforms[1] = m.Image.Platforms[1], m.Image.Platforms[0]
		},
		"same child digest": func(m *Manifest) {
			m.Image.Platforms[1].Digest = m.Image.Platforms[0].Digest
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newManifestFixture(t)
			mutate(&fixture.manifest)
			body, signature := fixture.signed(t)
			if _, err := Verify(body, signature, fixture.publicKey, VerifyOptions{}); err == nil {
				t.Fatal("non-canonical platform set was accepted")
			}
		})
	}
}

func TestVerifyRejectsNumericCompatibilityVersionOverflow(t *testing.T) {
	fixture := newManifestFixture(t)
	fixture.manifest.Compatibility.MinimumMacOS = "4294967296.0"
	body, signature := fixture.signed(t)
	if _, err := Verify(body, signature, fixture.publicKey, VerifyOptions{}); err == nil {
		t.Fatal("overflowing numeric compatibility version was accepted")
	}
}

func TestVerifyRejectsWrongSignatureKey(t *testing.T) {
	fixture := newManifestFixture(t)
	body, signature := fixture.signed(t)
	other := newManifestFixture(t)
	if _, err := Verify(body, signature, other.publicKey, VerifyOptions{}); err == nil {
		t.Fatal("wrong key was accepted")
	}
}

func TestManifestSHA256BindsExactBytes(t *testing.T) {
	body := []byte("manifest")
	digest := sha256.Sum256(body)
	if got := ManifestSHA256(body); got != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %s", got)
	}
}

func TestParseArtifactVersionRequiresCanonicalStableVersion(t *testing.T) {
	version, revision, ok := ParseArtifactVersion("2.40.0-r12")
	if !ok || version != "2.40.0" || revision != 12 {
		t.Fatalf("parsed = %q, %d, %t", version, revision, ok)
	}
	for _, value := range []string{
		"2.40.0-r0",
		"2.40.0-r01",
		"2.40.0-rc.1-r1",
		"2.40.0+build.1-r1",
		"v2.40.0-r1",
		"2.40.0-r",
		"4294967296.40.0-r1",
		"2.40.0-r18446744073709551616",
	} {
		t.Run(value, func(t *testing.T) {
			if _, _, ok := ParseArtifactVersion(value); ok {
				t.Fatal("non-canonical runtime artifact version was accepted")
			}
		})
	}
	version, revision, ok = ParseArtifactVersion("2.40.0-r18446744073709551615")
	if !ok || version != "2.40.0" || revision != ^uint64(0) {
		t.Fatalf("maximum UInt64 revision parsed = %q, %d, %t", version, revision, ok)
	}
}
