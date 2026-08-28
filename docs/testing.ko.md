# 검증 자산 및 증거

> 이 파일은 `testing.md`의 한국어 판본입니다. 명령, 경로, 환경 변수, endpoint와
> 검증값은 실행·판정 호환성을 위해 원문과 동일하게 유지했습니다.

전체 플랫폼의 점검 주기, 변경·rollback 순서와 날짜 고정 결과는 private deployment
overlay에 보존합니다. 이 문서는 각 검증 계층의 실행 방법과 판정 경계만 소유합니다.

## 로컬 자산 검사

프로젝트 루트에서 실행합니다.

```bash
bash -n pilot/scripts/*.sh ops/oci/*.sh client/relay/scripts/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test -count=1 ./... && go vet ./...)
(cd client/relay && go test -race -count=1 ./internal/handoff ./internal/routing)
(cd client/relay/macos/OpenCodexRelay && swift test && swift build)
git diff --check
```

별도 upstream `opencodex/` source를 함께 변경한 경우에는 outer clone이 해당 checkout을
제공하거나 pin하지 않는다는 점을 전제로, reviewed baseline과 nested diff를 먼저 확인한 뒤
다음을 별도로 실행합니다. 현재 변경의 baseline은
[`d9de89557c3bd154e5f1508125def7c8789ac8c5`](https://github.com/lidge-jun/opencodex/commit/d9de89557c3bd154e5f1508125def7c8789ac8c5)입니다.

```bash
git -C opencodex rev-parse HEAD
(cd opencodex && bun run typecheck && bun run test && bun run privacy:scan)
git -C opencodex diff --check
```

Python 테스트는 완전한 Responses SSE parsing을 검증하고 다음을 거부합니다.

- SSE가 아닌 `Content-Type`
- 실패 또는 오류 terminal event
- `response.completed` 이후의 data
- Chat Completions 방식의 `[DONE]` frame
- 불완전한 partial frame을 완전한 event로 잘못 처리하는 경우

이 단계는 source, script와 문서 계약의 정적 증거입니다. client relay, 중앙 gateway,
Desktop/Remote UI 또는 Voice의 현재 가용성을 증명하지 않습니다. 호환 계층의 계층별
배포 완료 판정은 [`local-codex-relay.ko.md`](local-codex-relay.ko.md)의 “검증 계층과
완료 조건”을 따릅니다.

exact Relay teardown profile에서 artifact 재구성, closure/hash 일치, adapter import/preflight와
Swift/Go test는 정적·격리 계약만 증명합니다. 자동 제거 acceptance에는 disposable macOS에서
사용자 data 보존, native Codex 복원, service·launcher 제거, 재탐색 확인과 중단 전이 복구를
추가로 수행해야 합니다. Compose render 또는 image build도 실제 container나 provider 경로를
증명하지 않습니다.

## OCI 호스트 smoke test

호스트 구성을 설치하거나 변경한 뒤 다음을 실행합니다.

```bash
cd /home/ubuntu/pilot
sudo ./scripts/smoke-test.sh
```

이 테스트는 service version과 identity, loopback 전용 listener, 비활성화된 RPC port
`111`, swap과 cgroup limit, Nginx/logrotate syntax, gateway key admission, endpoint blocking,
generation/WebSocket의 독립된 safety ceiling `32`, sibling generation 비직렬화 및
비활성 Responses WebSocket의 `426` fallback을 검증합니다. overlap probe는 request body를
열어 둔 채 대기하며 upstream model을 호출하지 않습니다.

## 외부 Cloudflare/SSE 테스트

자격증명을 숨김 prompt로 수집하므로 실제 TTY에서 실행합니다.

```bash
ssh -tt \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_INSTANCE_IP \
  "sudo env PUBLIC_BASE_URL='https://REPLACE_WITH_API_HOSTNAME' \
    EXPECTED_ACCESS_AUD='REPLACE_WITH_ACCESS_APPLICATION_AUD' \
    /home/ubuntu/pilot/scripts/external-smoke-test.sh"
```

이 스크립트는 command-line secret을 요구하지 않으며 mode-`0600` temporary header
file을 종료 시 독립적으로 삭제합니다. 다음을 각각 증명합니다.

1. Cloudflare Access는 service token이 없는 request를 거부합니다.
2. Access와 gateway key를 함께 사용하면 `/v1/models`에 도달합니다.
3. Access admission 이후 Nginx가 잘못된 gateway key를 거부합니다.
4. 공개 route가 OpenCodex management API를 차단합니다.
5. 실제 Responses SSE stream에 `response.created`, 비어 있지 않은 text delta와
   오류 또는 incomplete event가 없는 단 하나의 최종 `response.completed`가 포함됩니다.
6. 첫 public stream이 활성 상태일 때 두 번째 실제 Responses SSE 요청도 완료됩니다.
   Nginx emergency ceiling을 one-request scheduler로 사용하지 않습니다.

새로 등록한 macOS Codex 전용 client에는 다음도 요구합니다.

- Cloudflare client ID, Cloudflare client secret과 Nginx gateway key를 위한 서로 다른
  세 개의 login-Keychain item이 있고, Codex TOML 또는 service definition에 실제 credential이 없음
- `opencodex-relayctl status`가 loopback relay, relay-owned native
  `openai_base_url`, 기대한 catalog 상태를 보고함
- relay 인증 refresh 뒤 `0600` catalog에 비어 있지 않은 visible `.models` array가 있고
  hidden 항목이 없음
- 일반 `codex exec ...` 실행 한 번이 기대한 model을 보고하고 local relay를 통해 비어 있지
  않은 최종 답변을 받음
- Linux Remote Control 전용 home이 `gpt-5.6-luna`에 대해 `default_model_match=1`을 보고함
- local과 central의 명시적 opt-in 모두가 승인되지 않은 한 중앙 Voice gate는 `404`를
  유지함. Voice opt-in에는 실제 media/WebSocket call도 필요하며 HTTP route 수용만으로는
  충분하지 않음

`/v1/models`를 주소창에서 방문하는 것을 이 테스트로 인정하지 않습니다. 브라우저는
Service Token과 gateway header를 보내지 않으므로 Access page 또는 `401`이 예상됩니다.

## 대화형 Cloudflare SSH 테스트

위 API 테스트는 별도의 대화형 SSH Access application을 검증하지 않습니다. 두 alias가
[`ssh-and-client-access.ko.md`](ssh-and-client-access.ko.md)에 설명된 대로 구성된
관리자 장비에서 이 테스트를 수행합니다.

연결하기 전에 배포된 Cloudflare 구성을 검사하고 다음을 모두 요구합니다.

- 기존 Tunnel에서 하나의 정확한 SSH hostname이 `SSH`로 `localhost:22`에 라우팅됨
- wildcard hostname overlap이 없는 별도의 Self-hosted Access application
- application session duration이 `24h`이고 연결된 policy의 session duration도 동일함
- `Allow`와 `Include: Emails = REPLACE_WITH_ADMIN_EMAIL`
- `Require: Login Methods = One-time PIN`
- authenticator-app TOTP만 허용하는 독립 MFA와 `24h` duration
- `Use identity provider MFA`, Binding Cookie, Cloudflare One Client authentication과
  automatic cloudflared authentication이 초기 검증에서 비활성화됨
- `Bypass`, `Service Auth`, `Any Service Token`, 넓은 `Allow` 또는 API
  policy가 SSH application에 연결되지 않음

OCI 호스트에서 `sudo ./pilot/scripts/smoke-test.sh`를 다시 실행합니다. 이 스크립트는
`/etc/opencodex/expected-version`에 기록된 OpenCodex 버전(구형 호스트는 현재 `2.10.1`
fallback)을 검증합니다. 이 legacy fallback에서만 canonical `ocx` launcher가 없으면 설치된
package manifest를 읽을 수 있으며, 관리형 호스트는 계속 명시적 state file과 canonical launcher를
요구합니다. 이미 실행 중인 구형 배포를 독립적으로 검토한 뒤에는
`sudo ./pilot/scripts/upgrade-opencodex.sh adopt-current 2.10.1`로 상태를 기록하여,
이후 smoke가 migration fallback 대신 관리 상태 파일을 사용하게 합니다. 이어서 `sshd -T` 검사는
public-key authentication이 활성화되고 password,
keyboard-interactive, host-based, GSSAPI와 empty password authentication이 비활성화되어
있음을 보고해야 합니다. 저장소는 `sshd_config`를 배포하지 않습니다. listener가
있거나 key login에 성공했다는 사실만으로 fallback authentication이 비활성화되었다고
볼 수 없습니다. 호스트가 `Match` rule을 사용한다면 공용 recovery path를 인정하기
전에 관리자의 실제 source address에 대한 유효 구성을 `sshd -T -C`로 검사합니다.

이미 신뢰하는 직접 경로를 사용해 OpenSSH host-key fingerprint를 기록한 뒤, Access
hostname의 첫 prompt와 정확히 비교합니다.

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
ssh -o ClearAllForwardings=yes opencodex-relay-access \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
```

깨끗하거나 만료된 Access/MFA session에서 Access connection은 허용된 이메일의 PIN,
그 다음 독립 TOTP, 그 다음 기존 SSH 개인 키를 요구해야 합니다. 잘못된 PIN, 잘못된
TOTP 또는 잘못된 SSH key는 각각의 경계에서 실패해야 합니다. 문서화된 `24h` window
안의 reconnect가 유효한 Access와 MFA session을 재사용하는 것은 예상된 동작이며 MFA가
비활성화되었다는 증거가 아닙니다. 두 전송을 독립적으로 확인합니다.

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-access 'test "$(id -un)" = ubuntu'
ssh -o ClearAllForwardings=yes opencodex-relay-direct 'test "$(id -un)" = ubuntu'
```

직접 명령은 Access challenge 없이 성공해야 합니다. 유지된 공용 `22/tcp`는
Cloudflare Access 외부에 있으며 복구 경로이기 때문입니다. 이는 Cloudflare `Bypass`
policy를 사용하지 않습니다. 로컬 listener 검사는 OCI ingress rule을 증명하지 않으므로
이 외부 직접 연결이 계속 필요합니다.

마지막으로 한 터미널에서 권장 forwarding session을 시작하고 실행 상태로 둡니다.

```bash
ssh -N opencodex-relay-access
```

두 번째 터미널에서 Dashboard를 검증합니다.

```bash
curl --fail --silent --show-error http://127.0.0.1:11010/ >/dev/null
```

실제 계정 로그인 한 번을 수행하면서 `localhost:1455/auth/callback` 브라우저
request가 동일한 SSH session을 통해 대기 중인 OCI listener에 도달하는지도 확인합니다.
두 검사가 끝난 뒤 첫 번째 터미널을 `Ctrl-C`로 중지합니다.

제어된 재부팅 한 번 후 두 새 SSH connection을 반복하고, 직접 recovery path를 통해
connector를 확인합니다.

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'systemctl is-enabled cloudflared.service && systemctl is-active cloudflared.service'
```

pass/fail과 timestamp만 기록합니다. email PIN, TOTP 값 또는 seed, Access cookie/token,
verbose SSH log는 보존하지 않습니다.

## 과거 세션 증거

날짜가 고정된 Relay와 OpenCodex 결과 요약 및 안전한 합성 transcript 정본은 private
deployment overlay에 보존하며 여기에는 중복하거나 게시하지 않습니다.

2026-08-01 VS Code terminal은 여섯 가지 외부 assertion이 모두 통과하는 모습을
보였습니다. 캡처된 요약에는 당시 `gpt-5.3-codex-spark`를 사용한 21개 SSE event,
11개 text delta, 최종 `response.completed`와 공유 slot concurrency probe 성공이
보고되었습니다.

이는 과거 증거이며 현재 가용성을 보장하지 않습니다. 원시 response body, Cloudflare
credential 또는 gateway key는 날짜가 기록된 workspace에 보존되지 않았습니다.

## OAuth 탐색 검사

중첩된 OpenCodex branch `agent/remote-dashboard-oauth`에는 대체된 manual-code-only
실험이 포함되어 있습니다. 세션 중 다음의 집중 검사가 통과했습니다.

- callback-server tests: 3 passed
- Codex auth API tests: 140 passed
- GUI add-account OAuth tests: 3 passed
- root TypeScript `tsc --noEmit`: passed

전체 root/GUI/docs regression suite는 완료되지 않았으며, 이 실험은 요청된 custom-domain
callback을 구현하지 않습니다. patch는 research 전용으로 취급합니다.
