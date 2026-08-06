# 세션 바인딩 계약

> I1의 선행 결정. hop 안전성 전체가 이 문서 위에 올라간다. 여기서 정한 것을 바꾸면
> `internal/sessionref`·`internal/deliver`의 확정 조건이 함께 바뀐다.
>
> 관련: `.agents/plans/2026-08-03-pager-core-delivery.md` (Decisions → 세션 바인딩)

## 왜 우회로가 필요한가

**MCP 프로토콜은 호출자의 `session_id`를 주지 않는다.** 세션 5개는 MCP 서버 프로세스
5개(세션당 인스턴스 하나)이고, 환경변수에도 session_id가 없다. 그래서 MCP 경로는 자기
세션을 스스로 알 수 없다.

CLI도 같은 문제를 겪는다. **`pager send`가 핵심 발신 경로**인데, 에이전트가 Bash 툴로
부르는 CLI 역시 자기 session_id를 모른다. MCP만 해결하면 `pager send`가 전부
`unattributed`로 자기 정책에 막힌다.

세션 문맥을 아는 유일한 주체는 **훅**이다. 훅은 stdin으로 `session_id`를 받는다.
따라서 계약은 하나다 — **훅이 기록하고, 나머지가 조회한다.** CLI와 MCP는 같은
resolver(`internal/sessionref`)를 공유한다.

## 검증된 선례 — crux

같은 문제를 crux가 이미 실사용으로 풀었다. 방식을 그대로 따른다.

| 위치 | 역할 |
| --- | --- |
| `internal/infra/client/client.go` | `FromEnv()` — 호스트 툴 라벨 (`claude`/`codex`) |
| `internal/infra/hostproc/hostproc.go` | `DetectHost`/`Instance`/`Key`/`Alive` — 호스트 프로세스 동일성 |
| `internal/contextstore/session/active.go` | `WriteActive`/`ReadActive` — 활성 세션 포인터 |
| `internal/adapter/hook/active.go` | `updateActive` — 훅 쓰기 경로 (best-effort) |
| `internal/adapter/server/server.go:69` | `readActiveSession` — MCP 서버 조회 경로 |

pager가 다른 점은 **저장 위치 하나**다 (아래 "활성 바인딩은 어디 사는가").

## 호스트 프로세스 동일성

훅과 CLI/MCP는 서로 다른 프로세스다. 둘이 같은 세션을 가리키려면 **같은 호스트를
독립적으로 같은 값으로 지목**해야 한다.

```
Instance{ Pid int, Start int64 }
```

`Start`는 플랫폼 상대 프로세스 시작 토큰이다 — darwin은 `kern.proc.pid` sysctl의
`P_starttime`(ms), linux는 `/proc/<pid>/stat` 22번 필드(starttime jiffies). 단위는
무의미하고 **같은 머신의 두 프로세스가 같은 값을 읽는다는 안정성만** 의미가 있다.

탐지는 **조상 체인 상향 탐색**이다:

```
pid = os.Getppid()
최대 16단 상향:
    procInfo(pid) → (ppid, start, cmd)
    cmd의 comm 또는 argv0의 basename이 클라이언트명과 일치하면 → Instance{pid, start}
    아니면 pid = ppid
16단 안에 없으면 → 탐지 실패
```

- MCP 서버는 보통 호스트의 **직계 자식**이다.
- 에이전트 Bash 툴이 실행한 CLI는 셸을 한두 단 거친 **자손**이다.
- 훅도 마찬가지로 자손이다.

세 경로가 모두 같은 조상에 도달하므로 같은 `Instance`를 얻는다.

**전체 argv를 매칭에 쓰지 않는다.** 훅 명령의 인자에는 `~/.codex` 같은 경로가 들어갈 수
있어서, argv 전체를 보면 codex가 아닌 프로세스를 codex 호스트로 오인한다. `comm`과
`argv0`만 본다.

호스트 툴 라벨(`claude`/`codex`)은 `PAGER_CLIENT` 환경변수를 먼저 보고, 없으면 부모
프로세스 이름으로 폴백한다. `PAGER_` 접두사는 `CONTEXT_` 접두사가 아니므로 Claude Code의
`CONTEXT_*` 환경변수 스크럽 대상이 아니다.

