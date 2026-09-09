# 세션 바인딩 계약

[English](session-binding.md) · [한국어](session-binding.ko.md)

> hop 안전성 전체가 이 문서 위에 올라간다. 여기서 정한 것을 바꾸면
> `internal/sessionref`·`internal/deliver`의 확정 조건이 함께 바뀐다.
>
> 관련: [hooks.ko.md](hooks.ko.md), [reference.ko.md](reference.ko.md)

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

## 검증된 선례

이 구조는 새로 고안한 것이 아니다. 같은 문제 — MCP가 호출자의 세션을 알려주지 않는다 —
를 먼저 만난 자매 도구가 실사용으로 같은 답에 도달했다. 훅이 호스트 프로세스 동일성으로
활성 세션 포인터를 쓰고, CLI와 MCP가 그것을 읽는다.

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

## 주소 레이어는 둘이다 — 자동 이름은 세 번째가 아니다

| 레이어 | 값 | 성질 |
| --- | --- | --- |
| 세션 | `session_id` | 호스트가 주는 것. 불변, 사람이 읽을 것은 아님 |
| 이름 | `aliases.alias` | 논리적 주소. **자동으로 하나 붙고**, 사람이 더 붙일 수 있다 |

자동 이름은 별도 레이어가 아니라 **이름 레이어의 기본값**이다. 그래야 하는 이유는 배달 경로에
있다 — `Candidates`와 `revalidate`가 `messages JOIN aliases ON alias`로 수신자를 찾으므로
(`internal/deliver/lease.go`), 별칭 행이 아닌 주소는 수신 자체가 불가능하다. 자동 이름을
`sessions` 컬럼으로 두면 claim 재검증 SQL에 UNION을 넣어야 하고, 그것은 이 문서가 고정한
계약 중 가장 위험한 것을 건드리는 일이다.

발급 규칙:

- **언제** — 이름이 없는 세션이 훅을 돌릴 때마다, 그리고 `pager attach` 때. `SessionStart`는
  권장 등급이므로 그것만으로는 부족하다.
- **작업공간을 모르면 발급하지 않는다.** alias 행은 삽입 시점의 `root`·`tool`을 복사해 고정하고
  이후 아무도 갱신하지 않는다. 탐지 실패 상태로 붙이면 그 이름은 고아 발견에서 사라지고 claim으로도
  이전되지 않는다. 판정은 호출자가 들고 있는 값이 아니라 **저장된 `sessions` 행**으로 한다 —
  `RecordSession`이 빈 값으로 기존 값을 지우지 않으므로, 전에 `attach`가 채워둔 세션은 이번 훅의
  탐지가 실패해도 자격이 있다.
- **동시성** — 발급은 `NOT EXISTS (이 세션의 별칭)`을 조건에 넣은 단일 INSERT다. 검사와 삽입을
  나누면 동시 훅 둘이 각자 자기가 처음이라고 판단해 한 세션이 이름 둘을 갖는다.

### 어느 이름이 그 세션의 이름인가

`ORDER BY updated_at DESC, alias` — **나중에 정해진 이름이 이긴다.** 자동 이름은 세션이 시작할
때 붙으므로 사람이 나중에 고른 이름이 항상 그 뒤에 온다.

그래서 `SetAlias`와 `ClaimAlias`는 시계를 그대로 쓰지 않고 **대상 세션의 기존 이름들보다 엄격히
큰 값**을 쓴다: `max(now, (SELECT max(updated_at) ... WHERE session_id = 대상) + 1)`. 시계는
밀리초라 두 쓰기가 같은 값에 떨어질 수 있고, 그러면 타이브레이크가 알파벳순으로 내려가 자동
이름이 이겨버린다.

### 모호성은 이름이 아니라 세션 단위다 — 단, 세션을 지목하는 tier에서만

한 세션이 이름을 여럿 갖게 되면서 타겟 해석이 바뀌었다.

