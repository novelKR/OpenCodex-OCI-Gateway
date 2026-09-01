# Native Codex 호환 계층 운영 가이드

이 문서는 원격 OpenCodex와 로컬 native Codex CLI, Codex AppServer, Codex Desktop 및
Linux Remote Control 호스트를 연결하는 호환 계층의 정본입니다. 구현은
[`../client/relay/`](../client/relay/)에 있으며, 저장소 검증은 통과했습니다. external ARM
Remote의 live acceptance는 아래에 기록하고, local client/Desktop/Voice acceptance는 별도
단계로 유지합니다.

private release record, deployment별 rollout evidence, 날짜 고정 결과와 incident 또는
rollback 기록은 private deployment overlay에만 보존하며 public Core에는 게시하지 않습니다.
이 문서는 generic 구현과 검증 계약만 다룹니다.

## 목표와 범위

호환 계층의 목표는 Codex가 기본 제공 `openai` provider를 계속 사용하게 두면서,
Cloudflare Service Auth와 Nginx gateway credential만 로컬 loopback relay에서 주입하는
것입니다.

```text
Codex CLI / local AppServer / Desktop가 사용하는 Codex config
  | built-in openai provider
  v
127.0.0.1:18180  opencodex-relay
  | CF-Access-Client-Id
  | CF-Access-Client-Secret
  | X-OpenCodex-API-Key
  v
Cloudflare Access -> Tunnel -> OCI 127.0.0.1:18080 Nginx
  | exact route/method allowlist
  | edge credential 제거
  v
OCI 127.0.0.1:10100 OpenCodex -> 선택된 upstream account
```

이 구조가 보존하는 불변 조건은 다음과 같습니다.

- Codex thread의 provider identity는 기본 `openai`로 유지합니다.
- Codex의 `Authorization`은 보존하지만, client가 직접 넣은 세 admission header는 relay가
  제거하고 신뢰 저장소에서 읽은 값으로 교체합니다.
- relay와 중앙 OpenCodex/Nginx는 모두 loopback listener를 유지합니다.
- Dashboard, management API, 임의의 `/v1/*`는 공개하지 않습니다.
- credential 원문은 Codex TOML, launchd plist, systemd unit, 저장소와 로그에 넣지 않습니다.
- 현재 gateway key는 등록 장치가 공유하는 admission key이며 장치별 권한·quota 경계가
  아닙니다.

### 사전 빌드 macOS 앱 온보딩

내려받은 ad-hoc 서명 앱의 최종 사용자는 소스·패키지·출력·manifest 서명 값을 입력하지
않습니다. 응용 프로그램 폴더 밖에서 실행하면 Control Center가 서버 입력을 잠그고
**응용 프로그램으로 이동**을 표시합니다. 앱은 staging 복사본을 검증한 뒤
`/Applications/OpenCodexRelay.app`을 우선 사용하고, 쓰기 권한이 없을 때만 확인 후
`~/Applications/OpenCodexRelay.app`로 전환합니다. `sudo`, Authorization Services,
Terminal, quarantine 제거 또는 Gatekeeper 우회는 사용하지 않습니다. 다른 기존 대상은
확인 후에만 교체하며 nonce로 묶인 새 인스턴스 시작 확인 전까지 sibling backup으로
보존합니다. 원본 유지를 기본값으로 제공하고, 변경되지 않은 원본의 macOS 휴지통 이동은
별도 명시 선택입니다. 모호한 복구 상태에서는 bundle을 삭제하지 않습니다.

이전과 사용자의 일반 개인정보 보호 및 보안 허용이 끝난 뒤 Settings는 서버 주소, 인증
프로필과 필요한 자격 증명만 활성화합니다. 메뉴바 아이콘 왼쪽 클릭은 간단 상태 popover,
오른쪽 클릭은 Control Center, 새로 고침, 로그인 항목 설정과 종료만 제공합니다. routing
전환·복구·제거는 Control Center에 유지합니다.

## 설계 결정과 OpenCodex에서 차용한 부분

upstream OpenCodex는 본래 Codex와 같은 컴퓨터에서 `localhost:10100` proxy를 실행하고,
native Codex config와 model catalog를 주입·동기화하는 방식으로 통합합니다. catalog를
다시 쓴 뒤에도 장기 실행 AppServer가 메모리의 이전 목록을 유지할 수 있어, OpenCodex는
좁게 식별한 AppServer에 대한 명시적 restart 경로를 둡니다. 이 구현은 다음 계약을
차용합니다.

- Codex는 Responses API client와 local AppServer 역할을 그대로 유지합니다.
- `openai_base_url`과 startup-loaded `model_catalog_json`을 native 통합 지점으로 사용합니다.
- catalog 파일은 검증 후 원자적으로 교체하고, 장기 실행 AppServer activation은 별도
  수명주기 사건으로 다룹니다.
- CLI, AppServer, Desktop이 같은 `CODEX_HOME`을 읽는지 명시적으로 확인합니다.

원격 배치에서는 OpenCodex와 Codex가 같은 loopback을 공유하지 않으므로, upstream의 local
proxy 가정을 그대로 적용할 수 없습니다. 대신 각 client에 작은 relay를 두고 중앙
admission credential을 그 지점에서만 종료합니다. 중앙 OpenCodex의 Dashboard·management
API나 remote AppServer transport를 public data plane으로 노출하지 않습니다.

| 대안 | 장점 | 이 배치에서의 판정 |
| --- | --- | --- |
| Codex custom provider가 중앙 gateway에 직접 연결 | 구성 파일 수가 적음 | legacy rollback 전용. provider identity가 달라지고 admission secret이 Codex process 환경에 결합됨 |
| 중앙 OpenCodex를 client에 직접 공개 | local daemon 불필요 | 거부. credential 주입·exact route 제한·Desktop 동일-home 검증 경계가 사라짐 |
| SSH local forward만 사용 | 관리·복구에는 단순하고 강함 | Dashboard/OAuth 관리 경로로 유지. 상시 client data plane과 자동 catalog 배포에는 부적합 |
| 각 client에서 OpenCodex 전체를 실행 | upstream의 기본 local 형태와 가장 가까움 | 중앙 account pool과 운영 authority가 중복되므로 이 배치에서는 선택하지 않음 |
| built-in `openai` + loopback relay | native provider identity와 local product 기능을 유지하면서 admission secret을 격리 | 선택안 |

Codex 공식 설정도 built-in OpenAI provider를 proxy/router에 연결할 때 새 custom provider보다
`openai_base_url`을 사용하고, `model_catalog_json`은 시작 시 읽는 경로로 설명합니다.
AppServer와 Remote Control은 별도 native process 수명주기를 가지므로 Responses proxy와
같은 것으로 취급하지 않습니다.

## Native 기능 호환성

| 표면 | 동작 | 현재 판정 |
| --- | --- | --- |
| Codex CLI Responses·tool 호출 | 같은 `~/.codex/config.toml`의 `openai_base_url`을 통해 relay 사용 | ARM external과 x86 local-relay 경로에서 4-turn Responses/tool 시나리오 통과; Desktop acceptance는 별도 |
| local Codex AppServer | CLI와 같은 Codex home/config를 읽는 경우 relay 사용 | external ARM Remote daemon·catalog reader·proxy handshake live 검증; local process별 검증은 별도 |
| Codex Desktop project/session | Desktop의 local AppServer가 같은 Codex home/config를 읽을 때 relay 사용 | 설정 경로 구현, 실제 Desktop acceptance 미수행 |
| Linux Remote Control | 전용 `CODEX_HOME`과 routing mode별 catalog owner 사용 | ARM external 및 x86 local-relay Remote 경로 모두 live 검증 완료 |
| OpenAI-compatible image API | `/v1/images/generations`, `/v1/images/edits` 전달 | allowlist 구현 |
| Desktop native image UI | 별도 product control plane이면 relay가 가로채지 않음 | native 경로 유지, relay 검증 범위 밖 |
| GPT-Live/Realtime 호환 route | local·central double opt-in일 때만 전달 | route 구현, 실제 audio/WebRTC 미검증 |
| MCP·plugin·local tool 실행 | relay가 실행 경로나 권한 모델을 재작성하지 않음 | native 동작 유지 |

Desktop이 별도 `CODEX_HOME`이나 product 전용 control plane을 사용하면 이 relay를 통과하지
않습니다. 따라서 CLI 성공만으로 Desktop, image UI 또는 Voice까지 검증되었다고 판정하지
않습니다.

## 허용 API 계약

local relay와 중앙 Nginx는 다음 exact route만 허용합니다.

| 기능 | 메서드와 경로 | 추가 조건 |
| --- | --- | --- |
| 모델 | `GET`, `OPTIONS /v1/models` | catalog refresh 포함 |
| Responses | `POST`, `OPTIONS /v1/responses` | HTTP/SSE |
| Responses WebSocket | `GET /v1/responses` | `Upgrade: websocket` 필수 |
| Compact | `POST`, `OPTIONS /v1/responses/compact` | gateway safety ceiling만 공유하며 turn admission은 OpenCodex가 소유 |
| Images | `POST`, `OPTIONS /v1/images/generations`, `/v1/images/edits` | compatible API 호출만 |
| Artifact | `GET /v1/opencodex/artifacts/{opaque-id}` | slash 없는 단일 opaque ID |
| Search | `POST`, `OPTIONS /v1/alpha/search` | compatible OpenCodex extension |
| Live setup | `POST`, `OPTIONS /v1/live`, `/v1/realtime/calls` | local·central Voice enabled |
| Live sideband | WebSocket `GET /v1/live/{id}`, `/v1/realtime`, `/v1/realtime/calls/{id}` | local·central Voice enabled |

그 밖의 경로는 catch-all proxy로 전달하지 않습니다. local relay는 비허용 route에
`404 endpoint_not_enabled`를 반환하고, 중앙 gateway도 exact allowlist 밖을 `404`로
닫습니다.

## 지원 플랫폼

signed release는 다음 세 target을 생성합니다.

| OS | Architecture | 용도 |
| --- | --- | --- |
| macOS | `arm64` | 기본 사용자 환경인 Apple Silicon Mac |
| Linux | `amd64` | x86_64 Remote/외부 Codex 호스트 |
| Linux | `arm64` | AArch64 Remote/외부 Codex 호스트 |

relay는 CGO 없이 정적 Go binary로 빌드됩니다. 중앙 OCI OpenCodex pilot의 호스트
architecture와 client relay target은 별개입니다.

## 파일과 credential 경계

### macOS

| 경로/저장소 | 내용 | 보호 |
| --- | --- | --- |
| login Keychain service `opencodex-relay-cf-access-client-id` | Cloudflare client ID | Keychain |
| login Keychain service `opencodex-relay-cf-access-client-secret` | Cloudflare client secret | Keychain |
| login Keychain service `opencodex-relay-gateway-api-key` | 별도 Nginx gateway key | Keychain |
| `~/.config/opencodex-relay/relay.json` | secret 없는 relay 설정 | `0600` |
| `~/.codex/opencodex-relay-catalog.json` | 검증된 visible model catalog | `0600` |
| `~/.codex/config.toml` | marker-owned `openai_base_url`, `model_catalog_json` | 기존 사용자 파일 보존 |
| `$CODEX_HOME/opencodex-relay-interactive.config.toml` | 예약 listener를 명시적으로 선택하는 side-session profile; `openai_base_url`과 같은 `model_catalog_json`만 포함 | marker-owned `0600`; 기존 unmarked file이 있으면 설치 차단 |
| `~/.local/lib/opencodex-relay/relay/` | version별 binary와 `current` symlink | 사용자 전용 |
| `~/Library/LaunchAgents/io.github.novelkr.opencodex-relay.plist` | secret 없는 user service | `0600` |

### Linux

macOS Keychain 대신 `~/.config/opencodex-relay/credentials.env`를 사용합니다. directory는
`0700`, 파일은 `0600`이어야 하며 현재 사용자 소유 regular file이어야 합니다. 파일에는
다음 세 이름의 literal `NAME=value`만 허용합니다.

```text
CF_ACCESS_CLIENT_ID=REPLACE_WITH_SERVICE_TOKEN_ID
CF_ACCESS_CLIENT_SECRET=REPLACE_WITH_SERVICE_TOKEN_SECRET
OPENCODEX_GATEWAY_API_KEY=REPLACE_WITH_GATEWAY_KEY
```

