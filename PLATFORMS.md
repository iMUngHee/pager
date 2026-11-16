# Platform validation

## Session presence policy

- Linux: Go unit/integration tests, race detector, static analysis and managed
  Codex daemon read-only WebSocket observation are exercised locally.
- The runtime observer connects only to the canonical existing local control
  socket. Missing or unsupported endpoints produce unknown thread evidence.
- macOS arm64 and Windows amd64: `CGO_ENABLED=0 go build ./cmd/pager`
  cross-compilation succeeds; native runtime checks remain pending below.
- Caller identity remains subject to the existing hook freshness and host-key
  rules. Runtime observations do not authorize a caller.

## Automation and archive paths (0.8.0)

- Linux: timer CLI send/export/hook flows and schema 5-to-6 preservation were
  exercised with isolated databases. Literal URI punctuation paths, concurrent
  archive separation and prompt recognition have regression coverage.
- macOS 26.4 arm64 (APFS), Go 1.27.4, 2026-11-16: `make test` passes natively
  (race detector, every package), including the custom archive path, literal
  URI punctuation and schema 5-to-6 tests. Gaebu 0.9.0's real-pager
  automation test (`real_pager_delivers_an_automation_report`) passes against
  this build with a scratch database. A send from launchd has not been run.
- Windows amd64: pure-Go cross-builds succeeded. Native timer execution and
  filesystem behavior remain pending below.
- Windows filename rules exclude `?`; its path regression runs on other systems.
  Literal `#` and `%` filenames are covered on every platform by the test suite.

## Pending checks

- Windows: run the 0.8.0 suite natively, including custom archive paths and
  literal URI punctuation. macOS/Windows: run an automation send from an OS
  timer (launchd, Task Scheduler). Consistent database path spelling
  (including case) is required; aliases of the same underlying file are
  outside the archive naming contract.


- macOS: native control-socket runtime observation and end-to-end process probes
  were not run locally; cross-compilation is checked separately.
- Windows: native managed-daemon endpoint availability and runtime observation
  were not run locally; cross-compilation is checked separately. An unavailable
  Unix socket yields unknown rather than starting a server or declaring a thread
  gone.
