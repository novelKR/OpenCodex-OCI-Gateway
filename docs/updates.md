# Managed Codex and OpenCodex updates

This is the routine update path for this deployment. It keeps the central
OpenCodex proxy separate from Codex standalone packages on Remote Control
hosts, and preserves the loopback gateway, service identity, and Remote Codex
home.

[`local-codex-relay.md`](local-codex-relay.md) is authoritative for initial
enrollment, configuration fields, catalog activation, and manual Remote
rollback. This document focuses on the release order for already enrolled
components.

This public runbook defines shared update mechanics and safety boundaries.
Deployment-specific runtime ownership, private release publication, consumer
rollout, key rotation, platform cadence, incident classification, evidence
retention, and dated applied outcomes remain in the private deployment overlay.

> Historical deployment holds and superseded live evidence remain in the private
> deployment overlay. Past checks never establish current health or remediation.

## Update matrix

| Change | Automation | Interruption | Rollback boundary |
| --- | --- | --- | --- |
| Local native relay release | Explicit signed `install-relay.sh install VERSION` on each client | The local AppServer may reconnect; an existing CLI needs restart to refresh its picker | Reinstall the prior signed version; the non-secret relay JSON and prior catalog remain |
| Legacy/loopback Remote catalog change | `opencodex-remote-catalog-refresh.timer` fetches about every 10 minutes | The managed app-server restarts only after a verified catalog change | Previous catalog is retained in the Remote Codex backup chain |
| External relay Remote catalog change | The relay is the sole catalog writer; `opencodex-remote-relay-catalog-activation.timer` checks its marker about every minute | The managed app-server restarts after a zero-active health snapshot; use a maintenance window to exclude a request admitted after that cross-process snapshot | The relay's previous catalog is retained alongside its marker |
| Central OpenCodex proxy release | `upgrade-opencodex.sh apply VERSION` | Central proxy pauses for package swap and smoke test | Previous `/opt/opencodex` is retained under `/var/backups/opencodex/` |
| Remote Codex standalone release | `update-remote-codex.sh apply --allow-remote-interruption` | Remote Control work on that host can disconnect | Previous standalone `current` link and catalog are restored from the Remote Codex backup directory |

The legacy/loopback catalog timer validates a non-empty model array with unique
identifiers before atomically replacing the `0600` catalog. In external relay
mode, the installer disables that legacy timer. A manually invoked
`refresh --restart` also reports `relay_catalog_refresh=owned_by_relay` without
activating the marker: the relay validates and writes the catalog, then the
dedicated activation timer alone observes `.restart-pending`. This gives the
Remote home one catalog writer and one activator and prevents competing
activators. The manager's health read is a cross-process snapshot, not the
resident relay's admission gate, so strict no-new-request activation requires a
maintenance window. Codex reads
`model_catalog_json` at startup, so an existing TUI keeps its old picker until
it exits and starts again.

## Native relay release path

The public GitHub Actions workflow builds the four Linux helpers and the
self-contained Hardened Runtime, ad-hoc-signed `OpenCodexRelay.app.zip` on a
`macos-26` runner. The protected v2 Ed25519 private signing key stays in the
`relay-release` GitHub Environment and is exposed only to the build step. The
workflow publishes the five components, `THIRD_PARTY_NOTICES.md`, manifest, and
signature together as eight immutable assets. Each client verifies the manifest
signature, selected component checksums, and signed notice checksum before it
updates `current`.
Revision 1 and 2 releases remain available for rollback. The updater bootstrap
`0.3.8-rc.6` uses component-aware compatibility revision 4 with
`signing_mode: "adhoc"`; `0.3.8-rc.7` and later updater releases use revision 5.
The distribution app embeds the tracked release public key and uses the
monotonically increasing integer in `client/relay/RELEASE_BUILD_NUMBER` as its
`CFBundleVersion`. Stable releases are GitHub latest; preview prereleases are
not, and API display order is never used as version order. Revision 5 verifier
support begins in M0, while the first updater bootstrap `0.3.8-rc.6` remains a
revision 4 release that existing users must install manually. Revision 5 signs
the channel, minimum updater version, trust-key ID, minimum macOS version, and
integration/helper protocols. The updater compares bounded pagination
results by strict SemVer: stable considers only non-prereleases, while preview
considers both stable and prerelease versions and selects the maximum. Neither
channel selects a version below the installed version.