`export`, command substitution, 임의의 추가 변수와 shell 구문은 거부됩니다. Linux user
service는 `~/.config/systemd/user/opencodex-relay.service`이며 unit에 credential을
기록하지 않습니다.

## 릴리스 생성과 게시

production installer는 임의 binary URL을 신뢰하지 않습니다. off-repo Ed25519 private key로
manifest를 서명하고, manifest에 각 binary의 HTTPS URL과 SHA-256을 기록합니다.

```bash
./client/relay/scripts/build-release.sh REPLACE_WITH_VERSION \
  --base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --signing-key /secure/off-repo/opencodex-relay-release-ed25519.pem \
  --previous-build-number REPLACE_WITH_GREATEST_PUBLISHED_CF_BUNDLE_VERSION \
  --output /secure/release-staging/REPLACE_WITH_VERSION
```

게시 레이아웃은 다음과 같습니다.

```text
https://REPLACE_WITH_RELEASE_HOST/opencodex-relay/REPLACE_WITH_VERSION/
  manifest-REPLACE_WITH_VERSION.json
  manifest-REPLACE_WITH_VERSION.sig
  THIRD_PARTY_NOTICES.md
  OpenCodexRelay.app.zip
  opencodex-relay_linux_amd64
  opencodex-relay_linux_arm64
  opencodex-relayctl_linux_amd64
  opencodex-relayctl_linux_arm64
```

private key는 저장소·release server·client에 배포하지 않습니다. trusted public PEM은
release URL과 독립된 검토 경로로 client에 전달합니다. `0.3.8-rc.6` bootstrap은 revision 4를
사용합니다. `0.3.8-rc.7`부터 revision 5가 channel, minimum updater/macOS version, trust key
ID 및 integration/helper protocol을 추가로 결합합니다. 두 revision 모두 component, macOS
bundle ID, `signing_mode: "adhoc"`, app ZIP hash, notice URL/SHA-256를 함께 서명합니다.
installer는 Ed25519 manifest signature, 모든 asset SHA-256, zip shape,
nested ad-hoc signature, Hardened Runtime 및 Relay Team ID 부재를 확인한 뒤 app 내부
helper를 `current`로 선택합니다. notarization 도구를 호출하지 않으며 최초 Gatekeeper 차단을
설치 transaction 실패로 처리하지 않습니다. 지원 manifest는 권한 helper와 generic
`OpenCodexRelayHelperInstaller`를 포함하고, 별도 관리자 승인을 받은 `install` 또는 `update`가
고정 LaunchDaemon을 생성하며 app·installer·helper exact CDHash를 결합합니다. Linux에서는
선택한 relay/relayctl과 notice의 SHA-256을 확인합니다. 앱을 교체·제거하기 전에는
기존 guard 상태를 경로·mode 없이 조회합니다. busy 또는 recovery-required 저널이면 교체를
차단하고, ready 또는 approval-pending이면 먼저 unregister하며 transaction rollback 시 이전
등록을 복원합니다. 기존 revision 1/2 release는 rollback용으로 계속 지원하지만 parked
routing controller가 없으므로 MenuBar-managed Desktop 전환에는 사용하지 않습니다. 같은
version의 기존 directory가 manifest와 다르거나 기존 config의 upstream이 요청값과 다르면
fail closed합니다. native routing 준비 뒤 service activation이 실패하면 이전 relay JSON과
Codex config, `current` target, launchd plist/systemd unit 및 manager 상태를 정확히 복구합니다.
최초 enrollment 실패에서는 새 JSON·routing block·service artifact를 없던 상태로 되돌립니다.
검증된 version directory는 disk에 남을 수 있지만 선택·enable되지 않습니다.

배포용 앱의 `CFBundleShortVersionString`은 전체 SemVer이고 `CFBundleVersion`은
`client/relay/RELEASE_BUILD_NUMBER`의 단일 증가 정수입니다. builder는 이 값이 `1...9999`
범위이며 `--previous-build-number`보다 큰 경우에만 진행합니다. production app에는 tracked
Ed25519 public key의 정확한 byte와 fingerprint가 `Contents/Resources/ReleaseTrust` 아래에
포함됩니다. revision 5 verifier는 `channel`, minimum updater/macOS version, trust key ID 및
integration/helper protocol을 엄격히 검증하지만, 첫 updater bootstrap release인
`0.3.8-rc.6`은 기존 수동 installer 호환을 위해 revision 4로 게시하며 기존 사용자가 직접
설치해야 합니다.

`0.3.8-rc.6`은 알림 전용 update check를 추가합니다. production helper는 bounded GitHub
pagination, strict SemVer channel 선택, owner-only ETag cache와 exact-release read-back을
사용합니다. GitHub metadata는 candidate 발견에만 쓰며 앱에 포함된 tracked key로 manifest를
검증하고 signed app digest가 GitHub asset digest와 일치해야만 update를 보고합니다. 메뉴와
앱 정보 화면에는 channel, 마지막 확인, 상태 및 candidate별로 닫을 수 있는 badge가 표시됩니다.
자동 확인은 production에서 기본으로 켜지지만 local development에서는 network boundary에서
금지됩니다. 이 release는 어떤 항목도 download, stage, replace, relaunch 또는 restart하지
않습니다.

`0.3.8-rc.7`은 publication을 revision 5와 build `1001`로 전환합니다. production 사용자가
명시적으로 선택한 update만 `release stage`로 download·verify합니다. helper는 exact immutable
release를 다시 조회하고 signature, app digest, 안전한 ZIP shape, production bundle identity,
더 큰 build, arm64 executable 집합, nested ad-hoc signature, Team ID 부재, Hardened Runtime,
helper CDHash binding과 embedded tracked key를 재검증합니다. staging은 current-user 전용이며
lock과 release ID/manifest digest에 결합된 strict receipt를 사용합니다. 앱은 receipt와 bundle을
다시 검증하고 Foundation quarantine을 적용·read-back한 뒤 Finder에 표시합니다. 별도 확인
후에만 종료하며, 사용자가 Applications copy를 직접 교체하고 수동으로 엽니다. 앱은 quarantine을
제거하거나 Gatekeeper를 우회하거나 self-relocation update를 수행하거나 atomic app rollback을
보장하지 않습니다. privileged Helper lifecycle은 별도 관리자 승인으로 남습니다.

### public GitHub Release 배포 채널

official artifact는 public Core 저장소에 게시합니다. repository settings에서
[immutable releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)를 먼저 켭니다.
release tag는 `v1.2.3` 형식이 아니라 build version과 정확히 같은 `1.2.3`이어야 합니다.

release workstation의 publish 계정은 해당 repository에 쓰기 권한이 있는 `gh` 인증을 사용하고,
private signing key는 계속 workstation 밖으로 내보내지 않습니다.

```bash
./client/relay/scripts/build-release.sh 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --signing-key /secure/off-repo/opencodex-relay-release-ed25519.pem \
  --previous-build-number REPLACE_WITH_GREATEST_PUBLISHED_CF_BUNDLE_VERSION \
  --output /secure/release-staging/1.2.3

./client/relay/scripts/publish-github-release.sh 1.2.3 \
  --repo OWNER/opencodex-relay-releases \
  --input /secure/release-staging/1.2.3 \
  --public-key /secure/off-repo/opencodex-relay-release-ed25519.pub \
  --release-notes-fragment client/relay/release-notes/1.2.3.md
```

publisher는 public repository, 기존 version 부재, manifest signature, Linux helper 네 개와
signed macOS bundle component, notice URL/hash를 검증한 뒤 draft로 asset을 올립니다. 정확한 8개 asset을 확인한
후에만 publish하고 게시 후에도 같은 asset 집합을 다시 확인합니다. GitHub가 immutable release를
보고하지 않으면 실패로 끝내며, 그런 release에는 client를 enrollment하지 않습니다. GitHub
자체 attestation은 보조 증거이고, client의 out-of-band public PEM 및 manifest signature가
계속 신뢰 기준입니다.

stable tag는 `prerelease=false`, `latest=true`로 게시하고 prerelease tag는
`prerelease=true`, `latest=false`로 게시합니다. publisher는 exact tag, draft/prerelease,
immutable 상태, 8개 asset 이름과 GitHub API digest를 read-back합니다. API 목록 순서는
SemVer 순서로 간주하지 않습니다.

public release 다운로드에는 credential이 필요하지 않습니다. 익명 GitHub API rate limit이
부족할 때만 read-only 만료형 token을 선택적으로 사용할 수 있으며, relay service와 Codex
config에는 전달하지 않습니다.

```bash
install -d -m 0700 ~/.config/opencodex-relay
umask 077
${EDITOR:-vi} ~/.config/opencodex-relay/github-release.token
chmod 0600 ~/.config/opencodex-relay/github-release.token

./client/relay/scripts/install-relay.sh install 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

GitHub 소비 경로는 GitHub REST asset API와 `curl`·`jq`로 exact tag의 release asset만 받아
manifest와 선택 target의 checksum을 검증합니다. 선택적 token file을 쓰는 경우 current user
소유·`0600` regular file이 아니면 거부되고, owner-only temporary curl config에만 잠시 기록된
뒤 종료 시 제거됩니다. generic `--release-base-url` HTTPS 경로도 계속 지원합니다.

### 시스템을 통합하지 않는 Preview 실행

Desktop UX 검토는 저장소 루트의 `./script/build_and_run.sh`를 사용합니다. 이 명령은
`OpenCodexRuntimeMode=preview`인 helper 포함 완전한 app을 만들고 그 app만 실행합니다.
routing binding을 사용하거나 만들지 않고 launchd, Keychain, `/Library`, 기존 설치 앱을
변경하지 않으며 Terminal 또는 `sudo`를 실행하지 않습니다.

읽기 전용 `./script/build_and_run.sh --integration-preflight`는 bounded 코드만 출력하며
`ready`는 exit 0, `integration_required`는 exit 3, `unsafe`/`invalid`는 exit 4를 반환합니다.
실제 통합은 아래의 검토된 local-development build/install 절차로 분리됩니다.

### 개인·조직 내부 local-only development distribution

개인 간 직접 전달 또는 조직 내부 macOS 시험에는 별도 local-only development 경로를
사용합니다. 이는 production release가 아니며 official signed-manifest revision 4 계약을 약화하지
않습니다. macOS 26+ Apple Silicon만 지원하고 release URL, GitHub downloader, 자동 update,
Developer ID, notarization, Gatekeeper 검사, quarantine 제거를 사용하지 않습니다.

clean Git commit에서만 build합니다. local manifest schema 3은 exact commit,
ad-hoc-signed `OpenCodexRelay Dev.app.zip`, notices를 서명합니다. bundle에는 dev 권한 helper와
production과 구현을 공유하되 `.dev` identifier로 격리된 generic
`OpenCodexRelayHelperInstaller`가 포함되고 app metadata가 XPC peer 검증용 build별 exact
helper CDHash를 고정합니다. installer는 `status --json`, `install`, `update`, `uninstall`과 명시적
`recover`만 허용합니다. root-owned schema 2 transaction journal에는 bounded phase와 backup
witness만 기록하며, 중단 작업은 현재 exact bundle의 XPC 준비를 완료하거나 이전 artifact byte와
launchd 상태를 복원한 뒤에만 해제합니다. schema 1·malformed·불완전한 mutation 증거는 추정
복구하지 않습니다. 단, schema 2 `preparing`에서 원본 helper/plist witness와 launchd 실행 상태가
정확히 일치하면 아직 변경되지 않은 준비 transaction만 backup 없이 폐기합니다. `backups_ready`
이후 모든 단계는 계속 backup 증거를 요구합니다.

```bash
./client/relay/scripts/build-local-dev.sh 1.2.3-dev.1 \
  --signing-key /secure/off-repo/local-dev-ed25519.pem \
  --output /secure/local-transfer/1.2.3-dev.1
