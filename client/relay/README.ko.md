# OpenCodex native 호환 relay

이 디렉터리는 같은 host의 OpenCodex 또는 원격 OpenCodex에 native Codex CLI, 로컬
AppServer, Codex Desktop 및 Linux Remote Control host를 연결하는 device-local relay를
구현합니다. 경로는 CPU나 접속 가능성으로 추측하지 않고 시작 시 고정하며, 실패한 모델
요청을 다른 upstream으로 재전송하지 않습니다. 설치·migration,
credential, catalog, Voice, rollback과 live 완료 판정의 정본은
[`../../docs/local-codex-relay.ko.md`](../../docs/local-codex-relay.ko.md)입니다. 영문
구성요소 문서는 [`README.md`](README.md)를 참조합니다.

```text
local_opencodex:  Native Codex -> 127.0.0.1:18180 relay -> 127.0.0.1:10100 OpenCodex
external_gateway: Native Codex -> 127.0.0.1:18180 relay -> Cloudflare/Nginx -> OpenCodex
```

## 사전 빌드 macOS 앱으로 셀프 호스팅 서버 연결

내려받은 `OpenCodexRelay.app.zip`을 사용하는 일반 사용자는 소스 저장소, 패키지 버전,
출력 경로 또는 manifest 서명 키를 입력하지 않습니다.

1. 다운로드 출처와 게시된 SHA-256을 확인하고 압축을 푼 뒤 실제 앱을 엽니다. 앱이 응용
   프로그램 폴더 밖에 있으면 Settings에서 **응용 프로그램으로 이동**만 활성화됩니다.
   먼저 `/Applications/OpenCodexRelay.app`을 시도하고 쓰기 권한이 없을 때 사용자 확인 후
   `~/Applications/OpenCodexRelay.app`을 사용합니다. 검증된 새 앱이 시작된 뒤 정확한 이전
   프로세스의 종료와 교체 대상 backup 정리까지 확인해야 셀프 호스팅 설정이 활성화됩니다.
   원본 유지 또는 변경되지 않은 원본의 휴지통 이동은 그 뒤 독립적으로 선택합니다.
2. Finder에서 새 위치의 앱을 엽니다. macOS가 차단하면 **시스템 설정 → 개인정보 보호 및 보안 →
   확인 없이 열기**를 선택하고 다시 **열기**를 확인합니다. quarantine 속성을 삭제하거나
   Gatekeeper를 비활성화하지 않습니다.
3. **Control Center → 설정 → 셀프 호스팅 서버 연결**에서 서버 주소, 인증 방식, 선택한
   방식에 필요한 자격 증명만 입력합니다. HTTPS는 origin 또는 `/v1`을 받습니다. 사설 LAN
   HTTP는 RFC1918 IPv4 또는 IPv6 ULA IP literal만 허용합니다. Native Codex Authorization과
   요청 내용이 TLS로 보호되지 않으므로 모든 지원 인증 방식에서 평문 전송을 적용마다
   명시적으로 확인해야 합니다.
4. **Relay 준비**, **연결 테스트** 순서로 진행합니다. 준비 작업은 현재 사용자 소유의 설정,
   runtime, LaunchAgent, routing binding만 만들며 Terminal, `sudo`, `/Library` 또는 Codex
   `config.toml`을 건드리지 않습니다.
5. 연결 검증이 성공하면 **Codex를 이 서버로 전환**을 선택합니다. 알려진 legacy OpenCodex
   route는 `0600` 백업 후 전환 transaction 안에서 migration합니다. 알 수 없는 provider나
   사용자 override는 덮어쓰지 않고 외부 편집을 안내합니다.
6. 기존 OpenCodex 패키지 제거는 전환 뒤 선택 사항입니다. 데이터 정리는 다시 검증한 명시적
   선택 항목을 macOS 휴지통으로 옮기는 것만 의미하며 영구 삭제는 제공하지 않습니다.

인증 프로필은 **없음**, **Gateway API Key**, **Cloudflare Access + Gateway API Key** 세
가지입니다. 기존 Keychain 값은 설정됨/없음으로만 표시하고 노출·복사·로그 기록하지 않습니다.

메뉴바 아이콘의 왼쪽 클릭은 기존 간단 상태 popover를 유지합니다. 오른쪽 클릭 메뉴에는
**Control Center 열기…**, **새로 고침**, **로그인 항목 설정…**, **종료**만 두며 routing
전환·복구·제거는 Control Center의 명시적 흐름으로 유지합니다.

## 구성요소

