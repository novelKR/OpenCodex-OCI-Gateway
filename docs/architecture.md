# Architecture and trust boundaries

## Purpose

The OCI VM is a centralized OpenCodex account pool and Responses API gateway for a
small set of mutually trusted clients. It is not a multi-tenant service and does not
provide per-user RBAC, tenant-isolated credentials, or independent usage quotas.

## Data plane

```text
Native Codex CLI / AppServer / Desktop config
  | built-in openai provider -> local loopback relay
  v
127.0.0.1:18180 opencodex-relay
  | CF-Access-Client-Id / CF-Access-Client-Secret
  | X-OpenCodex-API-Key (injected only here)
  v
Cloudflare Access
  v
Named Tunnel connector
  v
127.0.0.1:18080 Nginx
  | exact path/method allowlist
  | gateway-key validation
  | one shared generation connection
  v
127.0.0.1:10100 OpenCodex
  v
selected ChatGPT/Codex account
```

The public API exposes only exact compatible routes:

- `GET /v1/models`
- `POST /v1/responses`
- `POST /v1/responses/compact`
- `POST /v1/images/generations` and `POST /v1/images/edits`
- `GET /v1/opencodex/artifacts/{opaque-id}` and `POST /v1/alpha/search`
- the Responses WebSocket upgrade; GPT-Live/Realtime setup and sideband
  WebSocket routes only after the central Voice gate is explicitly enabled.

Nginx returns `404` for Dashboard files, `/api/*`, `/healthz`, and all other API
surfaces. It strips Cloudflare and gateway credentials before forwarding to OpenCodex.
Voice is `404` by default. The native relay and central gateway each have a
separate Voice opt-in; neither enables the other.

[`local-codex-relay.md`](local-codex-relay.md) is authoritative for client
enrollment, catalog/AppServer lifecycle, Desktop feature boundaries, and
rollback. This data plane does not expose the Codex AppServer transport to the
internet.

## Management plane

The preferred management path adds an interactive Cloudflare Access gate in front of the
existing OpenSSH authentication:

```text
Mac OpenSSH
  -> client-side cloudflared
  -> Cloudflare Access (exact email One-time PIN + independent TOTP)
  -> separate SSH hostname and Named Tunnel
  -> OCI cloudflared -> 127.0.0.1:22 OpenSSH
  -> existing SSH private key authentication
```

The existing direct path remains available for recovery when Cloudflare, DNS, the identity
flow, or client-side `cloudflared` is unavailable:

```text
Mac OpenSSH -> OCI public TCP/22 -> OpenSSH -> existing SSH private key authentication
```

These paths have four explicit invariants:

- the SSH hostname, Access application, policy, and AUD are separate from the API application;
- both paths terminate at the same OpenSSH daemon, and the documented client aliases use the same
  SSH private key;
- direct public `22/tcp` intentionally bypasses Cloudflare Access and remains governed by OCI
  ingress rules;
- no Dashboard HTTP route is published through Cloudflare Tunnel.

The repository does not provision the host's `sshd_config`. The operational acceptance gate uses
`sshd -T` to require public-key authentication and reject password, keyboard-interactive,
host-based, GSSAPI, and empty-password alternatives. Until that live check passes, key-only server
authentication is an unverified deployment assumption rather than an architectural fact.

The Dashboard and management API stay on OpenCodex's loopback listener. After either SSH path is
authenticated, the administrator reaches them through a local forward:

```text
Mac 127.0.0.1:11010 -> SSH -> OCI 127.0.0.1:10100
```

ChatGPT browser login currently uses a second forward:

```text
Mac localhost:1455 -> SSH -> OCI 127.0.0.1:1455
```

These are separate concerns: adding an SSH hostname does not publish the Dashboard and does not
change the OAuth `redirect_uri`. See [Dashboard and OAuth](dashboard-and-oauth.md).

## Port inventory

| Port | Bind | Owner | Exposure |
| --- | --- | --- | --- |
| `22/tcp` | wildcard OpenSSH listener | OpenSSH | OCI ingress for direct SSH; `ssh://127.0.0.1:22` origin for the separate Tunnel hostname |
| `10100/tcp` | `127.0.0.1` | OpenCodex | host local and SSH forwarding only |
| `1455/tcp` | `127.0.0.1` while login is pending | OpenCodex OAuth callback | SSH forwarding only |
| `18080/tcp` | `127.0.0.1` | Nginx gateway | cloudflared origin only |
| `20241/tcp` | `127.0.0.1` | cloudflared metrics | host local only |

The client relay listens only on each enrolled machine's `127.0.0.1:18180`; it
is not a server listener or a Cloudflare origin.

OCI security lists/NSGs must not expose `10100`, `1455`, `18080`, or `20241`.

## Credential boundaries

| Credential | Termination point | Purpose |
| --- | --- | --- |
| Cloudflare service token | Cloudflare Access/cloudflared | Admit an approved machine to the API hostname |
| Exact-email One-time PIN and independent TOTP | Cloudflare Access | Interactive admission to the separate SSH hostname |
| Nginx gateway key | Nginx | Prevent origin or Access-policy mistakes from reaching OpenCodex |
| OpenCodex account tokens | OpenCodex credential store | Authenticate upstream ChatGPT/Codex calls |
| SSH private key | Administrator machine/OpenSSH | Authenticate to the host after either SSH transport path |

One credential must not be reused for another boundary.

## Persistence and recovery

- `opencodex.service` starts the proxy as the dedicated `opencodex` user.
- `nginx.service` owns the local API gateway.
- `cloudflared.service` reads its token through systemd `LoadCredential=`.
- All three should be enabled for `multi-user.target` and rechecked after one controlled reboot.
- Direct public SSH remains the independent recovery path for Tunnel or Access failures; its
  continued availability must be tested separately from the interactive SSH hostname.
- Dashboard startup warnings do not supersede the host's systemd status; this deployment
  deliberately does not use OpenCodex's own launcher shim.

The canonical unit/configuration files live in [`../pilot/`](../pilot/).
