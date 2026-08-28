# PW OpenCodex E2.1.Micro 파일럿 자산

Ubuntu 24.04 `amd64` 신규 인스턴스에 OpenCodex를 별도 사용자로 설치하고,
loopback 전용 Nginx API 게이트웨이와 Cloudflare Tunnel을 연결하기 위한 최소 자산입니다.
이 디렉터리에는 계정 토큰, Cloudflare Tunnel 토큰, 실제 도메인, Access AUD 또는 OCI
식별자가 없습니다. 외부 검증 대상은 실행 시 환경 변수로 전달합니다.

## 고정된 경계

```text
API hostname:
Cloudflare Access (service token) -> cloudflared -> 127.0.0.1:18080 (Nginx)
                                                   -> 127.0.0.1:10100 (OpenCodex)

SSH hostname:
Cloudflare Access (exact email PIN + independent TOTP)
  -> client-side cloudflared -> Named Tunnel -> 127.0.0.1:22 (OpenSSH + SSH key)
```

- OCI/호스트 인바운드에는 `10100`과 `18080`을 열지 않습니다. 기존 공인 `22/tcp`는
  Cloudflare 장애 시 직접 복구 경로로 유지합니다.
- Nginx는 root 전용 키 파일로 `X-OpenCodex-API-Key`를 직접 검증합니다.
- Nginx public allowlist는 `/v1/models`, `/v1/responses`,
  `/v1/responses/compact`, `/v1/images/generations`, `/v1/images/edits`,
  단일-segment `/v1/opencodex/artifacts/{id}`, `/v1/alpha/search`입니다.
  Voice/Realtime 경로는 root-owned flag 값이 정확히 `1`일 때만 추가로 열립니다.
- `/api/*`, GUI, OpenCodex `/healthz`는 HTTP Tunnel route로 공개하지 않습니다.
- 별도 SSH hostname만 `ssh://127.0.0.1:22`로 연결하며 기존 SSH key 인증을 유지합니다.
- Nginx의 generation 경계는 정상 요청을 직렬화하지 않는 전역 safety ceiling `32`이며,
  Responses WebSocket은 별도 `32` ceiling을 사용합니다. turn lifecycle·취소·account 선택은
  OpenCodex가 소유합니다.
- Responses WebSocket은 정확한 `/v1/responses` GET upgrade만 허용하며 별도 safety
  ceiling을 사용합니다. Voice WebSocket은 위 double opt-in flag가 켜진 정확한 경로만
  허용합니다. 그 밖의 API WebSocket과 catch-all `/v1/*`는 차단합니다. 별도 SSH
  hostname의 client-side `cloudflared` 비HTTP 전송은 이 제한과 별개입니다.
- OpenCodex와 Nginx는 모두 loopback에만 바인드합니다.

## 적용 순서

서버로 이 디렉터리를 복사한 뒤 다음을 수행합니다.

```bash
cd pilot
VERSION='REPLACE_WITH_REVIEWED_EXACT_VERSION'
sudo ./scripts/configure-swap.sh 4G
sudo env OPENCODEX_VERSION="${VERSION}" ./scripts/bootstrap-host.sh
```

1GB VM에서 npm 설치 중 메모리가 부족하지 않도록 swap을 먼저 만듭니다.
`configure-swap.sh`가 sysctl 정책도 함께 설치하므로 이 순서에서도
`vm.swappiness=10`과 `vm.vfs_cache_pressure=50`이 즉시 적용됩니다.
`VERSION`은 npm tag가 아니라 사전에 검토한 exact semver여야 합니다.

`bootstrap-host.sh`는 관리 unit, `/opt/opencodex`, package manifest, Runtime Adapter,
`runtime.json` 또는 `expected-version`이 하나도 없는 **신규 호스트 전용**입니다.
스크립트가 소유한 root 전용 recovery marker가 남은 중단 설치는 고정된 신규 자산을
정리한 뒤 재시도하지만, marker가 없는 완료 설치나 출처 불명의 부분 설치 흔적은
apt, systemd, npm 변경 전에 종료합니다.
이미 Runtime Adapter로 관리되는 호스트의 릴리스 갱신은
`upgrade-opencodex.sh`를 사용하고, 기존 legacy unit/drop-in을 Runtime Adapter로
이관할 때는 snapshot과 rollback을 제공하는 `configure-opencodex-runtime.sh`를
사용합니다. 기존 호스트에 bootstrap을 재실행하지 않습니다.

`bootstrap-host.sh`는 다음까지만 수행합니다.

