# SSH, Dashboard 및 Codex 클라이언트 접속

> 이 파일은 `ssh-and-client-access.md`의 한국어 판본입니다. 명령, 호스트 별칭,
> 포트, 환경 변수, 자격증명 이름과 URL은 실행 호환성을 위해 그대로 유지했습니다.

## 현재 장비 목록

장비별 값은 Git에서 의도적으로 제외된 `../local/connection.local.md`에 저장합니다.
SSH 개인 키는 `~/.ssh` 아래에 남겨 두며 이 프로젝트에는 복사하지 않고 참조만 합니다.

## 직접 관리 및 복구

```bash
ssh -tt \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_INSTANCE_IP
```

외부 Cloudflare smoke test를 포함해 TTY를 통해 프롬프트를 표시하는 스크립트에는
`-tt`를 사용합니다. 이 경로는 OCI 공용 `22/tcp`와 SSH 키를 직접 사용하며
Cloudflare Access를 통과하지 않습니다. 선택된 이중 경로 설계를 사용하는 동안 복구
경로로 계속 유지합니다.

## Cloudflare Access SSH

권장 경로는 기존 Named Tunnel에 전용 SSH hostname을 사용하는 것입니다. 이는 API
hostname이 아니며 HTTP를 통해 Dashboard를 게시하지 않습니다. 서버 측 route와 Access
application은 [`../pilot/README.md`](../pilot/README.md#cloudflare-access-ssh)에 설명된
대로 먼저 구성해야 합니다.

관리자 장치에 `cloudflared`를 설치하고 절대 경로를 기록합니다.

```bash
command -v cloudflared
cloudflared --version
```

Access application은 다음 두 가지 서로 다른 challenge를 통해 정확히 한 개의 이메일을
허용합니다.

1. Cloudflare email One-time PIN
2. authenticator-app TOTP를 사용하는 Cloudflare Access 독립 MFA

Access가 허용된 뒤에도 OpenSSH는 기존 SSH 개인 키를 계속 요구합니다. TOTP는 등록된
secret의 소유를 증명하며 WARP device-posture나 hardware-bound device check가 아닙니다.
고정된 단일 사용자 관리자 Mac mini에서는 Access application session과 독립 MFA session이
모두 `24h`이며 연결된 policy는 application session duration을 상속합니다.
이를 통해 새 work session 또는 만료된 work session을 시작할 때 email PIN,
authenticator TOTP와 SSH-key 경계를 적용하면서, 동일하게 인증된 Mac에서 장시간
coding-agent 실행과 일반적인 reconnect를 허용합니다.

이 duration은 Access connection을 시작하거나 갱신하기 위한 window입니다. 만료되었다고
모든 SSH packet을 다시 인증하거나 이미 수립된 SSH connection을 강제로 중단하지는
않습니다. Access 또는 MFA session이 만료된 후의 새 connection은 email PIN과 TOTP를
다시 통과해야 합니다. 이 application별 선택은 계정의 global session, API Service Auth
application 또는 별도로 관리하는 SSH Access application을 변경하지 않습니다.

## SSH 구성

두 alias는 이 repository가 아니라 사용자의 `~/.ssh/config`에 모두 유지합니다.
`command -v cloudflared`의 절대 결과로 `cloudflared` 경로를 바꿉니다.

Cloudflare Access SSH 경로만 여는 최소 macOS 구성은
[`ssh-config.macos.ko.example`](ssh-config.macos.ko.example)를 복사하고 검증합니다.
예제 파일은 사용자의 `~/.ssh/config`에 자동 설치되지 않습니다.

```sshconfig
Host opencodex-relay-access
    HostName REPLACE_WITH_SSH_ACCESS_HOSTNAME
    User ubuntu
    IdentityFile ~/.ssh/REPLACE_WITH_SSH_KEY
    IdentitiesOnly yes
    ExitOnForwardFailure yes
    ServerAliveInterval 15
    ServerAliveCountMax 3
    ProxyCommand REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h
    LocalForward 11010 127.0.0.1:10100
    LocalForward 1455 127.0.0.1:1455

Host opencodex-relay-direct
    HostName REPLACE_WITH_INSTANCE_IP
    User ubuntu
    IdentityFile ~/.ssh/REPLACE_WITH_SSH_KEY
    IdentitiesOnly yes
    ExitOnForwardFailure yes
    ServerAliveInterval 15
    ServerAliveCountMax 3
    LocalForward 11010 127.0.0.1:10100
    LocalForward 1455 127.0.0.1:1455
```

두 hostname은 서로 다른 `known_hosts` identity입니다. 첫 SSH-hostname prompt를
승인하기 전에 이미 신뢰하는 직접 경로를 통해 예상 host-key fingerprint를 확인합니다.

```bash
ssh -o ClearAllForwardings=yes opencodex-relay-direct \
  'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
```

해당 fingerprint를 첫 Access-hostname connection에 표시되는 값과 정확히 비교합니다.
그다음 필요에 따라 권장 경로 또는 복구 경로를 사용합니다.

```bash
ssh opencodex-relay-access
ssh -o ClearAllForwardings=yes opencodex-relay-direct
```

두 alias는 기본 forward가 동일한 로컬 포트를 사용하므로 동시에 기본 설정으로 실행하지
않습니다. Dashboard 또는 OAuth forwarding이 필요하지 않은 복구 명령에는
`ClearAllForwardings=yes`를 사용해도 됩니다.

### Tunnel hostname에 일반 SSH를 직접 연결하면 실패하는 이유

Access hostname은 OCI 인스턴스의 공인 TCP/22 주소가 아니라 Cloudflare Tunnel 진입점입니다.
Named Tunnel이 SSH origin으로 연결하려면 **클라이언트 측 `cloudflared`**가 먼저
Cloudflare Access SSH transport를 수립해야 합니다. 다음처럼 일반 SSH를 실행하면 그
프로세스가 시작되지 않습니다.

```bash
ssh -N \
  -L 11010:127.0.0.1:10100 \
  -L 1455:127.0.0.1:1455 \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_SSH_ACCESS_HOSTNAME
```

`-L`은 SSH transport가 수립된 뒤 local forwarding을 요청할 뿐이고, `-i`는 연결이
`sshd`에 도달한 뒤에 사용할 SSH key만 제공합니다. 두 옵션 모두 raw TCP/22 SSH를
Cloudflare Access SSH protocol로 바꾸지 않습니다. 따라서 연결은 필요한 Access/Tunnel
client transport 없이 Cloudflare edge 주소에 도달하므로 실패하는 것이 정상입니다.

흔한 직접 원인은 SSH configuration의 매칭입니다. 다음과 같은 구성이 있을 때,

```sshconfig
Host opencodex-relay-access
    HostName REPLACE_WITH_SSH_ACCESS_HOSTNAME
    ProxyCommand REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h
```

`ssh opencodex-relay-access`만 `Host opencodex-relay-access` stanza와 일치합니다. 확장된
hostname을 직접 입력하면 이 정확한 alias와 일치하지 않으므로 OpenSSH는 해당
`ProxyCommand`를 상속하지 않습니다. 평상시 경로에서는 alias를 사용합니다.

```bash
ssh -N opencodex-relay-access
```

alias 대신 hostname을 의도적으로 사용하는 일회성 명령에는 동일한 proxy를 명시합니다.

```bash
ssh -N \
  -L 11010:127.0.0.1:10100 \
  -L 1455:127.0.0.1:1455 \
  -o ExitOnForwardFailure=yes \
  -o 'ProxyCommand=REPLACE_WITH_CLOUDFLARED_PATH access ssh --hostname %h' \
  -i ~/.ssh/REPLACE_WITH_SSH_KEY \
  ubuntu@REPLACE_WITH_SSH_ACCESS_HOSTNAME
```

반대로 OCI 공인 IP에 대한 직접 SSH가 성공하는 이유는 이 배포가 공용 `22/tcp`를 독립적인
복구 경로로 의도적으로 유지하기 때문입니다.

```text
Mac OpenSSH -> OCI public IP:22 -> OCI sshd -> SSH key
```

이 경로는 Cloudflare Access를 우회하며 Tunnel hostname이 raw SSH를 수용한다는 뜻이
아닙니다. 권장 경로는 다릅니다.

```text
Mac OpenSSH -> ProxyCommand cloudflared access ssh -> Cloudflare Access
  -> Named Tunnel -> OCI 127.0.0.1:22 -> OCI sshd -> SSH key
```

연결을 진단하기 전 effective configuration을 비교합니다. alias에는 `proxycommand`가
보여야 하며, 별도 설정이 없는 확장 hostname에는 보이지 않습니다.

```bash
ssh -G opencodex-relay-access | rg '^(hostname|proxycommand|localforward)'
ssh -G REPLACE_WITH_SSH_ACCESS_HOSTNAME | rg '^(hostname|proxycommand|localforward)'
```

## Dashboard 및 OAuth 터널

기존 로컬 OpenCodex가 이미 `10100`을 사용할 수 있으므로 원격 Dashboard는 로컬
`11010` 포트를 사용합니다. 권장 Access 경로를 시작하거나 Cloudflare 장애 중에는
`opencodex-relay-direct`로 바꿉니다.

```bash
ssh -N opencodex-relay-access
```

그다음 다음 주소를 엽니다.

```text
http://127.0.0.1:11010/#codex-auth
```

계정 로그인 중 브라우저의 `http://localhost:1455/auth/callback` request는 동일한
SSH session을 통해 OCI 호스트의 대기 중인 callback listener로 전달됩니다.

이 `1455` forward는 ChatGPT/Codex callback 흐름에만 사용합니다. Cursor OAuth는 별도의
서버 측 PKCE polling 흐름이므로 `opencodex` 서비스 계정으로 등록하고, 출력된 Cursor URL을
관리자 Mac에서 엽니다. [SSH를 통한 OCI Cursor OAuth 등록](cursor-oauth-over-ssh.ko.md)을
참조하세요.

### macOS Dashboard launcher

[`../client/macos/open-dashboard.sh`](../client/macos/open-dashboard.sh)는 자격증명을
저장하거나 우회하지 않고 평상시 Access SSH 경로를 자동화합니다. 선택한 SSH alias에
`cloudflared access ssh` `ProxyCommand`가 있는지 검증하고, Dashboard와 ChatGPT callback
forward를 포함하는 전용 SSH ControlMaster를 시작한 뒤 macOS에서 Dashboard를 엽니다.

기본 alias는 현재 `ocx-ssh`입니다. 일반 예시 alias 또는 검증된 다른 Access alias를 쓸
때는 바꿉니다. 해당 alias는 문서화된 `11010 -> 127.0.0.1:10100`,
`1455 -> 127.0.0.1:1455` forward를 유지해야 합니다. Access session이 만료된 뒤 처음
시작할 때는 여전히 브라우저에서 일반적인 Cloudflare email/TOTP 승인이 필요합니다.

```bash
./client/macos/open-dashboard.sh
./client/macos/open-dashboard.sh status
./client/macos/open-dashboard.sh stop

# 일반 문서 alias를 사용하고 브라우저 창은 열지 않음
./client/macos/open-dashboard.sh --host opencodex-relay-access --no-open
```

launcher는 Cloudflare Access `ProxyCommand`가 없는 alias를 의도적으로 거부하며, OCI
공인 IP 복구 경로로 조용히 fallback하지 않습니다. `start`가 끝난 뒤에도 tunnel이 유지되도록
`~/.ssh` 아래에 자체 SSH control socket을 만들기 때문에 `stop`으로 종료해야 합니다.
stale control socket이 보고되면 제거하기 전에 먼저 상태를 확인하며, 활성 SSH socket을
삭제하지 않습니다.

## 서버 상태

```bash
sudo systemctl is-enabled opencodex nginx cloudflared
sudo systemctl is-active opencodex nginx cloudflared
sudo ss -lntup
sudo journalctl -b -u opencodex -u nginx -u cloudflared --no-pager
```

예상 application listener는 loopback 전용 `10100`, `18080`, `20241`입니다.
`1455`은 OAuth flow가 대기 중일 때만 존재합니다.

## 원격 Codex 클라이언트 (native relay)

새 macOS·Linux 클라이언트는 [Native Codex 호환 릴레이](local-codex-relay.ko.md)를
사용합니다. 기본 제공 `openai` provider를 유지하고 Cloudflare Service Token과 별도의
gateway key는 Codex 환경 변수나 custom provider table이 아니라 로컬 relay에만 둡니다.
signed 설치와 플랫폼별 credential 설정은
[`../client/relay/README.md`](../client/relay/README.md)를 따릅니다.

외부에 있는 Remote Control Linux host도 같은 방식입니다. 전용 catalog를 지정해
`linux/amd64` 또는 `linux/arm64` relay를 설치한 뒤
`configure-remote-codex-routing.sh enable-relay --allow-remote-interruption`를 실행합니다.
이 작업은 managed AppServer를 의도적으로 재시작하므로 live session을 안전하게 유지하는
작업이 아닙니다.

Voice는 client relay와 gateway feature gate를 모두 명시적으로 켜기 전까지 중앙에서
차단됩니다. 호환 `/v1` route를 사용하지 않는 Desktop native image 또는 voice control
plane은 이 설정으로 redirect되지 않고 원래 native 경로를 유지합니다.

## Legacy custom-provider rollback

이전 direct custom-provider 등록 절차는 더 이상 active runbook이 아닙니다. 새
`pw_opencodex` profile을 만들거나 admission credential을 Codex process 환경에 export하지
않습니다. 기존 client를 되돌려야 할 때는
[Native Codex 호환 계층 운영 가이드](local-codex-relay.ko.md#update와-rollback)의
timestamp backup 검사·복원 절차를 따릅니다.

credential rotation은 admission plane 작업으로 남습니다. Cloudflare Service Token은
replacement를 검증할 때까지 old/new selector를 overlap할 수 있지만, 현재 공유 Nginx
gateway key를 회전할 때는 등록된 모든 relay를 함께 갱신해야 합니다.
