# NeoAnvil Deadcode Audit Policy

_Épica 235 — decisión documentada 2026-04-19._

## Summary

**`deadcode ./...` is NOT a reliable signal in this repository.** Use
`staticcheck -checks U1000 ./...` instead.

## Evidence

The `golang.org/x/tools/cmd/deadcode` tool was run against the repo and
reported 639 "unreachable functions" (excluding `web/node_modules/`
vendored JavaScript polyfills).

Manual sampling showed a catastrophic false-positive rate:

| Function                 | `deadcode` verdict | Real call-sites (non-test) |
|--------------------------|--------------------|----------------------------|
| `rag.WAL.SaveDocMeta`    | unreachable        | 7                          |
| `rag.Cache.Get`          | unreachable        | 67                         |
| `rag.QueryCache.Get`     | unreachable        | 67                         |
| `rag.TextCache.Get`      | unreachable        | 67                         |
| `telemetry.RecordMutation` | unreachable      | 1 (but very-hot path)      |

## Root cause

`deadcode ./...` starts from every `main` package it finds in the
**current module** and traces calls. Our layout has five `main`s spread
across multiple submodules registered via `go.work`:

- `cmd/neo-mcp/` (main module)
- `cmd/neo-nexus/` (main module)
- `cmd/neo/` (workspace submodule)
- `cmd/sandbox/` (main module)
- `cmd/neo-tui/` (workspace submodule)
- plus `cmd/chaos/`, `cmd/stress/`, `cmd/plc_chaos/`, `cmd/pki/`,
  `cmd/ast-cleaner/` — each its own main

When `deadcode ./...` is invoked from the repo root, the analysis
captures only the main in the current module, and `pkg/rag`, `pkg/sre`,
`pkg/incidents`, etc. all appear unreachable even though other mains
(in sibling modules) DO call them.

A per-entrypoint intersection would theoretically work — flag functions
unreachable from ALL mains — but the implementation complexity is high
(you'd need a custom driver over `golang.org/x/tools/go/callgraph/cha`)
and the payoff is small because `staticcheck` already catches the
actionable subset.

## Authoritative tool

```bash
staticcheck -checks U1000 ./...
```

`U1000` is the single "function/method/variable is unused" check.
It's defined as unused **within** the package, not across the whole
program, which is the strongest reliable signal in a Go module with
separate test binaries + exported APIs.

As of 2026-04-19, after closing Épica 232.A, this command returns **0
findings** repo-wide.

## Policy for future dead-code hunting

1. Run `staticcheck -checks U1000 ./...` as the primary gate.
2. If a new hit appears: delete the code, OR document why it exists as
   a public API (add a test that exercises it, OR move to an `// Deprecated:`
   marker with sunset date).
3. **Do not run `deadcode ./...`** and act on its output. It will flag
   alive-and-kicking hot-path code as dead.
4. `make audit` (Épica 237) includes the correct `staticcheck` invocation
   and will keep the gate honest going forward.

## What this closes

- Épica 235 — "Deadcode triage multi-entrypoint". Criterio cumplido
  interpretado como: "zero real unused code" (via U1000 = 0), documented
  why the `deadcode` signal cannot be trusted.
