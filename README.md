# OpenCodex OCI Gateway and Runtime

This repository publishes two distinct OpenCodex delivery tracks: the existing OCI
Gateway deployment and a separately qualified, multi-architecture Runtime candidate.
They share reviewed source inputs and security tooling, but they are different images
with different operational and release contracts.

Sensitive identifiers, private keys, live tokens, generated credential maps, and
operational records stay outside this public repository.

## Choose the Right Track

| Track | Purpose | Package | Dockerfile target | Current maturity |
| --- | --- | --- | --- | --- |
| OCI Gateway | Operate a private OpenCodex backend behind Nginx and Cloudflare on an OCI host | `ghcr.io/novelkr/opencodex-oci-gateway` | `gateway` (default) | Existing host-deployment and release track; follow the Gateway acceptance runbook |
| OpenCodex Runtime | Qualify a pinned OpenCodex image for a future managed Apple Container lifecycle | `ghcr.io/novelkr/opencodex-runtime` | `runtime` | Public candidate only; no supported `latest` or stable Runtime installation |

Use the [OCI Gateway guide](docs/gateway.md) for host bootstrap, client connection,
topology, and verification. Use the canonical
[OpenCodex Runtime guide](containers/opencodex/README.md) or its
[Korean translation](containers/opencodex/README.ko.md) for the candidate image,
evidence model, and current acceptance limits.

> [!WARNING]
> A package version labeled **Latest** by GitHub Container Registry is a package-page
> ordering/display signal, not a stability, signature, or installation guarantee.
> Runtime candidates have no supported `latest` tag. Do not treat a GHCR-generated
> pull snippet or a historical `sha256-*` attestation version as a stable release;
> inspect only a reviewed exact index digest and its current qualification evidence.

## Security Principles

- **Zero committed secrets:** Private keys, OAuth codes, tokens, credentials, generated
  gateway-key maps, and live request logs are excluded from the repository.
- **Immutable identity:** Published inputs and qualified images are bound to exact
  versions, source revisions, integrity values, and OCI digests rather than mutable
  tags.
- **Fail-closed boundaries:** Gateway admission exposes only documented `/v1`
  endpoints, while Runtime staging remains unavailable until a separately signed
  stable Runtime manifest exists.
- **Separated credentials:** API data-plane credentials remain distinct from
  Dashboard and other management credentials.
- **Vulnerability reporting:** Report security issues through GitHub Private
  Vulnerability Reporting as described in [SECURITY.md](SECURITY.md).

## Documentation Hub

| Topic | Document | Scope |
| --- | --- | --- |
| Gateway overview | [OCI Gateway Guide](docs/gateway.md) | Host bootstrap, client setup, topology, and verification |
| Runtime candidate | [Runtime Guide](containers/opencodex/README.md) · [한국어](containers/opencodex/README.ko.md) | Image contract, exact-digest evidence, and acceptance boundaries |
| Architecture | [Architecture Specification](docs/architecture.md) · [한국어](docs/architecture.ko.md) | Network topology, ports, trust boundaries, and reboot recovery |
| Remote access | [SSH and Client Access](docs/ssh-and-client-access.md) · [한국어](docs/ssh-and-client-access.ko.md) | SSH configuration, Dashboard forwarding, and client setup |
| Client Relay | [Relay Operations Guide](docs/local-codex-relay.md) · [한국어](docs/local-codex-relay.ko.md) | Complete operational runbook for macOS and Linux relays |
| Host deployment | [Deployment Configuration](docs/deployment-configuration.md) | Schema for non-secret deployment rendering |
| Container boundaries | [Container Profile](docs/container-profile.md) | Gateway and Runtime packaging, evidence, and isolation profile |
| Operations | [Updates and Rollbacks](docs/updates.md) · [한국어](docs/updates.ko.md) | Version upgrade procedures and recovery routines |
| Verification | [Testing and Acceptance](docs/testing.md) · [한국어](docs/testing.ko.md) | Live acceptance criteria and test suites |
| Public Core | [Public Core and Overlay](docs/public-core-and-overlay.md) | Boundary between this public Core and private deployment state |

## Contributing

Review [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines. Run the full
verification suite before submitting a pull request:

```bash
bash -n pilot/scripts/*.sh pilot/libexec/* ops/oci/*.sh client/relay/scripts/*.sh tools/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test ./... && go test -race ./... && go vet ./...)
(cd client/relay/macos/OpenCodexRelay && swift test && swift build -c release)
git diff --check
```

## License

Distributed under the Apache License 2.0. Third-party notices and licenses are
documented in [NOTICE](NOTICE) and constituent subdirectories.