## 해석 순위 — CLI·MCP 공통

`internal/sessionref`의 단일 진입점이 이 순서대로 시도한다. **모든 발신 경로가 같은
순위를 쓴다** — CLI든 MCP든 예외가 없다.

| 순위 | 방법 | 적용 경로 |
| --- | --- | --- |
| 1 | `--session <id>` 인자 | 훅 (자기 stdin에 session_id가 있다), 스크립트 |
| 2 | `PAGER_SESSION` 환경변수 | 훅이 심어두면 그 세션의 자식 프로세스 전부가 상속 |
| 3 | 호스트 탐지 → 훅이 기록한 활성 세션 조회 | MCP 서버, 에이전트가 Bash로 부른 CLI |
| 4 | 실패 → `unattributed` | 기본 **거부** (`--human` 없으면) |

## 활성 바인딩은 어디 사는가

**`sessions` 테이블 컬럼.** 별도 포인터 파일을 두지 않는다.

crux가 `~/.crux/active/<key>.json`을 둔 이유는 **훅과 MCP 서버가 서로 다른 저장소를
쓰기 때문**이다 — 서버는 메트릭 DB에 쓰고 세션 문맥은 훅에만 있으니 둘을 잇는 파일이
필요했다. pager에는 그 전제가 없다: 훅·CLI·MCP가 **전부 같은 `~/.pager/msg.db`를 쓴다.**
포인터 파일을 두면 두 번째 저장소와 그 TTL·고아 정리가 통째로 추가 표면이 된다.

```sql
-- sessions 스키마 (I2에서 생성)
host_client TEXT,     -- 'claude' | 'codex'
host_pid    INTEGER,
host_start  INTEGER   -- 플랫폼 상대 시작 토큰
```

`heartbeat_at`이 crux의 `activeTTLms`(12h) 역할을 그대로 한다. 신선도 판정 축이
하나로 합쳐지므로 원자적 rename도, 별도 만료 파일도 필요 없다.

### 훅의 기록 — 최신 승자

같은 호스트에서 `/clear`나 resume을 하면 **호스트 pid는 그대로인데 session_id가 바뀐다.**
그래서 기록은 단순 UPDATE가 아니라 **키 이전**이어야 한다. 두 문장을 한 트랜잭션에서:

```sql
-- 1) 이 호스트 키를 들고 있던 다른 세션에서 떼어낸다
UPDATE sessions SET host_pid = NULL, host_start = NULL
 WHERE host_client = :c AND host_pid = :pid AND host_start = :start
   AND session_id <> :sid;

-- 2) 이 세션에 붙인다
UPDATE sessions
   SET host_client = :c, host_pid = :pid, host_start = :start, heartbeat_at = :now
 WHERE session_id = :sid;
```

**불변식: 하나의 `(host_client, host_pid, host_start)`를 보유한 행은 최대 1개다.**
1번 문장이 그것을 보장하고, 3순위 조회가 그 위에 올라간다. 이 불변식이 깨지면 조회가
복수 행을 만나 어느 세션인지 알 수 없게 되므로 **테스트로 고정한다.**

### 3순위 조회

```sql
SELECT session_id FROM sessions
 WHERE host_client = :c AND host_pid = :pid AND host_start = :start
   AND heartbeat_at >= :cutoff;
```

0행이면 4순위(`unattributed`)로 떨어진다. 위 불변식 덕에 1행을 넘을 수 없다.

## pid 재사용은 왜 오배달이 되지 않는가

crux는 `hostproc.Alive`로 pid 재사용을 따로 막는다. pager에는 그 검사가 **불필요하다** —
`host_start`가 이미 그 일을 한다:

호스트가 죽고 무관한 프로세스가 그 pid를 물려받았다고 하자. 그 프로세스에서 `pager send`를
부르면 조회 키는 **호출자 자신이 탐지한** `(client, pid, start)`다.

- 새 프로세스가 claude/codex 호스트가 아니면 → 조상 매칭 실패 → 탐지 실패 → 4순위
- 새 프로세스가 claude/codex 호스트이면 → 시작 시각이 다르므로 `start`가 다름 → 0행 → 4순위