- Ubuntu/아키텍처 검사
- Node.js 18 이상, npm, Nginx, logrotate 및 진단 도구 설치
- 파일럿에 불필요한 `rpcbind.socket`과 `rpcbind.service` 비활성화
- `opencodex` 시스템 사용자와 전용 경로 생성
- 검토된 OpenCodex 버전을 `/opt/opencodex`에 설치
- systemd, Nginx, logrotate, sysctl 템플릿 설치
- 실제 키가 설정되기 전에는 모든 Nginx 요청을 거부하는 root 전용 map 설치
- OpenCodex 서비스는 계정 설정 전이므로 활성화하거나 시작하지 않음

그다음 전용 사용자로 신규 설정을 진행합니다.

```bash
sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx setup
sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx config set hostname 127.0.0.1
sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx config set port 10100 --json
sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx config validate
```

`ocx setup`의 다음 두 질문에는 중앙 API 서버에 불필요한 로컬 통합을 만들지 않도록
반드시 `n`으로 답합니다.

```text
Inject into Codex config.toml? [Y/n]: n
Install Codex autostart shim? [Y/n]: n
```

여러 계정 로그인과 풀 구성도 위 Runtime Adapter를 통해 `sudo -u opencodex`로 실행합니다.
먼저 `sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx help account`로 현재 버전의
비대화형·수동 코드 로그인 옵션을 확인한 뒤 계정별 로그인을 수행합니다. OAuth
코드와 토큰은 명령행 인수나 이 저장소에 넣지 않습니다.

### PW Dashboard와 신규 OpenAI 계정

관리 UI는 Cloudflare API hostname이나 별도 Dashboard HTTP hostname에 공개하지 않습니다.
관리자의 Mac에서 기존 로컬 OpenCodex `10100`과 충돌하지 않는 `11010`으로 Dashboard를
포워딩하고, OAuth callback `1455`도 같은 SSH 세션에서 전달합니다. 초기 구축과
Cloudflare 장애 중에는 아래 공인 IP 경로를 사용하고, SSH Access 설정이 끝난 뒤에는
[`../docs/ssh-and-client-access.md`](../docs/ssh-and-client-access.md)의
`opencodex-relay-access` 경로를 우선 사용합니다.

```bash
ssh -N \
  -L 11010:127.0.0.1:10100 \
  -L 1455:127.0.0.1:1455 \
  -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=15 \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_INSTANCE_IP
```

Edge 등에서 `http://127.0.0.1:11010`을 열고 다음 경로를 사용합니다.

```text
프로바이더 → OpenAI (Codex login) → 사용 가능한 계정 → 추가
```

브라우저의 `http://localhost:1455/auth/callback`은 SSH 포워딩을 통해 PW의 대기 중인
로그인 흐름으로 자동 전달됩니다. callback URL이나 코드를 복사하지 않습니다. 신규
계정을 등록한 뒤 유효한 계정의 `계정 선택`을 눌러 만료된 `main` 자리표시자가 현재
계정으로 남지 않게 하고, 상태가 `준비됨`·`연결됨`인지 확인합니다. 계정 풀의 자동
전환은 새 세션부터 적용됩니다.

Dashboard가 자체 설치 방식만 검사해 재시작 보호가 없다고 표시하더라도, 이 파일럿의
부팅 복구 권한은 `/etc/systemd/system/opencodex.service`가 담당합니다. 실제 판정은
Dashboard 문구가 아니라 `systemctl is-enabled/is-active opencodex.service`와 재부팅
검증으로 합니다.

그다음 Nginx가 검증할 별도 게이트웨이 키를 표준 입력이나 숨김 프롬프트로
설정합니다. 키는 32~128자의 base64url 또는 16진수 형식이어야 합니다.

```bash
sudo ./scripts/configure-gateway-key.sh
```

스크립트는 원문 키를 `/etc/opencodex/gateway-api-key`, Nginx map을
`/etc/nginx/private/opencodex-api-key-map.conf`에 각각 `root:root 0600`으로 저장하고
`nginx -t` 후 reload합니다. 키를 출력하거나 저장소에 복사하지 않습니다.

계정과 키 구성이 완료된 후에만 서비스를 시작합니다.

```bash
sudo systemctl enable --now opencodex.service
sudo systemctl enable --now nginx.service
sudo ./scripts/smoke-test.sh
```

