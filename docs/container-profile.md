# Experimental OpenCodex container profile

This profile containerizes only OpenCodex. Nginx, cloudflared, OpenSSH, Remote
Codex home, Relay, and Control Center remain host-native. The separate Runtime
package and its current qualification limits are documented in the canonical
[OpenCodex Runtime guide](../containers/opencodex/README.md).

Linux network_mode: host preserves the fixed 127.0.0.1:10100 API and localhost
1455 OAuth callback contracts. It provides no network sandbox. The image process
is non-root; the container invocation uses a read-only root filesystem, drops all
capabilities, sets no-new-privileges, bounds PIDs and memory, and mounts only
/var/lib/opencodex. The Docker socket is not mounted.

The OpenCodex npm version and integrity, Bun version, dependency lock, and base
image index digest are pinned. Release images use
ghcr.io/novelkr/opencodex-oci-gateway:<gateway>-ocx-<opencodex> and publish
multi-arch amd64/arm64 manifests, SBOM, and provenance.

The `runtime` target and its future Apple Container lifecycle are separate from
the existing Gateway image and release lifecycle.
`containers/opencodex/upstream.lock.json` is the authoritative upstream release
and provenance record; it is not the image's only build input. The external
`lidge-jun/opencodex` repository is treated as immutable, read-only input and is
never modified. A six-hour detector verifies an immutable GitHub release, direct
commit tag, source `package.json`, npm metadata, tarball SRI, and the tarball's
package identity. It then runs the official npm signature verifier and requires
both the registry signature and Sigstore/SLSA provenance to bind that exact
SHA-512 tarball subject to
`https://github.com/lidge-jun/opencodex`,
`.github/workflows/release.yml`, and the direct-tag commit. The packument's
`gitHead` must also equal that commit, but is only corroborating metadata.

A repository-scoped GitHub App may then create a review-only
`automation/opencodex-<version>-r1` pull request. Its token requests GitHub's
implicit Metadata read permission plus Contents read/write and Pull requests
read/write for this repository only; it is not granted Actions, Workflows,
Packages, or Administration write permission. The Contents permission is real
write authority and is not itself a branch restriction. The workflow—not the
App permission model—uses an exact four-file writer, never force-pushes or writes
directly to `main`, and never enables auto-merge. Protected `main` enforces the
separate adoption boundary. The required remote contract for the `upstream-watch`
Environment restricts it to exact `main`, disables administrator bypass, and
intentionally has no required reviewer or wait timer, so proposal creation is
automatic while adoption remains a human-reviewed merge. Its public Client ID
is stored in
`OPENCODEX_UPSTREAM_WATCH_APP_CLIENT_ID`; only the private key in
`OPENCODEX_UPSTREAM_WATCH_APP_PRIVATE_KEY` is an Environment secret.

After a reviewed change reaches public `main`, GitHub-hosted builders publish
`ghcr.io/novelkr/opencodex-runtime:candidate-<version>-r<N>-<40-character-core-SHA>`
once for linux/amd64 and linux/arm64. Each executable child has its own BuildKit
OCI attestation manifest carrying SPDX SBOM and `mode=max` provenance. Separately,
the workflow signs the exact multi-architecture index with GitHub artifact
attestation. New candidates retrieve that signed index provenance through the
GitHub Attestations API; the workflow does not push a second registry-local
Sigstore mirror or create a GitHub package storage record. Historical GHCR
`sha256-*` attestation versions remain as prior evidence, but are not the
authority for new candidates.

The default runtime workflow uses GitHub-hosted runners only. A native
Linux/arm64 job pulls that image by exact index digest, verifies the selected
arm64 child digest, and exercises the image contract without rebuilding or
retagging it. A separate macOS 26 job builds and tests the Relay/relayctl and
Swift contracts with a fake Apple CLI; it never installs, starts, or invokes
Apple Container. Those two results and the candidate witness produce a strict
`hosted-candidate` qualification receipt with `apple_container_live=false` and
`stable_promotion_eligible=false`. GitHub attestation verification constrains the
repository, signer workflow, protected `main` source ref, exact source revision,
and GitHub-hosted-runner origin; it does not use `--bundle-from-oci`.

The runtime-image PR and candidate builders' executable inputs are fail-closed
as well. Both workflows download the reviewed Buildx v0.30.1 Linux/amd64
release asset only from its official GitHub release URL and check its
repository-pinned SHA-256 before the file is made executable or invoked. QEMU
registration is limited to arm64 and uses the exact
`tonistiigi/binfmt:qemu-v10.2.3` OCI index digest; the BuildKit docker-container
driver uses the exact `moby/buildkit:v0.26.1` OCI index digest. Each workflow
reads the running builder container back and verifies that it uses that selected
BuildKit image. Updating any of these three build inputs is therefore a
reviewed Core change, not a mutable tag or hosted-runner tool update.

