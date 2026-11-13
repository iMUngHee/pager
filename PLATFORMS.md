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

## Pending checks

- macOS: native control-socket runtime observation and end-to-end process probes
  were not run locally; cross-compilation is checked separately.
- Windows: native managed-daemon endpoint availability and runtime observation
  were not run locally; cross-compilation is checked separately. An unavailable
  Unix socket yields unknown rather than starting a server or declaring a thread
  gone.
