package codexconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDevelopmentOwnerRefusesProductionRoutingArtifacts(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	if err := EnableWithInteractiveProfile(path, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", filepath.Join(directory, "production.json")); err != nil {
		t.Fatalf("enable production routing: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightEnableWithInteractiveProfileForOwner(path, LocalDevelopmentOwner); err == nil {
		t.Fatal("local development owner accepted production routing artifacts")
	}
	if err := ValidateNativeRoutingForOwner(path, LocalDevelopmentOwner); err == nil {
		t.Fatal("local development owner treated production routing as native")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("foreign production routing was modified during local development preflight")
	}
}

func TestProductionOwnerRefusesLocalDevelopmentRoutingArtifacts(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	if err := EnableWithInteractiveProfileForOwner(path, LocalDevelopmentOwner, "http://127.0.0.1:18190/v1", "http://127.0.0.1:18192/v1", filepath.Join(directory, "development.json")); err != nil {
		t.Fatalf("enable local development routing: %v", err)
	}
	if err := PreflightEnableWithInteractiveProfileForOwner(path, ProductionOwner); err == nil {
		t.Fatal("production owner accepted local development routing artifacts")
	}
	if err := ValidateNativeRoutingForOwner(path, ProductionOwner); err == nil {
		t.Fatal("production owner treated local development routing as native")
	}
}

func TestOwnersUseDistinctInteractiveProfiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	production := InteractiveProfilePathForOwner(path, ProductionOwner)
	development := InteractiveProfilePathForOwner(path, LocalDevelopmentOwner)
	if production == development || filepath.Base(development) != "opencodex-relay-dev-interactive.config.toml" {
		t.Fatalf("profile namespaces are not distinct: production=%q development=%q", production, development)
	}
}
