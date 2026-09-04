# OpenCodex OCI Gateway Guide

Run a private OpenCodex backend on an Oracle Cloud Infrastructure (OCI) Always Free
eligible instance. Connect Codex CLI or Desktop through Cloudflare Zero Trust without
exposing OpenCodex directly to the public internet.

This is the Gateway track of the repository. The separate OpenCodex Runtime package
is a candidate-only artifact and is not a prerequisite or replacement for these host
deployment instructions. Return to the [repository overview](../README.md) to compare
the two tracks.

## When to Use the Gateway

- **Private self-hosted backend:** Run a dedicated OpenCodex server on cloud
  infrastructure (eligible for OCI Always Free, subject to capacity and tenancy
  limits) without managed API gateway costs.
- **Protected perimeter:** Connect through Cloudflare Tunnels and Nginx key checking
  without exposing the OpenCodex API, Dashboard, or internal metrics to public inbound
  ports, while retaining standard SSH for maintenance.
- **Native client compatibility:** Use standard Codex CLI and Desktop applications on
  macOS and Linux as if they were talking directly to a local endpoint.

## Prerequisites

Before getting started, ensure you have:

- **OCI host:** A Linux instance, such as Ubuntu 24.04 on an OCI Always Free eligible
  `VM.Standard.E2.1.Micro` shape.
- **Cloudflare account:** A domain with Cloudflare Zero Trust, including configured
  Tunnel and Access service tokens.
- **Client credentials:** A Cloudflare Access Service Token (Client ID and Secret) and
  a Gateway API key configured on the host.
- **Client software:** On Apple Silicon macOS 26+, download the reviewed
  `OpenCodexRelay.app.zip` from this repository's immutable GitHub Releases. For Linux
  source builds, install Go 1.24+.

> **macOS distribution:** Public Relay releases use SemVer tags without a `v` prefix
> (beginning with `0.3.6`). GitHub Actions builds and verifies every asset before
> publishing it as an immutable release. The app is ad-hoc signed with the Hardened
> Runtime, so first launch requires the macOS approval described below.

## Quick Start

Choose your starting path.

### Path A: Bootstrap and Configure the Host (OCI)

1. Clone this repository and prepare non-secret deployment configuration:

   ```bash
   cp config/deployment.example.toml deployment.toml
   python3 pilot/tools/deployment_config.py validate deployment.toml
   python3 pilot/tools/deployment_config.py render deployment.toml
   ```

2. On the target Ubuntu 24.04 instance, allocate swap and run host bootstrap:

   ```bash
   cd pilot
   VERSION='REPLACE_WITH_REVIEWED_EXACT_VERSION'
   sudo ./scripts/configure-swap.sh 4G
   sudo env OPENCODEX_VERSION="${VERSION}" ./scripts/bootstrap-host.sh
   ```

3. Complete service credentials, Nginx configuration, and Cloudflare Tunnel routing
   by following the full host runbook in [pilot/README.md](../pilot/README.md).

### Path B: Connect Your Client (macOS and Linux)

#### On macOS (Apple Silicon, macOS 26+)

