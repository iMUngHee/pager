# 훅 계약 — 이벤트 · 입력 · 출력 매트릭스

> 배달은 전적으로 훅이 한다. MCP는 pull 모델이라 서버가 수신자의 턴을 깨울 수 없고,
> 주입은 훅으로만 가능하다.
>
> 관련: [session-binding.md](session-binding.md)

## 왜 훅인가

pager의 보장은 "다음에 활동할 때 본다"이다. 그 "활동"을 관측할 수 있는 유일한 지점이 훅이다.
수신 세션이 훅을 한 번도 실행하지 않으면 **출력 시도는 0회**다 — 이것이 보장의 조건부 성격이다.

## 양쪽 툴이 같은 계약을 쓴다

Claude Code와 Codex CLI 모두 stdin으로 JSON을 받고, stdout에
`hookSpecificOutput.additionalContext`를 내면 호스트가 다음 턴 문맥에 접는다. 이미 쓰이고
있는 계약이라(대협의 기존 `inject-context.sh`가 양쪽에서 같은 JSON을 출력) 크로스툴 배관이
검증된 채로 존재한다.

```json
{
  "hookSpecificOutput": {
    "hookEventName": "UserPromptSubmit",
    "additionalContext": "pager: 1 message waiting for you.\n…"
  }
}
```

## 이벤트 매트릭스

| 이벤트 | 세션 기록 | 인과 리셋 | 메시지 배달 | 고아 별칭 힌트 |
| --- | --- | --- | --- | --- |
| `UserPromptSubmit` | ✅ | **프롬프트 본문이 있을 때만** | ✅ | ✅ (세션당 1회) |
| `SessionStart` | ✅ | ❌ | ✅ | ✅ (세션당 1회) |
| `Stop` | ✅ | ❌ | ✅ | ❌ |
| `SubagentStop` | ✅ | ❌ | ✅ | ❌ |

**Stop 계열에서 고아 힌트를 내지 않는 이유** — Stop 훅의 출력은 대화를 계속시킨다. 배달할
메시지도 없이 힌트만 내면 세션이 깨어나 "수신함이 오프라인입니다"만 읽고 할 일이 없다.
비용을 쓰면서 아무것도 전달하지 않는 턴이다. 단 **배달 자체는 Stop에서도 한다** — 억제 대상은
힌트뿐이다.

**인과 리셋의 판정** — 이벤트가 `UserPromptSubmit`이고 페이로드의 `prompt`가 공백이 아닐 때만
리셋한다. 호스트가 Stop 주입 뒤 스스로 재개하는 경우는 자기 프롬프트 본문이 없으므로 리셋되지
않고, 그때 나가는 발신은 여전히 `caused`로 계산된다.

> 이 판정이 핑퐁 방어에서 가장 약한 고리다. 호스트가 재개할 때 합성 프롬프트를 넣기 시작하면
> 체인이 한 단계 일찍 끊기고 깊이가 누적되지 않는다. 그때 남는 방어선은 **세는 것밖에 하지 않는
> breaker**다.

## 입력 필드

| 필드 | Claude Code | Codex | pager의 용도 |
| --- | --- | --- | --- |
| `session_id` | ✅ | ✅ | 1순위 세션 해석. 없으면 훅은 아무것도 하지 않는다 |
| `prompt` | ✅ | ✅ | 인과 리셋 판정 |
| `cwd` | ✅ | ✅ | workspace root (별칭 격리 축) |
| `project_dir` | — | ✅ | workspace root (우선) |
| `transcript_path` | ✅ | ✅ | 미사용 (1단계 범위 밖) |

workspace root 해석 순서: `project_dir` → `cwd` → `CLAUDE_PROJECT_DIR` → 프로세스 작업 디렉토리.

## 한 번의 훅 실행이 하는 일

순서에 의미가 있다.

```
1. 세션 기록      ← 아직 아무도 안 보낸 세션도 주소가 생겨야 별칭을 붙일 수 있다
2. 인과 리셋      ← 배달보다 먼저. 리셋 뒤 도착분이 새 체인의 원인이 된다
3. 후보 조회      ← claim 없음. 트랜잭션 밖
4. 예산 선정      ← 탈락분은 건드리지 않는다 (starvation 방지)
5. 선정분 claim   ← BEGIN IMMEDIATE 한 트랜잭션
6. stdout 출력
7. 확정           ← 출력 뒤. 사이에서 죽으면 lease 만료 후 재시도된다
8. prune 기회     ← gate 선점에 성공한 훅 하나만
```

**7번이 6번 뒤인 것이 핵심이다.** 먼저 확정하면 출력 전에 죽었을 때 메시지가 그냥 사라진다.

## fail-open

모든 경로가 fail-open이다 — **exit 0, stdout 무출력**.

| 상황 | 결과 |
| --- | --- |
| 손상된 JSON / 빈 입력 | 조용히 종료 |
| `session_id` 없음 | 조용히 종료 |
| DB 열기 실패 · 권한 없음 | 조용히 종료 |
| 배달 중 오류 | 조용히 종료 |
| prune 실패 | 무시하고 계속 (완료 시각을 갱신하지 않아 다음 훅이 재시도) |

대가는 **고장이 세션 안에서 보이지 않는다**는 것이다. 확인 수단은 `pager whoami`와 `pager ls`다.

훅 한 번의 실행 시간은 5초로 제한한다. 잠긴 DB에 매달려 세션을 붙잡는 것이 fail-open이 막으려던
바로 그 상황이기 때문이다.

## 등록

훅 등록 명령은 [README](../README.md)에 있다.