| tier | 지목 대상 | 복수 행일 때 |
| --- | --- | --- |
| `alias` | 이름 | 불가능 (기본 키) |
| `session` | **세션** | 같은 세션이므로 대표 이름으로 접는다 |
| `pm_ref` | **세션** | 전부 같은 세션이면 접고, 다른 세션이 섞이면 `AmbiguousError` |
| `live substring` | 살아있는 세션의 이름 패턴 | 후보를 보여주고 실패 — 사람이 고를 문제다 |
| `substring` | 이름 패턴 | 후보를 보여주고 실패 — 어느 이름을 말하는지 알 수 없다 |

접기 조건에 "`session_id`가 비어있지 않을 것"이 들어간다. 고아 별칭은 세션이 없고, 그 둘은
같은 값(빈 문자열)으로 비교되지만 실제로는 서로 다른 수신함이다.

### 부분일치가 두 층인 이유

**이름은 회수되지 않는다.** 한 번이라도 돌아간 세션은 이름을 하나씩 남기고, 그 이름은 세션이
끝난 뒤에도 그대로 있다. 부분일치가 한 층뿐이면 **사람이 고르는 집합과 참조가 해석되는 집합이
갈라진다** — `pager who`는 살아있는 것만 보여주는데 해석은 죽은 것까지 상대한다. 그러면 배울
당시에는 유일했던 축약이 몇 주 전에 끝난 세션의 이름과 부딪히기 시작하고, 그 분자는 시간이
지날수록 커지기만 한다.

`live substring`이 staleness 기준(`heartbeat_at >= now - staleAfter`)을 걸어 살아있는 이름만
본다. 여기서 여러 건이 나오면 **다음 tier로 넘어가지 않고 실패한다** — 살아있는 이름끼리
겹치는 것은 사람이 답할 진짜 질문이고, 범위를 넓히면 아무도 기다리지 않는 이름만 후보에
더해질 뿐이다.

넓은 `substring`이 뒤에 남아 있으므로 **세션이 떠난 이름도 여전히 부를 수 있다.** 이어받기는
그 수신함에 메일이 쌓이는 데서 시작하므로 도달 불가능해지면 안 된다. 달라진 것은 도달 가능성이
아니라 우선순위다.

이 변경은 패턴이 매칭되는 방식만 건드린다. **이름을 정확히 부르는 경로는 그대로다** — `alias`
tier가 1순위이고, 고아 알림이 사람에게 알려주는 것이 바로 그 정확한 이름이다.

## 활성 바인딩은 어디 사는가

**`sessions` 테이블 컬럼.** 별도 포인터 파일을 두지 않는다.

선례가 별도 포인터 파일을 둔 이유는 **훅과 MCP 서버가 서로 다른 저장소를 쓰기 때문**이다 —
서버는 자기 DB에 쓰고 세션 문맥은 훅에만 있으니 둘을 잇는 파일이 필요했다. pager에는 그
전제가 없다: 훅·CLI·MCP가 **전부 같은 `~/.pager/msg.db`를 쓴다.**
포인터 파일을 두면 두 번째 저장소와 그 TTL·고아 정리가 통째로 추가 표면이 된다.

```sql
-- sessions 테이블의 호스트 컬럼 (internal/store 스키마)
host_client TEXT,     -- 'claude' | 'codex'
host_pid    INTEGER,
host_start  INTEGER   -- 플랫폼 상대 시작 토큰
```

`heartbeat_at`이 포인터 파일 방식의 TTL 역할을 그대로 한다. 신선도 판정 축이
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

선례는 별도의 생존 검사로 pid 재사용을 막는다. pager에는 그 검사가 **불필요하다** —
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
| SQLite 드라이버 | `modernc.org/sqlite v1.50.1` | 순수 Go → CGO 불필요, 크로스 컴파일 가능 |
| MCP SDK | `github.com/mark3labs/mcp-go v0.52.0` | stdio 서버에 필요한 최소 표면만 쓴다 |
| syscall 래퍼 | `golang.org/x/sys v0.44.0` | darwin `SysctlKinfoProc` / linux `/proc` 파싱 |

세 패키지는 각각 `internal/store`, `internal/mcpsrv`, `internal/sessionref`·`internal/wake`가
import한다. 위 표는 버전을 올릴 때의 판단 근거다 — 특히 syscall 래퍼는 darwin과 linux의
경로가 갈리므로, 올릴 때 두 플랫폼 모두에서 `internal/sessionref` 테스트를 돌려야 한다.

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