| 경로 | 역할 |
| --- | --- |
| `cmd/opencodex-relay/` | exact route allowlist와 Responses normalizer; external mode에서만 credential 주입/catalog refresh 수행 |
| `cmd/opencodex-relayctl/` | 초기화, native config 전환, catalog refresh, 상태와 release 검증 CLI |
| `internal/` | config, credential, route, proxy, catalog, AppServer activation과 signed manifest 구현 |
| `scripts/bootstrap-keychain-signing-key.sh` + `keychain-signing-key.swift` | macOS release key의 fail-closed bootstrap 및 raw Keychain readback |
| `scripts/build-release.sh` | Hardened Runtime을 적용한 macOS 26+ Apple Silicon ad-hoc MenuBar app bundle 및 `linux/amd64`·`linux/arm64` helper artifact와 signed manifest 생성 |
| `scripts/install-relay.sh` | signature/checksum 검증 후 version 설치·migration·uninstall |
| `scripts/publish-github-release.sh` | 검증된 artifact bundle을 public immutable GitHub Release로 draft·publish |
| `macos/`, `systemd/` | credential을 포함하지 않는 launchd/user-systemd template |

`relayctl catalog refresh`는 relay-owned catalog를 fetch/write/activate하지 않습니다. resident
lifecycle이 유일한 writer임을 보고하며, `remote_manager` catalog도 해당 manager의 ownership만
보고합니다. 별도 CLI가 resident request 수를 추측하지 않으며, local AppServer activation은
resident relay의 nonblocking quiescence gate가 active request와 신규 admission을 모두 배제했을
때만 수행합니다.

자동 AppServer restart는 기본으로 비활성화됩니다. Linux에서만 `catalog.manage_app_server:
true`와 exact absolute `catalog.app_server_home`을 함께 명시한 opt-in이며, 대상 process가
Linux `/proc`에서 같은 `CODEX_HOME`을 증명해야 합니다. home이 없거나 다르거나 확인할 수
없으면 pending marker를 유지합니다. macOS는 의도적으로 fail-closed이므로 CLI/Desktop을
수동으로 다시 시작해야 합니다.

installer는 새 `current` link를 선택하기 전에 relay/config/credential/routing 검증을 마치며,
launchd 또는 systemd activation이 실패하면 이전 selected target, relay JSON, 선택한 Codex
configuration/routing, service artifact, manager의 active/enabled 상태를 transaction으로 복구한 뒤
실패를 반환합니다. 최초 enrollment에서는 새 routing block·relay JSON·plist/unit을 제거하므로
실행되지 않는 loopback relay를 Codex가 계속 바라보지 않습니다. 실패한 실행이 새로 만든 release
target도 완전한 rollback 뒤 제거합니다. 기존 release는 제거하지 않으며, 불완전한 rollback 뒤에도
선택 중일 가능성이 있는 target은 안전을 위해 보존합니다.

interactive profile도 같은 transaction에서 복구합니다. 동일 이름의 기존 profile에 정확한
opencodex-relay marker가 없으면 사용자 파일로 간주하여 설치 전에 실패하며 덮어쓰지 않습니다.

## macOS Native 전환과 Connection Status Center

macOS 26+ Apple Silicon release에는 `OpenCodexRelay.app`이 포함됩니다. reviewed
bundle ID, strict signature identifier, exact Apple Team ID가 모두 일치한 Codex Desktop만
저장·제어하며 lifecycle 경계마다 identity를 다시 검증합니다. 사용자가 선택한 path는 trust
근거가 아닙니다. 2026-08-23 Apple-notarized OpenAI Codex Desktop 설치본에서 독립 검증한
`com.openai.codex` / `2DC432GLL2` tuple을 production과 local-development bundle에 고정합니다.
빌더와 installer도 이 exact tuple의 누락·변경을 거부하며, 향후 Codex signer identity가 바뀌면
새 공식 signed build를 다시 독립 검증해 명시적으로 갱신할 때까지 Desktop 탐색·quit/relaunch,
routing apply, 안전 제거가 fail-closed됩니다. Desktop lifecycle에는 AppleScript·강제 종료를
쓰지 않습니다.

```text
opencodex-relayctl mode status --json
opencodex-relayctl mode request native|external|local_opencodex|relay --json
opencodex-relayctl mode apply --confirm-desktop-exited --json
opencodex-relayctl mode cancel --json
opencodex-relayctl mode recover --complete|--rollback --confirm-desktop-exited --json
opencodex-relayctl mode repair-native --expected-routing-generation N \
  --confirm-local-development-native-repair --json  # local-development 전용
```

### 셀프 호스팅 서버 연결

macOS에서는 **Control Center → Settings → 셀프 호스팅 서버 연결**에서 현재 사용자 Relay
통합을 준비하고, 주소와 인증 프로필을 선택한 뒤 연결을 테스트합니다. 선택한 프로필에 필요한
Keychain 값만 추가·교체하며 사용하지 않는 기존 값은 삭제하지 않고 무시합니다. 기존 값은
표시·복사·로그에 기록하지 않고 삭제 UI는 제공하지 않습니다. 새 항목은 Desktop 앱과
`/usr/bin/security`만
신뢰하는 login-Keychain ACL로 만들고, 기존 항목 교체 시 기존 ACL을 보존하면서 두 필수 신뢰
항목을 보정합니다. 비대화식 readback 검증까지 성공해야 저장 성공으로 표시합니다.

