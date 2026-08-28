# Verification assets and evidence

Platform cadence, change and rollback order, and dated outcomes belong to the
private deployment overlay. This document owns only how each validation layer
runs and what it proves.

## Local asset checks

Run from the project root:

```bash
bash -n pilot/scripts/*.sh ops/oci/*.sh client/relay/scripts/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test -count=1 ./... && go vet ./...)
(cd client/relay && go test -race -count=1 ./internal/handoff ./internal/routing)
(cd client/relay/macos/OpenCodexRelay && swift test && swift build)
git diff --check
```

When a change also modifies the separately managed upstream `opencodex/`
source, first verify the reviewed baseline and nested diff because the outer
clone neither supplies nor pins that checkout. The baseline for this change is
[`d9de89557c3bd154e5f1508125def7c8789ac8c5`](https://github.com/lidge-jun/opencodex/commit/d9de89557c3bd154e5f1508125def7c8789ac8c5).

```bash
git -C opencodex rev-parse HEAD
(cd opencodex && bun run typecheck && bun run test && bun run privacy:scan)
git -C opencodex diff --check
```

The Python tests validate complete Responses SSE parsing and reject:

- a non-SSE `Content-Type`;
- a failed/error terminal event;
- data after `response.completed`;
- Chat Completions-style `[DONE]` frames;
- an incomplete partial frame falsely treated as a complete event.

This layer is static evidence for source, scripts, and documentation contracts.
It does not prove current availability of a client relay, central gateway,
Desktop/Remote UI, or Voice. Use the layered deployment acceptance list in
[`local-codex-relay.md`](local-codex-relay.md) before declaring the
compatibility layer live.

For an exact Relay teardown profile, artifact reconstruction, closure/hash
matching, adapter import/preflight, and Swift/Go tests prove only the static and
isolated contract. Automatic-removal acceptance additionally requires a
disposable macOS run that preserves user data, restores native Codex, removes
the service and launchers, verifies rediscovery, and recovers an interrupted
transition. A Compose render or image build likewise does not prove a live
container or provider path.

## OCI host smoke test

After installing or changing the host configuration:

```bash
cd /home/ubuntu/pilot
sudo ./scripts/smoke-test.sh
```

It verifies service versions and identities, loopback-only listeners, disabled RPC port `111`, swap
and cgroup limits, Nginx/logrotate syntax, gateway key admission, endpoint blocking, separate
generation/WebSocket safety ceilings of `32`, non-serialized sibling generation admission, and the
disabled Responses WebSocket `426` fallback. The overlap probe holds a request body open and does not
invoke an upstream model.

## External Cloudflare/SSE test

Run from a real TTY because credentials are collected through hidden prompts:

```bash
ssh -tt \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_INSTANCE_IP \
  "sudo env PUBLIC_BASE_URL='https://REPLACE_WITH_API_HOSTNAME' \
    EXPECTED_ACCESS_AUD='REPLACE_WITH_ACCESS_APPLICATION_AUD' \
    /home/ubuntu/pilot/scripts/external-smoke-test.sh"
```

The script requires no command-line secret and removes its mode-`0600` temporary header files on
exit. It independently proves:

1. Cloudflare Access rejects a request without a service token.
2. Access plus the gateway key reaches `/v1/models`.
3. Nginx rejects an invalid gateway key after Access admission.
4. The public route blocks the OpenCodex management API.
5. A real Responses SSE stream contains `response.created`, non-empty text deltas, and one final
   `response.completed` without error/incomplete events.
6. A second real Responses SSE request can complete while the first public stream remains active;
   Nginx does not use its emergency ceiling as a one-request scheduler.

For a newly enrolled macOS Codex-only client, also require:

- three distinct login-Keychain items for the Cloudflare client ID, Cloudflare client secret, and
  Nginx gateway key, with no literal credential in either Codex TOML file or service definition;
- `opencodex-relayctl status` reports a loopback relay, relay-owned native
  `openai_base_url`, and the expected catalog state;
- the `0600` catalog contains a non-empty visible `.models` array after a
  relay-authenticated refresh, with hidden entries absent;
- one ordinary `codex exec ...` run reports the expected model and receives a
  non-empty final answer through the local relay;
- Linux Remote Control homes report `default_model_match=1` for `gpt-5.6-luna`;
- the central Voice gate remains `404` unless both the local and central
  explicit opt-ins were approved. A Voice opt-in additionally requires a real
  media/WebSocket call; HTTP route acceptance alone is not sufficient.

Do not accept an address-bar visit to `/v1/models` as this test: browsers do not supply the Service
Token and gateway headers, so an Access page or `401` is expected.

## Interactive Cloudflare SSH test

The API test above does not validate the separate interactive SSH Access application. Perform this
test from an administrator machine with both `opencodex-relay-access` and `opencodex-relay-direct`
configured as documented in [`ssh-and-client-access.md`](ssh-and-client-access.md).

Before connecting, inspect the deployed Cloudflare configuration and require all of the following:

- one exact SSH hostname routed as `SSH` to `localhost:22` on the existing Tunnel;
- a separate Self-hosted Access application with no wildcard hostname overlap;
- application session duration `24h` and an attached policy whose session duration is the same as
  the application;
- `Allow` with `Include: Emails = REPLACE_WITH_ADMIN_EMAIL`;
- `Require: Login Methods = One-time PIN`;
- custom independent MFA allowing only authenticator-app TOTP with a `24h` duration;
- `Use identity provider MFA`, Binding Cookie, Cloudflare One Client authentication and automatic
  cloudflared authentication disabled for the initial validation;
- no `Bypass`, `Service Auth`, `Any Service Token`, broad `Allow`, or API policy attached to the
  SSH application.

On the OCI host, rerun `sudo ./pilot/scripts/smoke-test.sh`. It verifies the OpenCodex version
recorded in `/etc/opencodex/expected-version` (or the current `2.10.1` fallback on an older host).
For that legacy fallback only, a missing canonical `ocx` launcher may be read from the installed
package manifest; managed hosts still require their explicit state file and canonical launcher.
After independently reviewing an already-running legacy deployment, record its state with
`sudo ./pilot/scripts/upgrade-opencodex.sh adopt-current 2.10.1` so subsequent smoke tests use
the managed state file rather than that migration fallback.
Its `sshd -T` checks must report
public-key authentication enabled and password, keyboard-interactive, host-based, GSSAPI, and empty
password authentication disabled. The repository does not deploy `sshd_config`; a listener alone
or a successful key login is not evidence that fallback authentication is disabled. If the host
uses `Match` rules, inspect the effective configuration for the administrator's actual source
address with `sshd -T -C` before accepting the public recovery path.

Use the already trusted direct path to record the OpenSSH host-key fingerprint, then compare it
exactly with the first prompt on the Access hostname:

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
ssh -o ClearAllForwardings=yes opencodex-relay-access \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
```

With a clean or expired Access/MFA session, the Access connection must require the allowed email's
PIN, then independent TOTP, then the existing SSH private key. A wrong PIN, wrong TOTP, or wrong SSH
key must fail at its own boundary. A reconnect within the documented `24h` window may reuse the
valid Access and MFA sessions; that is expected and is not evidence that MFA is disabled. Confirm
both transports independently:

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-access 'test "$(id -un)" = ubuntu'
ssh -o ClearAllForwardings=yes opencodex-relay-direct 'test "$(id -un)" = ubuntu'
```

The direct command is expected to succeed without an Access challenge: retained public `22/tcp` is
outside Cloudflare Access and is the recovery path. This does not use a Cloudflare `Bypass` policy.
A local listener check does not prove the OCI ingress rule, so this external direct connection
remains required.

Finally, start the preferred forwarding session in one terminal and leave it running:

```bash
ssh -N opencodex-relay-access
```

In a second terminal, verify the Dashboard:

```bash
curl --fail --silent --show-error http://127.0.0.1:11010/ >/dev/null
```

During one real account login, also confirm that the `localhost:1455/auth/callback` browser request
reaches the waiting OCI listener through the same SSH session. Stop the first terminal with
`Ctrl-C` after both checks.

After one controlled reboot, repeat both new SSH connections and confirm the connector through the
direct recovery path:

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'systemctl is-enabled cloudflared.service && systemctl is-active cloudflared.service'
```

Record only pass/fail and timestamps. Do not retain email PINs, TOTP values or seeds, Access
cookies/tokens, or verbose SSH logs.

## Historical session evidence

The dated Relay and OpenCodex outcome summaries and the canonical safe
synthetic transcript remain in the private deployment overlay; they are not
duplicated or published here.

The 2026-08-01 VS Code terminal showed all six external assertions passing. The captured summary
reported 21 SSE events, 11 text deltas, final `response.completed`, and a successful shared-slot
concurrency probe using `gpt-5.3-codex-spark` at that time.

This is historical evidence, not a current availability guarantee. No raw response bodies,
Cloudflare credentials, or gateway key were retained in the dated workspace.

## OAuth exploration checks

The nested OpenCodex branch `agent/remote-dashboard-oauth` contains a superseded manual-code-only
experiment. During the session, the following focused checks passed:

- callback-server tests: 3 passed;
- Codex auth API tests: 140 passed;
- GUI add-account OAuth tests: 3 passed;
- root TypeScript `tsc --noEmit`: passed.

The full root/GUI/docs regression suite was not completed, and the experiment does not implement
the requested custom-domain callback. Treat the patch as research only.