1. Download and expand `OpenCodexRelay.app.zip` from the exact version under
   [Releases](https://github.com/novelKR/OpenCodex-OCI-Gateway/releases). Developers
   who need an unpublished build should use the
   [local build guide](../client/relay/README.md).
2. Open the application. If macOS blocks it, navigate to
   **System Settings > Privacy & Security > Open Anyway**, then confirm **Open**.
3. In Settings, select **Move to Applications** to relocate the app to
   `/Applications/OpenCodexRelay.app` (or `~/Applications/`).
4. In **Control Center > Settings > Connect a self-hosted server**, select your
   authentication profile and enter your server URL and credentials.
5. Click **Prepare Relay**, then **Test connection**.
6. Once validated, click **Switch Codex to this server** to route your local Codex
   environment to the remote instance.

Detailed operational guidance and recovery procedures are documented in the
[Relay Operations Guide](local-codex-relay.md) ([Korean](local-codex-relay.ko.md)).

#### On Linux (CLI and Service)

For normal setups, use the signed installer. Store the three relay credentials
locally, then install the managed user service.

1. Create your local credentials file (`mode 0600`):

   ```bash
   mkdir -p ~/.config/opencodex-relay && chmod 700 ~/.config/opencodex-relay
   cat <<'EOF' > ~/.config/opencodex-relay/credentials.env
   CF_ACCESS_CLIENT_ID=REPLACE_WITH_SERVICE_TOKEN_ID
   CF_ACCESS_CLIENT_SECRET=REPLACE_WITH_SERVICE_TOKEN_SECRET
   OPENCODEX_GATEWAY_API_KEY=REPLACE_WITH_GATEWAY_KEY
   EOF
   chmod 600 ~/.config/opencodex-relay/credentials.env
   ```

2. Install and launch the managed relay service:

   ```bash
   ./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
     --github-repo novelKR/OpenCodex-OCI-Gateway \
     --public-key config/trust/opencodex-relay-release-ed25519.pub \
     --upstream https://REPLACE_WITH_API_HOSTNAME/v1
   ```

   Developers can instead compile the binaries directly:

   ```bash
   (cd client/relay && go build -o opencodex-relay ./cmd/opencodex-relay && go build -o opencodex-relayctl ./cmd/opencodex-relayctl)
   ```

   This command only builds the binaries; it does not configure or install the
   service. Follow [client/relay/README.md](../client/relay/README.md) for manual
   configuration and systemd registration.

## Verification

Check that your deployment is healthy:

- **Server-side smoke test (on the OCI host):**

  ```bash
  sudo ./pilot/scripts/smoke-test.sh
  ```

- **Local client Relay status (on the client):**

  ```bash
  opencodex-relayctl status
  curl -i http://127.0.0.1:18180/__relay/healthz
  ```

The complete distinction between static checks and live deployment acceptance is in
[Testing and Acceptance](testing.md).

## How It Works

```text
Codex CLI / AppServer / Desktop (macOS or Linux)
  │
  ▼ (HTTP 127.0.0.1:18180)
Device-Local Compatibility Relay
  │
  ▼ (Cloudflare Access Service Token + TLS)
Cloudflare Edge & Tunnel
  │
  ▼ (Loopback 127.0.0.1:18080)
Nginx Admission Boundary (API Key Validation & Endpoint Allowlist)
  │
  ▼ (Loopback 127.0.0.1:10100)
OpenCodex Core (OCI Linux Host)
```

- **API data plane:** Traffic from the local Codex client routes through loopback
  (`127.0.0.1:18180`) to the Cloudflare Tunnel. The server-side Nginx gateway
  validates API keys and admits only documented `/v1` endpoints.
- **Administrative control plane:** Routine administration connects through a
  dedicated SSH hostname protected by Cloudflare Access OTP/TOTP, with direct public
  `22/tcp` retained as an emergency SSH recovery path. Web dashboards and OAuth
  callbacks are reached exclusively through local SSH port forwarding, keeping
  management interfaces off the public web.

## Gateway Security Boundaries

- OpenCodex, Nginx backends, and cloudflared metrics bind exclusively to
  `127.0.0.1`.
- Unlisted endpoints, health metrics, and administration panels fail closed at the
  public perimeter.
- Cloudflare service-token credentials, the Nginx Gateway API key, provider tokens,
  and management credentials remain distinct and are never committed.
- See [SECURITY.md](../SECURITY.md) for vulnerability reporting.

## Migrating Existing Installations

For an existing legacy Relay setup, follow the migration section in the
[Relay Operations Guide](local-codex-relay.md) for journaled inspection, switch, and
rollback commands (`opencodex-relayctl migrate legacy-pw`).