**연결 테스트**는 5초 제한, redirect·retry 없음, 8 MiB 응답 상한으로 인증된
`GET /v1/models?client_version=…`를 정확히 한 번 수행하고 기존 catalog parser를 사용합니다.
catalog와 설정 파일은 바꾸지 않습니다. **적용**은 이 테스트를 항상 다시 수행합니다.
External 활성 상태에서는 admission을 park하고 기존 요청을 drain한 뒤 설정을 원자 저장하고
동일 External runtime을 hot-swap하므로 Codex Desktop을 종료하지 않습니다. Native 또는 Local
상태에서는 다음 External 전환에 사용할 주소만 저장합니다. credential은 검증 실패 후에도
저장되며 UI는 검증 필요, 인증 조합 불일치, 연결 불가, catalog 응답 오류를 구분합니다.
성공 영수증에는 config digest, Keychain 수정 시각, 검증 시각과 bounded 결과 코드만 남깁니다.

후보 주소는 argv가 아니라 JSON stdin으로만 전달합니다.

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

`apply`는 config digest, routing generation, credential 변경 경쟁과 pending/recovery 상태를
거부합니다. 활성 runtime 교체 실패 시 이전 설정과 runtime을 복구하며, 복구 완료를 입증하지
못하면 `recovery_required`로 닫습니다.

`request`는 의도만 기록합니다. 이전 `enable`/`disable` 표기도 deprecated request-only alias이며
Codex route를 즉시 바꾸지 않습니다. MenuBar는 선택 앱의 실제 종료를 확인한 뒤에만 `apply`하고
재실행합니다. 진행 중 요청은 drain할 뿐 다른 backend에 replay·handoff하지 않습니다. Native
및 recovery에서는 stale relay 요청이 typed `503`으로 닫히고 catalog/probe 원격 egress가
중단됩니다. local-development 상태가 recovery에서 고립되고 일반 복구 두 작업 모두 증거가
없을 때만 Control Center가 명시적 확인 후 네이티브 상태 복구를 제공합니다. helper는 화면의
exact generation, routing/removal journal·gate 부재, 물리 경로 바인딩, Relay·OpenCodex·다른
owner·unmanaged Codex routing artifact가 전혀 없다는 독립 증명을 모두 요구합니다. 성공해도
routing-state만 다음 native generation으로 바꾸며 production scope, Codex TOML, OpenCodex,
helper, 서비스 설정은 수정하지 않습니다.

### External ↔ Local OpenCodex(10100) profile

macOS 설치의 canonical/default relay profile은 `external_gateway`입니다. Control Center에서는
**External gateway**, **Local OpenCodex (10100)**, **Native ChatGPT Codex**를 명시적으로
선택합니다. Local은 relay가 credential 없이 proxy/redirect 없이 `/healthz`의
`service:"opencodex"`, `status:"ok"`, port `10100` 및 visible·중복 없는 `/v1/models`를
검증한 경우에만 활성화됩니다. Local catalog는 External과 다른 relay-owned 경로를 쓰며
catalog writer를 공유하지 않습니다.

**로컬 OpenCodex 설치 자동 탐색…**은 Tier A의 enrollment/canonical absolute launcher/native
prefix 증거에서 시작해 Tier B의 trusted npm 및 bounded version-manager root로 확장합니다.
Tier C는 별도 승인한 bounded local-volume scan이며 mutation authority가 아닙니다. Discovery
schema 4는 `teardown_capability`, `data_capability`, bounded compatibility reason과 기존
`homebrew_guarded_npm` / `homebrew_guard_required` 상태를 추가합니다. 앱은 schema 2·3·4를
읽지만 schema 2·3 후보는 표시 전용입니다. 자동 제거는 schema 4 Tier A/B 후보 중
npm/Node/Bun/CLI/package closure가 변하지 않았고 package identity가 검토된 Relay teardown
profile과 정확히 일치할 때에만 허용합니다. darwin/arm64 registry는 stable `2.22.0`, `2.23.0`,
`2.24.0`, `2.24.1`, `2.24.2`, `2.25.0`, `2.26.0`, `2.27.0`, `2.28.0`, `2.29.0`,
`2.31.0`, `2.32.0`, `2.32.1`, `2.33.0`을 exact allowlist로 승인합니다. 각 profile은 npm
integrity, 검토된 전체 설치 closure, 필수 module hash와 version-specific adapter ID를 고정하며,
preview와 stable `2.30.0`은 제외합니다. package identity는 profile registry와 adapter
implementation registry로 분리되어 있어, 향후 버전은 discovery 조건을 완화하거나 version
heuristic을 추가하지 않고 검토된 단일 profile과 adapter를 등록해 확장합니다. 누락·중복·불일치
profile 또는 adapter는 fail-closed됩니다. 별도의 sanitized·untruncated Tier-B discovery도 동일 installation ID와
fingerprint를 정확히 한 번 재현해야 하며, 모호·부분·Tier C·manual 결과는 manual-only입니다.