```

기본 direct-transfer는 source directory의 public PEM을 명시 승인 뒤 검증합니다. 이는
전송 경로 자체를 신뢰한다는 전제에서 accidental damage를 찾습니다. 반복 전달은 별도
경로에서 fingerprint를 확인한 public key를 먼저 Keychain에 pin합니다.

```bash
./client/relay/scripts/install-local-dev.sh trust enroll \
  --keychain-service opencodex-relay-local-dev-trust-example \
  --public-key /secure/separately-verified/local-dev-public-key.pem \
  --expected-fingerprint REPLACE_WITH_LOWERCASE_SHA256

./client/relay/scripts/install-local-dev.sh install 1.2.3-dev.1 \
  --source-dir /secure/local-transfer/1.2.3-dev.1 \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --keychain-service opencodex-relay-local-dev-trust-example \
  --acknowledge-local-development-source
```

manifest schema 3 bundle의 수동 관리자 helper가 아직 준비되지 않았으면 install은 기존 앱,
Relay service, binding을 변경하지 않고 검증한 exact bundle을
`~/.local/lib/opencodex-relay/relay-dev/pending/<version>/`에 owner-only로 보존합니다. 출력된
고정 `sudo ... OpenCodexRelayHelperInstaller install|update|recover` 명령을 실행한 뒤 같은
install 명령을 다시 실행합니다. 재실행은 signed artifact hash와 main/helper/installer CDHash,
candidate XPC `ready`를 다시 확인하고 동일 pending bundle만 최종 앱으로 승격합니다. 다른 source
artifact나 활성 보호·복구 상태는 fail-closed됩니다.

installer 실패 receipt는 schema 1을 유지하며 optional `failure_phase`, `failure_reason`,
`rollback_result`를 제공합니다. 값은 bounded code만 포함하고 경로, UID, CDHash, errno 원문,
launchctl/XPC 출력은 포함하지 않습니다. rollback이 완료되면 최초 실패 단계가 유지되고,
rollback 자체가 실패하면 상태는 `recovery_required`로 남습니다.

Keychain pin을 쓰지 않으면 `--keychain-service` 대신
`--acknowledge-local-source`를 사용합니다. dev installer는
`~/.local/lib/opencodex-relay/relay-dev`, 별도 config/binding,
`127.0.0.1:18190/18192`, `io.github.novelkr.opencodex-relay.dev`만 사용합니다. install은
native parked로 시작하며 Codex TOML, catalog, credential access, OpenCodex 상태를
바꾸지 않습니다. dev와 production namespace는 같은 Codex config를 소유할 수 없으므로
병행 시험에는 별도 `CODEX_HOME`을 사용합니다. 검토된 release channel 밖의 로컬 개발 app은
사용자가 직접 실행·승인하고,
login registration은 running app에서 선택적으로만 수행합니다.

`--acknowledge-unsigned-local-build`는 deprecated 호환 별칭으로만 유지됩니다.

`--config`은 반드시 `~/.config/opencodex-relay/relay-dev/` 아래의 clean absolute
path여야 합니다. installer는 symlink parent나 이 namespace 밖으로 resolve되는 경로를
거부합니다. uninstall은 fail-closed입니다. 설치된 dev helper와 dev service 중지 전후의 두
native ownership proof가 필요하며, config가 없는데 dev artifact가 남아 있으면 삭제하지 않고
수동 복구를 요구합니다.

## Apple Silicon macOS 설치

### 1. credential 등록

다음 명령은 값을 shell argument나 history에 넣지 않고 Keychain prompt로 받습니다.

```bash
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-cf-access-client-id -w
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-cf-access-client-secret -w
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-gateway-api-key -w
```

일상적인 Desktop 사용에서는 위 bootstrap 명령보다 **Control Center → Settings → 외부
게이트웨이**를 권장합니다. Settings 카드는 주소, **연결 테스트**, **적용**, 그리고 Cloudflare
Client ID, Cloudflare Client Secret, Gateway API Key를 각각 추가·교체하는 SecureField를
제공합니다. 기존 credential 값은 UI로 읽어오지 않고 삭제 기능도 제공하지 않습니다. 새 항목은
Desktop 앱과 `/usr/bin/security`만 신뢰하는 login-Keychain ACL로 만들고, 교체할 때는 기존 ACL
항목을 보존하면서 두 필수 신뢰 항목을 보정합니다. 앱은 `/usr/bin/security` 비대화식 readback을
입증한 뒤에만 저장 성공으로 표시합니다.

연결 테스트는 resident catalog parser로 인증된
`GET /v1/models?client_version=…`를 정확히 한 번 수행합니다. 5초 timeout, redirect·retry 없음,
8 MiB 응답 상한을 적용하며 catalog, pending marker, config를 쓰지 않습니다. 적용은 테스트를
다시 수행하고 inspect에서 본 config digest와 routing generation을 요구합니다. External이
활성 상태이면 신규 admission을 park하고 진행 중 요청을 drain한 뒤 config를 원자 저장하고 기존
RuntimeManager에 새 External generation 교체를 요청합니다. Codex Desktop은 계속 실행됩니다.
Native와 Local 상태에서는 이후 External 선택에 사용할 주소만 저장합니다.

credential 저장은 주소 적용과 의도적으로 독립적이며 테스트 실패 후에도 Keychain에 유지됩니다.
각 저장은 검증 영수증을 무효화합니다. UI는 검증 필요, 인증 조합 불일치, 연결 불가, catalog
응답 오류를 구분합니다. 영수증에는 config digest, Keychain 수정 시각, 검증 시각, 결과 코드만
저장합니다.

후보 주소는 owner-bound 진단에서도 JSON stdin으로만 전달합니다.

```bash
opencodex-relayctl gateway inspect --json
opencodex-relayctl gateway test --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
opencodex-relayctl gateway apply \
  --expected-config-digest REPLACE_WITH_INSPECTED_SHA256 \
  --expected-routing-generation REPLACE_WITH_INSPECTED_GENERATION \
  --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
```

apply는 pending/recovery 상태와 config, routing generation, credential 변경 경쟁을 모두
거부합니다. live swap이 실패하면 이전 config와 runtime을 복구하고, 그 완료를 입증하지 못하면
`recovery_required`로 fail-closed됩니다.

세 값은 서로 다른 credential입니다. interactive SSH의 email PIN/TOTP나 SSH private key를
API data plane credential로 재사용하지 않습니다.

### 2. 명시 version 설치

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

기존 root `model_provider = "pw_opencodex"`, `"opencodex"`, 또는
`"pw_opencodex_remote"`를 현재 native 경로로 전환할 때 `--migrate-legacy`를 추가합니다.
또한 과거 direct loopback `http://127.0.0.1:10100/v1` 또는
`http://localhost:10100/v1`도 명시적으로 지원합니다. migration은 기존 `config.toml`을
`config.toml.pre-opencodex-relay-<UTC>`로 `0600` backup하고 알려진 root assignment와
`model_catalog_json`만 제거하며 provider table은 유지합니다. 다른 custom provider 또는
임의의 `openai_base_url`은 자동 변경하지 않고 실패합니다.

### 3. 검증

```bash
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
curl --fail --silent http://127.0.0.1:18180/__relay/healthz
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl catalog refresh
codex debug models
```

수동 refresh 명령은 fetch/write/activation/AppServer 재시작을 수행하지 않습니다. relay-owned
catalog는 resident lifecycle이 소유하고, `remote_manager` catalog는 해당 manager가 소유한다는
상태만 보고합니다. `--no-apply`는 기존 CLI 호환을 위해서만 계속 허용합니다.

revision 4에서는 제거된 `codex_routing=true` shorthand 대신 `mode status --json`을 봅니다.
이미 active인 profile은 `phase=relay_active`, `relay_admission=allow`,
`catalog_refresh=run`입니다. 새 macOS External enrollment는 사용자가 Desktop을 선택해 정상
종료시키고 request를 apply하기 전까지 의도적으로 `phase=relay_pending_restart`,
`relay_admission=deny`, `catalog_refresh=pause`를 보입니다. catalog 변경 뒤에는
`.restart-pending` marker가 남으므로 broad AppServer kill 대신 같은 home의 CLI/Desktop을
재시작합니다. 새 `codex` process에서 실제 Responses 호출을 검증하고 Codex Desktop도 완전히
종료·재실행해 같은 local Codex home을 읽는지 확인합니다.

### 4. Native 전환과 Connection Status Center

macOS 26+ Apple Silicon의 지원 release manifest는 Hardened Runtime을 적용한 ad-hoc
`~/Applications/OpenCodexRelay.app` link를 만들며 설치 중 앱을 실행하거나 login item 성공을
요구하지 않습니다. MenuBar app은
reviewed bundle ID, strict code-signature signed identifier, exact 10자 대문자 Apple Team ID가
모두 일치하는 Codex Desktop `.app`만 저장하고, normal quit/relaunch 직전마다 같은 identity를
재검증합니다. 사용자가 고른 path, app 이름, 실행 중 process는 trust 근거가 아닙니다.

2026-08-23 `/Applications/ChatGPT.app`의 Codex Desktop `26.818.41509` 설치본을
`codesign --verify --deep --strict`, designated requirement 및 Gatekeeper
`Notarized Developer ID`로 독립 검증하여 `com.openai.codex` / `2DC432GLL2` tuple을
확정했습니다. production과 local-development plist 및 양쪽 builder/installer가 이 exact
tuple을 고정·검증합니다. 향후 signer identity가 달라지면 새 official signed build를 다시
독립 검증하고 명시적으로 갱신할 때까지 Desktop 제어가 fail-closed됩니다. 선택한 path에서
identity를 추론하거나 trust-on-first-use로 gate를 우회하지 않습니다.

Desktop lifecycle에는 AppleScript나 force quit을 사용하지 않습니다. 현행 preserve-only 제거
경로는 Finder Apple Events를 사용하거나 사용자 data를 이동하지 않습니다.
Relay 앱은 공증하지 않으므로 Finder에서 한 번 여십시오. macOS가 차단하면 즉시
**시스템 설정 → 개인정보 보호 및 보안**에서 OpenCodexRelay의 **확인 없이 열기(Open
Anyway)**를 선택하고 다시 **열기**를 확인합니다. 버튼은 차단된 실행 뒤 보통 약 1시간 동안
표시되며 update 뒤 같은 승인이 다시 필요할 수 있습니다. quarantine attribute 삭제나
Gatekeeper 비활성화는 하지 않습니다. staged update는 Finder handoff를 사용하고 새 앱이
정상 실행될 때까지 이전 앱을 수동 보관합니다. helper의 관리자 prompt는 별도 승인입니다.
`login_registration=pending`은 macOS Login Items 승인이 아직 필요하다는 뜻이므로, 승인 뒤 app을
다시 열거나 상태를 다시 확인합니다.

```text
opencodex-relayctl mode status --json
opencodex-relayctl mode request native|external|local_opencodex|relay --json
opencodex-relayctl mode apply --confirm-desktop-exited --json
opencodex-relayctl mode cancel --json
opencodex-relayctl mode recover --complete|--rollback --confirm-desktop-exited --json
opencodex-relayctl mode repair-native --expected-routing-generation N \
  --confirm-local-development-native-repair --json  # local-development 전용
opencodex-relayctl mode inspect-native-repair --expected-routing-generation N --json
opencodex-relayctl mode inspect-native-repair-owner --expected-routing-generation N \
  --expected-owner opencodex --installation-id ID --installation-fingerprint SHA256 \
  --native-restore-fingerprint SHA256 --ocx-executable PATH --ocx-sha256 SHA256 --json
opencodex-relayctl mode repair-native-routing --expected-routing-generation N \
  --expected-owner local_relay|opencodex --confirm-desktop-exited \
  --confirm-local-development-native-routing-repair \
  [--installation-id ID --installation-fingerprint SHA256 \
   --native-restore-fingerprint SHA256 --ocx-executable PATH --ocx-sha256 SHA256] \
  --json  # owner=opencodex일 때 증명 인수 필수, local-development 전용
```

