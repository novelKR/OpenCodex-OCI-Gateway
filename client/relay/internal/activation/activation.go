// Package activation applies a pending catalog only after the local relay has
// no active protocol sessions.
package activation

import (
	"errors"
	"fmt"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/appserver"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
)

type Result struct {
	Pending   bool
	Restarted []int
}

var (
	listAppServers    = appserver.ListForCodexHome
	restartAppServers = appserver.Restart
)

// ApplyWhileQuiesced assumes the resident relay already holds its admission
// gate. Callers that cannot prove quiescence must leave the pending marker for
// the resident owner instead of invoking this function.
func ApplyWhileQuiesced(catalogPath string, manageAppServer bool, appServerHome string) (Result, error) {
	if !catalog.Pending(catalogPath) {
		return Result{}, nil
	}
	result := Result{Pending: true}
	if !manageAppServer {
		return result, nil
	}
	if appServerHome == "" {
		return result, errors.New("automatic AppServer restart requires catalog.app_server_home")
	}
	selection, err := listAppServers(appServerHome)
	if err != nil {
		return result, fmt.Errorf("find Codex app-server processes: %w", err)
	}
	if len(selection.Unverifiable) > 0 {
		return result, fmt.Errorf("defer catalog activation: %d Codex app-server candidate(s) have an unverifiable CODEX_HOME", len(selection.Unverifiable))
	}
	if len(selection.Eligible) > 0 {
		restarted, err := restartAppServers(selection.Eligible)
		result.Restarted = restarted
		if err != nil {
			return result, err
		}
	}
	if err := catalog.ClearPending(catalogPath); err != nil {
		return result, err
	}
	result.Pending = false
	return result, nil
}
