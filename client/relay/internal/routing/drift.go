package routing

import (
	"errors"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
)

// AppliedRoutingDriftCheck validates only relay-owned Codex artifacts for a
// stable or pending state. It deliberately skips `applying`: the controller's
// journal is the authoritative crash witness while TOML/profile are changed
// as one local transaction. Any later stable state is checked on the next
// watcher refresh before data-plane admission remains open.
func AppliedRoutingDriftCheck(relayConfigPath string) DriftCheck {
	return func(state State) error {
		if state.Phase == PhaseApplying || state.Phase == PhaseRecoveryRequired || state.BoundCodexConfigPath == "" {
			return nil
		}
		cfg, err := config.Load(relayConfigPath)
		if err != nil {
			return errors.New("relay configuration is unavailable for routing drift check")
		}
		owner, err := codexconfig.OwnerForID(cfg.Scope())
		if err != nil {
			return errors.New("relay configuration has an unsupported routing ownership scope")
		}
		switch state.AppliedBackend {
		case BackendNone:
			return codexconfig.ValidateNativeRoutingForOwner(state.BoundCodexConfigPath, owner)
		case BackendExternal:
			return codexconfig.ValidateManagedRoutingForOwner(
				state.BoundCodexConfigPath,
				owner,
				"http://"+cfg.ListenAddress+"/v1",
				"http://"+cfg.Responses.Scheduler.InteractiveListenAddress+"/v1",
				cfg.Catalog.Path,
			)
		case BackendLocalOpenCodex:
			local, localErr := cfg.LocalOpenCodexRuntimeConfig()
			if localErr != nil {
				return errors.New("local OpenCodex profile is unavailable for routing drift check")
			}
			return codexconfig.ValidateManagedRoutingForOwner(
				state.BoundCodexConfigPath,
				owner,
				"http://"+local.ListenAddress+"/v1",
				"http://"+local.Responses.Scheduler.InteractiveListenAddress+"/v1",
				local.Catalog.Path,
			)
		case BackendLocalAppleContainer:
			local, localErr := cfg.LocalAppleContainerRuntimeConfig()
			if localErr != nil {
				return errors.New("local Apple Container profile is unavailable for routing drift check")
			}
			return codexconfig.ValidateManagedRoutingForOwner(
				state.BoundCodexConfigPath,
				owner,
				"http://"+local.ListenAddress+"/v1",
				"http://"+local.Responses.Scheduler.InteractiveListenAddress+"/v1",
				local.Catalog.Path,
			)
		default:
			return errors.New("routing backend is unknown")
		}
	}
}