정확한 arm64 전역 npm 설치가 `/opt/homebrew` 아래에 있고 current-user 소유권과 완전한 실행
증거가 확인되며 group-write만 기존 `exact_npm` 검증을 막을 때에는
`homebrew_guarded_npm`을 사용할 수 있습니다. production과 local-development 모두
**앱 정보**에서 generic `OpenCodexRelayHelperInstaller` 명령을 표시하되 service ID와 CDHash는
격리합니다. 사용자가 Terminal에서 명시적으로 실행해 고정 `/Library` 위치에 helper를
설치합니다. 앱은 관리자 암호를 전달받지 않습니다. 두 profile 모두 원래
inode/device/mode를 root-owned `0600` 저널에 먼저
기록하고 제거 작업 동안만 group-write를 해제한 뒤 역순으로 복원합니다. 이 helper는 OpenCodex
삭제, npm 실행, 소유권 변경, shell·`sudo` 실행 또는 임의 경로 처리를 하지 않습니다. 기존 Go
제거기와 routing/removal journal만 삭제 권한을 유지합니다. world-writable, ACL, foreign owner,
symlink, launcher 충돌, 불완전한 증거, 미승인 helper, 미해결 보호 저널은 계속 fail-closed
manual-only입니다.

Control Center의 **앱 정보** 또는 **유지보수 및 복구**에서 제거 후보를 먼저 선택하지 않고도
권한 helper 설정을 시작할 수 있습니다. production과 ad-hoc 개발판 모두 검토된 `install`,
`update` 또는 중단 transaction 전용 `recover` 명령만 복사하고 Terminal을 열며, 별도 관리자
승인 뒤 앱으로 돌아올 때 준비 상태를 다시
확인합니다. root-owned schema 2 installer journal에는 bounded phase와 backup witness만 기록됩니다.
local installer는 helper가 준비되기 전 exact 후보 bundle을 owner-only pending 위치에 보존하고 기존
앱·Relay service·binding을 변경하지 않습니다. 사용자가 동일 후보의 고정 관리자 명령을 실행한 뒤
install을 재실행하면 artifact hash와 CDHash, XPC readiness를 다시 검증한 동일 bundle만 승격합니다.
실패 JSON은 schema 1의 optional `failure_phase`, `failure_reason`, `rollback_result`로 단계와 rollback
결과를 구분하며 경로·UID·CDHash·child 출력은 기록하지 않습니다.
복구는 현재 exact bundle을 완료하거나 이전 helper·LaunchDaemon byte와 launchd 상태를 되돌리며
malformed·legacy journal은 추정하지 않고 차단합니다. 원본 파일 witness와 launchd 상태가 정확히
일치하는 pristine `preparing` transaction만 완성된 backup 없이 폐기할 수 있고, `backups_ready`
이후 단계는 계속 backup 증거를 요구합니다. 이 보조 경로는 Homebrew 보호나 package 제거를
시작하지 않습니다.

exact executable/fingerprint를 사용하는 비파괴 handoff 두 개, 즉 proxy 유지 + integration/Shim
해제와 proxy 유지 + integration만 해제는 계속 제공합니다. legacy handoff에서는 `ocx uninstall`을
제거했습니다. 완전 제거는 opaque installation ID/fingerprint 전용 wizard입니다. schema 4
제거는 `preserve_only`로 고정되어 data inventory나 Trash를 호출하지 않고 설정·자격 증명·로그를
포함한 모든 OpenCodex data root를 보존합니다. 검토한 `UInt64` routing generation은 고정됩니다.
caller path/glob/package name, implicit-all, `sudo`, 자동 elevation, permanent-delete fallback은
없고 Codex 앱도 제거하지 않습니다.

Relay가 버전별 teardown adapter를 소유하며, 검증된 Bun과 private immutable package snapshot으로
실행합니다. shell, ambient `PATH`, caller가 전달한 path는 사용하지 않습니다. adapter는 관리된
service/proxy 정지, native routing 복원, integration/environment/shell hook 해제와 Shim 안전
복원을 수행합니다. 내부 `relay_preserving_teardown` schema 1 receipt가
`data_preserved=true`, `config_root_removed=false`와 필수 component postcondition을 모두 증명해야
npm 제거가 시작됩니다. 공식 OpenCodex package는 수정하지 않고 별도 `opencodex/` checkout도
실행 의존성으로 사용하지 않습니다.

handoff 중에는 wizard를 닫지 않고 안전 사전 확인, Desktop 종료, 승인된 OpenCodex 작업,
Desktop 재실행, Relay 상태 재확인을 단계별로 표시합니다. recovery·applying·status 없음·
검증되지 않은 라우팅은 Desktop 종료와 OCX 호출 전에 차단합니다. Shim handoff 성공 뒤에는
status와 후보를 모두 다시 조회하며, 동일 canonical package root와 executable의 후보가 정확히
하나이고 새 fingerprint도 자동 제거 조건을 만족할 때만 제거를 다시 활성화합니다. 후보가
없거나 중복·변경되면 제거를 잠그고 재탐색을 요구합니다.
partial/unknown 결과 뒤에도 Desktop 재실행과 status 재확인을 시도해 확인된 복구 상태를
보여주되 기존 fail-closed 제거 guard를 우회하지 않습니다.