이 systemd unit은 OpenCodex 자체의 사용자 서비스 등록부와 별개이며
Runtime Adapter를 통해 OpenCodex를 시작합니다. 운영 중 시작·중지·재시작은
직접 `ocx start/stop`이나 관리 API를 호출하지 않고 다음처럼 systemd로 수행합니다.

```bash
sudo systemctl restart opencodex.service
sudo systemctl stop opencodex.service
```

## Native relay를 쓰는 원격 Codex 클라이언트

새 macOS ARM 및 Linux 클라이언트는 custom provider profile 대신
[`../client/relay/README.ko.md`](../client/relay/README.ko.md)의 static native relay를 사용합니다.
relay가 `127.0.0.1:18180`에서만 Cloudflare Service Token과 Nginx gateway key를 주입하고,
Codex CLI·AppServer·Desktop 구성은 기본 제공 `openai` provider를 유지합니다. signed
manifest·SHA-256 검증, Keychain/`0600` credential file, Voice double opt-in 및 업데이트는
해당 runbook을 따릅니다.

legacy custom-provider profile과 Codex process 환경 변수에 admission credential을 직접
넣는 절차는 폐기되었습니다. 신규 enrollment, migration, rollback, credential rotation은
[`../docs/local-codex-relay.ko.md`](../docs/local-codex-relay.ko.md)를 정본으로 사용합니다.

## SSH Remote Control용 Codex 홈

OCI에서 Codex Desktop의 Remote Control을 사용할 때는 일반 `~/.codex`와 분리한
`/home/ubuntu/.codex-remote-opencodex`를 사용합니다. 이 홈에는 해당 사용자에게
허용된 `auth.json`, native relay를 가리키는 `openai_base_url` 설정, 그리고 인증된
`/v1/models` 응답으로 만든 `opencodex-catalog.json`만 둡니다. 인증 파일이나
Cloudflare/Nginx 자격증명 원문은
저장소·unit·셸 기록에 넣지 않습니다.

`scripts/codex-remote-home-wrapper.sh`는 사용자 PATH의
`~/.local/bin/codex`에 설치되어 일반 CLI, SSH app-server proxy, Remote Control daemon이
같은 전용 홈·catalog·provider를 사용하게 합니다.
`scripts/manage-remote-codex-home.sh`는 catalog의 모델 식별자와 중복을 검증한 뒤
0600 임시 파일을 원자적으로 교체합니다.

legacy/external Remote의 전용 config root 기본 모델은 bare `gpt-5.6-luna`입니다.
local-relay는 현재 root가 `responses.model_modes`의 key와 대소문자 무시 exact
일치하면 `bounded_json`, 아니면 materialized catalog의 `.slug // .id`와 byte-exact로
한 번 일치할 때 `passthrough`로 분류합니다. 두 경우 모두 선택된 root를 그대로
보존합니다. 여기서 passthrough는 Relay의 request 정규화를 생략한다는 뜻이며, 경로는
계속 Relay에서 loopback OpenCodex로 향합니다. 중앙 서버에서 Cursor
호환 adapter를 제거하면서 현재 배포 catalog는 과거 40개에서 26개로 바뀌었고,
`cursor/*` 항목은 더 이상 배포되지 않습니다. 이는 Remote host-local filter가 아니라
중앙 `Model_Catalog` 변경입니다. 다음 명령은 local-relay 밖에서는 전용 config를 원자적으로
보정하고 값이 실제로 바뀐 경우에만 daemon을 재시작합니다. local-relay에서는 catalog나
policy가 잘못됐거나 root가 누락·중복·미등재이면 config를 쓰거나 daemon을 재시작하지 않고
중단합니다. `status`는 유효한 분류를
`default_model_relay_mode=bounded_json|passthrough`로 함께 출력합니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  set-default-model --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-default-model
```

legacy/loopback `systemd/opencodex-remote-catalog-refresh.timer`는 부팅 후 2분, 이후 약
10분마다 catalog를 확인합니다. external relay mode에서는 relay가 catalog를 쓰고,
`systemd/opencodex-remote-relay-catalog-activation.timer`가 약 1분마다 relay marker만
확인합니다. installer는 routing mode에 맞는 timer 하나만 활성화합니다. relay mode에서
일반 `refresh --restart`도 `relay_catalog_refresh=owned_by_relay`만 출력하므로 전용
activation timer와 재시작 경쟁을 만들지 않습니다. 바뀐 경우에만 해당 mode의 service가
managed app-server를 재시작하므로 다음 Remote UI 연결과 새 CLI 프로세스에 수정된 모델
목록이 노출됩니다. 변경이 없으면 daemon을 재시작하지 않습니다.

공식 managed Codex 설치 또는 업데이트가 `~/.local/bin/codex`를 덮어쓸 수 있으므로,
`opencodex-remote-codex-wrapper-repair.path`도 함께 활성화해야 합니다. 이 path unit과
catalog 갱신은 모두 검증된 wrapper를 다시 설치합니다. 사용자 systemd가 SSH 로그아웃
후에도 실행되도록 한 번만 linger를 켭니다.

```bash
sudo loginctl enable-linger ubuntu