`request`는 desired backend만 pending으로 바꾸고 기존 remote session을 끊지 않습니다.
MenuBar는 선택된 app이 실제로 종료한 뒤 `apply`를 실행하며, apply는 watcher acknowledgement,
catalog pause, active request drain 뒤에만 marker-owned `openai_base_url`과
`model_catalog_json`을 제거하거나 복원합니다. 진행 중 SSE/WebSocket/tool 요청을 native와
remote 사이에 replay·handoff하지 않습니다. local-development 전용 `repair-native`는 일반
복구 두 작업 모두 불가능한 고립된 `recovery_required` 상태에서만 Maintenance에 나타납니다.
명시적 확인, 화면의 exact generation, 물리 경로 바인딩, routing/removal journal·gate 부재,
Relay·OpenCodex·다른 owner·unmanaged routing artifact가 없다는 독립 검증을 모두 요구합니다.
성공해도 routing-state만 `native_active` 다음 generation으로 바꾸며 production scope, Codex
TOML, OpenCodex, helper, 서비스 설정은 수정하지 않습니다.


네이티브 검증을 막는 항목이 `openai_base_url` 또는 `model_catalog_json`뿐이면
`inspect-native-repair`는 값·URL·경로를 반환하지 않고 `state_only`, `local_relay`,
`opencodex`, `unavailable` 소유권만 분류합니다. 다른 소유자, 혼합·불완전 marker,
marker 없는 사용자 override는 자동 수정하지 않습니다. 현재 local Relay 소유 설정은
완전한 marker block과 managed interactive profile만 제거합니다. Discovery는 기존 제거 ID와
fingerprint를 바꾸지 않는 별도 native-restore 증명을 선택적으로 제공합니다. 따라서 자동 패키지
제거 권한이 manual인 Homebrew 설치도 복구 실행만 가능할 수 있습니다. Helper는 정확한 Tier A/B
후보를 다시 탐색하고 package tree를 private snapshot으로 고정한 뒤 snapshot의 bundled Bun과
CLI만 private 작업 디렉터리와 allowlist 환경으로 실행합니다. 선택한 `env node` launcher는 실행하지
않으며 전체 process group 정리를 증명한 bounded 결과만 인정합니다. 후보 선택 뒤 Desktop을 종료하기 전에
`inspect-native-repair-owner`가 OpenCodex 설정의 `valid|invalid|unavailable`과 Codex 통합의
`enabled|disabled|unknown`만 반환합니다. invalid 또는 unavailable이면 Desktop을 종료하지 않습니다. 변경 전 경로 비노출의
timestamped `0600` backup을 만들고, 네이티브 라우팅 검증 뒤에만 generation을 증가시킵니다.
TOML 복구 후 상태 저장만 실패하면 기존 recovery generation을 유지하고 상태 전용 복구를
다음 작업으로 활성화합니다. 이 흐름은 OpenCodex Codex 통합만 끄며 Shim·프록시·패키지·
data는 유지합니다. OpenCodex가 `success=false`, config `skipped`, `changed=false`의
증명된 무변경 desired-state 충돌을 반환한 경우에만 200/500/1000ms 간격으로 세 번 추가
재시도합니다. 소진·설정 오류·구조화 복구 실패·유효하지 않은 JSON 결과는 각각
`native_owner_busy`, `native_owner_configuration_invalid`, `native_owner_restore_failed`,
`native_owner_result_invalid`로 구분하며 값·경로·stdout·stderr는 로그에 남기지 않습니다.

`mode status --json`의 `connection`은 `local_relay`, `routing_sync`,
`remote_gateway`, `catalog`의 제한된 enum만 제공하고 URL, credential, account, raw health,
raw error는 제공하지 않습니다. `active_requests`는 local relay가 unreachable이면 `null`입니다.
popover는 선택 앱과 현재 적용 경로만 요약합니다. Control Center는 이 JSON만 읽고 기존
relayctl 안전 조건 안에서 승인된 제어를 제공하며, Desktop backend는 언제나
`unverifiable`로 표시합니다. 실제 native success는 재실행 뒤 live Codex task로만 판정합니다.

toolbar의 수동 새로 고침은 polling과 겹쳐도 버리지 않고 한 번으로 합치며, queued 수동 요청을
cadence 재시작보다 먼저 실행합니다. schema validation 뒤 snapshot 변경과 무변경을 구분하고,
완료 이벤트에는 `changed`, `generation`, `phase`만 기록합니다.

Sidebar의 **활동 로그**는 현재 세션 이벤트를 최대 500건 표시하고 같은 안전 이벤트를
macOS 통합 로그 `Activity` category에 기록합니다. 레벨 필터·검색·JSON Lines·현재 bundle용
로그 명령을 제공하며 allowlist된 상태만 기록합니다. 라우팅 snapshot은 개요와 연결 및
라우팅 카드 상태를 함께 포함합니다. 민감한 경로·출력·자격 증명은 제외하고 앱에서 목록을
지워도 시스템 로그는 유지합니다.

서명 앱은 bundle의 `AppIcon.icns`를 사용해 Dock에 표시됩니다. Dock 재열기와
**연결 세부 정보…**는 모두 앱을 활성화하고 하나뿐인 Control Center 창을 전면으로
가져옵니다. 이 창을 닫아도 resident Relay는 계속 실행됩니다. 창은 사용자 정의 chrome
대신 macOS 26 네이티브 sidebar·toolbar·sheet와 Liquid Glass control을 사용합니다.
상태·설명 텍스트는 macOS 기본 `body` 크기와 의미론적 label 색을 사용합니다. 값은 제한된
라벨 열 옆에 배치하고, 관련 버튼은 가로 그룹에서 좁은 폭일 때만 세로로 전환합니다.

**Codex Desktop** 화면에는 0600 routing binding이 지정한 정확한 Codex TOML을 위한 읽기
전용 설정 카드가 있습니다. metadata 확인·미리보기·외부 앱 열기 때마다 binding을 다시
읽고 대상을 `O_NOFOLLOW`로 열어 일반 파일인지 확인하며 접근 전후 `lstat`/`fstat`
identity를 비교합니다. 요약에는 위치·존재·크기·수정 시각·변경 여부·Relay phase·적용
backend를 표시합니다. 이 화면이 보이는 동안 2초마다 metadata를 확인하고 서로 다른 교체나
삭제마다 상태 새로 고침과 privacy-filtered 이벤트를 한 번만 발생시킵니다.

전체 TOML은 민감 정보 경고를 승인하기 전에는 읽지 않습니다. 선택 가능한 monospaced
미리보기는 1MiB 이하의 올바른 UTF-8로 제한하며, 창 소유 sheet를 닫으면 원문과 승인 상태를
모두 제거합니다. 과대 또는 비 UTF-8 일반 파일도 다시 검증한 뒤 외부 앱으로 열 수 있습니다.
시스템 기본 앱, 설치된 Visual Studio Code·Xcode·텍스트 편집기, **다른 앱…**은 shell이나
`code` CLI 없이 NSWorkspace만 사용하고 시스템 최근 항목 추가를 끕니다. Relay는 파일을
편집·저장하지 않으며 활동 이벤트에는 대상 앱과 제한된 결과만 기록하고 경로·원문·hash·
외부 앱 출력을 제외합니다.

MenuBar app의 **시스템 기본값**은 macOS의 첫 번째 preferred language만 사용합니다. `ko`는
한국어, `en`은 영어로 표시하고, 지원하지 않거나 확인할 수 없는 언어는 한국어로 fallback합니다.
Control Center 설정의 **언어** 메뉴에서 시스템 기본값, 한국어, English를 즉시 선택할 수 있습니다.
선택값은
bundle ID별 preference에 저장되므로 production과 local-development MenuBar app이 서로
공유하지 않습니다. 이 선택은 relayctl JSON, routing phase, CLI 식별자를 바꾸지 않으며,
protocol 값은 안정적인 영문으로 유지합니다.

macOS external gateway config는 `connection_probe.enabled=true`일 때 `relay_active`에서만
10분마다 bounded `GET /v1/models` 연결 관측을 수행합니다. catalog refresh 결과와 coalesce하며
5초 timeout, redirect/retry 없음, catalog write 없음으로 제한합니다. pending native 이후 새
probe는 중단하고, `applying`, `native_active`, `recovery_required`에서는 catalog/probe/credential
lookup/remote egress가 모두 중단됩니다.

### macOS External ↔ Local OpenCodex(10100) profile 경계

revision 4 macOS 설치의 canonical/default relay configuration은
`external_gateway`입니다. signed Control Center는 **External gateway**, **Local
OpenCodex (10100)**, **Native ChatGPT Codex** 세 선택지만 명시적으로 제공합니다.
Local은 단순 TCP port 확인이 아닙니다. relay가 credential 없이 proxy/redirect 없이
`/healthz`의 `service:"opencodex"`, `status:"ok"`, port `10100`과 visible·중복 없는
`/v1/models`를 bounded check로 모두 확인한 경우에만 선택할 수 있습니다. 확인에 실패하면
Local은 비활성화되고, 해당 요청을 External로 자동 fallback하지 않습니다.

relay는 External과 Local에 서로 다른 catalog file을 소유하고, 선택된 Desktop app 종료,
active request drain, `mode apply` 완료 뒤에만 알맞은 `model_catalog_json`을 원자적으로
선택합니다. listener/PID는 유지하지만 owner-only same-user Unix socket은 immutable upstream
runtime만 바꿉니다. SSE, WebSocket, tool work는 drain할 뿐 이관·replay하지 않습니다.

active Local runtime의 identity/catalog 확인이 나중에 사라지면 relay는 local catalog worker를
멈추고 typed `503 local_opencodex_unavailable`을 반환합니다. durable state는 Local로 남으며,
사용자가 External 또는 Native를 명시 요청하고 등록된 Desktop을 종료한 뒤 apply해야 합니다.

#### OpenCodex 자동 탐색과 권한 경계

**로컬 OpenCodex 설치 자동 탐색…**은 relayctl의 제한된 증거를 Tier 순서로 확인합니다.

- **Tier A**: 기존 enrollment, canonical absolute `PATH` launcher, native npm prefix처럼 현재
  사용자와 직접 연결된 증거만 검사합니다. MenuBar 자체가 임의 `PATH` directory를 순회하는
  것은 아닙니다.
- **Tier B**: trusted npm/Homebrew prefix와 bounded nvm, fnm, Volta, asdf 사용자 root를
  추가로 검사합니다.
- **Tier C**: A/B에서 후보를 찾지 못한 경우에만 사용자가 별도 승인하는 bounded local-volume
  scan입니다. 결과가 truncate될 수 있으며 inspection evidence일 뿐 mutation authority가
  아닙니다.

Discovery schema 4는 `teardown_capability=relay_preserve_v1`,
`data_capability=preserve_only`, bounded compatibility reason과 기존
`homebrew_guarded_npm` / `homebrew_guard_required` 상태를 추가합니다. Swift 앱은 schema 2·3·4를
읽지만 schema 2·3 후보는 표시 전용입니다. 자동 제거는 user-owned·user-writable이고 elevation이
필요 없는 schema 4 Tier A/B 후보 중 Volta가 아니며, npm/Node/Bun/CLI/package execution
closure가 변하지 않고 검토된 Relay teardown identity profile 하나와 정확히 일치하는 경우에만
허용합니다. 현재 darwin/arm64 registry는 stable `2.22.0`, `2.23.0`, `2.24.0`, `2.24.1`,
`2.24.2`, `2.25.0`, `2.26.0`, `2.27.0`, `2.28.0`, `2.29.0`, `2.31.0`, `2.32.0`,
`2.32.1`, `2.33.0`만 정확히 승인합니다. 각 profile은 공식 npm integrity, 독립 재구성한 전체
설치 closure digest, 필수 module hash와 version-specific adapter ID를 고정합니다. preview와
존재하지 않는 stable `2.30.0`은 승인하지 않습니다. 필수 module hash는 bounded 진단으로
유지하지만 전체 신뢰 증거를 대신하지 않습니다.

