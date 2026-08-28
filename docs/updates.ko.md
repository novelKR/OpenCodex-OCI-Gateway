# 관리형 Codex·OpenCodex 업데이트 런북

이 문서는 중앙 OpenCodex 프록시와 Remote Control 호스트의 Codex standalone을
분리해서 업데이트하는 정식 운영 경로입니다. loopback 게이트웨이, 전용 서비스 계정,
Remote Codex 홈을 그대로 보존합니다.

native relay의 최초 설치, 설정 필드, catalog activation 및 수동 Remote rollback은
[`local-codex-relay.ko.md`](local-codex-relay.ko.md)가 정본입니다. 이 문서는 이미 등록된
구성요소의 릴리스 순서에 집중합니다.

이 공개 runbook은 공통 update 절차와 safety boundary만 정의합니다.
배포별 runtime 소유권, private release publication, consumer rollout, key rotation,
플랫폼 점검 주기, incident 등급, 증거 보존과 날짜 고정 결과는 private
deployment overlay에 보존합니다.

> 과거 deployment hold와 대체된 live evidence는 private deployment overlay에
> 보존합니다. 과거 검증은 현재 health 또는 remediation을 증명하지 않습니다.

## 업데이트 구분

| 변경 | 자동화 | 중단 영향 | 롤백 경계 |
| --- | --- | --- | --- |
| Local native relay release | 각 client에서 signed `install-relay.sh install VERSION`을 명시 실행 | local AppServer가 reconnect될 수 있고, 기존 CLI는 picker 갱신을 위해 재시작 필요 | 이전 signed version을 다시 설치하며 non-secret relay JSON과 이전 catalog는 유지 |
| legacy/loopback Remote catalog 변경 | `opencodex-remote-catalog-refresh.timer`가 약 10분마다 fetch | 검증된 catalog가 달라질 때만 managed app-server 재시작 | Remote Codex 백업 체인의 이전 catalog |
| external relay Remote catalog 변경 | relay가 유일한 catalog writer이고 `opencodex-remote-relay-catalog-activation.timer`가 약 1분마다 marker 확인 | active request 0 health snapshot 뒤 managed app-server 재시작. snapshot 직후 요청도 배제하려면 maintenance window 사용 | relay의 이전 catalog와 marker를 유지 |
| 중앙 OpenCodex 프록시 릴리스 | `upgrade-opencodex.sh apply VERSION` | 패키지 교체와 smoke 동안 중앙 프록시 중단 | `/var/backups/opencodex/`의 이전 `/opt/opencodex` |
| Remote Codex standalone 릴리스 | `update-remote-codex.sh apply --allow-remote-interruption` | 해당 호스트의 Remote 작업 연결 중단 가능 | 이전 standalone `current` link와 catalog |

legacy/loopback catalog timer는 비어 있지 않고 식별자가 중복되지 않는 모델 배열만 받아
`0600` catalog를 원자적으로 교체합니다. external relay mode에서는 installer가 이 legacy
timer를 비활성화합니다. 수동 `refresh --restart`도 marker를 적용하지 않고
`relay_catalog_refresh=owned_by_relay`만 출력합니다. relay가 catalog를 검증·write하고 전용
activation timer만 `.restart-pending`을 관찰합니다. 따라서 Remote home에는 catalog writer와
activator가 각각 하나만 남아 competing activator를 막습니다. 다만 manager의 health read는
cross-process snapshot이며 resident relay의 admission gate가 아니므로, 새 요청까지 엄격히
배제하려면 maintenance window가 필요합니다. Codex는
`model_catalog_json`을 시작할 때 읽으므로, 이미 열린 TUI는 종료 후 다시 시작해야 새 모델
선택 목록을 읽습니다.

## Native relay release 경로

