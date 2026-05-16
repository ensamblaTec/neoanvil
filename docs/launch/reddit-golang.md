# r/golang post

**Subreddit:** [r/golang/submit](https://www.reddit.com/r/golang/submit)
**Flair:** show & tell
**Best time:** Tue/Wed 9–11am ET

> ⚠️ r/golang is *not* a marketing sub. Mods enforce the "show & tell" framing strictly. Lead with engineering content, not pitch. If you've not contributed there before, comment on others' threads first for a week.

---

## Title

**[show & tell] NeoAnvil — pure-Go MCP server with SSA-based Code Property Graph, HNSW vector index, and SIMD ARM64 assembly kernels.**

---

## Body

Some Go-engineering bits from a project I've been building that might be interesting to this sub. The "MCP server" framing is incidental — the meat is the runtime.

### 1. Pure-Go native, CGO only when tree-sitter shows up

The native build (`make build`) is zero-CGO. Cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vec via `GOAMD64=v3` and `GOARM64`. Docker stage-3 is the only place CGO turns on, and only for tree-sitter parsers (TS/Py/Rust).

That's been a forcing function for staying honest about dependencies. No surprise libc, no unexpected dynamic linking, single binary for ops.

### 2. SIMD assembly for hot-path vector ops (ARM64)

The HNSW distance kernels needed sub-microsecond performance on M-series hardware. The Go ARM64 assembler has quirks the docs don't surface well:

- `FMLA` mnemonics need a `V` prefix (`VFMLA`); `FADDP` doesn't exist for float — use `VEXT` rotation; `VADDP` is integer pairwise-add
- `VFMLA Vm.T, Vn.T, Vd.T` is AT&T dest-last → `Vd[i] += Vn[i]*Vm[i]`
- `Fn` and `Vn.S[0]` are the same physical register
- Horizontal reduction for `float32x4`: `VEXT $4/$8/$12` rotates lanes, then `FADDS`
- `VLD1.P 16(R0), [V3.S4]` loads 4×f32 with post-increment

I keep a directive note about this in the repo so future-me doesn't re-learn it from gdb. Verified against `$GOROOT/src/cmd/asm/internal/asm/testdata/arm64.s`.

### 3. Zero-allocation hot paths via sync.Pool + flat memory

The RAG query path, the MCTS search path, and the cert pipeline batch-processor all forbid `make()` / `new()`. Buffers are recycled via `sync.Pool` and slices are reset with `[:0]`. There's a custom `ZeroAllocJSONMarshal` in `pkg/sre/allocs.go` (not in `pkg/utils/` because tooling treats `utils` as fair game for refactors).

### 4. SSA-based Code Property Graph

Built on top of `golang.org/x/tools/go/ssa`. Stores:
- Call graph (SSA-exact when CPG is online; falls back to AST-regex CC otherwise)
- Containment graph (file → package → module)
- CFG edges
- Identifier index for grep-equivalent search at vector-index speed

Persistence: BoltDB for the graph metadata, a custom flat binary for the SSA dump (`cpg.bin`). Fast-boot in ~1.6s for a 1.25M-node graph.

Each call-graph entry includes `[cc_method:ssa_exact|ast_regex]` so downstream tools know whether the cyclomatic complexity is computed from SSA or estimated from regex.

### 5. BoltDB single-leader across tiers

4-tier config (workspace → project → org → nexus) all want BoltDB. bbolt doesn't support mixed RW+RO across processes — you get `EWOULDBLOCK`. Solution: each tier has a single-leader rule (which process owns the writable handle), and other processes go through that leader via SSE.

### 6. SafeHTTPClient — anti-SSRF dialer

`sre.SafeHTTPClient()` for external URLs and `sre.SafeInternalHTTPClient(timeoutSec)` for Nexus → child traffic (loopback only). The dialer iterates resolved IPs and rejects RFC1918 / link-local / loopback for external clients — fixing a quiet IPv6-first dual-stack drift bug I shipped and then had to chase.

### Stats

- ~145k LOC across 523 `.go` files in `pkg/` + 207 in `cmd/`
- 100% staticcheck clean (audit-baseline tracked in repo)
- audit-ci fails on new findings vs baseline

### Repo

<https://github.com/ensamblaTec/neoanvil>

Happy to dig into any of these in comments. The SSA/CPG construction was the trickiest piece — `ssa.Function.Blocks` walking with proper recovery from `ssa.PackageBuild` errors took a few rewrites to stop leaking memory under hot-reload.
