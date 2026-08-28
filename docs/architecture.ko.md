# 아키텍처와 신뢰 경계

> 이 파일은 `architecture.md`의 한국어 판본입니다. 명령, 경로, 식별자, URL과
> 프로토콜 용어는 원문과의 호환성을 위해 필요한 경우 그대로 유지했습니다.

## 목적

OCI VM은 상호 신뢰하는 소수의 클라이언트를 위한 중앙 집중식 OpenCodex 계정 풀 및
Responses API 게이트웨이입니다. 멀티테넌트 서비스가 아니며 사용자별 RBAC,
테넌트 격리 자격증명 또는 독립적인 사용량 할당량을 제공하지 않습니다.

## 데이터 평면

```text
Native Codex CLI / AppServer / Desktop 구성
  | 기본 제공 openai provider -> 로컬 loopback relay
  v
127.0.0.1:18180 opencodex-relay
  | CF-Access-Client-Id / CF-Access-Client-Secret
  | X-OpenCodex-API-Key (이 지점에서만 주입)
  v
Cloudflare Access
  v
Named Tunnel connector
  v
127.0.0.1:18080 Nginx
  | 정확한 경로/메서드 allowlist
  | 게이트웨이 키 검증
  | 하나의 공유 생성 연결
  v
127.0.0.1:10100 OpenCodex
  v
선택된 ChatGPT/Codex 계정
```

공용 API는 다음의 정확한 호환 route만 노출합니다.

- `GET /v1/models`
- `POST /v1/responses`
- `POST /v1/responses/compact`
- `POST /v1/images/generations`, `POST /v1/images/edits`
- `GET /v1/opencodex/artifacts/{opaque-id}`, `POST /v1/alpha/search`
- Responses WebSocket upgrade 및 중앙 Voice gate를 명시적으로 켠 뒤의 GPT-Live/Realtime
  setup·sideband WebSocket route

Nginx는 Dashboard 파일, `/api/*`, `/healthz`와 그 밖의 모든 API 표면에 대해 `404`를
반환합니다. OpenCodex로 전달하기 전에 Cloudflare 및 게이트웨이 자격증명을 제거합니다.
Voice는 기본적으로 `404`이며 native relay와 중앙 gateway는 각각 별도로 opt-in해야 합니다.
한쪽을 켠다고 다른 쪽이 켜지지 않습니다.

client 등록, catalog/AppServer 수명주기, Desktop 기능 경계와 rollback은
[`local-codex-relay.ko.md`](local-codex-relay.ko.md)를 정본으로 사용합니다. 이 data plane은
Codex AppServer transport 자체를 인터넷에 공개하는 구조가 아닙니다.

## 관리 평면

권장 관리 경로는 기존 OpenSSH 인증 앞에 대화형 Cloudflare Access 게이트를 추가합니다.

```text
Mac OpenSSH
  -> 클라이언트 측 cloudflared
  -> Cloudflare Access (exact email One-time PIN + independent TOTP)
  -> 별도 SSH hostname 및 Named Tunnel
  -> OCI cloudflared -> 127.0.0.1:22 OpenSSH
  -> 기존 SSH 개인 키 인증
```

Cloudflare, DNS, ID 흐름 또는 클라이언트 측 `cloudflared`를 사용할 수 없을 때 복구할 수
있도록 기존 직접 경로도 계속 사용할 수 있습니다.

```text
Mac OpenSSH -> OCI 공용 TCP/22 -> OpenSSH -> 기존 SSH 개인 키 인증
```

이 경로에는 다음 네 가지 명시적 불변 조건이 있습니다.

- SSH hostname, Access 애플리케이션, 정책과 AUD는 API 애플리케이션과 분리됩니다.
- 두 경로 모두 동일한 OpenSSH 데몬에서 종료되며, 문서화된 클라이언트 별칭은 동일한
  SSH 개인 키를 사용합니다.
- 공용 `22/tcp` 직접 경로는 의도적으로 Cloudflare Access를 우회하며 OCI 인그레스
  규칙의 적용을 받습니다.
- Cloudflare Tunnel을 통해 Dashboard HTTP 경로를 게시하지 않습니다.