Starting with the manually installed `0.3.8-rc.6` bootstrap, the production
menu-bar app checks this public repository directly. Stable is the default;
preview is an explicit opt-in. Automatic checks are enabled by default, begin
with a randomized 5–15 minute launch delay, and then run at most once every 24
hours. They can be disabled in Settings. Local-development bundles never make
automatic update requests. **Check for Updates…** always performs an immediate
check while reusing the bounded ETag cache. The app does not request Notification
Center permission. The `0.3.8-rc.6` bootstrap is notification-only.

The bundled control helper exposes the same read-only result as schema-versioned
JSON. Production fixes both the GitHub API and repository; there is deliberately
no repository or API override:

```bash
/Applications/OpenCodexRelay.app/Contents/Library/Helpers/opencodex-relayctl \
  release check \
  --channel stable \
  --current-version 0.3.8-rc.6 \
  --public-key /Applications/OpenCodexRelay.app/Contents/Resources/ReleaseTrust/opencodex-relay-release-ed25519.pub \
  --json
```

Expected remote conditions return one of `current`,
`newer_than_selected_channel`, `update_available`, `offline`, `rate_limited`,
`invalid_release`, `updater_too_old`, or `unsupported_system` in JSON without
making the current app or resident Relay unavailable. Invalid arguments, an
unsafe local trust-key path, and an invalid local JSON contract fail the
command. Candidate metadata is not trusted until the exact immutable release,
eight-asset set, manifest signature, and signed app ZIP digest all verify.
Release notes are opened only as the canonical GitHub tag URL in the external
browser; release-body HTML is never rendered in the app.

Starting with `0.3.8-rc.7` (`CFBundleVersion=1001`), a download begins only
after the user selects **Download and Verify**. The bundled control helper
re-fetches the exact immutable release instead of trusting the earlier check
result, verifies the signed revision 5 manifest and app digest again, rejects a
non-newer candidate, and stages only the verified app under the owner-only
`~/Library/Application Support/OpenCodexRelay/Updates` root. The release ID and
manifest digest identify the staging directory; an exclusive lock prevents
concurrent staging, and a strict schema-version-1 receipt binds the release,
channel, app digest, bundle fingerprint, trust key, and verified path.

The archive is limited to 128 MiB. Before extraction, the helper rejects
absolute or parent-traversing paths, empty path components, duplicates and
case-fold collisions, links and non-regular files, multiple roots, excessive
path length or file count, oversized expansion, and excessive compression
ratios. After extraction it verifies the exact production bundle ID, version,
newer numeric build, arm64 executable set, nested ad-hoc signatures, absent Team
ID, Hardened Runtime, helper CDHash binding, and byte-identical embedded trust
key. It validates the receipt and staged app again immediately before Finder
handoff.

The equivalent public command consumes the exact values returned by `release
check`:

```bash
/Applications/OpenCodexRelay.app/Contents/Library/Helpers/opencodex-relayctl \
  release stage \
  --channel preview \
  --current-version 0.3.8-rc.7 \
  --release-id REPLACE_WITH_RELEASE_ID \
  --tag REPLACE_WITH_EXACT_TAG \
  --expected-manifest-sha256 REPLACE_WITH_MANIFEST_SHA256 \
  --public-key /Applications/OpenCodexRelay.app/Contents/Resources/ReleaseTrust/opencodex-relay-release-ed25519.pub \
  --json
```