# repository checkout root에서 ubuntu 사용자로 실행합니다. installer가
# remote-opencodex.json의 routing_mode에 맞는 timer 하나만 활성화합니다.
./pilot/scripts/install-remote-codex-home.sh install --bootstrap-remote-control
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh status
```

아직 daemon이 없는 최초 host는 위 flag를 첫 호출부터 사용해야 합니다. 이미 Remote
Control daemon을 bootstrap한 host에서 managed asset만 다시 설치할 때에만 plain
`install`을 사용합니다.

운영 확인은 새 Codex 프로세스로 수행합니다. `codex debug models`의 JSON에서
`.models | length`가 기대한 수인지 확인하고, `codex app-server daemon version`이
단순히 `running`인 것뿐 아니라 `managedCodexVersion`, `cliVersion`,
`appServerVersion`이 모두 같은 managed standalone 버전인지 확인합니다.
`~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-daemon`은 이 불변식을
변경 없이 검증합니다. 이미 열려 있는 Codex의 모델 선택 화면은 시작 시 읽은 목록을
유지하므로 종료 후 다시 열어야 합니다.

일반 restart가 전용 Remote home의 구형·비관리 AppServer 때문에 거부되면 process-wide
kill이나 `SIGKILL`을 사용하지 않습니다. maintenance window에서만 다음 명시적 복구를
사용합니다. 이 명령은 같은 `CODEX_HOME`의 승인된 daemon pid loop와 Unix socket
AppServer command shape만 `TERM`으로 종료한 뒤 current managed daemon을 bootstrap하고,
버전 일치를 확인해야 pending marker를 지웁니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  recover-daemon --allow-remote-interruption
```

전용 `CODEX_HOME`을 쓰는 wrapper를 SSH login directory(`/home/ubuntu`)에서 실행할 때,
일반 `~/.codex/config.toml`이 project config로 겹쳐 전용 home의 model·reasoning 값을
덮지 않아야 합니다. 아래 명시적 조치는 **전용 Remote config 안에서만**
`/home/ubuntu` project를 `untrusted`로 표시하고 daemon을 재시작합니다. 일반
`~/.codex/config.toml`이나 그 안의 user preference는 수정하지 않습니다.

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  isolate-home-project-config --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-home-project-config
```

catalog는 upstream `visibility: "hide"`만 제거하고, 그 밖의 visible model은 account
선택에 따라 사용 가능성이 달라도 그대로 보존합니다. 특정 account에서 일시적으로
거부된 model을 host-local rule로 숨기지 않습니다. account-aware model을 선택한 뒤에는
새 CLI/Remote UI에서 catalog와 실제 Responses 요청을 다시 확인합니다.

`openai_base_url`이 loopback OpenCodex 또는 local relay를 가리킬 때 model picker는
base URL override 경고를 보일 수 있습니다. 이는 built-in `openai` provider의 proxy
경로가 있다는 정보이며 자체로 routing 실패 증거가 아닙니다. 이 경고를 없애려고
base URL을 제거하면 OpenCodex 경로를 우회하므로, authenticated catalog와 실제
Responses 결과로 판정합니다.

중앙 OpenCodex와 각 Remote Codex standalone의 계획된 업데이트, 자동 catalog 반영,
백업·롤백 순서는 [`../docs/updates.ko.md`](../docs/updates.ko.md)를 따릅니다.
일상적인 업데이트에서 `ocx update`, Dashboard self-update, `npm update` 또는
`bootstrap-host.sh`를 대신 사용하지 않습니다.

Ubuntu 24.04에서 `Codex's Linux sandbox uses bubblewrap` 경고가 나면 user namespace
전역 제한을 끄지 말고, root로 `./scripts/configure-codex-linux-sandbox.sh --user ubuntu`를
실행합니다. 이 스크립트는 배포판 `bwrap`과 전용 AppArmor profile만 설치·검증합니다.

## Cloudflare Tunnel

