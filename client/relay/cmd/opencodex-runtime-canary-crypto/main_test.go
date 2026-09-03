package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSignVerifyAndTLSLoopbackIdentity(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := generate([]string{"--directory", directory}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	input := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(input, []byte("{\"schema\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signature := filepath.Join(directory, "manifest.sig")
	if err := sign([]string{
		"--private-key", filepath.Join(directory, "canary-ed25519.pem"),
		"--input", input,
		"--output", signature,
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verify([]string{
		"--public-key", filepath.Join(directory, "canary-ed25519.pub"),
		"--input", input,
		"--signature", signature,
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	certificatePath := filepath.Join(directory, "tls.crt")
	keyPath := filepath.Join(directory, "tls.key")
	if _, err := tls.LoadX509KeyPair(certificatePath, keyPath); err != nil {
		t.Fatalf("TLS keypair: %v", err)
	}
	encoded, err := os.ReadFile(certificatePath)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(encoded)
	if block == nil || len(rest) != 0 {
		t.Fatal("TLS certificate PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := certificate.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("TLS certificate lacks exact loopback SAN: %v", err)
	}
	if !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("TLS certificate cannot serve as the isolated fixture trust anchor")
	}
}

func TestVerifyRejectsTamperedSignatureAndGenerateWillNotOverwrite(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := generate([]string{"--directory", directory}); err != nil {
		t.Fatal(err)
	}
	if err := generate([]string{"--directory", directory}); err == nil {
		t.Fatal("generate overwrote existing key material")
	}
	input := filepath.Join(directory, "manifest.json")
	signature := filepath.Join(directory, "manifest.sig")
	if err := os.WriteFile(input, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sign([]string{
		"--private-key", filepath.Join(directory, "canary-ed25519.pem"),
		"--input", input,
		"--output", signature,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(signature)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] == 'A' {
		data[0] = 'B'
	} else {
		data[0] = 'A'
	}
	if err := os.WriteFile(signature, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify([]string{
		"--public-key", filepath.Join(directory, "canary-ed25519.pub"),
		"--input", input,
		"--signature", signature,
	}); err == nil {
		t.Fatal("tampered signature verified")
	}
}
