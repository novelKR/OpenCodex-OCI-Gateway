package main

import (
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

func TestCatalogCommandReportsTheSingleWriter(t *testing.T) {
	if got := catalogRefreshOwnership(config.CatalogOwnerRelay); got != "catalog_refresh=owned_by_resident" {
		t.Fatalf("relay catalog ownership = %q", got)
	}
	if got := catalogRefreshOwnership(config.CatalogOwnerRemoteManager); got != "catalog_refresh=owned_by_remote_manager" {
		t.Fatalf("Remote catalog ownership = %q", got)
	}
}