초기 UI 작업에서 Cloudflare Zero Trust에 Named Tunnel과 별도의 API·SSH Access
애플리케이션을 생성합니다. **API의 Service Auth·AUD·origin JWT 검증과 SSH의 interactive
Access·independent MFA를 각각 완성하고, 검증되지 않은 hostname route를 활성화하지
않습니다.** SSH 단계에서는 Cloudflare가
표시하는 토큰을 `/etc/cloudflared/tunnel.token`에 `root:root 0600`으로 저장합니다.
`systemd/cloudflared.service`는 systemd `LoadCredential=`로 이 파일을 런타임 전용
credential 디렉터리에 복사하고, 전용 `cloudflared` 사용자에서 `--token-file`로
읽습니다. 토큰을 `--token`, 환경 변수, unit 파일 또는 셸 기록에 넣지 않습니다.
APT로 설치한 패키지는 `--no-autoupdate`로 자체 업데이트를 끄고 APT로만 갱신합니다.
실제 토큰은 이 저장소에 기록하지 않습니다.

`bootstrap-host.sh`는 Cloudflare 패키지 저장소를 추가하지 않습니다. Cloudflare의 공식
Linux 설치 문서에 따라 `cloudflared`를 설치한 뒤, unit이 기대하는 경로를 먼저
검증합니다. 최신 설치 명령은 고정 복사하지 않고 공식 문서를 확인합니다.

- <https://developers.cloudflare.com/tunnel/downloads/>

```bash
command -v cloudflared
test -x /usr/bin/cloudflared
cloudflared --version
```

`/usr/bin/cloudflared`가 없으면 아래 unit을 설치하거나 활성화하지 않습니다. 바이너리
설치 후 서비스 준비는 자격증명을 넣기 전에 수행할 수 있습니다.

```bash
sudo groupadd --system cloudflared 2>/dev/null || true
sudo useradd --system --gid cloudflared --home-dir /var/lib/cloudflared \
  --shell /usr/sbin/nologin cloudflared 2>/dev/null || true
sudo install -d -o root -g root -m 0700 /etc/cloudflared
sudo install -d -o cloudflared -g cloudflared -m 0700 /var/lib/cloudflared
sudo install -o root -g root -m 0644 \
  systemd/cloudflared.service /etc/systemd/system/cloudflared.service
sudo systemctl daemon-reload
sudo systemd-analyze verify /etc/systemd/system/cloudflared.service
```

Access 애플리케이션과 정책을 먼저 검증한 뒤, Tunnel 토큰을 숨김 입력 또는 표준
입력에서 받아 원문을 출력하지 않는 절차로 root 전용 파일에 설치하고 서비스를
활성화합니다. 브라우저에서 복사한 토큰을 명령행 인수에 붙여넣지 않습니다.

```bash
sudo install -o root -g root -m 0600 /dev/stdin \
  /etc/cloudflared/tunnel.token
sudo systemctl enable --now cloudflared.service
```

Dashboard-managed Tunnel을 권장합니다. 공개 hostname의 origin service는 다음으로
설정합니다.

```text
http://127.0.0.1:18080
```

이 API hostname의 Access 정책은 action을 `Service Auth`, Include selector를 발급한
특정 Service Token으로 설정합니다. `Any Service Token`, `Allow`, `Bypass` 정책은
추가하지 않고, Additional settings의 `401 Response for Service Auth policies`를
활성화합니다. 이 파일럿은 Dashboard HTTP hostname을 만들지 않습니다. 나중에 브라우저
사용자용 Dashboard가 필요해지면 별도 보안 검토 후 API 정책과 겹치지 않는 hostname과
interactive `Allow` 정책으로 설계합니다. Tunnel의 API origin 설정에서는 Access JWT
검증을 활성화하고 Zero Trust team name과 해당 API application AUD를 지정합니다.
개인용이라도 generic 공유 token 대신 신뢰하는 클라이언트별 token을 권장합니다. 새
클라이언트 등록은 `token 발급 -> 클라이언트 비밀 저장소 기록 -> exact selector 정책 추가
-> 외부 검증` 순서이고, 회수는 해당 exact selector를 정책에서 제거하는 것으로 시작합니다.

### Cloudflare Access SSH

SSH는 API와 같은 Named Tunnel을 사용하지만 hostname, Access application, AUD와 정책은
완전히 분리합니다. 이 경로는 기존 공인 `22/tcp`를 제거하거나 대체하지 않습니다.
공인 IP 직접 경로는 Cloudflare Access 범위 밖에서 OpenSSH 정책만 적용되는 의도된 복구
경로입니다. Cloudflare의 `Bypass` 정책을 추가한다는 의미가 아닙니다. 이 저장소는
`sshd_config`를 배포하지 않으므로 `smoke-test.sh`의 effective-config 검사가 통과하기 전에는
서버가 실제로 key-only라고 간주하지 않습니다.

