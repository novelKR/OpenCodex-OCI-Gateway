# OpenCodex OCI Gateway

Run a private OpenCodex backend on an Oracle Cloud Infrastructure (OCI) Always Free eligible instance. Connect Codex CLI or Desktop through Cloudflare Zero Trust without exposing OpenCodex directly to the public internet.

This repository provides the host deployment scripts, gateway rules, and local compatibility relays needed to operate it. Sensitive identifiers, private keys, live tokens, and operational records stay outside this repository.

## When to Use This Project

- **Private Self-Hosted Backend**: Run a dedicated OpenCodex server on cloud infrastructure (eligible for OCI Always Free, subject to capacity and tenancy limits) without managed API gateway costs.
- **Protected Perimeter**: Connect securely over Cloudflare Tunnels and Nginx key-checking without exposing OpenCodex API, Dashboard, or internal metrics to public inbound ports (retaining standard SSH for maintenance).
- **Native Client Compatibility**: Use standard Codex CLI and Desktop applications on macOS and Linux as if they were talking directly to a local endpoint.

## Prerequisites

Before getting started, ensure you have:

- **OCI Host**: A Linux instance (e.g., Ubuntu 24.04 on an OCI Always Free eligible `VM.Standard.E2.1.Micro` shape).
- **Cloudflare Account**: A domain with Cloudflare Zero Trust (Tunnel and Access service tokens configured).
- **Client Credentials**: A Cloudflare Access Service Token (Client ID and Secret) and a Gateway API Key configured on your host.
- **Client Software**: On Apple Silicon macOS 26+, use a reviewed `OpenCodexRelay.app.zip` release when available. For Linux source builds, install Go 1.24+.

> **macOS release status:** Check [Releases](https://github.com/novelKR/OpenCodex-OCI-Gateway/releases) for `OpenCodexRelay.app.zip`. If no archive is listed, a prebuilt app is not available yet; developers can follow the [local build guide](client/relay/README.md).

## Quick Start

Choose your starting path:

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

3. Complete service credentials, Nginx configuration, and Cloudflare Tunnel routing by following the full host runbook in [pilot/README.md](pilot/README.md).

### Path B: Connect Your Client (macOS & Linux)

#### On macOS (Apple Silicon, macOS 26+)

1. Download and expand `OpenCodexRelay.app.zip` from [Releases](https://github.com/novelKR/OpenCodex-OCI-Gateway/releases). If it is not listed, use the developer build path linked above.
2. Open the application. If macOS blocks it, navigate to **System Settings > Privacy & Security > Open Anyway**, then confirm **Open**.
3. In Settings, select **Move to Applications** to relocate the app to `/Applications/OpenCodexRelay.app` (or `~/Applications/`).
4. In **Control Center > Settings > Connect a self-hosted server**, select your authentication profile and enter your server URL and credentials.
5. Click **Prepare Relay**, then **Test connection**.
6. Once validated, click **Switch Codex to this server** to route your local Codex environment to the remote instance.

Detailed operational guidance and recovery procedures are documented in the [Relay Operations Guide](docs/local-codex-relay.md) ([Korean](docs/local-codex-relay.ko.md)).

#### On Linux (CLI & Service)

For normal setups, use the signed installer. Store the three relay credentials locally, then install the managed user service:

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
     --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
     --public-key /path/to/opencodex-relay-release-ed25519.pub \
     --upstream https://REPLACE_WITH_API_HOSTNAME/v1
   ```

   Developers can instead compile the binaries directly:
   ```bash
   (cd client/relay && go build -o opencodex-relay ./cmd/opencodex-relay && go build -o opencodex-relayctl ./cmd/opencodex-relayctl)
   ```

   This command only builds the binaries; it does not configure or install the service. Follow [client/relay/README.md](client/relay/README.md) for manual configuration and systemd registration.

### Verification

Check that your deployment is healthy:

- **Server-side smoke test (on OCI host)**:
  ```bash
  sudo ./pilot/scripts/smoke-test.sh
  ```

- **Local client relay status (on client)**:
  ```bash
  opencodex-relayctl status
  curl -i http://127.0.0.1:18180/__relay/healthz
  ```

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

- **API Data Plane**: Traffic from your local Codex client routes through loopback (`127.0.0.1:18180`) to the Cloudflare Tunnel. The server-side Nginx gateway validates API keys and admits only documented `/v1` endpoints.
- **Administrative Control Plane**: Routine administration connects via a dedicated SSH hostname protected by Cloudflare Access OTP/TOTP (with direct public 22/tcp retained as an emergency SSH recovery path). Web dashboards and OAuth callbacks are reached exclusively through local SSH port forwarding, keeping management interfaces off the public web.

## Security Principles

- **Zero committed secrets**: Private keys, tokens, credentials, and live logs are strictly excluded from this repository.
- **Strict loopback isolation**: OpenCodex, Nginx backends, and cloudflared metrics bind exclusively to `127.0.0.1`.
- **Fail-closed admission**: Unlisted endpoints, health metrics, and administration panels are blocked at the perimeter.
- **Vulnerability reporting**: Report security issues through GitHub Private Vulnerability Reporting as described in [SECURITY.md](SECURITY.md).

## Documentation Hub

| Topic | Document | Scope |
| --- | --- | --- |
| Architecture | [Architecture Specification](docs/architecture.md) | Network topology, ports, trust boundaries, and reboot recovery |
| Remote Access | [SSH and Client Access](docs/ssh-and-client-access.md) | SSH configuration, dashboard port forwarding, and client setup |
| Client Relay | [Relay Operations Guide](docs/local-codex-relay.md) | Complete operational runbook for macOS and Linux relays |
| Host Deployment | [Deployment Configuration](docs/deployment-configuration.md) | Schema specification for non-secret deployment rendering |
| Isolation | [Container Profile](docs/container-profile.md) | Packaging and process isolation profile |
| Operations | [Updates and Rollbacks](docs/updates.md) | Version upgrade procedures and recovery routines |
| Verification | [Testing and Acceptance](docs/testing.md) | Live acceptance criteria, SSE/WebSocket validation, and test suite |
| Public Core | [Public Core and Overlay](docs/public-core-and-overlay.md) | Boundary separation between public core and private overlay |

### Migrating Existing Installations
If you have an existing legacy relay setup, refer to the migration section in the [Relay Operations Guide](docs/local-codex-relay.md) for journaled inspection, switch, and rollback commands (`opencodex-relayctl migrate legacy-pw`).

## Contributing

Review [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines. Run the full verification suite before submitting a pull request:

```bash
bash -n pilot/scripts/*.sh pilot/libexec/* ops/oci/*.sh client/relay/scripts/*.sh tools/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test ./... && go test -race ./... && go vet ./...)
(cd client/relay/macos/OpenCodexRelay && swift test && swift build -c release)
git diff --check
```

## License

Distributed under the Apache License 2.0. Third-party notices and licenses are documented in [NOTICE](NOTICE) and constituent subdirectories.