성공은 strict receipt가 package 부재, data 보존, 최종 routing 재검증, terminal relay cleanup을
모두 증명한 뒤에만 표시합니다. process cleanup을 증명하지 못하면 platform-attested Mac 전체
재시작이 필요하고 routing recovery가 남으면 admission도 fail-closed입니다. UI recovery session은
versioned/path-free입니다. terminal cleanup은 exact selector, package 부재, 검토한 routing
generation을 독립적으로 다시 확인한 뒤 recovery gate를 durable하게 해제하고 journal을 소비할
때까지 별도의 terminal finalization witness로 admission을 계속 닫습니다. teardown·package
child는 실행 직전에 각각 durable typed execution witness를 arm하며, witness가 남아 있는 동안
replay·package resume·finalization을 모두 막습니다. 기존 data-inventory/Trash recovery record는
preserve-only flow로 자동 변환하거나 재개하지 않고 검토된 수동 복구가 필요하도록 차단합니다.
saved reboot recovery는 일반
uninstall predicate를 완화하지 않는 별도 recovery-only predicate를 사용하며, relay가
unreachable이어도 저장된 동일 durable generation에 한해서만 review할 수 있습니다. child
미시작 routing 거부나 cleanup이 확인된 malformed receipt는 durable marker를 먼저 기록하고,
필요하면 routing을 park한 다음에만 active witness를 typed retry phase로 해제합니다. gated
`generation=0`은 표시 전용이며 어떤 action도 승인하지 않습니다.
routing recovery action 자체는 healthy relay를 요구하며, exact journal gate는 controller
recovery 중에도 유지되고 동일 opaque installation selector·검토한 routing generation·검증된
routing transaction witness·새 stable route·acknowledged live health·exact Codex ownership을
다시 검증한 뒤에만 해제합니다. 더 높은 안전한 recovery generation은 동일 opaque selector와
함께 action 없이 checkpoint하고 다시 명시적으로 확인해야 합니다. attested
changed boot 뒤 teardown은 durable operation intent로 돌아가 child를 replay하지 않고 package는
absence/residual 분기를 따릅니다. 정확한 전제·확인·disposable acceptance는
[한국어 canonical runbook](../../docs/local-codex-relay.ko.md)과
[영문 운영 가이드](../../docs/local-codex-relay.md)를 따릅니다.

External ↔ Local도 Desktop quit → request drain → apply → relaunch 경계를 사용합니다.
relay PID/listener는 유지되지만 요청 replay·자동 redirect는 없습니다. active Local backend의
identity 확인이 사라지면 relay는 typed `503 local_opencodex_unavailable`으로 주차하며,
Desktop-safe External 또는 Native apply를 사용자가 명시적으로 수행해야만 벗어납니다.

MenuBar popover는 선택 앱과 현재 적용 경로만 요약합니다. Sidebar 기반 Control Center는
bundle helper의 redacted JSON만 읽고, 기존 relayctl 안전 조건 안에서 라우팅·Desktop·Local
OpenCodex·유지보수·설정을 제어합니다. local relay, routing acknowledgement, catalog, active request, 최근 원격 관측을
보이되 upstream URL, credential, raw error를 보이지 않습니다. macOS installer가 활성화한
external gateway probe는 `relay_active`에서만 10분 간격으로 실행됩니다. Desktop의 실제
native backend 성공은 live Codex task로만 확인합니다.

toolbar 새로 고침은 진행 중 polling과 겹치면 요청을 버리지 않고 한 번으로 합쳐 다시 실행합니다.
검증된 새 status를 받은 뒤에만 이전 status 오류를 지우며, snapshot 변경 여부에 따라
`상태가 업데이트됨`과 `최신 상태이며 변경 없음`을 구분합니다. 완료 로그에는 changed,
generation, phase만 기록합니다.

Control Center의 **활동 로그**는 현재 세션 이벤트를 최대 500건 보관하고, 필터 결과의
JSON Lines 또는 현재 bundle용 macOS 로그 명령을 복사합니다. allowlist된 상태만 기록하며
라우팅 snapshot은 개요와 연결 및 라우팅 카드 상태를 함께 포함합니다. 민감한 경로·출력·
자격 증명은 제외하며, 앱에서 지워도 시스템 로그는 유지됩니다.

앱은 Dock에 고유한 `AppIcon.icns`로 표시되는 일반 macOS 앱입니다. Dock 아이콘 또는
popover의 **연결 세부 정보…**를 누르면 중복 창을 만들지 않고 동일한 Control Center를
전면으로 올립니다. Control Center는 macOS 26의 네이티브 sidebar·toolbar·sheet와
Liquid Glass 버튼 스타일을 사용하며, 별도 사용자 정의 배경 재질을 덧씌우지 않습니다.
상태와 설명은 macOS 기본 `body` 크기와 의미론적 label 색을 사용합니다. 상태값은 라벨
열 바로 뒤에 배치하고, 관련 버튼은 가로로 묶되 좁은 상세 폭에서만 세로로 전환합니다.

