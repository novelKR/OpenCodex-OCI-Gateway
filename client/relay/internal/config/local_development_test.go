package config

import (
	"path/filepath"
	"testing"
)

func TestLocalDevelopmentScopeRequiresIsolatedListeners(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceKeychain)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InstallationScope = InstallationScopeLocalDevelopment
	if err := cfg.Validate(); err == nil {
		t.Fatal("local development scope accepted production listener")
	}
	cfg.ListenAddress = LocalDevelopmentListenAddress
	cfg.Responses.Scheduler.InteractiveListenAddress = LocalDevelopmentInteractiveListen
	cfg.Catalog.Path = filepath.Join(t.TempDir(), LocalDevelopmentExternalCatalog)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("isolated local development config rejected: %v", err)
	}
}

func TestLocalDevelopmentScopeRejectsProductionCatalogNamespace(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceKeychain)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InstallationScope = InstallationScopeLocalDevelopment
	cfg.ListenAddress = LocalDevelopmentListenAddress
	cfg.Responses.Scheduler.InteractiveListenAddress = LocalDevelopmentInteractiveListen
	cfg.Catalog.Path = filepath.Join(t.TempDir(), "opencodex-relay-catalog.json")
	if err := cfg.Validate(); err == nil {
		t.Fatal("local development scope accepted a production catalog namespace")
	}
}