신뢰하는 macOS release workstation에서 Linux helper 네 개와 Hardened Runtime을 적용한
self-contained ad-hoc `OpenCodexRelay.app.zip`을 빌드합니다. Ed25519 private signing key는
저장소 밖에 둡니다. 명시 version directory, 다섯
component, `THIRD_PARTY_NOTICES.md`, manifest와 signature를 함께 publish하고, 각 client는
`current`를 바꾸기 전에 manifest signature, 선택 component checksum, signed notice checksum을
모두 검증합니다. revision 1/2는 rollback artifact로 남지만 새 build는
`signing_mode: "adhoc"`을 포함하는 component-aware compatibility revision 4를 사용합니다.

```bash
./client/relay/scripts/build-release.sh REPLACE_WITH_VERSION \
  --base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --signing-key /secure/off-repo/release-ed25519.pem \
  --output /secure/release-staging/REPLACE_WITH_VERSION

./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

### public GitHub Release를 사용하는 경우

public Core 저장소에서는 `--base-url` 대신 GitHub 전용 source를 사용할 수 있습니다. 먼저
repository에 immutable releases를 활성화하고 release tag를 exact semver로 게시합니다.

```bash
./client/relay/scripts/build-release.sh 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --signing-key /secure/off-repo/release-ed25519.pem \
  --output /secure/release-staging/1.2.3

./client/relay/scripts/publish-github-release.sh 1.2.3 \
  --repo OWNER/opencodex-relay-releases \
  --input /secure/release-staging/1.2.3 \
  --public-key /secure/off-repo/release-ed25519.pub

./client/relay/scripts/install-relay.sh install 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --public-key /secure/path/release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

consumer token은 필요하지 않습니다. API rate limit 때문에 선택적 read-only token file을
쓸 때만 current user 소유 `0600`이어야 하고,
installer가 owner-only temporary curl config에만 일시적으로 기록한 뒤 제거합니다. GitHub
release·manifest·target checksum이 모두 검증되기 전에는 `current`가 바뀌지 않습니다. token은
relay JSON, Codex TOML, launchd plist, systemd unit에 기록하지 않습니다.

Remote Control Linux home에는 [`local-codex-relay.ko.md`](local-codex-relay.ko.md)에
기록된 전용 `--catalog-path`, `--codex-executable`, `--manage-app-server false`를 전달한 뒤
명시적으로 `configure-remote-codex-routing.sh enable-relay
--allow-remote-interruption`를 실행합니다. relay update 중에도 별도의 중앙 feature gate와
local JSON을 모두 의도적으로 켜지 않는 한 Voice는 비활성 상태를 유지합니다.

## Remote 호스트 자동화 최초 설치

Remote 설치기는 검토된 스크립트와 user systemd unit만 복사합니다.
`auth.json`, `remote-opencodex.json`, `credentials.env`는 만들거나 복사하지 않으며,
각 호스트의 소유자 전용 사전 조건으로 남습니다.

각 Remote Control 호스트에서 SSH 로그아웃 후에도 user service가 유지되도록 한 번만
설정합니다.

```bash
sudo loginctl enable-linger ubuntu
```

아직 daemon이 없는 새 Remote host에서는 첫 호출부터 bootstrap 형식을 사용합니다.
`ubuntu` 사용자로 이 저장소 전체가 있는 위치에서 실행합니다.

```bash
cd /path/to/OpenCodex-OCI-Gateway/pilot/scripts
./install-remote-codex-home.sh install --bootstrap-remote-control
```

이미 Remote Control을 bootstrap한 host에서 managed asset만 다시 설치할 때에는 plain
형식을 사용합니다.

```bash
./install-remote-codex-home.sh install
```

비밀값을 출력하지 않는 확인 명령은 다음과 같습니다.

```bash
~/.local/lib/opencodex-relay/update-remote-codex.sh status
codex debug models | jq '.models | length'
```

### Ubuntu 24.04 Linux sandbox 사전 조건

Linux의 Codex는 `bubblewrap`과 동작하는 user namespace가 필요합니다. Ubuntu 24.04에서는
`kernel.apparmor_restrict_unprivileged_userns` 전역 제한을 끄지 않고, 배포판의 좁은
`bwrap` AppArmor profile을 로드합니다.