**Codex Desktop** 화면은 owner-only routing binding이 지정한 정확한 TOML도 요약합니다.
확인할 때마다 binding을 다시 읽고 TOML을 symlink follow 없이 연 뒤, 검사 전후의 일반 파일
identity를 비교합니다. 전체 원문은 민감 정보 경고 승인 뒤에만 읽기 전용·선택 가능한
sheet로 표시하고 sheet를 닫으면 메모리에서 제거합니다. 미리보기는 1MiB 이하의 올바른
UTF-8만 지원합니다. 시스템 기본 앱, Visual Studio Code, Xcode, 텍스트 편집기, 다른 앱
선택기는 NSWorkspace만 사용하며 열기 직전에 파일을 다시 검증하고 최근 사용 항목에
추가하지 않습니다. Relay는 Codex TOML을 편집·저장하지 않고 제한된 결과 토큰만 기록합니다.

**앱 정보** 페이지는 앱 버전·빌드·번들 식별자·배포 유형·최소 macOS·실행 아키텍처를
표시합니다. 또한 앱에 고정적으로 포함된 Go 실행 파일 opencodex-relay와
opencodex-relayctl의 제한된 로컬 버전 명령으로 포함 여부와 버전 일치를 확인합니다.
revision 4 bundle은 권한 Homebrew guard helper 버전·등록 상태·protocol version도 표시합니다.
실행 파일 경로·mode·UID·서명 hash·원시 출력·프로세스 오류는 화면이나 로그에 표시하지
않습니다.

MenuBar UI의 **시스템 기본값**은 macOS의 첫 번째 preferred language만 사용합니다. `ko`는
한국어, `en`은 영어로 표시하고 지원하지 않거나 확인할 수 없는 언어는 한국어로 fallback합니다.
Control Center 설정의 **언어** 메뉴에서 시스템 기본값, 한국어, English를 즉시
고를 수 있습니다. 선택값은 bundle ID별
preference에 따로 저장되므로 `OpenCodexRelay`와 `OpenCodexRelay Dev`가 서로 공유하지
않습니다. relayctl JSON 필드와 CLI 식별자는 안정적인 영문 protocol 값으로 유지합니다.

macOS installer는 `~/Library/Application Support/OpenCodexRelay/routing-binding.json`에
owner-only·무비밀의 exact path binding을 기록합니다. 이 파일이 없거나 malformed, symlink,
broad permission, 다른 소유자이면 app은 default Codex home으로 fallback하지 않고 안전하게
unavailable 상태를 표시합니다.

Codex AppShot acceptance는 workspace root를 물리 checkout
`/path/to/OpenCodex-OCI-Gateway`로 새로 연 작업에서
수행합니다. `/path/to/IdeaProjects` 파일시스템 symlink는 유지하고, Codex 앱 안의
오래된 workspace 별칭만 제거한 뒤 물리 경로를 다시 열고 Codex를 재시작하여 새
writable-root profile을 구성합니다. 새 작업에서 sandbox 명령 실행, 단일 Control Center
창 탐색, AppShot 첨부 이미지 렌더링을 순서대로 확인합니다. 마지막 단계만 실패하면 Relay
window sharing과 분리하여 화면 기록·손쉬운 사용 권한을 조사합니다.

## Responses normalizer

`responses.model_modes`의 exact model ID에 `bounded_json`을 opt-in할 수 있습니다.
대소문자는 무시하지만 앞뒤 공백, case-fold 중복, provider alias 또는 colon-family 상속은
허용하지 않습니다.

```json
"responses": {
  "websocket_mode": "http_fallback",
  "model_modes": {
    "opencode-go-responses/gpt-5.6-luna": "bounded_json"
  }
}
```

선택된 `POST /v1/responses`의 top-level `stream:true`만 `false`로 바꾸고 upstream은
정확히 한 번 호출합니다. 완결 JSON을 엄격히 검증한 뒤 `response.created`, output item별
`response.output_item.done`, 원래 terminal status, `[DONE]` 1개와 EOF를 합성합니다.
ID, function/custom call payload, screenshot data URL, usage와 알 수 없는 field는 보존합니다.
오류·초과·timeout·예상 밖 SSE는 재시도 없이 닫습니다.

identity/zstd와 즉시 unlink되는 `0600` spool을 지원하고 동시 normalization은 2개로
제한합니다. Hosted image tool은 기존 streaming으로 bypass합니다. Native Codex 0.147이
실행할 수 없는 hosted `computer`는 upstream 호출 전 400으로 차단하지만, plugin/MCP
Computer Use의 일반 `function_call`은 정상 처리합니다. 이 profile의 Responses WebSocket은
426으로 HTTP/SSE fallback하며 Images, Live, Realtime, Voice, AppServer transport는 영향을
받지 않습니다.

## 일반 lane과 명시적 interactive lane

relay는 설정된 일반 listener(통상 `127.0.0.1:18180`)와 서로 다른 numeric-loopback
interactive listener를 함께 엽니다. `responses.scheduler.interactive_listen_address`가 없으면
일반 listener의 IPv4/IPv6 계열에서 port `18182`를 기본값으로 사용합니다. 명시한 다른 port도
relay config validator를 통과하면 그대로 사용합니다.

