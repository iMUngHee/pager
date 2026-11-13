# pager

[English](README.md) · [한국어](README.ko.md)

코딩 에이전트 세션끼리 메시지를 주고받는 도구입니다. 툴이 달라도, 프로젝트가 달라도 됩니다.

전화가 아니라 삐삐처럼 동작합니다. 보내면 상대는 **다음에 무언가를 할 때** 읽습니다. 폴링하지
않고, 생각 중인 세션을 끊지도 않습니다.

```
codex 세션                        claude 세션
    │                                 │
    │  pager send review-box "…"      │
    ├────────────► ~/.pager/msg.db    │
    │                                 │
    │                          (다음 훅 실행 시)
    │                                 │◄── 컨텍스트로 주입
```

## 어디에 쓰나

에이전트 세션 둘을 띄워 뒀다고 해봅시다. 하나는 API 레포, 하나는 웹 레포. API 쪽에서 응답
형태를 바꿨고 웹 쪽이 그걸 알아야 합니다. 지금은 한쪽 터미널에서 읽어다 다른 쪽에 다시
타이핑합니다.

pager가 자동화하는 건 그 **옮겨 적는 일**입니다. **알아차리는 일**은 자동화하지 않습니다 —
[범위 밖](docs/reference.ko.md#범위-밖)을 보세요.

## 설치

```bash
make build     # ~/.local/bin/pager 로 빌드
```

C 툴체인이 필요 없습니다. SQLite 드라이버가 순수 Go라 크로스 컴파일도 됩니다. 저장소는
`~/.pager/msg.db` 파일 하나이고, macOS와 Linux에서는 디렉토리 0700 · 파일 0600으로
만들어집니다. Windows에는 그 뜻을 담을 mode 비트가 없고 pager가 ACL을 걸지도 않으므로,
거기서는 파일이 놓인 디렉토리만큼만 사적입니다.

그다음 훅을 등록합니다. **배달은 훅이 합니다.** 보내는 건 DB에 쓰는 것이고, 세션 컨텍스트에
텍스트를 넣을 수 있는 건 훅뿐입니다.

**Claude Code** — `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "matcher": "", "hooks": [
        { "type": "command", "command": "pager hook", "timeout": 10000 }
      ]}
    ],
    "SessionStart": [
      { "matcher": "", "hooks": [
        { "type": "command", "command": "pager hook", "timeout": 10000 }
      ]}
    ]
  }
}
```

**Codex CLI** — `~/.codex/config.toml`:

```toml
[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "pager hook"
timeout = 10
```

`UserPromptSubmit`은 빠지면 안 됩니다. `SessionStart`는 새 세션이 대기 중인 메일을 바로 보게
해주므로 권장합니다. 이벤트 이름은 인자로 주지 마세요 — `pager hook`이 페이로드에서 읽으므로
오타가 나지 않습니다.

## 첫 메시지 보내기

```bash
pager who                          # 누구에게 보낼 수 있나
pager send hica "파서 작업 넘긴다"  # 보내기
pager ls                           # 내 앞으로 온 것
pager whoami                       # 내 세션과 내 이름
```

세션은 첫 훅 실행에서 `hica` 같은 네 글자 이름을 받습니다. 발음할 수 있고 **아무 뜻도 없습니다**
— 의도한 것입니다. 직접 정하려면 `pager alias review-box`.

## 언제 도착하나

보내는 건 언제나 즉시입니다. 갈리는 건 상대가 언제 들여다보느냐입니다.

- **닿을 수 있으면 바로.** `send`가 상대 호스트를 두드리고, 상대는 몇 초 안에 턴을 한 번
  돕니다. 그 턴의 훅이 배달합니다.
- **아니면 다음 활동 때.** 두드리기가 실패해도 잃는 건 없습니다. 메시지는 훅이 돌 때까지
  수신함에서 기다립니다.

두드리기는 내용을 나르지 않습니다. "메일 왔다" 한 줄이고 배달은 언제나 훅이 하므로, 두드리기가
실패해서 잃는 것은 지연뿐입니다. `pager send`가 어느 길로 갔는지 말해줍니다.

## 하나만 알아둘 것

**배달은 보장이 아니라 조건부입니다.** 수신 세션의 훅이 한 번도 돌지 않으면 pager는 출력할
기회를 얻지 못하고, 그 사실을 알려주지도 않습니다 — 모든 훅 경로는 세션을 막는 대신 조용히
끝나도록 되어 있습니다.

같은 메시지를 두 번 볼 수도 있습니다. 재시도가 배달을 살리는 방법이고 중복은 그 대가입니다.
정확한 조건은 [reference.ko.md](docs/reference.ko.md#보장--조건부다)에 있습니다.

## 자주 쓰는 명령

| 명령 | 답해주는 것 |
| --- | --- |
| `pager who` | 최근 세션과 런타임에서 실행을 확인한 오래된 세션 |
| `pager who --all` | 현재 상태를 확인하지 못한 과거 세션까지 보기 |
| `pager send <이름> "…"` | 보내기 |
| `pager send <이름>@<peer> "…"` | 다른 저장소의 세션에게 보내기 (relay가 실어 나른다) |
| `pager ls` | 내 앞으로 온 것 |
| `pager ls --waiting` | 아직 아무도 챙기지 않은 것 |
| `pager inbox` | 세션 무관, 메일이 밀려 있는 수신함 전부 |
| `pager whoami` | 내 세션과 내 이름 |
| `pager alias <이름>` | 이름 직접 정하기 |
| `pager claim <이름>` | 주인이 없거나 전용 호스트의 종료를 확인한 수신함 이어받기 |
| `pager claim <이름> --allow-unknown` | 오래된 미확인 주인을 명시적으로 교체. 최근 활동·실행 확인 시에는 거부 |
| `pager export` | 저장소 전체를 JSONL로 |

`LAST`는 마지막 훅 이후 시간이며 세션 종료 시한이 아닙니다. `HOST`는 기록된 프로세스만
나타내고, Codex의 여러 thread가 한 daemon을 공유할 수 있습니다. `THREAD`는 실행 중인
Codex managed daemon을 읽기 전용으로 조회한 결과입니다. `active`·`idle`이면 오래된 세션도
명부에 남습니다. `notLoaded`·결과 누락·조회 실패는 종료 증거가 아닙니다. 기본 명부는 오래된
미확인 세션의 건수를 알리며, `--all` 또는 정확한 별칭으로 과거 세션에 접근할 수 있습니다.
MCP `msg_roster`도 같은 정책이며 `all: true`를 받습니다.

상태 조회는 daemon 시작, thread resume/archive, 훅 기반 호출자 인증 변경을 수행하지 않습니다.
런타임 조회와 SQLite 쓰기가 하나의 트랜잭션은 아닙니다. 별칭 회수는 쓰는 순간 소유자·lease
세대·heartbeat·host 키를 다시 비교하여 조회 중 기록이 바뀌면 거부합니다. 오래된 미확인 주인은
명시적 override가 필요하며 고아 수신함으로 자동 추천하지 않습니다. Codex 조회는 `CODEX_HOME`
(기본 `~/.codex`) 아래 기존 제어 소켓만 사용하며 제한 시간은 1초입니다. 연결할 수 없으면
thread 상태는 미확인으로 남습니다. 플랫폼 검증 범위는 PLATFORMS.md를 참고하세요.

## 에이전트 안에서 쓰기 (선택)

CLI만으로 충분합니다. 툴 호출로 쓰고 싶으면:

```bash
claude mcp add pager -s user -- pager mcp
codex  mcp add pager       -- pager mcp
```

도구는 셋입니다. `msg_send`로 보내고, `msg_list`로 내 메일을 읽고, `msg_roster`로 보낼 상대를
봅니다. 에이전트는 자기 이름은 훅에서 알지만 남의 이름은 받은 메일로만 알게 되므로 로스터가
필요합니다.

## 다른 머신의 세션 (선택)

머신마다 자기 저장소를 따로 씁니다. 다른 머신의 세션에 닿으려면 두 저장소를 짝지어 두고, 직접
돌리는 전송 수단(transport)이 그 사이로 메일을 옮기게 합니다 — pager 자신은 원격 머신으로 연결을 열지
않습니다:

```bash
laptop$ pager peer add vm --store "$(ssh vm pager relay id)"
vm$     pager peer add laptop --store <laptop에서 `pager relay id`로 본 값>
laptop$ pager send kipa@vm "파서 작업 넘긴다"
laptop$ pager relay out --peer vm | ssh vm pager relay in --peer laptop | pager relay ack --peer vm
```

인과 정보와 버전별 이벤트 메타데이터가 메시지와 함께 넘어갑니다. 원격 작업 신호가 unknown이면
루프라고 단정해 차단하지 않습니다. 상태 값, exit
code, 레코드 형식, peer 수명 주기 같은 전체 계약은
[reference.ko.md](docs/reference.ko.md#저장소-사이의-relay)에 있습니다.

## 안전에 관해 하나

메시지 본문은 다른 에이전트가 쓴 텍스트가 내 컨텍스트에 들어오는 것입니다. 프롬프트 주입
경로이고, 없앨 수는 없고 줄일 수만 있습니다. pager는 항상 발신자를 표시하고, 본문을 지시가
아니라 데이터로 감싸고, 길이를 제한합니다.

저장소는 내 사용자 계정 전용이고, pager는 네트워크 연결을 열지 않습니다. 다른 저장소에서
relay된 메일은 pager가 직접 쓰는 줄로 그 사실이 표시됩니다.

## 문서

- [reference.ko.md](docs/reference.ko.md) — 설정, 보관, 유휴 루프 보호, 컬럼의 뜻, 고장났을 때
- [hooks.ko.md](docs/hooks.ko.md) — 훅 계약: 이벤트 · 입력 · 출력 · fail-open
- [session-binding.ko.md](docs/session-binding.ko.md) — 세션을 어떻게 식별하나

네 문서 모두 영문판이 같은 자리에 `*.md`로 있습니다.
릴리스마다의 패치 노트는 [CHANGELOG.md](CHANGELOG.md)에 있습니다.

## 릴리스

버전을 올릴 때는 같은 커밋에 패치 노트를 씁니다.

1. `internal/version/version.go`의 `Version`을 올립니다.
2. [CHANGELOG.md](CHANGELOG.md) 맨 위에 `## <version> — <date>`를 추가합니다.
   pager를 쓰는 사람 입장에서 바뀐 점과 커밋 id, 그리고 store schema ·
   relay 프로토콜 · 재시작할 것을 적은 Upgrading 단락을 씁니다.

항목이 없으면 `TestChangelogHasCurrentVersion`이 `go test ./...`를, 따라서
`make reinstall`도 실패시킵니다. 버전은 git 태그가 아니라서
`go install …@latest`는 계속 `main`의 최신 커밋을 설치합니다.

## 라이선스

MIT — [LICENSE](LICENSE).