```bash
sudo ./pilot/scripts/configure-codex-linux-sandbox.sh --user ubuntu
```

이 스크립트는 `bubblewrap`, `apparmor-profiles`, `apparmor-utils`를 설치하고
`bwrap-userns-restrict`를 로드하며, 지정 사용자의 `bwrap --unshare-user` 동작을
검증합니다. 직접 실행한 `unshare --user --map-root-user`는 계속 거부될 수 있는데,
이는 `bwrap` AppArmor profile에만 예외를 부여하기 위한 의도된 동작입니다.

### AppServer 버전 수렴과 명시적 복구

`running`만으로는 Remote Codex update가 성공한 것이 아닙니다. catalog restart marker를
지우기 전에 `managedCodexVersion`, `cliVersion`, `appServerVersion`이 모두 managed
standalone 버전과 같은지 확인합니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-daemon
```

일반 restart가 AppServer가 실행 중이나 daemon이 관리하지 않는다고 보고하면 `pkill`,
넓은 process match 또는 `SIGKILL`을 사용하지 않습니다. 이미 interrupting operation인
`restart-daemon`(legacy catalog timer의 `refresh --restart` 포함)을 요청한 경우에는 먼저 일반
Codex restart를 시도하고, 그 명령이 allowlist Remote-home daemon/AppServer pair를 거부할
때만 동일한 좁은 복구로 fallback합니다. 이로써 Codex 0.147 ownership 보고만으로 검증된
변경 catalog가 pending 상태에 남는 것을 막습니다.

독립적인 진단/takeover가 필요할 때는 maintenance window에서 아래 명시적 복구 명령을
사용합니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  recover-daemon --allow-remote-interruption
```

이 경로는 승인된 Remote `CODEX_HOME`을 소유한 정확한 구형 daemon pid-update loop와
Unix-socket AppServer command shape에만 `TERM`을 보내고, managed daemon을 bootstrap한 뒤
version equality를 기다립니다. caller가 이미 daemon restart를 선택했고 exact allowlist가
일치할 때만 fallback하며, unrelated process로 범위를 넓히지 않습니다.