package identity와 실행 구현은 별도 registry입니다. identity profile은 package name, version,
artifact variant, platform, registry integrity, reviewed closure digest, adapter ID와 required-module
진단을 고정합니다. closure digest는 정렬된 상대 경로, entry 종류, 실행 비트, regular-file content
digest, raw symlink target을 길이 구분해 계산합니다. UID·GID·mtime·비실행 permission bit는 기존
소유권/mode 및 discovery-time tree 검증에서 별도로 확인합니다. adapter registry는 해당 ID를
검토된 entrypoint와 embedded source 집합에 고정합니다. 향후 지원 버전 또는 같은 버전의 정상
artifact는 정확한 profile을 추가해 확장하고, 실행 시 discovery가 선택한 artifact variant를 다시
선택합니다.
version range, heuristic fallback, 부분 일치는 사용하지 않습니다. 일치 profile 없음·복수 일치,
adapter 구현 누락·중복, closure entry 추가·삭제·retarget·내용 변경은 모두 manual-only입니다.
snapshot 생성 후에도 전체 closure digest와 독립적인 discovery-time execution-tree fingerprint를
다시 검증합니다. 별도의
sanitized·untruncated Tier-B authority pass도 동일 installation ID와 aggregate fingerprint를
정확히 한 번 재현해야 하며, 화면의 package/relative path는 삭제 권한이 아닙니다.

`homebrew_guarded_npm`은 정확한 arm64 전역 npm `/opt/homebrew` layout, current-user 소유권,
완전한 실행 증거, 그리고 기존 `exact_npm`을 막는 원인이 일반 Homebrew group-write뿐인 경우로
제한합니다. world-write, ACL, foreign owner, symlink, 외부·충돌 launcher, 불완전한 탐색은
manual-only입니다. production과 local-development 모두 generic fixed installer의 검토된
`sudo` 명령으로 distribution별 LaunchDaemon을 설치하며 별도 macOS 관리자 승인이 필요합니다.
두 profile은 service ID와 CDHash가 격리되고 app·installer·helper exact CDHash 및 XPC readiness를
검증합니다. 이 승인은 `Open Anyway` 승인과 별개입니다.

기존의 두 비파괴 handoff, 즉 **proxy 유지 + Codex integration/Shim 해제**와 **proxy 유지 +
integration만 해제**는 사용자가 승인한 exact executable/fingerprint로 계속 수행할 수 있습니다.
legacy alert의 `ocx uninstall` 경로는 제거했습니다. 완전 제거는 opaque 24-hex installation ID와
64-hex aggregate fingerprint만 받는 별도 안전 제거 wizard를 사용하며 caller path, glob,
package name, implicit-all selection을 받지 않습니다.

이 wizard는 handoff 중에도 유지되며 안전 사전 확인, Desktop 종료, 승인한 OpenCodex 작업,
Desktop 재실행, Relay status 재확인을 단계별로 표시합니다. recovery·applying·status 없음·
검증되지 않은 라우팅은 Desktop 종료와 OCX 실행 전에 차단합니다. Shim handoff 성공 뒤에는
status와 후보를 모두 다시 조회하고, 같은 canonical package root와 executable에 정확히 하나의
새 후보와 적격 fingerprint가 결합된 경우에만 자동 제거를 활성화합니다. 후보가 없거나 중복·
변경되면 제거를 잠그고 재탐색을 요구합니다.
helper가 partial/unknown 결과를 반환해도 Desktop 재실행과 status 재확인을 시도해 확인된
recovery 상태를 표시하지만 context별 recovery gate를 우회하지 않습니다.

#### 제거 context

자동 제거는 discovery 전에 다음 context 중 하나를 선택하고 작업 중 다른 context로 바꾸지
않습니다.

- **Integrated**는 owner-only routing binding이 ready인 경우에만 사용합니다. healthy resident
  Relay와 안정적으로 검증된 External 또는 Native route를 요구하는 기존 조건을 유지합니다.
- **Standalone Native**는 `routing-binding.json`이 정확히 없고 부분 Relay 자산이나 integration
  recovery가 없는 경우에만 사용합니다. 표준 `~/.codex/config.toml`만 사용하고 clean Native 또는
  검증된 OpenCodex 소유 설정만 허용하며, package command를 시작하기 전에 복원된 Native Codex
  설정과 `clientIntegrations.codex=false`를 검증합니다. 원격 Gateway에 연결하지 않으며
  server URL, Gateway 자격 증명, Relay config, LaunchAgent, Keychain 항목 또는 실행 중인 Relay
  service를 요구하지 않습니다. 이 Relay integration 자산은 작업 전후에 변경되지 않습니다.

unsafe·invalid binding, preview mode, integration recovery, 충돌하거나 손상된 journal, custom
`CODEX_HOME`, local Relay, mixed, foreign, unmanaged Codex 설정은 계속 fail-closed입니다.
자동 경로는 canonical app-managed root로 한정하며 ambient `XDG_CONFIG_HOME` 또는
`OPENCODEX_HOME` override는 거부합니다.

공유하는 owner-only lifecycle lock이 Relay 준비·integration과 두 제거 context의 동시 실행을 막습니다.
recovery record는 최초 context에 고정되며 기존 integrated record를 standalone 제거로 옮기거나
재해석하지 않습니다.

앱은 Native 전용 relayctl operation인 `discover-open-codex-native`,
`inspect-open-codex-native-removal`, `inspect-open-codex-native-data`,
`remove-open-codex-native`를 소유합니다. strict schema 1 응답은 `standalone_native`,
`boundary_revision`, `native_state`, `native_recovery_required`, opaque 후보 identity를 결합합니다.
Gateway 입력이나 caller가 선택한 filesystem path를 받지 않으며 거부된 설정의 수동 fallback으로
사용하지 않습니다.

#### 안전한 npm 제거 절차

안전 제거를 시작하려면 reviewed identity를 통과한 Codex Desktop이 선택되어 있어야 합니다.
integrated 제거는 추가로 resident Relay가 정상이고 Local OpenCodex가 아닌 **검증된 External 또는
Native** route가 안정적으로 적용돼 있어야 합니다. standalone 제거는 대신 위 Native 경계를
요구하며 원격 Gateway나 Relay health를 조회하지 않습니다. 조건이 바뀌면 제거를 시작하지 않고
새 검토를 요구합니다. integrated 제거는 계속 `preserve_only`입니다. standalone 제거도 모든
OpenCodex data root 보존이 기본값이며, 적격 `selective_trash_v1` 후보인 경우에만 검증된
inventory에서 항목을 명시적으로 선택하고 기존 두 번째 Trash 확인을 거칠 수 있습니다.
implicit-all selection과 permanent-delete fallback은 없습니다.

권한 helper는 Control Center의 **앱 정보** 또는 **유지보수 및 복구**에서 미리 설정할 수
있습니다. 등록 요청 뒤 승인이 필요하면 **로그인 항목 및 확장 프로그램 열기…**로 시스템
설정에 이동하고, Relay로 돌아오면 상태를 자동 재확인합니다. 이 경로만으로 Homebrew mode나
OpenCodex package는 변경하지 않습니다.

1. integrated discovery는 정확한 `relay_preserve_v1` identity profile 하나와 `preserve_only`를
   schema 4로 반환해야 합니다. standalone discovery는 일치하는 `boundary_revision`, bounded
   `native_state`, `native_recovery_required=false`, 정확한 적격 후보 하나를 strict schema 1
   `standalone_native` contract로 반환해야 합니다. schema 2·3, 미지원 버전, 변경된 module 또는
   transitive closure entry, 모호한 registry entry는 화면에 표시하되 manual-only입니다.
2. 검토 화면은 보존 대상(모든 OpenCodex data)과 제거 대상(exact npm package 및 검증된 managed
   integration)을 구분합니다. integrated 검토는 nonzero `UInt64` routing generation을,
   standalone 검토는 Native boundary fingerprint를 freeze하고 package 제거를 한 번 명시
   확인합니다. integrated 검토에는 data selector가 없습니다. standalone은 preserve가 기본이며,
   후보가 `selective_trash_v1`을 명시하면 검증된 inventory의 선택 항목에 두 번째 확인과 exact
   inventory revision을 요구합니다. 어느 context도 caller path/glob이나 implicit-all을 받지 않습니다.
3. 실제 실행 직전에 integrated route 안전 조건 또는 standalone Native 경계와
   `homebrew_guarded_npm` 권한 helper 준비 상태를 다시 확인합니다. 미등록·미승인은 exact trusted
   Desktop을 종료하기 전에 차단합니다.
4. root helper가 `prepare`로 Homebrew group-write를 임시 해제하고 동일 후보를 재탐색한 뒤
   `commit`합니다. helper는 `openat`·`O_NOFOLLOW`·`fstat`으로 허용 경로를 확인하고 원래
   inode/device/mode를 root-owned `0600` 저널에 기록하며 package 삭제나 npm 실행을 하지 않습니다.
5. Go 제거 coordinator는 private immutable package snapshot의 검증된 Bun으로 Relay 소유 adapter를
   실행합니다. shell, ambient `PATH`, caller path, 수정된 설치 source는 사용하지 않습니다.
   preflight는 무변경이며, 실제 pass는 managed service/proxy 정지, native routing 복원,
   client integration/environment/shell hook 해제, OpenCodex의 canonical
   `codex-shim.autorestore.lock` 아래 Shim 복원을 수행합니다. Relay 소유 Shim module은 전체
   state/wrapper/backup/destination을 먼저 검증하고 각 destination directory에 rollback hardlink를
   staging합니다. 모든 backup과 state는 전체 no-replace publication이 검증될 때까지 보존합니다.
   중간 실패는 역순 보상하고, rollback을 증명할 수 없으면 lock·state·staging 증거를 유지하여
   명시적 recovery로 전환합니다. 일반 lock owner record는 OpenCodex schema 1의 numeric timestamp
   계약을 사용하므로 변경 전 종료된 owner는 회수할 수 있습니다. 첫 변경 직전에는 별도의 recovery
   marker를 추가하여, 중단되었거나 보상을 증명하지 못한 batch를 명시적 recovery 전까지 OpenCodex가
   stale lock으로 회수하지 못하게 합니다.
6. 내부 `relay_preserving_teardown` schema 1 receipt가 예상 adapter ID,
   `data_preserved=true`, `config_root_removed=false`, 모든 필수 component postcondition을 증명해야
   합니다. refused·malformed·partial·unverified receipt에서는 npm을 시작하지 않습니다. 변경
   가능성이 있는 unknown 결과는 자동 재시도하지 않고 recovery로 전환합니다.
7. teardown과 context별 integrated routing 또는 standalone Native postcondition을 모두 통과한
   뒤에만 standalone selective Trash가 검토한 항목을 옮길 수 있으며 직전·직후 Native 경계를
   확인합니다. 그 다음 기존 private npm snapshot runner가 package를 제거합니다. package 부재와
   같은 경계를 재검증하고, 마지막으로 `release`가 Homebrew mode를 역순 복원한 뒤 Desktop 재실행과
   status refresh를 수행합니다.

exit code나 path 부재만으로 성공을 표시하지 않습니다. strict receipt가 `completed`,
`package_absent`, `data_preserved`, context recovery 불필요, 최종 integrated routing 또는
standalone Native 재검증, 재실행 가능한 terminal journal 유지를 모두 증명해야 합니다. 앱은
context가 포함된 schema 3 `terminal_ack_pending` recovery checkpoint에 정확한
`terminal_receipt_digest`를 먼저 저장하고 readback합니다. 이어서
`discover-open-codex-native --acknowledge-terminal-receipt-digest <digest>`를 호출합니다. bare
discovery나 다른 digest는 journal을 유지합니다. 일치하는 acknowledgement가 같은 경계와 package
부재를 재검증하여 정확한 terminal journal만 소비하고 `ready/native`를 반환한 뒤에만 앱이 로컬
checkpoint를 삭제·readback하고 exact trusted Desktop을 다시 실행합니다. backend acknowledgement
후 로컬 삭제 전 앱이 종료되면 durable checkpoint가 같은 acknowledgement를 idempotent하게
재시도합니다. Relay Apply와 Recover는
남아 있거나 손상된 standalone journal을 모두 거부하므로 acknowledgement 중단이 split-brain
integration 상태를 만들 수 없습니다. commit 전 Homebrew guard crash는 자동 복원하고 commit 이후 불명확한 상태는 보호를
유지한 채 명시적 복구를 요구합니다. production/dev는 단일 system lock을 공유합니다. 공식
OpenCodex package와 data는 수정하지 않고 별도 `opencodex/` checkout도 사용하지 않습니다.

#### 중단 복구

- teardown·package child는 실행 전에 kind·attempt·boot session을 담은 typed active execution
  witness를 journal에 fsync합니다. witness가 남아 있는 동안 routing 변경,
  replay·package resume·finalization을 모두 거부합니다.
