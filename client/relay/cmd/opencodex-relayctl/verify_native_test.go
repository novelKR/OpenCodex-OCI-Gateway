package main

import (
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

func TestLocalDevelopmentRoutingControllerRejectsProductionScope(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := localDevelopmentRoutingController(configPath, filepath.Join(filepath.Dir(configPath), "config.toml")); err == nil {
		t.Fatal("production relay config was accepted for local-dev native verification")
	}
}
