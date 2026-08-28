// Package localdev verifies the deliberately isolated, offline-only
// development-distribution manifest. It is intentionally not an extension of
// internal/release: production release verification must never accept an
// unsigned or non-notarized local-development artifact.
package localdev

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	ManifestSchemaVersion  = 1
	Distribution           = "local_development"
	BundleFile             = "OpenCodexRelay Dev.app.zip"
	BundleID               = "io.github.novelkr.opencodex-relay.dev"
	NoticesFile            = "THIRD_PARTY_NOTICES.md"
	ComponentMenuBarBundle = "macos_menu_bar_bundle"
)

var (
	semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Manifest struct {
	Schema       int        `json:"schema"`
	Distribution string     `json:"distribution"`
	Version      string     `json:"version"`
	SourceCommit string     `json:"source_commit"`
	Artifacts    []Artifact `json:"artifacts"`
	Documents    []Document `json:"documents"`
}

type Artifact struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Component string `json:"component"`
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	BundleID  string `json:"bundle_id"`
}

type Document struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Verify proves both the Ed25519 signature and the strict local-only schema.
// It deliberately rejects fields unknown to this schema, including production
// artifact URLs, Team IDs, and notarization metadata.
func Verify(manifestBytes, signatureBytes, publicKeyPEM []byte) (Manifest, error) {
	key, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureBytes)))
	if err != nil {
		return Manifest{}, fmt.Errorf("decode local development manifest signature: %w", err)
	}
	if !ed25519.Verify(key, manifestBytes, signature) {
		return Manifest{}, errors.New("local development manifest signature is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode local development manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("local development manifest contains trailing JSON")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func VerifyFiles(manifestPath, signaturePath, publicKeyPath string) (Manifest, error) {
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return Manifest{}, err
	}
	key, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return Manifest{}, err
	}
	return Verify(manifest, signature, key)
}

func (m Manifest) Validate() error {
	if m.Schema != ManifestSchemaVersion || m.Distribution != Distribution || !semverPattern.MatchString(m.Version) || !commitPattern.MatchString(m.SourceCommit) {
		return errors.New("local development manifest header is invalid")
	}
	if len(m.Artifacts) != 1 {
		return errors.New("local development manifest must contain exactly one artifact")
	}
	artifact := m.Artifacts[0]
	if artifact.OS != "darwin" || artifact.Arch != "arm64" || artifact.Component != ComponentMenuBarBundle || artifact.File != BundleFile || artifact.BundleID != BundleID || !shaPattern.MatchString(artifact.SHA256) {
		return errors.New("local development manifest bundle artifact is invalid")
	}
	if len(m.Documents) != 1 || m.Documents[0].File != NoticesFile || !shaPattern.MatchString(m.Documents[0].SHA256) {
		return errors.New("local development manifest notices document is invalid")
	}
	return nil
}

func parsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("local development public key is not a PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse local development public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("local development public key must be Ed25519")
	}
	return key, nil
}