installer는 `$CODEX_HOME/opencodex-relay-interactive.config.toml`을 atomic하게 관리합니다.
ownership marker를 제외하면 이 파일의 설정은 정확히 두 개뿐입니다.

```toml
openai_base_url = "http://127.0.0.1:18182/v1"
model_catalog_json = "/ABSOLUTE/CODEX_HOME/opencodex-relay-catalog.json"
```

`model`, reasoning 옵션, agent 제한이나 다른 runtime policy는 기록하지 않습니다. 일반 TUI,
`exec`, `review`, daemon은 계속 base listener를 사용합니다. 예약 lane은 사용자가 다음처럼
명시적으로 선택합니다.

```bash
codex --profile opencodex-relay-interactive
codex exec --profile opencodex-relay-interactive 'REPLACE_WITH_PROMPT'
```

두 `/__relay/healthz` endpoint는 `listener_lane`, 두 listener 주소, scheduler limit와 비음수
동적 counter를 보고합니다. installer와 Remote gate는 일반 endpoint의 `general`, interactive
endpoint의 `interactive` lane 및 두 static contract가 `relay.json`과 일치해야만 activation을
수락합니다.

## 개발 검증

저장소 루트에서 실행합니다.

```bash
bash -n client/relay/scripts/*.sh
(cd client/relay && go test ./... && go vet ./...)
python3 -m unittest pilot.tests.test_compatibility_relay_assets
```

이 검사는 구현과 문서 계약을 검증하지만 실제 Cloudflare, 중앙 gateway, Desktop 또는
Remote UI 연결을 증명하지 않습니다. 배포 완료는 정본 운영 가이드의 live acceptance
목록으로 별도 판정합니다.

public 배포의 정본은 `.github/workflows/relay-release.yml`입니다. `v` 접두사가 없는
lightweight strict-SemVer tag만 public `novelKR/OpenCodex-OCI-Gateway` 저장소에서 workflow를
시작합니다. preflight는 tag가 현재 public `main`과 일치하는지, exact commit의 `linux`,
`macos`, `analyze`가 성공했는지, immutable releases가 켜져 있는지와 같은 version release가
없는지를 확인합니다. 보호된 `relay-release` Environment의 v2 Ed25519 private key는 macOS
build step에서만 사용하며 tracked public key와 일치해야 합니다. CI는 정확히 8개 asset과
GitHub build provenance를 만들고 검증한 뒤 `publish-github-release.sh`로 immutable release를
게시합니다. 이 public CI 경로의 최초 version은 `0.3.6`입니다.

소비 host에서는 `install-relay.sh --github-repo novelKR/OpenCodex-OCI-Gateway`와
`config/trust/opencodex-relay-release-ed25519.pub`를 사용합니다. 익명 API rate limit이 부족할
때만 current-user `0600` token file을 선택적으로 지정합니다. private key, base64 secret,
생성된 release directory는 저장소에 커밋하지 않습니다.
revision 4 bundle은 Linux helper 네 개, Hardened Runtime ad-hoc `OpenCodexRelay.app.zip`,
`THIRD_PARTY_NOTICES.md`, manifest, signature의 총 8개 asset입니다. manifest는 component,
macOS bundle ID, `signing_mode: "adhoc"`, final zip hash, notice URL/SHA-256를 함께
서명합니다. installer는 Ed25519 signature, 모든 asset hash, nested ad-hoc signature,
Relay Team ID 부재 및 Hardened Runtime을 검증합니다. 기존 revision 1/2 release도 rollback용으로 지원하지만
parked routing controller가 없어 MenuBar-managed Desktop 전환에 사용하면 안 되며 legacy
compatibility path를 쓰기 전 Desktop을 종료해야 합니다. 앱에 포함된 generic
`OpenCodexRelayHelperInstaller`의 `install|update|uninstall|recover|status`는 별도 관리자
승인이 필요하며 app·installer·helper의 exact CDHash를 결합합니다. busy 또는
recovery-required 보호 저널은 앱 교체를 차단합니다.
전체 절차는 [`../../docs/local-codex-relay.ko.md`](../../docs/local-codex-relay.ko.md)를
따릅니다.

macOS 앱은 의도적으로 공증하지 않으며 Apple publisher identity도 없습니다. Finder에서 앱을
한 번 여십시오. 차단되면 즉시 **시스템 설정 → 개인정보 보호 및 보안**에서
OpenCodexRelay의 **확인 없이 열기(Open Anyway)**를 선택하고 다시 **열기**를 확인합니다.
이 버튼은 차단된 실행 뒤 보통 약 1시간 동안 표시됩니다. 앱을 rebuild하거나 update하면 같은
승인이 다시 필요할 수 있습니다. quarantine attribute를 삭제하거나 Gatekeeper를 비활성화하지
마십시오. helper 설치는 이 앱 승인과 별개의 관리자 승인을 요구합니다.

## 시스템을 통합하지 않는 macOS Preview 실행

