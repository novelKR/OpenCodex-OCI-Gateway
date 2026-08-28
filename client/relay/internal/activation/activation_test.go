package activation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/appserver"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
)

func TestApplyWhileQuiescedFailsClosedWithoutAnAppServerHome(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog.PendingPath(catalogPath), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyWhileQuiesced(catalogPath, true, "")
	if err == nil {
		t.Fatal("automatic restart accepted a missing app_server_home")
	}
	if !result.Pending || !catalog.Pending(catalogPath) {
		t.Fatal("pending marker was cleared without an AppServer identity")
	}
}

func TestApplyWhileQuiescedKeepsMarkerForManualActivation(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog.PendingPath(catalogPath), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyWhileQuiesced(catalogPath, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pending || !catalog.Pending(catalogPath) {
		t.Fatal("manual activation cleared the pending marker")
	}
}

func TestApplyWhileQuiescedKeepsMarkerForUnverifiableAppServer(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog.PendingPath(catalogPath), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousList := listAppServers
	previousRestart := restartAppServers
	t.Cleanup(func() {
		listAppServers = previousList
		restartAppServers = previousRestart
	})
	listAppServers = func(string) (appserver.Selection, error) {
		return appserver.Selection{Unverifiable: []appserver.Process{{PID: 101, Command: "codex app-server daemon"}}}, nil
	}
	restartAppServers = func([]appserver.Process) ([]int, error) {
		t.Fatal("activation must not restart a partial AppServer selection")
		return nil, nil
	}

	result, err := ApplyWhileQuiesced(catalogPath, true, "/home/test/.codex")
	if err == nil {
		t.Fatal("activation accepted an unverifiable AppServer candidate")
	}
	if !result.Pending || !catalog.Pending(catalogPath) {
		t.Fatal("pending marker was cleared while an AppServer candidate was unverifiable")
	}
}

func TestApplyWhileQuiescedClearsMarkerOnlyAfterVerifiedSelectionRestarts(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog.PendingPath(catalogPath), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousList := listAppServers
	previousRestart := restartAppServers
	t.Cleanup(func() {
		listAppServers = previousList
		restartAppServers = previousRestart
	})
	listAppServers = func(string) (appserver.Selection, error) {
		return appserver.Selection{Eligible: []appserver.Process{{PID: 101, Command: "codex app-server daemon"}}}, nil
	}
	restartAppServers = func(processes []appserver.Process) ([]int, error) {
		if len(processes) != 1 || processes[0].PID != 101 {
			t.Fatalf("restart selection = %#v, want PID 101", processes)
		}
		return []int{101}, nil
	}

	result, err := ApplyWhileQuiesced(catalogPath, true, "/home/test/.codex")
	if err != nil {
		t.Fatal(err)
	}
	if result.Pending || catalog.Pending(catalogPath) {
		t.Fatal("pending marker was not cleared after the verified AppServer restart")
	}
	if len(result.Restarted) != 1 || result.Restarted[0] != 101 {
		t.Fatalf("restarted = %#v, want PID 101", result.Restarted)
	}
}
