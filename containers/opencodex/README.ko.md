# OpenCodex Runtime Candidate

이 문서는 정본인 [영문 README](README.md)의 한국어 번역본입니다.

`ghcr.io/novelkr/opencodex-runtime`은 이 저장소의 공개 GitHub-hosted 검증
파이프라인이 만드는 multi-architecture OpenCodex Runtime candidate입니다. Dockerfile의
명시적인 `runtime` target으로 `linux/amd64`와 `linux/arm64` 이미지를 빌드합니다.

> [!CAUTION]
> 이 package는 **candidate-only**입니다. 지원되는 `latest` tag, stable Runtime tag,
> 서명된 stable Runtime manifest 또는 지원되는 설치 경로가 없습니다. candidate를
> 익명으로 pull할 수 있다는 사실만으로 production-ready가 되거나 Relay lifecycle에서
> 사용할 수 있게 되는 것은 아닙니다.

이 저장소는 Dockerfile의 기본 `gateway` target에서 만드는 별도 image인
`ghcr.io/novelkr/opencodex-oci-gateway`도 배포합니다. 이 image는 기존 OCI host 배포
lifecycle에 속합니다. 두 package나 target을 서로 바꿔 사용하지 마십시오.
[저장소 개요](../../README.md)와 [Gateway 안내서](../../docs/gateway.md)에서 두 경계를
비교할 수 있습니다.

## Candidate Identity와 Exact-Digest 검사

Candidate tag의 형식은
`candidate-<upstream-version>-r<image-revision>-<40-character-public-core-SHA>`이지만,
tag는 탐색용 label일 뿐입니다. Candidate receipt를 검토하고, 모든 검사에는 receipt에
기록된 exact OCI index digest를 사용하십시오. GHCR package 화면의 **Latest** 표시,
package가 생성한 pull 예시, mutable tag 또는 과거 `sha256-*` attestation version을
설치나 안정성 신호로 사용해서는 안 됩니다.

다음 명령은 사전에 검토한 identity를 검사하기 위한 것이며 설치 절차가 아닙니다.

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

위 `gh attestation verify`는 GitHub Attestations API를 조회합니다. 새 candidate는
registry-local Sigstore mirror를 배포하지 않으므로 `--bundle-from-oci`로 검증하면 안
됩니다. 과거 registry의 `sha256-*` attestation version은 감사 연속성을 위해
보존하지만, 그 존재는 현재 candidate가 stable이라는 증거가 아닙니다.

## Runtime Image Contract

`runtime` target은 일반 OpenCodex container 실행보다 의도적으로 더 좁은 process·secret
interface를 갖습니다.

- Image는 non-root UID/GID `10001:10001`로 실행되고 guest API는 port `10100`에서
  서비스됩니다.
- Persistent state는 `/var/lib/opencodex`에만 mount하며 이 경로가 process home이 됩니다.
- API credential과 administration credential은 서로 다른 32-byte base64url token입니다.
  두 token은 `/run/opencodex/bootstrap.sock`을 통해 canonical length-prefixed envelope로
  한 번만 전달됩니다. Bootstrap은 이를 OpenCodex child에만
  `OPENCODEX_API_AUTH_TOKEN`과 `OPENCODEX_ADMIN_AUTH_TOKEN`으로 전달합니다.
- 지원되는 lifecycle은 one-client bootstrap Unix socket을 mount하고 acknowledgement를
  기다린 뒤 소비가 끝난 socket을 제거해야 합니다. Token을 image metadata, command
  line, 저장소 파일 또는 persistent container environment에 두어서는 안 됩니다.
- Runtime은 read-only root filesystem, 모든 Linux capability drop,
  `no-new-privileges`, PID·memory 제한, 제한된 temporary filesystem 및 Docker socket
  미사용 조건으로 실행해야 합니다.
- Host publication은 loopback-only여야 합니다. Container의 guest port는 public
  listener를 허가하지 않습니다.

따라서 일반적인 `docker run` 명령은 지원되는 설정 경로가 아닙니다. 이 image에는
orchestrated secret socket, exact-digest 선택, state ownership, confinement 및 lifecycle
transaction이 필요합니다. 저장소의 experimental Compose profile은 기존 host container
경계를 설명할 뿐이며 Runtime candidate를 지원되는 stable 설치로 만들지 않습니다.

## Supply-Chain Evidence

[`upstream.lock.json`](upstream.lock.json)은 권위 있는 upstream release·provenance
record입니다. 선택된 immutable upstream GitHub release와 direct-tag commit을 npm package
identity, version, tarball 및 SHA-512 integrity에 결합합니다. Detector는 update를 제안하기
전에 GitHub release, source package, packument, tarball, npm registry signature와 npm
Sigstore/SLSA provenance를 교차 검증합니다. 외부 `lidge-jun/opencodex` 저장소는 read-only
input이며 이 프로젝트가 수정하지 않습니다.

빌드된 각 index에는 서로 관련되지만 구별되는 두 evidence layer가 있습니다.

1. **OCI index 내부의 BuildKit platform evidence:** 각 executable child
   (`linux/amd64`, `linux/arm64`)에는 해당 child를 subject로 하는 BuildKit 생성 SPDX SBOM과
   `mode=max` provenance가 별도 OCI attestation manifest descriptor로 존재합니다.
2. **Exact index에 대한 GitHub signed provenance:** Candidate workflow는 정확한
   multi-architecture index digest를 subject로 하는 GitHub artifact attestation을 만듭니다.
   새 candidate의 권위 있는 조회 경로는 GitHub Attestations API입니다. Workflow는 그
   attestation의 두 번째 사본을 GHCR에 push하지 않으며 package용 GitHub storage record도
   만들지 않습니다.