전용 `CODEX_HOME`의 wrapper를 SSH login directory(`/home/ubuntu`)에서 실행할 경우,
일반 `~/.codex/config.toml`이 trusted project config로 overlay되어 전용 Remote
model/reasoning default를 덮을 수 있습니다. 다음 명시적 조치는 일반 home의 file이나
preference를 수정하지 않고, **전용 Remote config에서만** 해당 project를 `untrusted`로
표시한 뒤 daemon을 재시작합니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  isolate-home-project-config --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-home-project-config
```

### Catalog 가용성과 base URL 경고

manager와 relay는 upstream `visibility: "hide"`만 제거하고, 그 밖의 visible model은
특정 account에서만 사용 가능하더라도 catalog에 보존합니다. response 시점의 account
제한은 account 선택으로 해결하며, host-local filter로 visible model을 숨기지 않습니다.
새 account를 선택한 뒤에는 새 CLI process에서 catalog와 실제 Responses 요청을 다시
검증합니다.

built-in `openai`가 loopback OpenCodex 또는 local relay에 `openai_base_url`을 사용할 때,
Codex model picker는 overridden base URL에서 model selection이 완전히 지원되지 않을 수
있다는 경고를 보일 수 있습니다. 이는 proxy routing 정보이며 response 실패의 증거가
아닙니다. 경고를 없애려고 override를 제거하면 의도한 OpenCodex path를 우회하므로,
authenticated catalog와 실제 Responses 요청으로 판정합니다.

## Runtime Adapter rollout 상태

저장소의 Runtime Adapter는 root-owned runtime contract와
`/usr/local/libexec/opencodex-runtime`을 통해 Node/npm/OpenCodex entry를 검증하고
실행하도록 보강한 자산입니다. 저장소에 구현·검증되었다는 사실만으로 현재 서버에
배포된 것으로 간주하지 않습니다.

rollout은 세 단계입니다.

1. 저장소 구현과 자동 테스트
2. 리뷰한 exact source를 root-owned, non-listable이지만 `opencodex`가 traverse할 수
   있는 secret-free 임시 디렉터리에서 실행하는 read-only canary
3. 별도 maintenance 승인, idle preflight와 snapshot 뒤의 live migration

`configure-opencodex-runtime.sh check`는 root-owned executable `/usr/local` staging
parent 아래에 byte-identical Adapter와
후보 contract를 함께 staging하고, root로 Adapter `check`와 `describe --json`을,
`opencodex`로 `ocx --version`, `config validate`, `npm --version`을 실행합니다.
성공·실패·signal 모두에서 임시 pair를 제거하며 production unit, drop-in, prefix,
config, service 상태와 backup을 변경하지 않습니다. live migration은
`configure-opencodex-runtime.sh apply ... --allow-service-restart`를 명시적으로
승인한 경우에만 수행합니다. local smoke와 interactive external smoke의 two-stream
overlap이 모두 통과하기 전에는 migration 완료로 기록하지 않습니다.

후보 Node/npm 파일과 이를 교체할 수 있는 상위 디렉터리는 모두 root-owned이고
group/world-writable이 아니어야 합니다. 사용자 소유 Homebrew·version-manager tree는
경로를 canonicalize해도 직접 migration할 수 없습니다. read-only canary 전에 검토한
root-managed runtime을 준비하고, `/home` 아래에 둘 경우 Adapter가 도출한 runtime root
하나만 read-only bind로 노출되는지 확인합니다.

후보 검증은 mutation 없이 먼저 실행합니다.

```bash
sudo ./pilot/scripts/configure-opencodex-runtime.sh check \
  --node-bin /ABSOLUTE/ROOT_MANAGED/PATH/TO/node \
  --npm-cli /ABSOLUTE/ROOT_MANAGED/PATH/TO/npm-cli.js
```

별도 maintenance 승인 뒤에만 같은 exact path로 적용합니다. 기존 실행 경로를 바꾸는
unmanaged drop-in이 있으면 검토한 direct `.conf` 경로 하나를 명시해야 하며, 다른
execution drop-in은 fail-closed입니다.

idle gate가 통과한 뒤 active service는 후보 contract가 managed asset을 교체하기 전에
명시적으로 정지합니다. 이 동안 새 admission을 차단하며, 실패하면 snapshot과 원래의
active/enabled 상태를 복구한 뒤 migration 실패를 보고합니다.

```bash
sudo ./pilot/scripts/configure-opencodex-runtime.sh apply \
  --node-bin /ABSOLUTE/ROOT_MANAGED/PATH/TO/node \
  --npm-cli /ABSOLUTE/ROOT_MANAGED/PATH/TO/npm-cli.js \
  --allow-service-restart \
  --replace-legacy-drop-in \
  /etc/systemd/system/opencodex.service.d/REVIEWED-LEGACY.conf