즉 **조회 키는 살아있는 호출자로부터만 만들어지므로**, 죽은 호스트의 낡은 행에는 도달할
방법이 없다. `heartbeat_at` cutoff가 막는 것은 다른 것이다 — **호스트는 살아있는데 훅이
한동안 실행되지 않은 경우**(훅 미등록·고장). 그때는 기록이 낡았다고 보고 거부한다.

## 재시작·복수 세션 격리

| 상황 | 결과 |
| --- | --- |
| 호스트 재시작 | pid 또는 start가 달라져 **새 키**. 이전 행은 아무 호출자와도 매칭되지 않는 잔여물이 되고, stale 판정으로 별칭 claim 대상이 된다 |
| 같은 호스트에서 `/clear`·resume | pid 동일, session_id 변경 → 키 이전으로 **새 세션이 승계**. 이전 세션 행은 host_* 가 NULL이 되어 3순위로 도달 불가 |
| 같은 프로젝트에서 claude와 codex 동시 | `host_client`가 달라 키가 다르다 → 서로 간섭 없음 |
| 같은 툴 인스턴스 2개 (다른 창) | pid가 달라 키가 다르다 → 서로 간섭 없음 |
| 같은 세션이 여러 워크스페이스 | session_id가 축이므로 무관. 별칭 격리는 `root + tool`로 별도 보장 |

## fail-closed

탐지 실패·조회 0행은 **거부 방향**으로 떨어진다. `unattributed` 상태에서 `--human` 없는
발신은 non-zero exit이고 큐에 삽입되지 않는다.

이 선택의 대가는 명확하다: 호스트 프로세스 구조가 바뀌면 **CLI 발신 전체가 즉시 막힌다.**
조용히 오배달되는 것보다 낫고, 즉시 감지된다는 점에서 이 방향이 맞다.

단 **훅 경로는 예외다.** 훅은 전 경로 fail-open이어야 한다 (exit 0, stdout 무출력).
훅이 실패하면 세션 자체가 막히기 때문이다. 훅은 1순위(`--session`)로 자기 세션을 알기
때문에 이 계약의 3순위에 의존하지 않는다.

## `--human`은 검증이 아니다

`--human`은 **호출자의 operator assertion**이며 보안상 검증된 human-origin이 아니다.
자동 에이전트도 붙일 수 있고 그러면 hop 체인이 끊긴다. 따라서 이 플래그는 방어선이
아니고, **최종 방어선은 breaker**다.

## 고정된 의존성

| 항목 | 버전 | 근거 |
| --- | --- | --- |
| SQLite 드라이버 | `modernc.org/sqlite v1.50.1` | 순수 Go → CGO 불필요. crux와 동일 |
| MCP SDK | `github.com/mark3labs/mcp-go v0.52.0` | crux가 사용하는 것과 동일 |
| syscall 래퍼 | `golang.org/x/sys v0.44.0` | darwin `SysctlKinfoProc` / linux `/proc` 파싱. crux 검증 버전 |

I1 시점에는 이 세 패키지를 **import하는 코드가 아직 없다**(스토어는 I2, MCP는 I9,
호스트 탐지는 I3). `go.mod`·`go.sum`에는 올라가 있지만 `go mod tidy`를 지금 돌리면
제거된다. 위 표가 그 경우의 복구 기준이다.

## 이 계약이 깨지는 조건

- **호스트 실행 파일명이 바뀐다** — `comm`/`argv0`가 `claude`/`codex`로 시작하지 않게
  되면 탐지가 실패한다. `PAGER_CLIENT`로 라벨은 덮을 수 있지만 조상 매칭 자체가 실패하면
  덮을 수 없다.
- **호스트가 CLI를 자손으로 실행하지 않는다** — 부모 체인이 끊기면 3순위가 무력해진다.
  이때는 1순위(`--session`)나 2순위(`PAGER_SESSION`)로 명시해야 한다.
- **16단보다 깊은 래퍼 체인** — 탐지 실패.
- **darwin/linux 이외** — `procInfo`가 미지원이므로 3순위가 항상 실패한다. 1·2순위만
  동작한다.

전부 fail-closed 방향이고, 첫 번째와 두 번째는 CLI 발신이 통째로 막히면서 즉시 드러난다.
