# SSH, Dashboard, and Codex client access

## Current machine inventory

Machine-specific values are stored in `../local/connection.local.md`, which is deliberately ignored
by Git. The SSH private key remains under `~/.ssh`; it is referenced, never copied into this project.

## Direct administration and recovery

```bash
ssh -tt \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_INSTANCE_IP
```

Use `-tt` for scripts that prompt through a TTY, including the external Cloudflare smoke test.
This path uses OCI public `22/tcp` and the SSH key directly; it does not pass through Cloudflare
Access. Keep it available as the recovery path while the selected dual-path design is in use.

## Cloudflare Access SSH

The preferred path uses a dedicated SSH hostname on the existing Named Tunnel. It is not the API
hostname and does not publish the Dashboard over HTTP. The server-side route and Access application
must be configured first as documented in [`../pilot/README.md`](../pilot/README.md#cloudflare-access-ssh).

Install `cloudflared` on the administrator machine and record its absolute path:

```bash
command -v cloudflared
cloudflared --version
```

The Access application admits one exact email through these two distinct challenges:

1. Cloudflare email One-time PIN;
2. Cloudflare Access independent MFA using an authenticator-app TOTP.

After Access admission, OpenSSH still requires the existing SSH private key. TOTP proves possession
of the enrolled secret; it is not a WARP device-posture or hardware-bound device check. For the
fixed, single-user administrator Mac mini, both the Access application session and the independent
MFA session are `24h`; the attached policy inherits the application session duration. This keeps the
email PIN, authenticator TOTP, and SSH-key boundary at the start of a new or expired work session
while allowing long coding-agent runs and ordinary reconnects from the same authenticated Mac.

The duration is a window for initiating or refreshing Access connections. Its expiry does not
reauthenticate every SSH packet or forcibly interrupt an already-established SSH connection. A new
connection after the Access or MFA session expires must pass the email PIN and TOTP again. This
application-specific choice does not change the account global session, the API Service Auth
application, or any separately managed SSH Access application.

## SSH config

Keep both aliases in the user's `~/.ssh/config`, not in this repository. Replace the `cloudflared`
path with the absolute result from `command -v cloudflared`.

For a minimal macOS configuration that only opens the Cloudflare Access SSH path, copy and verify
[`ssh-config.macos.example`](ssh-config.macos.example). The example file is not installed into the
user's `~/.ssh/config` automatically.

```sshconfig
Host opencodex-relay-access
    HostName REPLACE_WITH_SSH_ACCESS_HOSTNAME
    User ubuntu
    IdentityFile ~/.ssh/REPLACE_WITH_SSH_KEY
    IdentitiesOnly yes
    ExitOnForwardFailure yes
    ServerAliveInterval 15
    ServerAliveCountMax 3
    ProxyCommand REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h
    LocalForward 11010 127.0.0.1:10100
    LocalForward 1455 127.0.0.1:1455

Host opencodex-relay-direct
    HostName REPLACE_WITH_INSTANCE_IP
    User ubuntu
    IdentityFile ~/.ssh/REPLACE_WITH_SSH_KEY
    IdentitiesOnly yes
    ExitOnForwardFailure yes
    ServerAliveInterval 15
    ServerAliveCountMax 3
    LocalForward 11010 127.0.0.1:10100
    LocalForward 1455 127.0.0.1:1455
```

The two hostnames are separate `known_hosts` identities. Before accepting the first SSH-hostname
prompt, obtain the expected host-key fingerprint through the already trusted direct path:

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
```

Compare that fingerprint exactly with the one shown by the first Access-hostname connection. Then
use the preferred or recovery path as needed:

```bash
ssh opencodex-relay-access
ssh -o ClearAllForwardings=yes opencodex-relay-direct
```

Do not run both aliases with their default forwards at the same time because they claim the same
local ports. `ClearAllForwardings=yes` is suitable for recovery commands that do not need Dashboard
or OAuth forwarding.

### Why direct SSH to the Tunnel hostname fails

The Access hostname is a Cloudflare Tunnel entry point, not the OCI instance's public TCP/22
address. A Named Tunnel accepts the SSH origin only after the **client-side `cloudflared`** process
has established the Cloudflare Access SSH transport. A plain command such as the following does
not start that process:

```bash
ssh -N \
  -L 11010:127.0.0.1:10100 \
  -L 1455:127.0.0.1:1455 \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_SSH_ACCESS_HOSTNAME
```

`-L` only requests local forwarding after an SSH transport has been established, and `-i` only
supplies an SSH key after the connection reaches `sshd`. Neither option converts raw SSH over
TCP/22 into the Cloudflare Access SSH protocol. The connection therefore reaches a Cloudflare
edge address without the required Access/Tunnel client transport and is expected to fail.

The common accidental cause is SSH configuration matching. Given this configuration:

```sshconfig
Host opencodex-relay-access
    HostName REPLACE_WITH_SSH_ACCESS_HOSTNAME
    ProxyCommand REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h
```

only `ssh opencodex-relay-access` matches the `Host opencodex-relay-access` stanza. Typing the expanded
hostname directly does not match that exact alias, so OpenSSH does not inherit its `ProxyCommand`.
Use the alias for the normal path:

```bash
ssh -N opencodex-relay-access
```

For a one-off command that intentionally uses the hostname rather than the alias, specify the
same proxy explicitly:

```bash
ssh -N \
  -L 11010:127.0.0.1:10100 \
  -L 1455:127.0.0.1:1455 \
  -o ExitOnForwardFailure=yes \
  -o 'ProxyCommand=REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h' \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_SSH_ACCESS_HOSTNAME
```

In contrast, direct SSH to the OCI public IP succeeds only because this deployment intentionally
keeps public `22/tcp` as an independent recovery route:

```text
Mac OpenSSH -> OCI public IP:22 -> OCI sshd -> SSH key
```

It bypasses Cloudflare Access and does not prove that the Tunnel hostname accepts raw SSH. The
preferred path is different:

```text
Mac OpenSSH -> ProxyCommand cloudflared access ssh -> Cloudflare Access
  -> Named Tunnel -> OCI 127.0.0.1:22 -> OCI sshd -> SSH key
```

Compare the effective configuration before troubleshooting a connection. The alias should show a
`proxycommand`; an unconfigured expanded hostname will not:

```bash
ssh -G opencodex-relay-access | rg '^(hostname|proxycommand|localforward)'
ssh -G REPLACE_WITH_SSH_ACCESS_HOSTNAME | rg '^(hostname|proxycommand|localforward)'
```

## Dashboard and OAuth tunnel

The existing local OpenCodex may already use port `10100`, so the remote Dashboard uses local
port `11010`. Start the preferred Access path, or substitute `opencodex-relay-direct` during a
Cloudflare outage:

```bash
ssh -N opencodex-relay-access
```

Then open:

```text
http://127.0.0.1:11010/#codex-auth
```

During account login, the browser's `http://localhost:1455/auth/callback` request crosses the same
SSH session to the pending callback listener on the OCI host.

This `1455` forward is for the ChatGPT/Codex callback flow only. Cursor OAuth is a separate
server-side PKCE polling flow: register it as the `opencodex` service account and open the printed
Cursor URL on the administrator's Mac. See [Registering Cursor OAuth on OCI over SSH](cursor-oauth-over-ssh.md).

### macOS Dashboard launcher

[`../client/macos/open-dashboard.sh`](../client/macos/open-dashboard.sh) automates the normal
Access SSH path without storing or bypassing any credential. It verifies that the selected SSH
alias has a `cloudflared access ssh` `ProxyCommand`, starts a dedicated SSH ControlMaster with
the Dashboard and ChatGPT callback forwards, then opens the Dashboard in macOS.

The default alias is the current `ocx-ssh`; override it for the generic example alias or another
verified Access alias. That alias must retain the documented `11010 -> 127.0.0.1:10100` and
`1455 -> 127.0.0.1:1455` forwards. The first start after an Access session expires still requires
the normal Cloudflare email/TOTP approval in a browser.

```bash
./client/macos/open-dashboard.sh
./client/macos/open-dashboard.sh status
./client/macos/open-dashboard.sh stop

# Generic documentation alias, without opening a browser window
./client/macos/open-dashboard.sh --host opencodex-relay-access --no-open
```

The launcher intentionally refuses an alias with no Cloudflare Access `ProxyCommand`; it does not
silently fall back to the OCI public-IP recovery path. It creates its own SSH control socket under
`~/.ssh`, so the tunnel survives after `start` returns and must be closed with `stop`. If a stale
control socket is reported, inspect it before removing it; do not delete an active SSH socket.

## Server status

```bash
sudo systemctl is-enabled opencodex nginx cloudflared
sudo systemctl is-active opencodex nginx cloudflared
sudo ss -lntup
sudo journalctl -b -u opencodex -u nginx -u cloudflared --no-pager
```

Expected application listeners are loopback-only `10100`, `18080`, and `20241`. Port `1455`
exists only while an OAuth flow is waiting.

## Remote Codex client (native relay)

New macOS and Linux clients use the native compatibility relay described in
[Native Codex compatibility relay](local-codex-relay.md). It keeps the
built-in `openai` provider and puts the Cloudflare Service Token and distinct
gateway key in the local relay, not in Codex's environment or a custom provider
table. Follow the signed installation and platform-specific credential setup in
[`../client/relay/README.md`](../client/relay/README.md).

The same pattern is used for an externally located Remote Control Linux host:
install the `linux/amd64` or `linux/arm64` relay with its dedicated catalog,
then run `configure-remote-codex-routing.sh enable-relay
--allow-remote-interruption`. This intentionally restarts the managed
AppServer; it is not a live-session-safe operation.

Voice stays off centrally until the operator enables both the client relay and
the gateway feature gate. A native Desktop image or voice control plane that
does not use the compatible `/v1` route remains native and is not redirected by
this setup.

## Legacy custom-provider rollback

The former direct custom-provider enrollment is no longer an active runbook.
Do not export admission credentials into a Codex process or create a new
`pw_opencodex` provider profile. If an existing client must be rolled back,
follow the inspected timestamp-backup procedure in
[Native Codex compatibility-layer operations](local-codex-relay.md#update-and-rollback).

Credential rotation remains an admission-plane operation: overlap the old and
new Cloudflare Service Tokens long enough to test the replacement, but update
every enrolled relay together when rotating the currently shared Nginx gateway
key.