Candidate witness는 index, 두 executable child digest, 잠긴 upstream identity, public Core
source revision, workflow run identity, BuildKit SBOM 및 BuildKit provenance를 결합합니다.
GitHub API attestation 검증은 추가로 repository, signer workflow, `main` source ref, source
revision과 GitHub-hosted-runner 출처를 제한합니다.

## Qualification Level

Qualification state는 evidence를 설명할 뿐 promotion을 의미하지 않습니다.

| 상태 | 통과한 범위 | 의미하지 않는 것 |
| --- | --- | --- |
| `hosted-candidate` | GitHub-hosted build, exact-index evidence, native hosted Linux/arm64 image-contract canary 및 fake Apple CLI를 사용한 macOS Relay/CLI contract test | Live Apple Container 실행이나 stable promotion 자격이 아님 |
| `public_ready=true` | 별도의 제한된 workflow가 성공한 candidate run 하나를 resolve하고 빈 Docker credential configuration으로 exact index digest를 익명 pull함 | Production-ready, stable, signed Runtime release 또는 Apple Container acceptance가 아님 |
| Stable Runtime | 제공되지 않음 | 지원되는 tag, signed stable manifest, Runtime GitHub Release 또는 production signing authority가 없음 |

따라서 모든 public-candidate receipt는 다음 사실을 그대로 유지합니다.

```text
public_ready=true
anonymous_exact_digest_pull=true
apple_container_live=false
stable_promotion_eligible=false
```

Hosted Linux/arm64 canary는 image contract, 고정 guest port, Unix-socket secret bootstrap,
분리된 API/admin credential, HTTP·WebSocket 동작, confinement, secret 비노출, graceful stop과
hosted Docker runtime에서 가능한 state reuse를 검증합니다. Apple Container socket mount,
host publication, APFS cloning, Relay routing, lifecycle rollback/recovery, Desktop UI,
real-provider OAuth 또는 macOS logout/login recovery는 검증하지 않습니다. GitHub-hosted
macOS job은 fake Apple CLI를 사용하며 live Apple Container acceptance에 필요한 nested
virtualization을 제공할 수 없습니다.

## Stable Trust Boundary

[`config/trust/opencodex-runtime-release-ed25519.pub`](../../config/trust/opencodex-runtime-release-ed25519.pub)는
Relay packaging과 strict verifier test를 계속 사용할 수 있게 하는 tracked, syntactically
valid bootstrap public key입니다. 대응하는 private half는 보존하지 않았습니다. 이 key는
production signing authority가 아닙니다.

이 key로 서명된 immutable stable Runtime GitHub Release가 없습니다. 따라서
`relayctl container-runtime check`는 `stable_runtime_manifest_unavailable`을 보고하고,
Control Center는 Runtime을 unavailable로 표시하며 어느 쪽도 candidate를 stage하거나
activate할 수 없습니다. Candidate tag, digest, hosted qualification receipt와
`public_ready=true`는 이 signed-manifest boundary를 우회할 수 없습니다.

Stable promotion과 live Apple acceptance에는 별도 승인된 설계, production signing
authority, immutable signed Runtime release 및 관리되는 physical Apple Silicon capacity가
필요합니다. 이들은 의도적으로 candidate pipeline 범위에 포함하지 않습니다.

## 자동 제안, 수동 채택

6시간 주기의 upstream watcher는 read-only detection과 repository-scoped writer를
분리합니다. GitHub App token은 이 저장소 하나에만 GitHub가 암묵적으로 부여하는
Metadata read와 Contents read/write, Pull requests read/write 권한을 가집니다. 이 권한은
repository content를 쓸 수 있으며 그 자체가 branch 제한은 아닙니다. Workflow는 App을
사용해 `automation/opencodex-<version>-r1` pull request를 제안합니다. App permission
model이 아니라 workflow가 고정 branch·정확한 4개 파일 writer를 사용하고 force-push,
`main` 직접 write, auto-merge 및 candidate promotion을 하지 않습니다. 보호된 `main`은
별도의 adoption boundary를 제공합니다.

필수 `upstream-watch` Environment configuration은 exact `main`으로 제한하고 administrator
bypass를 비활성화하며, 의도적으로 required reviewer와 wait timer를 두지 않습니다. 이
구성은 proposal 생성을 자동으로 유지하지만 protected `main`에 채택하려면 사람이 diff를
검토하고 merge해야 합니다. 2인 또는 독립 review를 수행했다고 주장하지 않습니다.

- `OPENCODEX_UPSTREAM_WATCH_APP_CLIENT_ID`는 Environment variable입니다. Client ID는
  인증 secret이 아니라 공개 identifier입니다.
- `OPENCODEX_UPSTREAM_WATCH_APP_PRIVATE_KEY`는 Environment secret이며 commit하거나
  출력해서는 안 됩니다.
- 생성할 수 있는 경로는 이 directory의 `UPSTREAM_NOTICES.md`, `bun.lock`, `package.json`,
  `upstream.lock.json` 정확히 네 개입니다.

## Source Reference

- [Runtime Dockerfile](Dockerfile)
- [권위 있는 upstream lock](upstream.lock.json)
- [Third-party notices](UPSTREAM_NOTICES.md)
- [Runtime candidate workflow](../../.github/workflows/opencodex-runtime.yml)
- [Upstream watcher workflow](../../.github/workflows/opencodex-upstream-watch.yml)
- [Container profile과 acceptance boundary](../../docs/container-profile.md)
- [저장소 license](../../LICENSE)