The npm verifier is equally pinned. The detector downloads the official
`npm@11.19.1` package by its repository-recorded SHA-512 SRI, safely extracts
it, asserts its package version, and invokes `npm audit signatures --json
--include-attestations` inside the exact multi-architecture
`node:24.20.0-bookworm-slim` OCI index digest recorded in the verifier module.
The same wrapper is required when PR, candidate, public-candidate, and legacy
gateway release jobs verify the locked tree. Missing signature or provenance
evidence for a newly observed release is `awaiting-npm`; missing evidence for
the already locked version and any present-but-mismatched evidence fail closed.

Candidate publication is deliberately separate from package visibility. The
workflow never changes GHCR visibility. After the first candidate creates the
package, an operator may separately approve making it public. A bounded
`verify-public-candidate` dispatch accepts only an existing workflow run ID,
derives the exact source and digest witnesses from that run's artifacts, and
tests an anonymous exact-digest pull on a credential-free hosted Linux/arm64
runner. It does not accept a caller-supplied digest and cannot rebuild, retag,
or promote the candidate. Its `public_ready=true` result means only that this
repository's `public-candidate` contract and anonymous exact-digest pull passed.
It never means production readiness, stable promotion eligibility, or live Apple
Container acceptance; the receipt continues to record
`apple_container_live=false` and `stable_promotion_eligible=false`.

This hosted-only phase creates no stable OCI tag, signed Runtime Manifest,
Runtime GitHub Release, production Runtime signing key, new registry-local
GitHub attestation mirror, or Relay application release. It also provisions no
self-hosted runner and uses no personal Mac. The repository therefore includes
only candidate receipt creation and signature-first stable-manifest verification;
the stable-manifest creation subcommand and Runtime GitHub Release publisher are
deliberately absent.
GitHub-hosted macOS arm64 runners cannot provide the nested virtualization
required by Apple Container, so their contract tests are not live Apple
Container acceptance. A future, separately approved PR may restore stable
promotion only after project-owned, sponsored, or externally managed physical
Apple Silicon capacity and its operating cost are explicitly approved.

The tracked Runtime public key at
`config/trust/opencodex-runtime-release-ed25519.pub` exists as a syntactically
valid bootstrap trust root so Relay packaging and strict verifier tests remain
usable; the corresponding private half was not retained and the key is not a
production signing authority. With no immutable stable Runtime Release signed
by that key,
`relayctl container-runtime check` reports
`stable_runtime_manifest_unavailable`, the Control Center presents the runtime
as unavailable, and neither surface can stage or activate a candidate. The
stage command accepts only the SHA-256 of a manifest returned by the stable
release check and re-fetches that immutable release before any pull. It has no
image-digest, candidate-tag, alternate-key, or release-URL argument. Activation
accepts only the CAS witness of an already staged, locally retained, signed
manifest. A hosted qualification receipt is therefore never an installation
input and cannot bypass the Runtime signature boundary.

The hosted Linux canary covers the container image contract, fixed ports, UDS
secret bootstrap, data/admin credential separation, protocol behavior,
confinement, secret non-disclosure, graceful stop, and state-volume reuse where
the hosted runtime permits. It does not cover Apple Container socket mounting,
host publication, APFS cloning, Relay routing, lifecycle rollback/recovery,
Desktop UI, real-provider OAuth, or macOS logout/login recovery. Those remain
blocked acceptance layers, not inferred successes.

The Control Center's activate, stop, and recover operations all capture the
current runtime state digest and routing generation before requesting a
graceful exit from the exact registered Codex Desktop bundle. They revalidate
that bundle is still stopped and the captured runtime witness is still current
before `relayctl` receives the explicit Desktop-exited confirmation. Refusal,
timeout, restart, or stale state performs no runtime or routing mutation.

opencodex.service and opencodex-container.service conflict. The container unit
also refuses a tag-only image: container.env must provide an @sha256 digest
from the signed release manifest.

Static Compose tests and builds are not live acceptance. The existing Gateway
image release retains its separate OCI E2.1.Micro canary covering
memory/restarts, catalog, Responses, WebSocket, Dashboard, SSH OAuth callback,
loopback listeners, and reboot recovery; it is not evidence for the Runtime
candidate or live Apple Container operation.
