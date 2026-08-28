# Experimental OpenCodex container profile

This profile containers only OpenCodex. Nginx, cloudflared, OpenSSH, Remote
Codex home, Relay, and Control Center remain host-native.

Linux network_mode: host preserves the fixed 127.0.0.1:10100 API and localhost
1455 OAuth callback contracts. It provides no network sandbox. The image is
non-root, read-only, capability-free, no-new-privileges, PID and memory bounded,
and receives only /var/lib/opencodex. The Docker socket is not mounted.

The OpenCodex npm version and integrity, Bun version, dependency lock, and base
image index digest are pinned. Release images use
ghcr.io/novelkr/opencodex-oci-gateway:<gateway>-ocx-<opencodex> and publish
multi-arch amd64/arm64 manifests, SBOM, and provenance.

opencodex.service and opencodex-container.service conflict. The container unit
also refuses a tag-only image: container.env must provide an @sha256 digest
from the signed release manifest.

Static Compose tests and builds are not live acceptance. Promotion requires an
OCI E2.1.Micro canary covering memory/restarts, catalog, Responses, WebSocket,
Dashboard, SSH OAuth callback, loopback listeners, and reboot recovery.