```

교체할 legacy drop-in이 없으면 마지막 두 줄을 생략합니다. public schema v1과 invoker subcommand 정본은 이 Core와 script help output에
있습니다.

## 권장 릴리스 순서

먼저 점검 시간을 잡습니다. 중앙 OpenCodex 릴리스는 모든 클라이언트에 영향을 줄 수
있고, Codex standalone 릴리스는 한 Remote Control 호스트의 작업을 끊을 수 있습니다.
두 Remote 호스트를 동시에 업데이트하지 않습니다.

1. immutable upstream tag, published package와 provenance가 일치하는 exact stable
   version을 검토한 뒤 한 번만 변수에 기록합니다. `latest`, preview 또는 source
   checkout HEAD를 사용하지 않습니다.

   ```bash
   VERSION=REPLACE_WITH_REVIEWED_EXACT_VERSION
   sudo ./pilot/scripts/upgrade-opencodex.sh check "$VERSION"
   ```

2. service 중단 전에 idle 상태를 확인합니다. 다음 값은 content-free 판정이며,
   snapshot과 적용 사이에 새 요청이 들어오지 않는 maintenance window에서 수행합니다.

   ```bash
   sudo -u opencodex /usr/local/libexec/opencodex-runtime \
     ocx observe memory --json |
     jq -e '.activeTurnCount == 0 and .isDraining == false'
   ss -Htan state established '( sport = :10100 or dport = :10100 )' |
     awk 'END { exit(NR == 0 ? 0 : 1) }'
   sudo systemctl is-active opencodex.service
   sudo systemctl is-enabled opencodex.service
   ```

   `observe memory`는 management credential을 내부에서 사용하는 local OpenCodex CLI를
   통해 gated `/api/system/memory`의 scalar lifecycle만 읽습니다. 인증되지 않은
   `/healthz`에는 이 두 필드가 없으므로 해당 endpoint를 idle 판정에 사용하지 않습니다.
   명령 실패, JSON shape 불일치 또는 non-zero counter는 모두 fail-closed입니다.

3. 검토한 정확한 버전을 적용합니다. 스크립트는 Runtime Adapter 계약, companion smoke
   hash, 기존/신규 config를 검증하고,
   원래 서비스 상태를 복원하며, 기본적으로 로컬 smoke를 실행합니다. 모두 성공한
   뒤에만 `/etc/opencodex/expected-version`을 갱신합니다.

   관리형 npm 설치는 npm version마다 의미가 달라지는 allowlist에 의존하지 않습니다.
   먼저 `--ignore-scripts`로 모든 dependency install lifecycle을 차단합니다. prefix가
   `opencodex` 계정 소유인 동안 Runtime Adapter의 version-bound
   `prepare-bundled-bun VERSION`이 service-owned package chain과 exact Bun dependency를
   검증하고, 검토된 exact launcher의 `--version`만 허용하여 bundled Bun을 준비합니다.
   이후 root 소유권을 복원한 상태에서 일반 Adapter `check`, version과 config 검증을
   실행합니다. 기존 후보와 새로 준비된 Bun executable은 같은 ownership chain 내부의
   canonical non-symlink 안전 mode 파일이어야 하며, root-owned Adapter 경로도 이
   불변성을 다시 검증합니다. local smoke는 manifest의
   exact Bun version과 실제 service가 보고하는 `bunVersion`이 같고 runtime source가
   `bundled`인지 검증합니다.

   ```bash
   sudo ./pilot/scripts/upgrade-opencodex.sh apply "$VERSION"
   sudo ./pilot/scripts/external-smoke-test.sh
   ```

   `--skip-smoke`는 의도적으로 중지한 호스트에서만 허용하며 일반 운영 단축 경로가
   아닙니다. external smoke는 실제 TTY에서 credential을 숨김 prompt로 받고 두 실제
   Responses stream overlap까지 검증합니다.

   검토가 끝난 구형 호스트가 이미 요청 버전이지만
   `/etc/opencodex/expected-version`보다 먼저 설치된 경우, 같은 `apply VERSION`
   명령은 조기 종료하지 않고 패키지 변경 없는 상태 채택을 수행합니다. 설치 패키지
   manifest, `opencodex.service`의 활성화·부팅 활성화, loopback `/healthz` JSON을
   모두 확인한 뒤에만 상태 파일을 기록합니다. npm registry 조회 없이 이 채택만
   수행하려면, 설치 릴리스를 독립적으로 검토한 뒤 아래의 명시적 명령을 사용합니다.

   ```bash
   sudo ./pilot/scripts/upgrade-opencodex.sh adopt-current "$VERSION"
   ```

   `adopt-current`는 패키지, 서비스 설정, 서비스 lifecycle을 변경하지 않습니다.
   실제 업그레이드 뒤에 필요한 전체 smoke 및 external-smoke 검사를 대체하는 명령이
   아니라 상태 기록 복구 경로입니다.

4. Remote Control 호스트는 한 대씩 업데이트합니다. 확인 플래그는 의도적입니다.
   managed app-server가 재시작되어 진행 중인 Remote 작업을 끊을 수 있습니다.

   ```bash
   ~/.local/lib/opencodex-relay/update-remote-codex.sh status
   ~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
     set-default-model --allow-remote-interruption
   ~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-default-model
   ~/.local/lib/opencodex-relay/update-remote-codex.sh apply --allow-remote-interruption
   ```

   업데이터는 공식 standalone 설치기를 전용 `CODEX_HOME`, 비공개
   `CODEX_INSTALL_DIR`, non-interactive 모드로 실행합니다. 이후 보이는 launcher를
   복구하고, 설치된 Codex 버전에 맞춰 OpenCodex catalog를 갱신하며, 필요한 경우
   daemon을 재시작하고 Remote Control·`gpt-5.6-luna` managed 기본값·catalog·daemon·
   로컬 WebSocket handshake를 검증합니다. 현재 26개 catalog는 중앙 Cursor adapter 제거를
   반영하며, updater는 별도 host-local Cursor filter를 추가하거나 Cursor를 기본값으로
   복원하지 않습니다.

5. Codex Desktop에서 이미 열린 terminal Codex 프로세스를 종료하고 다시 실행한 뒤
   **Remote Control**을 새로고침합니다. `codex debug models`의 모델 수와 Remote
   호스트 연결됨 상태를 확인합니다.

## 복구와 금지된 단축 경로

두 스크립트는 복구 자료를 삭제하지 않고 보관합니다.

- OpenCodex: `/var/backups/opencodex/upgrade-<from>-to-<to>.*`
- Remote Codex: `~/.codex-remote-opencodex/.upgrade-backups/upgrade-<from>.*`

OpenCodex는 패키지 설치, config 검증, 서비스 시작, 기본 smoke 중 하나라도 실패하면
이전 package prefix와 서비스 상태를 복원합니다. Remote Codex는 검증이 실패하면
이전 standalone link와 catalog를 되돌린 뒤 Remote Control을 재시작합니다. 수동 정리는
보관된 backup을 조사한 뒤에만 수행합니다.

관리 호스트의 정기 업데이트에는 `npm update`, `latest` 같은 mutable npm tag,
OpenCodex Dashboard self-update, `ocx update`, `bootstrap-host.sh`를 사용하지 않습니다.
이 경로들은 명시 버전 계약, 서비스 상태 복구, Remote launcher 복구, smoke gate를
하나의 작업으로 보장하지 않습니다.

상위 저장소의 `opencodex/`는 별도 upstream checkout이며 운영 배포 수단이 아닙니다.
소스 작업에서는 해당 checkout의 자체 지침에 따라 갱신하되, 관리형 프록시 호스트에는
검토한 publish package 버전을 `upgrade-opencodex.sh`로 승격합니다.

성공·실패 결과는 private deployment overlay에 날짜 고정된 content-free 항목으로
추가하고, host alias, exact snapshot path와 unit/drop-in hash는 gitignored evidence에만
둡니다. 현재 health나 한 번의 배포 관측은 formal deployment gate를 닫지 않습니다.

## Codex 경계의 공식 근거

Codex 공식 설정 문서는 `CODEX_HOME`이 standalone 상태의 루트이고,
`CODEX_INSTALL_DIR`이 노출되는 명령 위치이며, `CODEX_NON_INTERACTIVE=1`이 자동 설치용임을
명시합니다. 또한 `model_catalog_json`은 시작 시 로드됩니다. 명령 문서는
`codex remote-control`과 managed app-server 경로를 설명합니다.

- [Codex 설정 참조](https://learn.chatgpt.com/docs/config-file/config-reference)
- [Codex CLI 명령 참조](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
- [Codex Remote connections](https://learn.chatgpt.com/docs/remote-connections)
