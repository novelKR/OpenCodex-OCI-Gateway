// Command opencodex-runtime-canary-crypto provides only the ephemeral
// cryptographic operations needed by the dedicated Apple lifecycle canary.
// It is built from the candidate source on the runner and is never bundled in
// a Relay release.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const maximumInputBytes = 64 << 10

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("operation is required"))
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generate(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = errors.New("operation is invalid")
	}
	if err != nil {
		fatal(err)
	}
}

func generate(arguments []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("directory", "", "owner-only output directory")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || !cleanAbsolute(*directory) {
		return errors.New("generate arguments are invalid")
	}
	info, err := os.Lstat(*directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("output directory is unsafe")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate Ed25519 key")
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return errors.New("encode Ed25519 private key")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return errors.New("encode Ed25519 public key")
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := writeExclusive(filepath.Join(*directory, "canary-ed25519.pem"), privatePEM, 0o600); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(*directory, "canary-ed25519.pub"), publicPEM, 0o600); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(*directory, "canary-ed25519.der"), publicDER, 0o600); err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil || serial.Sign() <= 0 {
		return errors.New("generate certificate serial")
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "OpenCodex lifecycle canary loopback"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return errors.New("create loopback certificate")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	if err := writeExclusive(filepath.Join(*directory, "tls.key"), privatePEM, 0o600); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(*directory, "tls.crt"), certificatePEM, 0o600); err != nil {
		return err
	}
	digest := sha256.Sum256(publicDER)
	fmt.Printf("{\"schema\":1,\"trust_key_id\":\"%s\"}\n", hex.EncodeToString(digest[:]))
	return nil
}

func sign(arguments []string) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	privatePath := flags.String("private-key", "", "Ed25519 PKCS8 PEM")
	inputPath := flags.String("input", "", "canonical manifest")
	outputPath := flags.String("output", "", "base64 signature")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || !cleanAbsolute(*privatePath) || !cleanAbsolute(*inputPath) || !cleanAbsolute(*outputPath) {
		return errors.New("sign arguments are invalid")
	}
	private, err := readPrivate(*privatePath)
	if err != nil {
		return err
	}
	input, err := readBounded(*inputPath, maximumInputBytes)
	if err != nil {
		return err
	}
	signature := ed25519.Sign(private, input)
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(signature))+1)
	base64.StdEncoding.Encode(encoded, signature)
	encoded[len(encoded)-1] = '\n'
	return writeExclusive(*outputPath, encoded, 0o600)
}

func verify(arguments []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	publicPath := flags.String("public-key", "", "Ed25519 PKIX PEM")
	inputPath := flags.String("input", "", "canonical manifest")
	signaturePath := flags.String("signature", "", "base64 signature")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || !cleanAbsolute(*publicPath) || !cleanAbsolute(*inputPath) || !cleanAbsolute(*signaturePath) {
		return errors.New("verify arguments are invalid")
	}
	public, err := readPublic(*publicPath)
	if err != nil {
		return err
	}
	input, err := readBounded(*inputPath, maximumInputBytes)
	if err != nil {
		return err
	}
	encoded, err := readBounded(*signaturePath, 4096)
	if err != nil {
		return err
	}
	encoded = bytes.TrimSpace(encoded)
	signature, err := base64.StdEncoding.Strict().DecodeString(string(encoded))
	if err != nil || len(signature) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(signature) != string(encoded) || !ed25519.Verify(public, input, signature) {
		return errors.New("signature verification failed")
	}
	return nil
}

func readPrivate(path string) (ed25519.PrivateKey, error) {
	data, err := readBounded(path, 4096)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("private key PEM is invalid")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("private key is not Ed25519")
	}
	return key, nil
}

func readPublic(path string) (ed25519.PublicKey, error) {
	data, err := readBounded(path, 8192)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("public key PEM is invalid")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	key, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("public key is not Ed25519")
	}
	return key, nil
}

func readBounded(path string, maximum int64) ([]byte, error) {
	if !cleanAbsolute(path) {
		return nil, errors.New("input path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("input file is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("input file exceeds limit")
	}
	return data, nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	if !cleanAbsolute(path) || len(data) == 0 {
		return errors.New("output is invalid")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
