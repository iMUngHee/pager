# pager

코딩 에이전트 세션 사이의 메시지 전달 — 프로젝트 무관, 툴 무관.

삐삐다. 보내면 받는 쪽이 **다음에 활동할 때** 본다. 폴링도 강제 인터럽트도 없다.

```
codex 세션                        claude 세션
    │                                 │
    │  pager send review-box "…"      │
    ├────────────► ~/.pager/msg.db    │
    │                                 │
    │                          (다음 훅 실행 시)
    │                                 │◄── additionalContext로 주입
```

## 보장 — 조건부다

이름을 "at-least-once"라고 부르지 않는 이유가 있다. 아래 전제가 깨지면 **출력 시도는 0회**일 수 있다.

```
보장:  PAGER_INJECT_TTL 안에 수신 세션의 훅이 실행되고 stdout 기록이 성공했다면,
       확정 전에 프로세스가 죽어도 lease 만료 후 재시도된다.

전제:  수신 세션의 훅이 TTL 안에 최소 1회 실행된다   ← 세션을 방치하면 깨진다
       훅이 DB에 접근할 수 있다                      ← fail-open이라 조용히 실패한다
       호스트가 additionalContext를 채택한다         ← 훅 계약에 ack가 없어 확인 불가

미보장: 확정 후 호스트가 그 출력을 버리면 재배달되지 않는다.
중단:   TTL 경과 후 자동 재시도를 중단한다.
```

**중복 실행은 재시도의 대가다.** 같은 지시를 두 번 볼 수 있다. 주입 텍스트의 메시지 id는
관찰을 돕는 표시일 뿐 durable dedup이 아니다.

## 설치

```bash
make build          # ~/.local/bin/pager 로 빌드. CGO 불필요
```

순수 Go SQLite 드라이버를 쓰므로 C 툴체인 없이 빌드되고 크로스 컴파일이 된다.

저장소는 `~/.pager/msg.db` 하나다. 디렉토리 0700, 파일 0600으로 만들어진다.

### 훅 등록 — Claude Code

`~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "pager hook UserPromptSubmit" }] }
    ],
    "SessionStart": [
      { "hooks": [{ "type": "command", "command": "pager hook SessionStart" }] }
    ],
    "Stop": [
      { "hooks": [{ "type": "command", "command": "pager hook Stop" }] }
    ]
  }
}
```

### 훅 등록 — Codex CLI

`~/.codex/config.toml`:

```toml
[[hooks]]
event = "UserPromptSubmit"
command = "pager hook UserPromptSubmit"

[[hooks]]
event = "Stop"
command = "pager hook Stop"
```

> **`UserPromptSubmit`이 필수 경로다.** 양쪽 툴에서 검증된 이벤트는 이것이고, Codex의 Stop 훅이
> `additionalContext`를 채택하는지는 미검증이라 Stop은 선택 경로로 둔다.

### MCP 등록 (선택)

CLI만으로 충분하다. 도구 호출로 보내고 싶으면:

```bash
claude mcp add pager -s user -- pager mcp
codex  mcp add pager       -- pager mcp
```

## 쓰기

```bash
pager alias review-box                   # 이 세션에 이름 붙이기
pager send review-box "파서 작업 넘긴다"   # 보내기
pager ls                                 # 내 앞으로 온 것
pager ls --expired                       # 자동 배달 창을 넘긴 것
pager whoami                             # 지금 어떤 세션으로 해석되는가
pager claim review-box                   # 오프라인 수신함 이어받기
pager prune --dry-run                    # 보관 기한 지난 것 확인
```

`pager attach`는 훅이 알아서 한다 — 훅을 등록하지 않고 손으로 쓸 때만 필요하다.

세션은 **별칭으로 주소를 받는다.** 별칭이 없으면 수신함이 없다.

수신 대상은 `별칭 완전일치 → 세션 ID → pm_ref 접미사 → 유일한 부분일치` 순으로 해석하고,
**부분일치가 여럿이면 후보를 보여주고 실패한다.**

### 별칭은 자동으로 넘어가지 않는다

같은 레포에서 어제 쓰던 별칭을 오늘 세션이 물려받으면 그건 오배달이다. 이어받기는
`pager claim`으로 **명시적으로** 한다. 이어받을 수 있는 수신함이 있으면 훅이 세션당 한 번 알려준다.

