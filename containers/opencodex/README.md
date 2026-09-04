# OpenCodex Runtime Candidate

English is the canonical version of this document. See the
[Korean translation](README.ko.md).

`ghcr.io/novelkr/opencodex-runtime` is the multi-architecture OpenCodex Runtime
candidate produced by this repository's public, GitHub-hosted qualification pipeline.
It is built from the Dockerfile's explicit `runtime` target for `linux/amd64` and
`linux/arm64`.

> [!CAUTION]
> This package is **candidate-only**. It has no supported `latest` tag, stable Runtime
> tag, signed stable Runtime manifest, or supported installation path. A candidate
> becoming anonymously pullable does not make it production-ready or usable by the
> Relay lifecycle.

The repository also publishes the separate
`ghcr.io/novelkr/opencodex-oci-gateway` image from the Dockerfile's default `gateway`
target. That image belongs to the existing OCI host-deployment lifecycle. Do not
substitute one package or target for the other; see the
[repository overview](../../README.md) and [Gateway guide](../../docs/gateway.md).

## Candidate Identity and Digest-Only Inspection

Candidate tags have the shape
`candidate-<upstream-version>-r<image-revision>-<40-character-public-core-SHA>`, but
the tag is only a discovery label. Review the candidate receipt and use its exact OCI
index digest for every inspection. Do not use the GHCR package page's **Latest** label,
a package-generated pull snippet, a mutable tag, or a historical `sha256-*`
attestation version as an installation or stability signal.

The following commands inspect a previously reviewed identity; they are not an
installation procedure:

```bash
RUNTIME_IMAGE='ghcr.io/novelkr/opencodex-runtime'
RUNTIME_INDEX_DIGEST='sha256:REPLACE_WITH_REVIEWED_INDEX_DIGEST'
SOURCE_REVISION='REPLACE_WITH_REVIEWED_40_CHARACTER_PUBLIC_CORE_SHA'

docker buildx imagetools inspect "${RUNTIME_IMAGE}@${RUNTIME_INDEX_DIGEST}"

gh attestation verify "oci://${RUNTIME_IMAGE}@${RUNTIME_INDEX_DIGEST}" \
  --repo novelKR/OpenCodex-OCI-Gateway \
  --signer-workflow novelKR/OpenCodex-OCI-Gateway/.github/workflows/opencodex-runtime.yml \
  --source-ref refs/heads/main \
  --source-digest "${SOURCE_REVISION}" \
  --deny-self-hosted-runners
```

The `gh attestation verify` invocation queries the GitHub Attestations API. New
candidates do not publish a registry-local Sigstore mirror and must not be verified
with `--bundle-from-oci`. Historical registry `sha256-*` attestation versions are
retained for audit continuity, but their existence is not evidence that a current
candidate is stable.

## Runtime Image Contract

The `runtime` target has a deliberately narrower process and secret interface than a
plain OpenCodex container invocation:

- The image runs as non-root UID/GID `10001:10001` and serves the guest API on port
  `10100`.
- Persistent state is mounted only at `/var/lib/opencodex` and becomes the process
  home.
- API and administration credentials are distinct 32-byte base64url tokens. They are
  delivered once through `/run/opencodex/bootstrap.sock` in a canonical,
  length-prefixed envelope; the bootstrap then passes them only to the OpenCodex child
  as `OPENCODEX_API_AUTH_TOKEN` and `OPENCODEX_ADMIN_AUTH_TOKEN`.
- A supported lifecycle must mount the one-client bootstrap Unix socket, wait for its
  acknowledgement, remove it after consumption, and never place either token in image
  metadata, a command line, a repository file, or a persistent container environment.
- The runtime is expected to have a read-only root filesystem, all Linux capabilities
  dropped, `no-new-privileges`, bounded PIDs and memory, a constrained temporary
  filesystem, and no Docker socket.
- Host publication must remain loopback-only. The container's guest port does not
  authorize a public listener.

For those reasons, a generic `docker run` command is not a supported setup path. The
image needs an orchestrated secret socket, exact-digest selection, state ownership,
confinement, and lifecycle transaction. The checked-in experimental Compose profile
documents the older host container boundary; it does not turn a Runtime candidate into
a supported stable installation.

## Supply-Chain Evidence

The authoritative upstream release and provenance record is
[`upstream.lock.json`](upstream.lock.json). It binds the selected immutable upstream
GitHub release and direct-tag commit to the npm package identity, version, tarball, and
SHA-512 integrity. The detector cross-checks the GitHub release, source package,
packument, tarball, npm registry signature, and npm Sigstore/SLSA provenance before an
update can be proposed. The external `lidge-jun/opencodex` repository is read-only
input and is never modified by this project.

Each built index has two related but distinct evidence layers:

1. **BuildKit platform evidence inside the OCI index.** Each executable child
   (`linux/amd64` and `linux/arm64`) has BuildKit-generated SPDX SBOM and `mode=max`
   provenance represented by its own OCI attestation manifest descriptor.