Installation remains Finder-managed and user-approved. The app applies
Foundation quarantine metadata to the staged bundle, reads it back, reveals the
bundle in Finder, and offers a separate **Quit App** confirmation. It does not
remove quarantine, invoke `spctl`, copy over `/Applications`, relaunch itself,
or claim an atomic application rollback. After the app quits, the user copies
or replaces the app in Applications and opens that exact copy manually,
including **Open Anyway** when macOS requires it. Keep a manual copy of the
previous app until the replacement has launched successfully.

This Finder handoff is deliberately separate from the first-install relocation
card. An ad-hoc app launched under App Translocation may not run from its final
Applications path before Gatekeeper approval, so the update flow never uses
self-relocation as an installation transaction. A busy lifecycle or an existing
recovery journal prevents update staging or handoff without mutating the current
app or resident Relay. The privileged Helper is never replaced by this flow; a
version mismatch remains `manual_update_required` and needs its own administrator
approval.

### Public GitHub Release option

Use a lightweight strict-SemVer tag without a `v` prefix. Before approving the
protected Environment deployment, confirm the tag points to current public
`main` and the exact commit has successful `linux`, `macos`, and `analyze`
checks. Release `0.3.6` is the first version published through this path.

```bash
./client/relay/scripts/install-relay.sh install 1.2.3 \
  --github-repo novelKR/OpenCodex-OCI-Gateway \
  --public-key config/trust/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

No consumer token is required. An optional read-only token file used only for
API rate limits must
be current-user-owned mode `0600`, and the installer writes it only to an
owner-only temporary curl config before removing it. `current` remains unchanged
until the immutable GitHub release, manifest, and target checksums all verify.
The token is never written to relay JSON, Codex TOML, a launchd plist, or a
systemd unit.

For Remote Control Linux homes, pass the dedicated `--catalog-path`,
`--codex-executable`, and `--manage-app-server false` arguments documented in
[`local-codex-relay.md`](local-codex-relay.md), then run the explicit
`configure-remote-codex-routing.sh enable-relay --allow-remote-interruption`.
Voice stays disabled during a relay update unless the separate central feature
gate and local JSON were both intentionally enabled.

## Install Remote-host automation once

The Remote installer copies only reviewed scripts and user systemd units. It
does not create or copy `auth.json`, `remote-opencodex.json`, or
`credentials.env`; those are host-local, owner-only prerequisites.

On every Remote Control host, enable user services across SSH logout once:

```bash
sudo loginctl enable-linger ubuntu
```

For a new Remote host with no daemon yet, run the bootstrap form on the first
invocation as `ubuntu` from a complete checkout:

```bash
cd /path/to/OpenCodex-OCI-Gateway/pilot/scripts
./install-remote-codex-home.sh install --bootstrap-remote-control
```

On a host where Remote Control is already bootstrapped, reinstall only the
managed assets with the plain form:

```bash
./install-remote-codex-home.sh install
```

Validate without printing credentials:

```bash
~/.local/lib/opencodex-relay/update-remote-codex.sh status
codex debug models | jq '.models | length'
```

### Ubuntu 24.04 Linux sandbox prerequisite

Codex on Linux uses `bubblewrap` and needs a working user namespace. On Ubuntu
24.04, keep the global AppArmor restriction enabled and load the distribution's
specific `bwrap` profile instead of disabling
`kernel.apparmor_restrict_unprivileged_userns` globally:

```bash
sudo ./pilot/scripts/configure-codex-linux-sandbox.sh --user ubuntu
```

The script installs `bubblewrap`, `apparmor-profiles`, and `apparmor-utils`,
loads `bwrap-userns-restrict`, and proves `bwrap --unshare-user` works as the
named user. A direct `unshare --user --map-root-user` probe may remain denied;
that is intentional because only the `bwrap` AppArmor profile is granted this
exception.

### AppServer version convergence and explicit recovery

`running` alone is not a successful Remote Codex update. Before clearing a
catalog restart marker, require `managedCodexVersion`, `cliVersion`, and
`appServerVersion` all to equal the managed standalone version:

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-daemon
```