먼저 Zero Trust의 Identity providers에서 `One-time PIN`을 활성화합니다. 그다음 Access
settings의 independent MFA에서 `Authenticator application`만 허용하고 다음을 적용합니다.

- `Use identity provider MFA`: 끔. IdP의 `otp` 표시로 Access TOTP가 생략되지 않게 함
- `Apply global MFA settings by default`: 끔. API의 Service Auth application에 영향 방지
- App Launcher에서 `REPLACE_WITH_ADMIN_EMAIL`로 email PIN을 통과한 뒤 TOTP를 등록
- TOTP seed, PIN, Access cookie와 복구 자료를 저장소나 로그에 기록하지 않음

SSH hostname에 대한 Self-hosted Access application은 다음 값으로 만듭니다.

| 항목 | 값 |
| --- | --- |
| Hostname | `REPLACE_WITH_SSH_ACCESS_HOSTNAME` 한 개. wildcard 사용 안 함 |
| Session duration | `24h`. 고정 설치된 개인용 관리자 Mac의 장시간 코딩 작업 기준 |
| Policy action | `Allow` |
| Include | `Emails = REPLACE_WITH_ADMIN_EMAIL` 정확히 한 주소 |
| Require | `Login Methods = One-time PIN` |
| Policy session duration | `Same as application session duration` (`24h`) |
| Independent MFA | Custom, `Authenticator application`만, custom duration `24h` |
| Binding Cookie | 끔. non-HTTP SSH WebSocket과 충돌 방지 |
| Authenticate with Cloudflare One Client | 끔 |
| Automatic cloudflared authentication | 최초 검증 동안 끔 |

여러 `Include`는 OR이므로 `Include Login Methods = One-time PIN`을 사용하지 않습니다.
그 설정은 정확한 이메일 제한 없이 모든 유효 이메일을 허용할 수 있습니다. SSH
application에는 API의 `Service Auth`, service token, `Any Service Token`, `Bypass`, API AUD,
Nginx gateway key를 추가하지 않습니다. TOTP는 등록 secret의 소지를 증명하지만 특정
물리 기기에 바인드된 device-posture 검사는 아닙니다. 선택한 `24h` MFA 지속 시간은 고정
설치된 개인용 관리자 Mac에서 작업일을 넘는 코딩 세션과 일시적인 재연결을 허용하기 위한
운영 절충입니다. 최초 접근 또는 24시간이 지난 접근에는 email PIN과 TOTP가 다시 필요합니다.
애플리케이션이나 MFA 지속 시간이 만료되어도 이미 열린 SSH 연결을 강제로 끊거나 모든
패킷을 다시 인증하지는 않습니다. 계정 전역 세션은 이 애플리케이션 변경 범위에 포함하지
않습니다.

Access application과 정책을 먼저 저장한 뒤 기존 Tunnel에 Published application route를
추가합니다.

```text
Hostname: REPLACE_WITH_SSH_ACCESS_HOSTNAME
Service:  SSH
Target:   localhost:22
```

Cloudflare가 표시하는 origin 표현은 `ssh://localhost:22`입니다. 클라이언트에는
`cloudflared`가 필요하며 `ProxyCommand cloudflared access ssh --hostname %h` 뒤에 기존
OpenSSH key 인증이 이어집니다. 구체적인 SSH config, host-key 확인과 Dashboard/OAuth
forward는 [`../docs/ssh-and-client-access.md`](../docs/ssh-and-client-access.md)를 따릅니다.

공식 문서:

- <https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-cloudflared-authentication/>
- <https://developers.cloudflare.com/cloudflare-one/access-controls/access-settings/independent-mfa/>
- <https://developers.cloudflare.com/cloudflare-one/access-controls/policies/>

롤백은 SSH Published application route와 그 Access application만 비활성화하거나 제거합니다.
기존 API route, connector unit과 공인 `22/tcp` 직접 경로는 변경하지 않습니다.

`cloudflared` 설치/서비스 생성 후에는 다음을 확인합니다.

```bash
sudo systemctl status cloudflared --no-pager
sudo journalctl -u cloudflared -n 100 --no-pager
```

공식 설치 및 서비스 문서:

- <https://developers.cloudflare.com/tunnel/setup/>
- <https://developers.cloudflare.com/tunnel/advanced/local-management/as-a-service/linux/>
- <https://developers.cloudflare.com/tunnel/advanced/origin-parameters/>

Dashboard-managed 방식 대신 로컬 관리 Tunnel을 선택할 때만 API와 SSH route가 모두 든
`cloudflared/config.yml.example`을 별도 검토해 사용합니다. 두 방식을 섞지 않습니다.

## 런타임 검증

### 로컬 게이트웨이

`smoke-test.sh`는 다음을 실제로 검사합니다.

구형 호스트가 이미 검토한 버전으로 실행 중인데 `/etc/opencodex/expected-version`만
없는 경우에는 `sudo ./pilot/scripts/upgrade-opencodex.sh adopt-current VERSION`으로
패키지 변경 없이 설치 manifest, 활성화·부팅 활성화된 서비스, loopback health를 확인한
뒤 상태 파일을 기록할 수 있습니다. 실제 업그레이드의 smoke 검사를 대체하지 않습니다.

- `/etc/opencodex/expected-version`에 기록된 OpenCodex 버전, 전용 프로세스 사용자,
  unit 활성화·부팅 활성화
- loopback 리스너만 존재하는지
- SSH 22 외 wildcard TCP listener가 없고 TCP/UDP 111이 닫혔는지
- 4GB swap, sysctl, cgroup 메모리·태스크 한도
- Nginx와 logrotate 전체 구성
- 무키·오류 키 401과 `X-OpenCodex-Gateway-Rejection: api-key`, 정상 키 200,
  관리·비허용 경로 404
- 느린 요청 본문 중 sibling generation endpoint가 Nginx에 의해 429로 직렬화되지 않는지
- 같은 시점의 Responses WebSocket probe가 generation slot과 충돌하지 않고 upstream의
  비활성화 응답 `426`에 도달하는지
- generation과 Responses WebSocket의 독립된 emergency ceiling이 각각 `32`인지

overlap 검증용 첫 요청은 본문 전송이 끝나기 전에 종료하므로 provider 요청을
실행하지 않습니다. ceiling `32`는 overload 보호값이며 provider/account scheduler가 아닙니다.

### Cloudflare Access SSH

interactive SSH는 API service-token smoke test와 별도 경계입니다. 깨끗하거나 만료된
Access session에서 email PIN, independent TOTP, 기존 SSH key가 차례로 필요하고 다음이
모두 성립하는지 [`../docs/testing.md`](../docs/testing.md)에 따라 수동 검증합니다.

- `opencodex-relay-access`와 공인 IP 복구 alias가 각각 새 SSH 연결에 성공
- 두 경로의 OpenSSH host-key fingerprint가 동일
- Access 경로에서 Dashboard `11010`과 OAuth callback `1455` local forward가 동작
- 제어된 재부팅 후 두 SSH 경로와 `cloudflared.service`가 다시 동작

성공 여부와 시각만 기록하고 PIN, TOTP, Access cookie, SSH verbose log는 보관하지 않습니다.

### Cloudflare와 SSE

클라이언트에서 mode `0600`인 임시 curl 헤더 파일을 만들고 다음 세 자격증명을
각각 한 줄의 HTTP 헤더로 넣습니다. 파일 자체는 저장소 밖에 둡니다.

```text
CF-Access-Client-Id: ...
CF-Access-Client-Secret: ...
X-OpenCodex-API-Key: ...
```

그다음 공개 hostname에서 다음을 확인합니다.

```bash
# Access 자격증명 없음: 2xx가 아니어야 함
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://REPLACE_WITH_API_HOSTNAME/v1/models

# Access + gateway 키 모두 정상: 200
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H @/path/to/root-or-user-only.headers \
  https://REPLACE_WITH_API_HOSTNAME/v1/models

# 정상 자격증명이어도 관리 API는 404
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H @/path/to/root-or-user-only.headers \
  https://REPLACE_WITH_API_HOSTNAME/api/config
```

오류 게이트웨이 키와 정상 Cloudflare 서비스 토큰 조합도 별도 헤더 파일로 실행해
401과 `X-OpenCodex-Gateway-Rejection: api-key`가 함께 반환되는지 확인합니다.
이 origin 전용 마커가 없으면 Cloudflare Access의 401일 수 있으므로 실패로
처리합니다. SSE는 실제 사용 가능한 모델 ID로 한 번만 호출해 첫 이벤트가 지연
없이 도착하고 마지막에 완료 이벤트가 오는지 확인합니다.