2. **GitHub signed provenance for the exact index.** The candidate workflow creates a
   GitHub artifact attestation whose subject is the exact multi-architecture index
   digest. The GitHub Attestations API is the authoritative retrieval path for new
   candidates; the workflow does not push a second copy to GHCR and does not create a
   GitHub storage record for the package.

The candidate witness binds the index, both executable child digests, the locked
upstream identity, public Core source revision, workflow run identity, BuildKit SBOMs,
and BuildKit provenance. GitHub API attestation verification additionally constrains
the repository, signer workflow, `main` source ref, source revision, and
GitHub-hosted-runner origin.

## Qualification Levels

Qualification states describe evidence, not promotion:

| State | What passed | What it does not mean |
| --- | --- | --- |
| `hosted-candidate` | GitHub-hosted build, exact-index evidence, native hosted Linux/arm64 image-contract canary, and macOS Relay/CLI contract tests with a fake Apple CLI | No live Apple Container execution and no stable promotion eligibility |
| `public_ready=true` | A separate bounded workflow resolved one successful candidate run and anonymously pulled that exact index digest with an empty Docker credential configuration | Not production-ready, not stable, not a signed Runtime release, and not Apple Container acceptance |
| Stable Runtime | Not available | No supported tag, signed stable manifest, Runtime GitHub Release, or production signing authority exists |

Every public-candidate receipt therefore retains all of the following facts:

```text
public_ready=true
anonymous_exact_digest_pull=true
apple_container_live=false
stable_promotion_eligible=false
```

The hosted Linux/arm64 canary covers the image contract, fixed guest port, Unix-socket
secret bootstrap, separate API/admin credentials, HTTP and WebSocket behavior,
confinement, secret non-disclosure, graceful stop, and state reuse where the hosted
Docker runtime permits. It does **not** cover Apple Container socket mounting, host
publication, APFS cloning, Relay routing, lifecycle rollback/recovery, Desktop UI,
real-provider OAuth, or macOS logout/login recovery. The GitHub-hosted macOS job uses a
fake Apple CLI and cannot provide the nested virtualization required for live Apple
Container acceptance.

## Stable Trust Boundary

[`config/trust/opencodex-runtime-release-ed25519.pub`](../../config/trust/opencodex-runtime-release-ed25519.pub)
is a tracked, syntactically valid bootstrap public key used to keep Relay packaging and
strict verifier tests functional. The corresponding private half was not retained. It
is not a production signing authority.

No immutable stable Runtime GitHub Release is signed by that key. Consequently,
`relayctl container-runtime check` reports `stable_runtime_manifest_unavailable`, the
Control Center presents the Runtime as unavailable, and neither surface can stage or
activate a candidate. Candidate tags, digests, hosted qualification receipts, and
`public_ready=true` cannot bypass this signed-manifest boundary.

Stable promotion and live Apple acceptance require a separately approved design,
production signing authority, immutable signed Runtime release, and managed physical
Apple Silicon capacity. They are intentionally outside this candidate pipeline.

## Automated Proposal, Manual Adoption

The six-hour upstream watcher separates read-only detection from a repository-scoped
writer. Its GitHub App token is limited to this repository with GitHub's implicit
Metadata read permission plus Contents read/write and Pull requests read/write. Those
permissions can write repository contents; they are not themselves a branch
restriction. The workflow uses the App to propose an
`automation/opencodex-<version>-r1` pull request. The workflow—not the App permission
model—uses a fixed branch and exact four-file writer, never force-pushes or writes
directly to `main`, never enables auto-merge, and never promotes a candidate. Protected
`main` supplies the separate adoption boundary.

The required `upstream-watch` Environment configuration is restricted to exact
`main`, has administrator bypass disabled, and deliberately has no required reviewer
or wait timer. This keeps proposal creation automatic; adoption still requires a
human-reviewed merge into protected `main`. It does not claim two-person or
independent review.

- `OPENCODEX_UPSTREAM_WATCH_APP_CLIENT_ID` is an Environment variable. A Client ID is
  a public identifier, not an authentication secret.
- `OPENCODEX_UPSTREAM_WATCH_APP_PRIVATE_KEY` is an Environment secret and must never
  be committed or printed.
- The only generated paths are `UPSTREAM_NOTICES.md`, `bun.lock`, `package.json`, and
  `upstream.lock.json` in this directory.

## Source References

- [Runtime Dockerfile](Dockerfile)
- [Authoritative upstream lock](upstream.lock.json)
- [Third-party notices](UPSTREAM_NOTICES.md)
- [Runtime candidate workflow](../../.github/workflows/opencodex-runtime.yml)
- [Upstream watcher workflow](../../.github/workflows/opencodex-upstream-watch.yml)
- [Container profile and acceptance boundary](../../docs/container-profile.md)
- [Repository license](../../LICENSE)