If a normal restart reports that an AppServer is running but is not daemon
managed, do not use `pkill`, a broad process match, or `SIGKILL`. A requested
`restart-daemon` (including the legacy catalog timer's already-interrupting
`refresh --restart`) first tries the normal Codex command and then falls back
to the same exact recovery only when that command rejects an allowlisted
Remote-home daemon/AppServer pair. This keeps a verified changed catalog from
remaining pending solely because of the Codex 0.147 ownership report.

For a standalone diagnostic/takeover, use the explicit recovery command in a
maintenance window:

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  recover-daemon --allow-remote-interruption
```

It only sends `TERM` to the exact older daemon pid-update loop and Unix-socket
AppServer command shapes owned by the approved Remote `CODEX_HOME`, then
bootstraps the managed daemon and waits for version equality. No fallback runs
unless the caller had already selected a daemon restart and the exact allowlist
matches; it never broadens to unrelated processes.

When the dedicated `CODEX_HOME` wrapper is invoked from the SSH login
directory (`/home/ubuntu`), an ordinary `~/.codex/config.toml` can load as a
trusted project config and override the dedicated Remote model/reasoning
defaults. The explicit command below modifies neither that ordinary file nor
its user preferences. It marks only that project as `untrusted` in the
dedicated Remote config, then restarts the daemon:

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  isolate-home-project-config --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-home-project-config
```

### Catalog availability and base-URL warning

The manager and relay remove only upstream `visibility: "hide"` entries. All
other visible models remain in the catalog even when they are available only to
a selected account. Resolve a response-time account restriction by selecting
the eligible account, not by hiding the model with a host-local filter. After
selecting an account, validate the catalog and a real Responses request from a
new CLI process.

When built-in `openai` uses `openai_base_url` for the loopback OpenCodex or a
local relay, the Codex model picker may warn that model selection through an
overridden base URL is not fully supported. This is expected proxy-routing
information, not proof of a failed response. Do not remove the override merely
to silence the warning: that would bypass the intended OpenCodex path. Use the
authenticated catalog and a real Responses request as the acceptance test.

## Runtime Adapter rollout state

The repository Runtime Adapter validates and invokes Node, npm, and the
OpenCodex entry through a root-owned runtime contract and
`/usr/local/libexec/opencodex-runtime`. Repository implementation and
verification are not evidence that it is deployed on the current server.

Rollout has three stages:

1. repository implementation and automated tests;
2. a read-only canary from the exact reviewed source in a root-owned,
   non-listable but `opencodex`-traversable, secret-free temporary directory;
3. a separately approved live migration after idle preflight and snapshot.

`configure-opencodex-runtime.sh check` stages a byte-identical Adapter and a
candidate contract together beneath the root-owned executable `/usr/local`
staging parent, runs Adapter `check` and
`describe --json` as root, then runs `ocx --version`, `config validate`, and
`npm --version` as `opencodex`. It removes the temporary pair on success,
failure, or signal and changes no production unit, drop-in, prefix,
configuration, service state, or backup. Live migration requires an explicit
`configure-opencodex-runtime.sh apply ...
--allow-service-restart`. Do not record the migration as complete until the
local smoke test and interactive external-smoke two-stream overlap both pass.

The candidate Node/npm files and every ancestor directory able to replace them
must be root-owned and not group- or world-writable. Canonicalizing a
user-owned Homebrew or version-manager tree does not make it eligible for
direct migration. Provision a reviewed root-managed runtime before the
read-only canary and, when it lives beneath `/home`, verify that only the one
runtime root derived by the Adapter is exposed through a read-only bind.

Validate the candidate before any mutation:

```bash
sudo ./pilot/scripts/configure-opencodex-runtime.sh check \
  --node-bin /ABSOLUTE/ROOT_MANAGED/PATH/TO/node \
  --npm-cli /ABSOLUTE/ROOT_MANAGED/PATH/TO/npm-cli.js
```

Apply the same exact paths only after separate maintenance approval. When an
unmanaged drop-in changes the execution path, name exactly one reviewed direct
`.conf` path; any other execution drop-in fails closed.

After the idle gate succeeds, an active service is explicitly stopped before
the candidate contract replaces managed assets. This blocks new admission while
the runtime is swapped; a failure restores the snapshot and the original
active/enabled state before reporting the migration failed.

```bash
sudo ./pilot/scripts/configure-opencodex-runtime.sh apply \
  --node-bin /ABSOLUTE/ROOT_MANAGED/PATH/TO/node \
  --npm-cli /ABSOLUTE/ROOT_MANAGED/PATH/TO/npm-cli.js \
  --allow-service-restart \
  --replace-legacy-drop-in \
  /etc/systemd/system/opencodex.service.d/REVIEWED-LEGACY.conf
```

Omit the final two lines when no legacy drop-in is being replaced. The public schema v1 and invoker subcommand contract in this Core and the
script help output are canonical.

## Standard release order

Schedule a maintenance window. A central OpenCodex release can affect all
clients, and a Codex standalone release can interrupt one Remote Control host.
Do not update both Remote hosts simultaneously.

1. Review an exact stable version whose immutable upstream tag, published
   package, and provenance agree. Record it in one variable. Do not use
   `latest`, a preview, or source-checkout HEAD.

   ```bash
   VERSION=REPLACE_WITH_REVIEWED_EXACT_VERSION
   sudo ./pilot/scripts/upgrade-opencodex.sh check "$VERSION"
   ```

2. Confirm idle state before service interruption. These are content-free
   checks; run them in a maintenance window that excludes a request arriving
   between the snapshot and mutation.

   ```bash
   sudo -u opencodex /usr/local/libexec/opencodex-runtime \
     ocx observe memory --json |
     jq -e '.activeTurnCount == 0 and .isDraining == false'
   ss -Htan state established '( sport = :10100 or dport = :10100 )' |
     awk 'END { exit(NR == 0 ? 0 : 1) }'
   sudo systemctl is-active opencodex.service
   sudo systemctl is-enabled opencodex.service
   ```

   `observe memory` uses the local OpenCodex CLI's management credential to read
   only scalar lifecycle state from gated `/api/system/memory`. The
   unauthenticated `/healthz` does not contain these fields and is not an idle
   gate. A command failure, JSON-shape mismatch, or non-zero counter fails
   closed.

3. Apply that exact version. The script validates the Runtime Adapter contract,
   companion smoke hash, old and new configuration,
   restores the original service state, runs the local smoke test by default,
   and writes `/etc/opencodex/expected-version` only after success.

   The managed npm install does not rely on an allowlist whose semantics vary
   across npm versions. It first blocks every dependency install lifecycle with
   `--ignore-scripts`. While the prefix belongs to the `opencodex` account, the
   Runtime Adapter's version-bound `prepare-bundled-bun VERSION` path verifies the
   service-owned package chain and exact Bun dependency, then permits only the
   reviewed exact launcher's `--version` path to prepare bundled Bun. Root
   ownership is restored before the normal Adapter `check`, version, and config
   validation run. Existing and newly prepared Bun executable candidates must
   remain canonical, non-symlink files with safe modes inside the same ownership
   chain; the root-owned Adapter path enforces the same invariant. The local smoke test
   requires the exact Bun version in the OpenCodex manifest and the running
   service's reported `bunVersion` to agree, with `bundled` as the runtime source.

   ```bash
   sudo ./pilot/scripts/upgrade-opencodex.sh apply "$VERSION"
   sudo ./pilot/scripts/external-smoke-test.sh
   ```

   `--skip-smoke` is only for an intentionally stopped host; it is not a normal
   production shortcut. The external smoke test runs in a real TTY, collects
   credentials through hidden prompts, and includes two overlapping actual
   Responses streams.

   If a reviewed legacy host is already at the requested version but predates
   `/etc/opencodex/expected-version`, the same `apply VERSION` command performs
   a no-package-mutation state adoption instead of exiting early. It verifies
   the installed package manifest, that `opencodex.service` is both active and
   enabled, and the loopback `/healthz` JSON response before writing the state.
   To perform that adoption without an npm registry lookup, use the explicit
   command below only after independently reviewing the installed release:

   ```bash
   sudo ./pilot/scripts/upgrade-opencodex.sh adopt-current "$VERSION"
   ```

   `adopt-current` never changes the package, service configuration, or service
   lifecycle. It is a state-recording recovery path, not a replacement for the
   full smoke and external-smoke checks required after a real upgrade.

