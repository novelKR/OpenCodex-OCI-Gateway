# Contributing

Open an issue before making a change that alters a public interface, security
boundary, deployment topology, persistent state, or release process.

For code changes:

1. Keep examples generic and secret-free.
2. Update the matching test and runbook.
3. Run the verification commands in README.md.
4. State which checks are static and which were exercised on a live target.
5. Do not commit local/, opencodex/, research/, generated files, build output,
   credentials, operational ledgers, or private deployment evidence.

Pull requests from forks never receive signing, notarization, GHCR, Cloudflare,
or deployment credentials. A maintainer-approved release environment owns
publishing.

Contributors may propose a new Relay teardown identity profile by supplying
secret-free reproduction instructions for the official npm integrity, complete
darwin/arm64 installation-closure digest, required-module hashes, adapter
import/preflight results, and disposable-macOS preserve-data teardown and
interruption-recovery evidence. Do not attach raw live logs, credentials, user
paths, or operational records. A profile remains manual-only until a maintainer
independently reproduces the artifact and acceptance evidence and explicitly
adds it to the trust registry; contributor evidence alone does not grant
automatic-removal authority.