- child 시작 전 routing 거부와 cleanup이 검증된 뒤의 malformed receipt는 finite resolution
  marker를 먼저 fsync하고, 필요 시 routing을 durable하게 park한 다음에만 active witness를
  operation retry·package retry·data refresh phase로 해제합니다. 어느 경계에서 crash가 나도
  marker에서 같은 순서를 재개하며 routing park와 journal phase 전환이 모두 끝난 뒤에만
  `routing_recovery_persisted`를 발행합니다.
- 기존 integrated data-inventory/Trash journal은 schema 4로 변환하거나 standalone 제거로
  재해석하지 않습니다. fail-closed 상태로 유지하고 검토된 수동 복구를 요구합니다. 새 standalone
  inventory와 Trash witness는 schema 6 `standalone_native` journal 및 Native 경계에 고정됩니다.
- cleanup intent 이전의 검증된 실패는 durable removal recovery가 아닙니다. 정확히 allowlist된
  request·candidate·data-policy·teardown-preflight 순서이고 cleanup journal, child 시작,
  package/data/routing mutation, reboot, process-unknown 증거가 전혀 없는 receipt만 Swift의 임시
  `.inFlight` checkpoint를 지웁니다. exact trusted Desktop을 다시 실행하고 status를 갱신하되
  wizard에는 원래 bounded failure code를 유지합니다. post-intent 또는 불명확한 receipt는 모두
  기존 fail-closed recovery에 남습니다.
- `process_cleanup_unverified`는 **Mac 전체 재시작**이 필요합니다. PID 확인, 대기, MenuBar/Relay
  재시작 또는 Codex relaunch는 증거가 아닙니다. helper가 변경된 platform boot session을
  증명한 뒤 exact package/launcher 부재를 다시 확인해야 합니다.
- changed-boot reconciliation은 kind별로 다릅니다. teardown은 active witness를 기존
  `operation_intent`로 되돌리고 routing을 park한 뒤 teardown을 replay하지 않습니다. package는
  기존 exact-absence 또는 residual-pending 분기를 유지합니다. 이후 lifecycle action에는 새
  routing recovery와 review가 필요합니다.
- `routing_recovery_required` 또는 package 실행이 불명확한 동안 watcher/controller admission은
  fail-closed입니다. saved reboot/in-flight recovery도 narrow recovery predicate와 검증된
  durable generation만 사용할 수 있으며 relay/local routing sync가 모두 unreachable인 정확한
  projection에서도 이 saved predicate만 사용할 수 있습니다. 일반 uninstall predicate는 완화하지
  않습니다. gated `generation=0`은 opaque fail-closed 상태 표시만 가능하고 recovery,
  removal, routing mutation을 승인하지 않습니다. 저장된 journal-backed routing recovery는 저장한
  generation과 일치해야 합니다. reboot/in-flight reconciliation과 달리 routing action 자체는
  healthy relay와 actionable Complete capability를 요구합니다. helper는 routing mutation 전에
  저장된 opaque installation ID/fingerprint·검토한 generation·검증된 routing-transaction
  witness를 함께 고정합니다. exact journal gate는 controller recovery 동안에도 유지되며 새
  stable state·acknowledged live relay health·exact Codex ownership을 다시 검증한 뒤에만
  해제하므로 recovery 실패나 중단 뒤에도 재시도할 수 있습니다. 더 높은 안전한 generation을
  관측하면 동일 opaque selector와 함께 checkpoint만 하고 현재 review를 무효화하며, recovery
  또는 phase continuation 전에 두 번째 명시적 action이 필요합니다. journal을 삭제·수정하거나
  다른 lifecycle mutation을 실행하지 말고 저장된 wizard session을 다시 열어 routing recovery와
  새 generation 검토를 완료합니다.

사용자에게 저장되는 receipt와 versioned recovery session은 opaque ID, mode, adapter ID,
component 상태, finite code만 포함하며 absolute path, module hash, child stdout/stderr, raw error,
credential, data 또는 live log를 저장하지 않습니다. 이는 helper 내부 owner-only durable cleanup
journal까지 path-free라는 뜻은 아닙니다. 재부팅 복구는 disposable 설치에서 별도 acceptance가
필요합니다.

## 일반 Linux client 설치

credential file을 editor로 입력하고 literal 값을 command line이나 history에 남기지
않습니다.

```bash
install -d -m 0700 ~/.config/opencodex-relay
umask 077
${EDITOR:-vi} ~/.config/opencodex-relay/credentials.env
chmod 0600 ~/.config/opencodex-relay/credentials.env
```

그 다음 macOS와 같은 signed installer 명령을 실행합니다. target은 `uname` 결과에 따라
`linux/amd64` 또는 `linux/arm64`로 자동 선택됩니다.

```bash
systemctl --user status opencodex-relay.service
journalctl --user -u opencodex-relay.service --since today --no-pager
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
```

user service를 SSH logout 이후에도 유지해야 하는 Remote host에서는 운영 승인 후
`sudo loginctl enable-linger ubuntu`를 한 번 적용합니다.

## Linux Remote Control 전용 home

Remote host는 일반 `~/.codex` 대신 `/home/ubuntu/.codex-remote-opencodex`를 사용합니다.
먼저 [`../pilot/scripts/install-remote-codex-home.sh`](../pilot/scripts/install-remote-codex-home.sh)로
Remote manager·wrapper·timer를 설치하고, Ubuntu 24.04 sandbox는 다음 narrow AppArmor
경로로 검증합니다.

```bash
sudo ./pilot/scripts/configure-codex-linux-sandbox.sh --user ubuntu
```

전역 `kernel.apparmor_restrict_unprivileged_userns=0`은 사용하지 않습니다.

legacy managed Remote baseline은 bare `gpt-5.6-luna`입니다. local-relay에서 manager는
`opencode-go-responses/gpt-5.6-luna` 같은 bounded-policy root와 대소문자 무시 exact
일치하거나, materialized catalog에서 byte-exact로 한 번 나타나는 root를 허용합니다.
전자는 `bounded_json`, 후자는 `passthrough`로 보고하며 두 선택을 모두 보존합니다.
Cursor compatibility adapter 제거로
과거 40개 snapshot이 역사적 26개 snapshot으로 바뀌었고, Relay `0.2.1` acceptance
시점에는 두 Remote에서 27개를 확인했습니다. Catalog 수는 고정 계약이 아니므로 현재
upstream-visible set과 현재 reader 결과를 비교해야 합니다. 이는 Remote-local filter가
아니라 중앙 `Model_Catalog` 변경입니다. Policy/catalog가 잘못됐거나 root가
누락·중복·비정상·미등재이면 config write나 daemon restart 전에 실패합니다. 전용 Remote
config만 다음과 같이 보정·검증하며, `status`는
`default_model_relay_mode=bounded_json|passthrough`도 출력합니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  set-default-model --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-default-model
```

relay 설치 전에 Remote 역할을 분류합니다. External enrollment 경로는
`"mode": "external"`에서만 사용할 수 있습니다. `"mode": "loopback"`은 그 경로나 edge
credential을 사용하지 않습니다. 현재 central x86 Remote는 `local_opencodex` 기반으로
edge credential을 주입하지 않고 일반 Native Codex Authorization을 보존하는 loopback
relay service인 `local-relay`를 사용합니다. Bare catalog-visible model과 qualified
bounded-policy model은 모두 유효한 선택입니다. Bare passthrough는 Relay 정규화만
생략하며, Relay를 우회해 upstream service로 가지 않고 계속 loopback OpenCodex로
전달됩니다.

```bash
jq -e '.mode == "external"' ~/.config/opencodex-relay/remote-opencodex.json >/dev/null || {
  printf '%s\n' 'loopback Remote의 external relay enrollment를 거부합니다.' >&2
  exit 2
}
```

`install-remote-codex-relay.sh install`도 artifact를 가져오기 전에 external 조건을
fail-closed로 반복 확인합니다. 명시적인 `install-local` action이 loopback case를
소유합니다. `linux/amd64`/`linux/arm64`는 binary 선택 기준일 뿐 topology를 결정하지
않습니다.

relay 설치 시 catalog와 Codex executable을 전용 home으로 지정하고 AppServer restart는
Remote manager에 맡깁니다.

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --config /home/ubuntu/.config/opencodex-relay/relay.json \
  --codex-config /home/ubuntu/.codex-remote-opencodex/config.toml \
  --catalog-path /home/ubuntu/.codex-remote-opencodex/opencodex-catalog.json \
  --codex-executable /home/ubuntu/.codex-remote-opencodex/packages/standalone/current/codex \
  --manage-app-server false
```

public GitHub Release 운영에서는 Remote home automation을 한 번 갱신한 뒤, 아래 wrapper를
사용합니다. wrapper는 release installer/service bootstrap을 함께 배치하지만 GitHub token,
public PEM, `credentials.env` 및 `auth.json`의 내용을 복사하지 않습니다.

```bash
cd /path/to/OpenCodex-OCI-Gateway/pilot/scripts
./install-remote-codex-home.sh install --with-relay-bootstrap

~/.local/lib/opencodex-relay/install-remote-codex-relay.sh install 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --public-key ~/.config/opencodex-relay/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --allow-remote-interruption
```

전환 대상이 `pw_opencodex`, `opencodex`, `pw_opencodex_remote` 또는 exact old loopback
(`127.0.0.1:10100`/`localhost:10100`)라면 마지막 줄에 `--migrate-legacy`를 추가합니다.
다른 custom provider나 user-owned root `openai_base_url`은 자동으로 추측·삭제하지 않고
실패하므로, 해당 backup과 config를 먼저 검토해야 합니다.

legacy provider migration은 relay installer가 native routing을 enable하기 **전에** 수행해야
합니다. 따라서 raw installer 경로에서는 위 명령에 `--migrate-legacy`를 붙이고, daemon
restart만 담당하는 routing command에는 다시 붙이지 않습니다. 이 작업은 Remote 작업을 끊을
수 있으므로 maintenance window에서 명시적으로 승인합니다.

```bash
~/.local/lib/opencodex-relay/configure-remote-codex-routing.sh \
  enable-relay --allow-remote-interruption

~/.local/lib/opencodex-relay/configure-remote-codex-routing.sh status
~/.local/lib/opencodex-relay/update-remote-codex.sh status
codex debug models | jq '.models | length'
```

Schema v1 `remote-opencodex.json`은 `routing_mode`를 필수로 요구하며 누락된 파일을 거부합니다.
전환 스크립트는 native config가 준비된 뒤에만 `"routing_mode": "relay"`를 기록하고 managed
daemon을 재시작합니다. relay mode wrapper는 세 admission variable을 unset한 뒤 Codex를
실행하므로 credential이 native Codex process로 전달되지 않습니다.

## 설정 참조

external normalizer config 예시는 다음과 같습니다. 실제 credential 값은 포함하지 않습니다.

```json
{
  "listen_address": "127.0.0.1:18180",
  "upstream_mode": "external_gateway",
  "upstream_base_url": "https://REPLACE_WITH_API_HOSTNAME/v1",
  "voice_enabled": false,
  "connection_probe": {
    "enabled": true
  },
  "credentials": {
    "source": "keychain",
    "file": "/path/to/user/.config/opencodex-relay/credentials.env"
  },
  "responses": {
    "websocket_mode": "http_fallback",
    "model_modes": {
      "opencode-go-responses/gpt-5.6-luna": "bounded_json"
    },
    "scheduler": {
      "interactive_listen_address": "127.0.0.1:18182",
      "max_classifications": 8,
      "max_pending_requests": 24,
      "max_pending_encoded_bytes": 536870912,
      "queue_timeout_ms": 60000,
      "max_general_upstream": 4,
      "interactive_reserved_upstream": 1,
      "max_concurrent_transforms": 2,
      "max_open_deliveries": 16
    }
  },
  "catalog": {
    "owner": "relay",
    "path": "/path/to/user/.codex/opencodex-relay-catalog.json",
    "refresh_interval": "10m",
    "manage_app_server": false,
    "codex_executable": "codex"
  }
}
```

