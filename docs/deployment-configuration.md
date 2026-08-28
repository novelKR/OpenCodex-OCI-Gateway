# deployment.toml schema v1

deployment.toml contains only non-secret deployment identifiers. The public
repository provides config/deployment.example.toml; each operator creates a
private copy.

Accepted values cover the API origin, SSH Access hostname, Cloudflare
tunnel/AUD/team identifiers, exact OpenCodex version, Remote routing mode,
release repository and public-key fingerprint, and native/container profile.
Ports and loopback listeners are fixed security contracts.

The public renderer accepts only `relay` for an external Remote and
`local-relay` for a loopback Remote. Runtime scripts continue to understand
`legacy` for migration and rollback of existing deployments, but schema v1
does not create new legacy configurations.

The validator rejects unknown fields, secret-like fields, environment
interpolation, shell expressions, URL paths/query/ports, and non-canonical
hostnames. Rendered files under .generated/<deployment>/ are deterministic.

remote-opencodex.json replaces the old sourceable shell configuration. Shell
consumers use pilot/scripts/load-remote-config.sh and jq with an exact key
allowlist. Codex ~/.codex/config.toml is not a deployment source; Relay edits
only its marker-owned section transactionally.