이 저장소는 호스트의 `sshd_config`를 프로비저닝하지 않습니다. 운영 수용 게이트는
`sshd -T`를 사용해 공개 키 인증을 요구하고 password, keyboard-interactive, host-based,
GSSAPI 및 빈 비밀번호 대안을 거부합니다. 이 실시간 검증을 통과하기 전까지 키 전용 서버
인증은 아키텍처의 사실이 아니라 검증되지 않은 배포 가정입니다.

Dashboard와 관리 API는 OpenCodex의 loopback listener에 계속 남습니다. 어느 SSH 경로로든
인증을 완료한 뒤 관리자는 local forward를 통해 접근합니다.

```text
Mac 127.0.0.1:11010 -> SSH -> OCI 127.0.0.1:10100 OpenCodex Dashboard
```

현재 ChatGPT 브라우저 로그인은 두 번째 forward를 사용합니다.

```text
Mac localhost:1455 -> SSH -> OCI 127.0.0.1:1455
```

이는 서로 별개의 관심사입니다. SSH hostname을 추가해도 Dashboard가 게시되거나 OAuth
`redirect_uri`가 변경되지 않습니다. [Dashboard와 OAuth](dashboard-and-oauth.ko.md)를
참조하세요.

## 포트 목록

| 포트 | 바인드 | 소유자 | 노출 범위 |
| --- | --- | --- | --- |
| `22/tcp` | wildcard OpenSSH listener | OpenSSH | 직접 SSH를 위한 OCI 인그레스; 별도 Tunnel hostname의 `ssh://127.0.0.1:22` origin |
| `10100/tcp` | `127.0.0.1` | OpenCodex | 호스트 로컬 및 SSH forwarding만 |
| `1455/tcp` | 로그인 대기 중 `127.0.0.1` | OpenCodex OAuth callback | SSH forwarding만 |
| `18080/tcp` | `127.0.0.1` | Nginx 게이트웨이 | cloudflared origin만 |
| `20241/tcp` | `127.0.0.1` | cloudflared 메트릭 | 호스트 로컬만 |

client relay는 등록된 각 장치의 `127.0.0.1:18180`에만 listen합니다. 서버 listener나
Cloudflare origin이 아닙니다.

OCI security list/NSG는 `10100`, `1455`, `18080`, `20241`을 노출해서는 안 됩니다.

## 자격증명 경계

| 자격증명 | 종료 지점 | 목적 |
| --- | --- | --- |
| Cloudflare service token | Cloudflare Access/cloudflared | 승인된 장치를 API hostname에 허용 |
| Exact-email One-time PIN 및 독립 TOTP | Cloudflare Access | 별도 SSH hostname에 대화형 접근 허용 |
| Nginx 게이트웨이 키 | Nginx | origin 또는 Access 정책 오류가 OpenCodex에 도달하지 않도록 차단 |
| OpenCodex 계정 토큰 | OpenCodex 자격증명 저장소 | 상위 ChatGPT/Codex 호출 인증 |
| SSH 개인 키 | 관리자 장치/OpenSSH | 어느 SSH 전송 경로를 사용하든 호스트 인증 |

하나의 자격증명을 다른 경계에 재사용해서는 안 됩니다.

## 지속성과 복구

- `opencodex.service`는 전용 `opencodex` 사용자로 proxy를 시작합니다.
- `nginx.service`는 로컬 API 게이트웨이를 관리합니다.
- `cloudflared.service`는 systemd `LoadCredential=`을 통해 토큰을 읽습니다.
- 세 서비스 모두 `multi-user.target`에 대해 enable되어야 하며 제어된 재부팅 후 다시
  확인해야 합니다.
- 공용 SSH 직접 경로는 Tunnel 또는 Access 장애 시 독립적인 복구 경로로 남습니다.
  계속 사용할 수 있는지는 대화형 SSH hostname과 별도로 테스트해야 합니다.
- Dashboard 시작 경고가 호스트의 systemd 상태보다 우선하지 않습니다. 이 배포는
  의도적으로 OpenCodex 자체 launcher shim을 사용하지 않습니다.

표준 unit/구성 파일은 [`../pilot/`](../pilot/)에 있습니다.