```bash
MODEL_ID='REPLACE_WITH_MODEL_ID'
jq -nc --arg model "${MODEL_ID}" \
  '{model:$model,input:[{type:"message",role:"user",content:[{type:"input_text",text:"Reply with OK."}]}],stream:true,store:false,max_output_tokens:16}' |
timeout 120 curl -N --no-buffer -sS \
    -H @/path/to/root-or-user-only.headers \
    -H 'Content-Type: application/json' \
    --data-binary @- \
    https://REPLACE_WITH_API_HOSTNAME/v1/responses
```

배포된 서버에서는 위 검사를 `scripts/external-smoke-test.sh`로 한 번에 실행할 수
있습니다. root 전용 게이트웨이 키는 파일에서 직접 읽고, Cloudflare Access Client
ID와 Secret은 대화형 프롬프트에서만 받습니다. 두 값은 셸 인수·환경 변수·로그에
기록하지 않고 mode `0600` 임시 헤더 파일로만 사용한 뒤 종료 시 삭제합니다.

```bash
cd /home/ubuntu/pilot
sudo env \
  PUBLIC_BASE_URL='https://REPLACE_WITH_API_HOSTNAME' \
  EXPECTED_ACCESS_AUD='REPLACE_WITH_ACCESS_APPLICATION_AUD' \
  ./scripts/external-smoke-test.sh
```

스크립트는 유효한 게이트웨이 키만 전송해 Access 음성 경계를 분리 검증하고, 정상
Access·게이트웨이 조합, 잘못된 게이트웨이 키, 관리 API 차단을 차례로 확인합니다.
실제 모델 응답은 `Content-Type: text/event-stream`과 JSON SSE 프레임을 파싱해
`response.created`, 텍스트 delta, 마지막 `response.completed`를 요구하고 실패·불완전
이벤트를 거부합니다. overlap 검증은 긴 `/v1/responses` 스트림이 활성 상태일 때 두 번째
실제 Responses SSE를 시작하여 둘 다 `response.completed`로 끝나는 것을 요구합니다.
경합으로 첫 요청이 먼저 끝나면 최대 세 번까지만 재시도합니다. 기본으로
`/v1/models`의 첫 모델을 선택하며 특정 모델을 검증하려면 root 셸에서만 `MODEL_ID`를
지정합니다.

### 재부팅 복구

Cloudflare API와 SSH 검증까지 끝난 뒤 한 번 정상 재부팅하고 두 SSH 경로로 각각
재접속한 후 다시 검사합니다.

```bash
sudo systemctl reboot
# SSH 재접속 후
cd pilot
sudo ./scripts/smoke-test.sh
sudo systemctl is-active cloudflared.service
sudo journalctl -b -u opencodex -u nginx -u cloudflared --no-pager
```

## 운영 기준

- 기본 swap은 `4G`, `vm.swappiness=10`입니다. swap은 OOM 안전망이지 RAM 확장이 아닙니다.
- `opencodex.service`는 `MemoryHigh=650M`, `MemoryMax=800M`, `MemorySwapMax=2G`입니다.
- 단일 활성 요청을 유지하고, 장시간 `vmstat`의 `si`/`so`가 발생하면 인스턴스가 작습니다.
- OpenCodex 파일 로그는 10MiB 또는 매주 회전하며 7세대를 보관합니다. Nginx 로그는
  Ubuntu `nginx-common`의 기존 `/var/log/nginx/*.log` 규칙에 맡겨 중복 정의하지 않습니다.
- 관리 작업은 SSH에서만 수행합니다. SSH transport는 공인 `22/tcp` 직접 경로나 별도
  Cloudflare Access SSH hostname일 수 있습니다. 관리 UI/API는 HTTP Tunnel hostname에
  연결하지 않습니다.
- 최초 재부팅 후 OpenCodex, Nginx, cloudflared가 모두 자동 복구되는지 다시 검사합니다.

관찰 명령:

```bash
free -h
vmstat 1
cat /proc/pressure/memory
systemctl status opencodex nginx cloudflared --no-pager
journalctl -u opencodex -n 100 --no-pager
```

## 제거

파일럿 서비스만 중단하려면:

```bash
sudo systemctl disable --now opencodex.service nginx.service cloudflared.service
```

이 명령은 계정 또는 상태 파일을 삭제하지 않습니다. `/var/lib/opencodex` 제거는
자격증명과 기록의 영구 삭제이므로 별도 승인 후 수행합니다.