저장소 루트에서 `./script/build_and_run.sh`를 실행하면 완전한 Preview bundle을 build하고
실행합니다. Control Center의 모든 상태를 검토할 수 있도록 권한 helper와 고정 installer도
포함하지만 `OpenCodexRuntimeMode=preview`가 routing binding 사용과 launchd, Keychain,
`/Library`, 기존 설치 앱 및 Codex routing 변경을 차단합니다. Terminal 실행과 자동 권한
상승도 하지 않습니다.
앱 이전 카드는 검토용으로 표시하지만 파일 작업은 비활성화됩니다.

`./script/build_and_run.sh --integration-preflight`는 읽기 전용입니다. 경로를 노출하지 않는
bounded 상태 코드만 출력하며 exit 0은 `ready`, exit 3은 `integration_required`, exit 4는
`unsafe` 또는 `invalid`입니다. Preview는 소비자용 셀프 호스팅 온보딩을 검토용으로 표시하지만
통합을 적용하거나 시스템을 변경할 수 없습니다.

별도의 **소스 → 패키지 → Gateway → 서명 → 검토** 생산자 안내는 Settings에 표시하지
않습니다. local development 빌드를 `--enable-producer-tools`로 실행한 경우에만 Developer
메뉴에서 열 수 있습니다. 입력값 저장, Keychain 조회, PEM 내용 읽기, Terminal 열기 또는
명령 실행은 하지 않으며 복사한 명령의 종류만 기록합니다.

## 개인·조직 내부 local-only 개발 배포

`build-local-dev.sh`와 `install-local-dev.sh`는 검토한 development bundle을 개인 또는
소수 조직 내부에서 직접 전달할 때만 쓰는 **별도** 경로입니다. revision 4 production
builder/installer를 느슨하게 만들지 않으며 release URL, GitHub download, Developer ID,
notarization, Gatekeeper 검사·우회, quarantine 제거, 자동 update를 사용하지 않습니다.

builder는 clean Git commit만 허용하고 Ed25519 manifest와 helper/app의 ad-hoc structural
signature를 만듭니다.

```bash
./client/relay/scripts/build-local-dev.sh 1.2.3-dev.1 \
  --signing-key /secure/off-repo/local-dev-ed25519.pem \
  --output /secure/local-transfer/1.2.3-dev.1

./client/relay/scripts/install-local-dev.sh install 1.2.3-dev.1 \
  --source-dir /secure/local-transfer/1.2.3-dev.1 \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --acknowledge-local-source \
  --acknowledge-local-development-source
```

반복 전달은 별도 fingerprint를 확인한 public PEM을 current-user Keychain에 pin한 뒤
`--keychain-service`로 설치할 수 있습니다. dev installer는
`relay-dev`, `127.0.0.1:18190/18192`, `io.github.novelkr.opencodex-relay.dev`,
`OpenCodexRelay Dev.app`만 사용하며 native parked 상태로 시작합니다. MenuBar가
Desktop 종료 후 apply하기 전에는 Codex routing을 수정하지 않습니다. 하나의 Codex config는
production 또는 dev 중 한 owner만 가질 수 있습니다. 검토된 release channel 밖의 로컬 개발
app은 사용자가 직접 실행·승인해야 하며 installer는 login item을 등록하지 않습니다.
기존 `--acknowledge-unsigned-local-build` 표기는 deprecated 호환 별칭으로만 유지됩니다.

### 소유권 기반 local-development 네이티브 복구

`inspect-native-repair --expected-routing-generation N --json`은 URL·경로·TOML 값을 노출하지
않고 `state_only`, `local_relay`, `opencodex`, `unavailable`과 두 managed key의 존재 여부만
반환합니다. 다른 소유자, 혼합·불완전 marker, marker 없는 사용자 override는 자동 수정하지
않습니다. `repair-native-routing`은 exact generation, Desktop 종료 확인, journal/gate 부재와
물리 경로 바인딩을 다시 검증합니다. local Relay 소유 설정은 marker block과 managed profile만
제거합니다. OpenCodex 복구 권한은 패키지 제거 권한과 분리됩니다. Discovery는 기존 installation
ID/fingerprint를 유지한 채 선택적 native-restore fingerprint를 결합하고, helper는 정확한 Tier A/B
후보를 다시 탐색해 bundled Bun·CLI·package tree의 private snapshot만 정제된 환경에서 실행합니다.
후보 선택 뒤 read-only `inspect-native-repair-owner`가 설정
유효성 및 Codex 통합 ON/OFF를 제한 토큰으로 확인하며, invalid/unavailable이면 Desktop을
종료하지 않습니다. 변경 전 경로 비노출 timestamped `0600` backup을 만듭니다.
네이티브 라우팅을 재검증한 뒤에만 generation을 증가시킵니다. TOML 복구 후 상태 저장만 실패하면
기존 recovery generation을 유지하고 상태 전용 복구를 다음 작업으로 활성화합니다. OpenCodex
Codex 통합만 꺼지며 Shim·프록시·패키지·data는 유지됩니다. 증명된 무변경 desired-state
충돌만 200/500/1000ms로 세 번 추가 재시도하고, busy/설정 오류/복구 실패/유효하지 않은
결과를 서로 다른 bounded code로 반환합니다.