4. Update Remote Control hosts one at a time. The acknowledgement flag is
   deliberate because the managed app-server can restart.

   ```bash
   ~/.local/lib/opencodex-relay/update-remote-codex.sh status
   ~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
     set-default-model --allow-remote-interruption
   ~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-default-model
   ~/.local/lib/opencodex-relay/update-remote-codex.sh apply --allow-remote-interruption
   ```

   The updater uses the official standalone installer with the dedicated
   `CODEX_HOME`, a private `CODEX_INSTALL_DIR`, and non-interactive mode. It
   repairs the visible launcher, refreshes the OpenCodex catalog for the
   installed Codex version, restarts the daemon when necessary, re-enables
   Remote Control, and verifies the `gpt-5.6-luna` managed default, catalog,
   daemon, and local WebSocket handshake. The current 26-entry catalog reflects
   removal of the central Cursor adapter; the updater does not add a separate
   host-local Cursor filter or restore Cursor as a default.

5. In Codex Desktop, exit any existing terminal Codex process, relaunch it,
   then refresh **Remote Control**. Confirm the picker from `codex debug
   models` and the connected Remote host.

## Recovery and prohibited shortcuts

Both scripts retain recovery material instead of silently deleting it:

- OpenCodex: `/var/backups/opencodex/upgrade-<from>-to-<to>.*`
- Remote Codex: `~/.codex-remote-opencodex/.upgrade-backups/upgrade-<from>.*`

OpenCodex restores the previous package prefix and service state if package
installation, config validation, service start, or the default smoke test
fails. Remote Codex restores the previous standalone link and catalog before
restarting Remote Control if verification fails. Inspect retained backups before
manual pruning.

Do not use `npm update`, mutable npm tags such as `latest`, an OpenCodex
Dashboard self-update, `ocx update`, or `bootstrap-host.sh` as the routine
update path on these managed hosts. They do not preserve this deployment's
explicit version contract, service-state restoration, Remote launcher repair,
and smoke gates as one operation.

The outer repository's `opencodex/` directory is a separate upstream checkout,
not a production deployment mechanism. Update that checkout under its own
upstream guidance when doing source work, but promote a reviewed published
package version through `upgrade-opencodex.sh` for the managed proxy host.

Append success or failure as a dated, content-free entry in the private
deployment overlay. Keep host aliases, exact snapshot paths, and unit/drop-in
hashes only in its gitignored evidence. Current health or one deployment
observation never closes a formal deployment gate.

## Sources for the Codex boundary

The Codex configuration reference documents `CODEX_HOME` as standalone state,
`CODEX_INSTALL_DIR` as the visible command location,
`CODEX_NON_INTERACTIVE=1` for scripted installation, and startup-loaded
`model_catalog_json`. The command reference documents `codex remote-control`
and the managed app-server route.

- [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
- [Codex CLI command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
- [Codex Remote connections](https://learn.chatgpt.com/docs/remote-connections)