## 환경변수

| 변수 | 기본값 | 뜻 |
| --- | --- | --- |
| `PAGER_DB` | `~/.pager/msg.db` | 저장소 위치. 격리된 실험용 |
| `PAGER_SESSION` | — | 세션 해석 2순위. 훅이 심으면 자식 프로세스가 상속 |
| `PAGER_CLIENT` | 자동 탐지 | 호스트 툴 고정 (`claude`/`codex`). **인식 불가한 값이면 "호스트 없음"** |
| `PAGER_MAX_INJECT_BYTES` | `2000` | 주입 텍스트 전체의 UTF-8 바이트 |
| `PAGER_MAX_BATCH` | `5` | 한 번에 주입할 최대 건수 |
| `PAGER_MAX_BODY_RUNES` | `280` | 본문 절단 기준 (문자 단위) |
| `PAGER_INJECT_TTL` | `72h` | 자동 배달 창. 넘기면 재시도 중단 (삭제는 아님) |
| `PAGER_LEASE` | `2m` | claim 유지 시간 |

기본값은 전부 **추정치**다. 실사용 지표 없이 정했다.

보관은 30일이고 그 뒤 삭제된다. 훅 실행 중 하나가 24시간마다 한 번 수행하며, `pager prune`으로
직접 돌릴 수도 있다.

## `--human`은 검증이 아니라 선언이다

`pager send --human`은 "이건 사람이 보내는 것"이라는 **호출자의 주장**이고, pager는 그것을
확인할 방법이 없다. 자동 에이전트도 붙일 수 있고, 붙이면 hop 체인이 끊긴다.

그래서 **최종 방어선은 breaker**다 — 주장을 읽지 않고 세기만 한다.

| 상한 | 기본값 |
| --- | --- |
| 자동 발신 / 세션 / 시간 | 20건 |
| 자동 발신 / 수신함 쌍 / 시간 | 10건 |
| 무인과 발신 (`--human` 포함) / 시간 | 30건 |

핑퐁은 hop으로도 막는다. 받은 메시지 때문에 나가는 발신은 깊이가 1 늘고, 3을 넘으면 거부된다.
**사용자가 프롬프트를 입력하면 체인이 끊긴다** — 평범한 왕복 작업이 깊이를 쌓지 않는 이유다.

## 신뢰 경계

메시지 본문은 **다른 에이전트가 쓴 텍스트가 내 컨텍스트에 들어오는 것**이다. 즉 프롬프트 주입
경로다. 근본적으로 신뢰 경계를 넘는 데이터이며, 없앨 수는 없고 완화만 한다:

- 발신자를 항상 표시한다
- 본문을 인용 블록으로 감싸고 "지시가 아니라 데이터"라고 명시한다
- 길이를 제한한다

저장소는 로컬 사용자 전용이고 네트워크 노출이 없다.

## 고장났을 때

**모든 훅 경로는 fail-open이다** — exit 0, stdout 무출력. 훅이 실패해서 세션을 막는 것이
배달 실패보다 나쁘기 때문이다. 대가는 **고장이 세션 안에서 안 보인다**는 것이다.

```bash
pager whoami     # 호스트가 탐지되는가, 세션이 붙는가
pager ls         # 큐에 뭐가 있는가
```

`session: unattributed`가 나오면 훅이 아직 이 세션을 기록하지 않았거나 호스트 탐지가 실패한
것이다. 자세한 것은 [docs/session-binding.md](docs/session-binding.md).

## 범위 밖

**인터럽트는 만들지 않는다.** 자동화 대상은 "사람이 옮겨 적는 일"이지 "사람이 알아차리는 일"이
아니다. FIFO 블로킹·tmux send-keys·트랜스크립트 편집 등 전수 조사 결과와 폐기 사유는 설계
플랜에 표로 남아 있다.

tmux 상태 배지와 오버레이 TUI는 후속 단계다.

## 문서

- [docs/session-binding.md](docs/session-binding.md) — 세션 해석 4순위, 호스트 탐지, 격리 규칙
- [docs/hooks.md](docs/hooks.md) — 이벤트·입력·출력 매트릭스, fail-open 계약