| 필드 | 계약 |
| --- | --- |
| `listen_address` | `127.0.0.1` 또는 `::1`만 허용 |
| `upstream_mode` | 생략 시 `external_gateway`; local은 명시적 `local_opencodex` |
| `upstream_base_url` | external은 absolute HTTPS `/v1`; local은 exact numeric loopback `10100/v1`만 허용 |
| `voice_enabled` | local Voice route opt-in, 기본 `false` |
| `connection_probe.enabled` | macOS MenuBar installer가 external gateway에서만 켜는 10분 low-frequency reachability observation; native/recovery에서는 항상 중단 |
| `credentials.source` | external은 macOS `keychain`/Linux `file`; local은 `none` |
| `responses.websocket_mode` | 기본 passthrough; model normalizer가 있으면 `http_fallback` 필수 |
| `responses.model_modes` | 공백 없는 case-insensitive exact model key와 `bounded_json` 값만 허용 |
| `responses.scheduler.*` | 위 예시 값이 기본; interactive listener는 일반 listener와 달라야 하며 모든 수치는 validator의 bounded range 안이어야 함 |
| `catalog.owner` | external은 `relay`, local은 `remote_manager` |
| `catalog.refresh_interval` | 최소 1분, 기본 10분 |
| `catalog.manage_app_server` | 자동 AppServer restart opt-in, 기본 `false`; Remote home은 반드시 `false` |
| `catalog.app_server_home` | Linux opt-in 시 exact absolute `CODEX_HOME`; 없거나 검증 불가하면 restart하지 않음 |
| `catalog.codex_executable` | catalog query의 `client_version`을 읽을 Codex binary |

`relayctl init --force`는 기존 relay JSON 전체를 교체하므로 단순 update 명령이 아닙니다.
upstream 또는 catalog 경로를 변경할 때만 기존 설정을 backup·검토한 뒤 명시적으로
사용합니다.

local mode의 핵심 필드는 다음과 같습니다. 실제 파일에서는 listen, catalog path/interval,
Codex executable 및 AppServer 관리 필드를 생략하지 않습니다.

```json
{
  "upstream_mode": "local_opencodex",
  "upstream_base_url": "http://127.0.0.1:10100/v1",
  "credentials": { "source": "none" },
  "responses": {
    "websocket_mode": "http_fallback",
    "model_modes": {
      "opencode-go-responses/gpt-5.6-luna": "bounded_json"
    }
  },
  "catalog": { "owner": "remote_manager" }
}
```

bounded path는 identity/zstd 요청에서 top-level `stream:true`만 변경하고 upstream을 한 번만
호출한 뒤 검증된 terminal JSON을 HTTP/SSE로 합성합니다. 내장 `opencode-go`의 Chat
Completions mapping은 바꾸지 않습니다. local normalizer 활성화 전에 Remote routing
helper가 service 계정으로 effective colocated OpenCodex config를 검증하고
`images.videoBridgeEnabled=false` 또는 미설정을 요구합니다. Service의 Node/CLI 경로를
직접 사용하지 않고 root-owned Runtime Adapter의 `describe --json`을 passwordless
`sudo`로 실행하여 exact config source를 구한 뒤, `OPENCODEX_HOME`과 `CODEX_HOME`을
해제하고 adapter의
`ocx config validate/show --json`을 `opencodex` 계정으로 실행합니다. Canonical Node,
entry 및 service HOME은 adapter 계약이 선택합니다. Redacted config는 파일에 쓰지 않은 채
`jq`로 전달하며 adapter, contract, description, source 또는 service-account 검증이
불가능하면 routing 변경 전에 fail-closed합니다. 이 server-side 기능은 요청만으로 판별할
수 없습니다.

### Dual listener와 명시적 interactive profile

설치된 relay 한 process는 일반 listener와 서로 다른 numeric-loopback interactive listener를
함께 가집니다. 일반 주소는 `listen_address`(통상 `127.0.0.1:18180`)이고, interactive 주소는
`responses.scheduler.interactive_listen_address`입니다. 미설정이면 일반 listener 주소 계열의
port `18182`를 기본값으로 사용합니다. relay validator를 통과하고 일반 listener와 다르면 다른
numeric-loopback port도 사용할 수 있습니다.

일반 Codex config는 보통 TUI, `exec`, `review`, AppServer 및 daemon 작업에서 계속 권위가
있습니다. installer는 명시적으로 선택하는 side session을 위해
`$CODEX_HOME/opencodex-relay-interactive.config.toml`도 atomic하게 관리합니다.

```toml
# opencodex-relay-managed-interactive-profile-v1
openai_base_url = "http://127.0.0.1:18182/v1"
model_catalog_json = "/ABSOLUTE/CODEX_HOME/opencodex-relay-catalog.json"
```

marker는 ownership metadata일 뿐 추가 설정이 아닙니다. profile에는 model, reasoning 옵션,
agent 제한 또는 숨은 fallback이 없습니다. 정확한 marker가 없는 동일 이름 file이 존재하면
installer와 Remote activation은 덮어쓰기 전에 실패합니다. 선택은 자동이 아닙니다.

```bash
codex --profile opencodex-relay-interactive
codex exec --profile opencodex-relay-interactive 'REPLACE_WITH_PROMPT'
```

service swap 전 installer는 현재 active managed relay가 두 health contract에 모두 응답할 때만
점유된 interactive port를 허용합니다. 그 밖에는 설정한 port가 비어 있어야 합니다. process를
종료하지 않으며 bind race는 activation 실패와 transaction rollback으로 닫습니다.

Linux에서 자동 restart를 명시적으로 허용하려면 두 값이 모두 필요합니다. 대상 AppServer
environment의 `CODEX_HOME`이 `app_server_home`과 정확히 같아야 하며, command line만으로는
허용하지 않습니다. macOS에는 이 구현에서 동등하게 검증된 identity source가 없으므로 marker를
유지하고 CLI/Desktop을 수동 restart합니다.

```json
"manage_app_server": true,
"app_server_home": "/home/REPLACE_WITH_USER/.codex"
```

## Catalog lifecycle과 AppServer activation

relay는 시작 시 한 번, 이후 `refresh_interval`마다 다음 순서를 수행합니다.

1. 지정 Codex binary의 `--version`에서 explicit semver를 읽습니다.
2. authenticated `GET /v1/models?client_version=...`를 최대 8 MiB로 제한해 가져옵니다.
3. `.models` 또는 `.data` array를 받아 `visibility: "hide"`를 제거합니다.
4. 비어 있는 catalog, model ID 누락, visible ID 중복, 복수 JSON value를 거부합니다.
5. 기존 파일과 다를 때만 `0600` temp file을 rename하고 `.previous`와
   `.restart-pending`을 남깁니다.
6. 명시 Linux opt-in이 있을 때만 nonblocking quiescence gate로 기존 active request와 신규
   admission을 함께 배제하고, environment가 exact `CODEX_HOME`을 증명한 Codex AppServer
   process에만 restart 신호를 보냅니다. identity가 없거나 다르거나 확인 불가하거나 gate가
   busy이면 후보 process 어느 것도 restart하지 않고 marker를 유지해 다음 activation tick에서
   다시 시도합니다.

external Remote home의 `manage_app_server=false`는 의도된 설정입니다. relay가 유일한 catalog
writer이고 `opencodex-remote-relay-catalog-activation.timer`가 매분 relay의
`.restart-pending` marker를 확인한 뒤 `active_requests == 0` health snapshot일 때 Remote
manager를 호출합니다. resident activation과 달리 이 cross-process 확인은 relay admission gate를
계속 보유하지 않으므로, 새 요청도 배제해야 하는 엄격한 activation에는 maintenance window가
필요합니다. 기존 10분 manager timer는 legacy/loopback fetch 동작을 유지하며 relay Remote에서는
비활성화됩니다. relay mode에서 `refresh --restart`를 수동 호출해도
`relay_catalog_refresh=owned_by_relay`만 출력하므로 전용 activation timer가 유일한 marker
activator로 남습니다. 같은 Remote home에 또 다른 catalog normalizer나 activator를 추가하지
않습니다.

`"routing_mode": "local-relay"`에서는 relay의 `catalog.owner=remote_manager`가 relay 자체 catalog
lifecycle을 완전히 끕니다. 기존 Remote manager가 colocated `10100` catalog를 계속 가져와
유일한 writer와 marker activator로 남고, Native Responses data traffic만 `18180`으로
이동합니다.

resident local activation에서 “idle”은 **restart가 끝날 때까지 기존 request와 신규
admission이 quiescence gate로 배제됨**을 뜻합니다. 열린 Desktop window나 논리적으로 진행
중인 사용자 session이 없다는 뜻은 아닙니다. 자동 restart는 위 Linux home identity opt-in이
없으면 꺼져 있으며, macOS에서는 항상 명시 CLI/Desktop restart로 catalog를 반영합니다. 열린
CLI TUI 자체는 relay가 재시작하지 않습니다.

Remote home에서는 relay와 기존 manager가 서로 다른 marker 이름을 사용할 수 있어 manager가
둘 다 인식합니다. timer는 sampled health에서 보이는 active request의 재시작을 피하지만,
그 직후 admission된 요청은 여전히 끊길 수 있습니다. 이 cross-process race를 허용할 수 없다면
maintenance window에서 적용해야 합니다.

## Voice double opt-in

Voice/Realtime은 기본적으로 양쪽에서 닫혀 있습니다.

1. local `relay.json`의 `voice_enabled`를 `true`로 변경합니다.
2. local service를 재시작합니다.
3. 중앙 OCI host에서 feature gate를 켭니다.

```bash
# macOS
launchctl kickstart -k "gui/$(id -u)/io.github.novelkr.opencodex-relay"

# Linux
systemctl --user restart opencodex-relay.service

# Central gateway
sudo ./pilot/scripts/configure-gateway-features.sh voice on
sudo ./pilot/scripts/smoke-test.sh
```

local `false` 또는 central `off` 중 하나라도 남아 있으면 Voice route는 사용할 수 없습니다.
긴급 차단은 central에서 `voice off`를 먼저 적용한 뒤 local 설정을 `false`로 되돌립니다.
HTTP setup 성공과 WebSocket `101`만으로 audio/WebRTC media path가 검증되지는 않습니다.
실제 Codex Desktop/CLI Voice call의 연결·양방향 audio·종료·재연결을 별도 기록해야 합니다.

## 운영 상태와 진단

### 공통 상태

```bash
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
curl --fail --silent http://127.0.0.1:18180/__relay/healthz
curl --fail --silent http://127.0.0.1:18182/__relay/healthz
```

일반 endpoint는 `listener_lane=general`, interactive endpoint는
`listener_lane=interactive`를 보고해야 합니다. 둘 다 두 listener 주소, 설정 scheduler limit,
공유 `active_requests`, 비음수 classification/queue/upstream/transform/delivery/rejection counter를
반환하며 credential 값은 노출하지 않습니다.

| 증상 | 우선 확인 | 의미·조치 |
| --- | --- | --- |
| `relay_running=0` | launchd/systemd 상태와 relay error log | service 미기동, config 오류 또는 port 충돌 |
| `credential_unavailable` | Keychain item 이름·account 또는 Linux file owner/mode | 세 credential 중 하나라도 없거나 unsafe |
| 중앙 `401` | Cloudflare Service Token policy와 gateway key rotation | Access 거부와 Nginx key 거부를 구분 |
| 기본 모델만 표시 | resident catalog lifecycle/log, catalog path, `codex debug models` | catalog 미갱신 또는 Desktop/CLI가 다른 Codex home 사용; revision 4의 `relayctl catalog refresh`는 보고 전용 |
| catalog `.restart-pending` marker가 남음 | resident lifecycle이 변경한 catalog 또는 active/unidentified AppServer | 같은 home의 CLI/Desktop을 수동 restart하거나 명시 Linux identity opt-in을 사용; broad process kill 금지 |
| Desktop 목록 불변 | Desktop 완전 종료 후 동일 Codex home으로 재실행 | picker는 startup-loaded일 수 있음 |
| Remote UI offline | daemon status, `app-server proxy` handshake, Remote 등록 상태 | SSH 성공과 Remote Control online은 별도 lifecycle |
| 전용 config와 실제 선택 model/reasoning이 다름 | `home_project_trust`, 일반 `~/.codex/config.toml` | SSH home directory가 trusted project overlay인지 확인 후 `isolate-home-project-config` |
| `base URL is overridden` model picker 경고 | `openai_base_url`, authenticated catalog, 실제 Responses | proxy/base URL 경로에서는 예상 가능; 경고 제거를 위해 OpenCodex routing을 우회하지 않음 |
| legacy loopback `responses_websocket`의 `426` log | 이어지는 HTTP/SSE Responses 결과 | HTTP fallback이 성공하면 model/Remote Control 장애 증거는 아님; native Responses WebSocket 지원은 relay 배포 뒤 별도 검증 |
| catalog에 보이나 model 요청 거부 | 같은 model의 실제 Responses error와 account 선택 | `visibility: "hide"`가 아닌 model은 유지하고, 해당 model을 지원하는 account를 선택 |
| Voice `404` | local JSON과 central feature flag | double opt-in 중 적어도 하나가 off |
| `bubblewrap` warning | narrow AppArmor setup과 `bwrap --unshare-user` probe | 전역 user namespace 제한을 끄지 않음 |

