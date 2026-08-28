# Public Core and private deployment overlay

The public repository is the only source of truth for shared code, issues,
pull requests, releases, and container images. A private deployment repository
contains only deployment.toml, private operational evidence, migration and
rollback evidence, and a core.lock pin.

The overlay records repository, release, and commit in core.lock and checks the
Core out under ignored work/core/. It must not copy shared source files or use a
submodule. Secrets remain outside both repositories.

Public history starts with one parentless initial commit. It is exported through
an explicit allowlist; fork, mirror, subtree split, and copying .git are
prohibited. After publication, public history is never rewritten.

The publication source must be a clean HEAD. `prepare-public-core.sh` binds the
full private source commit and tree to a standalone public commit, tree, tag,
and allowlist blob in a private publication evidence file. It runs the complete
local validation suite in a disposable copy before creating the clean public
staging checkout. The public commit uses only the explicitly supplied public
author identity.

GitHub Actions jobs are fail-closed while the repository is private. A private
staging push may create skipped workflow-run metadata, but no CI, CodeQL, or
container-release job is sent to a runner. The repository owner validates the
remote refs, parentless history, tree, hygiene, and skipped jobs before changing
visibility. CI and CodeQL are dispatched only after the repository is public;
the initial container release is dispatched only after those public checks pass.
Container publication must run from the exact lightweight release tag and fails
if its version, GitHub workflow commit, or checked-out commit differs.

This deliberately leaves CodeQL unavailable as a pre-publication gate. Local
shell, Python, Go, Swift, hygiene, and whitespace validation must all pass before
the private staging push. If a public check later fails, release and deployment
stop and the public repository is fixed forward without rewriting history or
cycling visibility.

The private pre-split tip is retained by a signed private tag and an encrypted
git bundle. Neither artifact is copied into the public repository or CI.