macOS log는 `~/Library/Logs/opencodex-relay/relay.log`과 `relay-error.log`, Linux log는
`journalctl --user -u opencodex-relay.service`에서 확인합니다. relay는 method, path와
오류 종류만 기록하며 credential 값이나 response body를 기록하지 않습니다.

## Update와 rollback

같은 upstream의 새 signed version은 installer를 다시 실행합니다. relay JSON과 catalog는
보존되며 새 binary 검증 후 `current`가 바뀝니다. Codex/OpenCodex 전체 release 순서는
[`updates.ko.md`](updates.ko.md)를 따릅니다.

local uninstall은 먼저 native intent를 기록합니다. relay route가 active이면 선택한 Desktop이
종료된 뒤 `--confirm-desktop-exited`를 붙여 다시 실행하거나 MenuBar app에서 전환을 끝낼
때까지 service를 유지합니다. native apply가 성공한 뒤에만 service, managed login item,
relay-owned Codex block, marker-owned interactive profile을 제거하며 relay JSON, catalog와
version directory는 보존합니다.

```bash
./client/relay/scripts/install-relay.sh uninstall \
  --codex-config ~/.codex/config.toml \
  --confirm-desktop-exited
```

`--migrate-legacy`를 사용했다면 출력된 timestamp backup을 먼저 검사합니다. 이전 provider로
돌아가려면 현재 config를 별도 보존한 뒤 그 backup을 `0600`으로 복원해야 합니다. 자동
rollback은 임의 custom provider를 추측해 복원하지 않습니다.

Remote home rollback은 완전 자동화되어 있지 않습니다. maintenance window에서 다음 상태를
모두 일치시켜야 합니다.

1. `config.toml.pre-opencodex-relay-*` 중 전환 시 생성된 정확한 backup을 식별·검사합니다.
2. Remote `config.toml`을 그 backup으로 복원합니다.
3. `/home/ubuntu/.config/opencodex-relay/remote-opencodex.json`의
   `"routing_mode": "legacy"`를 owner-only editor로 되돌립니다.
4. `manage-remote-codex-home.sh restart-daemon`으로 legacy provider·credential 경계를
   확인합니다.
5. relay installer의 `uninstall --codex-config
   /home/ubuntu/.codex-remote-opencodex/config.toml`을 실행합니다.
6. `codex debug models`, daemon version과 Remote Control 연결을 다시 검증합니다.

backup 식별이 불확실하거나 legacy credential이 회수된 상태에서는 rollback하지 않고
relay 복구를 우선합니다.

## 검증 계층과 완료 조건

저장소 정적 검증:

```bash
bash -n pilot/scripts/*.sh ops/oci/*.sh client/relay/scripts/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test -count=1 ./... && go vet ./...)
(cd client/relay && go test -race -count=1 ./internal/handoff ./internal/routing)
(cd client/relay/macos/OpenCodexRelay && swift test)
git diff --check
```

### Codex AppShot workspace acceptance

Codex가 workspace 작업을 열 때 writable-root profile을 만들기 때문에 AppShot 검증은
작업별로 수행합니다. 부모가 심볼릭 링크인 writable alias를 유지한 작업은 그 alias가 신뢰한
물리 checkout과 같은 inode를 가리켜도 명령이나 화면 캡처 도구가 시작되기 전에 실패할 수
있습니다. 해결을 위해 파일시스템 symlink를 삭제하지 않습니다.

이 macOS checkout에서는 project trust를
`/path/to/OpenCodex-OCI-Gateway`에 두고, Codex 앱에서
오래된 workspace 별칭만 제거한 뒤 이 물리 경로를 다시 열고 Codex를 재시작하여 짧은 새
검증 작업을 만듭니다. 다음 canary를 서로 독립적으로 기록합니다.

1. sandbox 명령이 symlink writable-root 오류 없이 실행됩니다.
2. Relay Control Center가 공유 가능한 창으로 탐색됩니다.
3. 새 AppShot을 첨부하고 작업 안에서 실제 이미지로 렌더링합니다.

기존 작업은 증거로 유지하지만 새 permission profile의 통과 증거가 될 수 없습니다. 1단계는
통과하고 3단계가 실패하면 제한된 capture 오류를 기록하고 Codex의 화면 기록·손쉬운 사용
권한을 확인합니다. Relay 창이 이미 sharing enabled라면 window-sharing 코드는 바꾸지
않습니다.

`opencodex/`는 outer clone이 vendor·publish·pin하지 않는 별도 upstream checkout입니다. 이 변경의
local source 검증은 `https://github.com/lidge-jun/opencodex.git`의
[`d9de89557c3bd154e5f1508125def7c8789ac8c5`](https://github.com/lidge-jun/opencodex/commit/d9de89557c3bd154e5f1508125def7c8789ac8c5)
baseline과 별도로 검토한 nested working-tree diff가 모두 있을 때만 다음 명령을 실행합니다. clean
outer clone에 이 directory나 변경이 있다고 가정하지 말고, 배포에는 이 source tree가 아니라
검토한 publish package version을 사용합니다.

```bash
git -C opencodex rev-parse HEAD
(cd opencodex && bun run typecheck && bun run test && bun run privacy:scan)
git -C opencodex diff --check
```

정적 검증은 source·route·config transform·signature/catalog 처리와 script 문법을
검사하지만 실제 서비스 가용성을 증명하지 않습니다. 배포 완료 판정에는 다음 증거가 모두
필요합니다.

1. 중앙 host `nginx -t`, `smoke-test.sh`, 외부 Cloudflare/SSE smoke 통과
2. 각 client의 relay health, signed installed version, credential lookup 통과
3. visible model catalog 수와 `codex debug models` 일치
4. 일반 Codex CLI Responses/tool 호출 성공
5. Codex Desktop에서 동일 project/session 경로와 model picker 확인
6. 각 `"mode": "external"` Linux Remote host에서 daemon running, WebSocket proxy `101`, Remote UI online 확인
7. Images 또는 Voice를 enable했다면 각 기능의 별도 live acceptance 통과

macOS 탐색·제거 변경은 [`testing.ko.md`](testing.ko.md)의 정적 검증에 더해 다음 disposable
acceptance를 별도로 남깁니다. live credential이나 실제 사용자 데이터를 사용하지 않습니다.

1. 빈/malformed Codex bundle·Team metadata 및 서명 변경이 discovery, 저장, quit/relaunch,
   routing, 제거를 모두 차단하는지 확인합니다.
2. 정확히 하나의 검토된 identity profile과 adapter에 맞는 schema 4 Tier A/B 후보만 자동 제거
   대상이고, schema 2·3, Tier C, Volta, root/elevation, modified, manual 후보는 manual-only인지
   확인합니다.
3. 성공 fixture에서 모든 OpenCodex data root가 byte 단위로 보존되고 strict teardown receipt를
   통과하기 전에는 npm이 시작되지 않는지 확인합니다.
4. generation 변경, receipt unknown key/stage mismatch, package/routing/relay terminal proof 누락이
   성공 처리·relaunch를 차단하는지 확인합니다.
5. injected teardown/package interruption이 bounded recovery로 전환되고, 실제 Mac 재시작 뒤
   boot-session-attested process recovery가 통과하며, routing recovery 중 admission이 닫히는지
   확인합니다.

reviewed Codex identity는 production plist에 반영됐지만, 위 acceptance를 수행·기록하기 전에는
구현·정적 테스트 완료를 production readiness로 기록하지 않습니다.

과거 세션 `019fd7cc-0f03-76c3-9da1-4d36a7bf85a7`은 x86 Linux와 ARM Linux Remote
host, dedicated Codex home과 bubblewrap/AppArmor 조건을 설계 입력으로 제공했습니다. 그
세션의 성공·실패 화면은 현재 server health나 이 구현의 live 배포 증거가 아닙니다.

## 공식 Codex 근거

다음 공개 문서를 2026-08-07 기준으로 확인했습니다. 이 저장소의 relay와 중앙 gateway는
자체 구현이므로, 공식 문서는 Codex 설정·process·product surface의 경계만 뒷받침합니다.

- [Codex advanced configuration](https://learn.chatgpt.com/docs/config-file/config-advanced)
- [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
- [Codex CLI command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
- [Codex Remote connections](https://learn.chatgpt.com/docs/remote-connections)
- [Image generation](https://learn.chatgpt.com/docs/image-generation)
- [ChatGPT Voice](https://learn.chatgpt.com/docs/features/voice)

## 구현 파일 지도

| 책임 | 경로 |
| --- | --- |
| relay daemon | [`../client/relay/cmd/opencodex-relay/`](../client/relay/cmd/opencodex-relay/) |
| control/migration CLI | [`../client/relay/cmd/opencodex-relayctl/`](../client/relay/cmd/opencodex-relayctl/) |
| OpenCodex Tier discovery/execution closure | [`../client/relay/internal/handoff/npm_discovery.go`](../client/relay/internal/handoff/npm_discovery.go), [`execution_closure.go`](../client/relay/internal/handoff/execution_closure.go) |
| removal coordinator/runner/journal | [`../client/relay/internal/handoff/removal.go`](../client/relay/internal/handoff/removal.go), [`npm_runner.go`](../client/relay/internal/handoff/npm_runner.go), [`removal_journal.go`](../client/relay/internal/handoff/removal_journal.go) |
| Codex Desktop signature trust | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/CodexDesktopTrust.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/CodexDesktopTrust.swift) |
| Swift removal protocol/receipt verifier | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayCore/OpenCodexRemoval.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayCore/OpenCodexRemoval.swift) |
| removal flow/wizard | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalFlow.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalFlow.swift), [`OpenCodexRemovalWizardView.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalWizardView.swift) |
| Korean-first localization | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayLocalization/`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayLocalization/) |
| Relay 소유 package identity/teardown adapter | [`../client/relay/internal/handoff/teardown_compatibility.go`](../client/relay/internal/handoff/teardown_compatibility.go), [`../client/relay/internal/handoff/adapter/relay_preserve_v1.ts`](../client/relay/internal/handoff/adapter/relay_preserve_v1.ts) |
| exact route 계약 | [`../client/relay/internal/compat/routes.go`](../client/relay/internal/compat/routes.go) |
| credential resolver | [`../client/relay/internal/credentials/`](../client/relay/internal/credentials/) |
| catalog lifecycle | [`../client/relay/internal/catalog/`](../client/relay/internal/catalog/) |
| Codex config marker/migration | [`../client/relay/internal/codexconfig/`](../client/relay/internal/codexconfig/) |
| signed release | [`../client/relay/internal/release/`](../client/relay/internal/release/) |
| platform installer | [`../client/relay/scripts/install-relay.sh`](../client/relay/scripts/install-relay.sh) |
| central allowlist | [`../pilot/nginx/opencodex-api.conf`](../pilot/nginx/opencodex-api.conf) |
| central Voice gate | [`../pilot/scripts/configure-gateway-features.sh`](../pilot/scripts/configure-gateway-features.sh) |
| Remote mode migration | [`../pilot/scripts/configure-remote-codex-routing.sh`](../pilot/scripts/configure-remote-codex-routing.sh) |
| Remote catalog/daemon manager | [`../pilot/scripts/manage-remote-codex-home.sh`](../pilot/scripts/manage-remote-codex-home.sh) |
