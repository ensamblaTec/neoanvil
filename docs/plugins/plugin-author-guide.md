# Plugin Author Guide — NeoAnvil Subprocess MCP Plugins

> Authoritative reference for building third-party plugins for neoanvil.
> Architecture decision: [ADR-005](./adr/ADR-005-plugin-architecture.md).
> PILAR XXIII Épica 126.1.

---

## TL;DR

A plugin is a standalone executable that:

1. Speaks **MCP JSON-RPC over stdio** (newline-delimited JSON).
2. Reads **secrets and active context from environment variables**.
3. Returns **clean Markdown** in tool responses.
4. **Honors SIGTERM** within 5 seconds.
5. Lives in `cmd/plugin-<name>/` with a single `main.go`.

The reference implementation is [`cmd/plugin-echo/main.go`](../cmd/plugin-echo/main.go) — copy it, rename, and replace `echo`/`version` with your tools.

A real-world plugin with credentials, REST client, audit log, and write
operations: [`cmd/plugin-jira/main.go`](../cmd/plugin-jira/main.go).

---

## Plugin lifecycle

```
~/.neo/plugins.yaml manifest
        ↓
Nexus boot (when nexus.plugins.enabled: true)
        ↓
PluginPool.Start(spec):
   exec.Cmd /usr/local/bin/neo-plugin-jira
   stdin  ← Nexus writes JSON-RPC requests here
   stdout → Nexus reads JSON-RPC responses here
   stderr → ~/.neo/logs/plugin-jira.log (mode 0600)
        ↓
plugin.Client.Initialize(ctx)
   Nexus sends:   {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}
   Plugin replies: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05",...}}
   Nexus sends:   {"jsonrpc":"2.0","method":"notifications/initialized"}
                  (notification — no response expected)
        ↓
plugin.Client.ListTools(ctx)
   Nexus sends:   {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
   Plugin replies: {"jsonrpc":"2.0","id":2,"result":{"tools":[...]}}
        ↓
Tool aggregation: each plugin's tools get prefixed with its
   namespace_prefix and merged into the MCP tools/list response
   served to Claude. So `get_context` becomes `jira/get_context`.
        ↓
Claude invokes a tool:
   POST /mcp/message {"method":"tools/call","params":{"name":"jira/get_context",...}}
   Nexus detects plugin prefix → routes via plugin.Client.CallTool
   Plugin runs the tool, returns content
        ↓
Nexus shutdown:
   PluginPool.StopAllGracefully(5s):
      SIGTERM → wait 5s for clean exit → SIGKILL if still alive
```

---

## File layout

```
cmd/plugin-myname/
├── main.go        ← entrypoint, MCP handshake, tool dispatch
└── main_test.go   ← unit tests for formatters / business logic

pkg/myname/        ← (optional) library code if plugin grows
├── client.go      ← REST client, business logic
└── client_test.go
```

Add the plugin name to `cmd/plugin-*` and `make build-plugins` picks it up automatically.

---

## Manifest entry — `~/.neo/plugins.yaml`

```yaml
manifest_version: 1
plugins:
  - name: myname                # unique. Regex: ^[a-z0-9][a-z0-9_-]*$
    description: "What I do."
    binary: /usr/local/bin/neo-plugin-myname
    args: []                    # optional CLI args
    env_from_vault:             # vault → env var injection at spawn
      - MYNAME_TOKEN
      - MYNAME_EMAIL
      - MYNAME_DOMAIN
      - MYNAME_ACTIVE_SPACE     # context (from ~/.neo/contexts.json)
    tier: nexus                 # workspace | project | nexus
    namespace_prefix: myname    # tool routing prefix; defaults to name
    enabled: false              # opt-in
```

The vault resolves names by **`<PROVIDER>_<FIELD>` convention**:
- `MYNAME_TOKEN` → `CredEntry.Token` for provider `myname`
- `MYNAME_EMAIL` → `CredEntry.Email`
- `MYNAME_DOMAIN` → `CredEntry.Domain`
- `MYNAME_ACTIVE_SPACE` → `Space.SpaceID` of active context for provider `myname`
- `MYNAME_ACTIVE_BOARD` → `Space.BoardID`

Operator stores credentials with `neo login --provider myname --token X --email Y --domain Z` and active context with `neo space use --provider myname --id <space>`.

---

## MCP handshake template

```go
package main

import (
    "bufio"
    "encoding/json"
    "fmt"
    "os"
)

const (
    protocolVersion = "2024-11-05"
    pluginVersion   = "0.1.0"
)

func main() {
    scanner := bufio.NewScanner(os.Stdin)
    scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
    enc := json.NewEncoder(os.Stdout)

    for scanner.Scan() {
        var req map[string]any
        if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
            fmt.Fprintln(os.Stderr, "plugin-myname: bad json:", err)
            continue
        }
        resp := handle(req)
        if resp == nil {
            continue // notification
        }
        if err := enc.Encode(resp); err != nil {
            fmt.Fprintln(os.Stderr, "plugin-myname: encode:", err)
            return
        }
    }
}

func handle(req map[string]any) map[string]any {
    method, _ := req["method"].(string)
    id := req["id"]
    switch method {
    case "initialize":
        return ok(id, map[string]any{
            "protocolVersion": protocolVersion,
            "capabilities":    map[string]any{"tools": map[string]any{}},
            "serverInfo":      map[string]any{"name": "plugin-myname", "version": pluginVersion},
        })
    case "notifications/initialized":
        return nil
    case "tools/list":
        return handleToolsList(id)
    case "tools/call":
        return handleToolsCall(id, req)
    }
    return rpcErr(id, -32601, "method not found: "+method)
}

func ok(id any, result map[string]any) map[string]any {
    return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, msg string) map[string]any {
    return map[string]any{
        "jsonrpc": "2.0",
        "id":      id,
        "error":   map[string]any{"code": code, "message": msg},
    }
}
```

---

## Tool input schemas

Each tool exposed via `tools/list` declares an input schema:

```go
{
    "name": "do_thing",
    "description": "Brief — appears in Claude's tool picker.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "ticket_id": {
                "type":        "string",
                "description": "What this means.",
            },
            "verbose": {
                "type":        "boolean",
                "default":     false,
            },
        },
        "required": []string{"ticket_id"}, // ALWAYS provide, even if empty
    },
}
```

**Critical:** `Required` must be a slice — never `nil`. The MCP SDK silently
drops tools with `required: null` (LEY 9 in `neo-code-quality.md`).

---

## Tool output: Markdown content

```go
return ok(id, map[string]any{
    "content": []map[string]any{
        {"type": "text", "text": "## Result\n- item 1\n- item 2"},
    },
})
```

Claude consumes plain Markdown well. Avoid:
- Atlassian Document Format (ADF), HTML, or other rich formats — render to plaintext server-side.
- Massive payloads — Claude's context budget. Truncate descriptions, limit lists, paginate.
- ANSI escape codes, terminal colors.

---

## Read secrets from env

```go
token := os.Getenv("MYNAME_TOKEN")
email := os.Getenv("MYNAME_EMAIL")
domain := os.Getenv("MYNAME_DOMAIN")
if token == "" || email == "" || domain == "" {
    fmt.Fprintln(os.Stderr, "plugin-myname: required env vars missing")
    os.Exit(1)
}
```

The vault layer (`pkg/auth/vault.go`) injects these at spawn from
`~/.neo/credentials.json` (or OS keychain). Active context env vars
(`MYNAME_ACTIVE_SPACE`, etc.) come from `~/.neo/contexts.json`.

**Never write secrets to stdout** — that's the JSON-RPC channel. Only write to:
- stdout: JSON-RPC frames only
- stderr: human-readable diagnostics (captured by Nexus)

---

## Mutating tools — mandatory audit log

Tools that change state (create/update/delete external resources, transition
tickets, close issues, send messages) MUST append to a per-plugin audit log
before returning success.

```go
import "github.com/ensamblatec/neoanvil/pkg/auth"

// At boot
auditPath := filepath.Join(os.Getenv("HOME"), ".neo", "audit-myname.log")
audit, err := auth.OpenAuditLog(auditPath)
if err != nil {
    fmt.Fprintln(os.Stderr, "open audit log:", err)
    os.Exit(1)
}
defer audit.Close()

// On mutation success
_, err = audit.Append(auth.Event{
    Kind:     "myname_action",
    Actor:    "plugin-myname",
    Provider: "myname",
    Tool:     "myname/do_thing",
    Details: map[string]any{
        "resource_id": id,
        "action":      "transitioned",
    },
})
if err != nil {
    // Mutation succeeded externally; audit failed. Alert loudly.
    fmt.Fprintln(os.Stderr, "AUDIT FAILED:", err)
    return rpcErr(reqID, -32603, "mutation succeeded but AUDIT LOG WRITE FAILED")
}
```

**Why per-plugin file:** `pkg/auth/audit` uses `sync.Mutex` (in-process), not
file locks. Sharing one audit log between Nexus + plugins would race on the
hash chain.

---

## Rate limiting + backoff

External APIs throttle. Use a client-side throttle to be a good citizen:

```go
type Client struct {
    mu      sync.Mutex
    lastReq time.Time
    minGap  time.Duration  // 100ms = 10 req/s
}

func (c *Client) throttle() {
    c.mu.Lock()
    defer c.mu.Unlock()
    elapsed := time.Since(c.lastReq)
    if elapsed < c.minGap {
        time.Sleep(c.minGap - elapsed)
    }
    c.lastReq = time.Now()
}
```

On 429 responses, parse `Retry-After` and surface as a typed error so the
caller can backoff or fail fast. See `pkg/jira/client.go` for the pattern.

---

## Testing

Three layers:

1. **Unit tests** for formatters / business logic (`main_test.go` next to
   `main.go`). No subprocess spawn; just functions.

2. **REST mock** for API clients (`pkg/<name>/client_test.go`) using
   `httptest.NewServer`. Examples in `pkg/jira/client_test.go`.

3. **End-to-end** via `pkg/nexus/plugin_integration_test.go` pattern: build
   the binary at test time, spawn via `PluginPool`, drive MCP handshake,
   call tools, verify Markdown output, graceful Stop.

Skip e2e under `go test -short` — CI can run it on PRs.

---

## SIGTERM handling

Keep clean shutdown trivial — Nexus sends SIGTERM then waits 5 seconds before SIGKILL:

```go
// Default Go behavior is "exit on SIGTERM" — usually fine.
// Override only if you need to flush state:
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
go func() {
    <-sigCh
    audit.Close()       // flush + close audit log
    os.Exit(0)
}()
```

Don't ignore SIGTERM — Nexus will SIGKILL after 5s anyway, and SIGKILL skips
deferred cleanup (audit log not flushed, partial writes possible).

---

## Build + register

```bash
# 1. Build
make build-plugins              # picks up cmd/plugin-myname automatically
# Output: bin/neo-plugin-myname

# 2. Install (or set absolute path in plugins.yaml)
sudo cp bin/neo-plugin-myname /usr/local/bin/

# 3. Register credentials
neo login --provider myname --token X --email Y --domain Z

# 4. Set active context (if applicable)
neo space use --provider myname --id <space-id> --name "..."

# 5. Add manifest entry
$EDITOR ~/.neo/plugins.yaml     # set enabled: true

# 6. Enable plugin pool in nexus
$EDITOR ~/.neo/nexus.yaml       # nexus.plugins.enabled: true

# 7. Restart
make rebuild-restart

# 8. Verify
curl http://127.0.0.1:9000/api/v1/plugins
# Should show: {"enabled":true, "plugins":[{"name":"myname",...}], "tools":[...]}
```

---

## Common pitfalls

| Mistake | Symptom | Fix |
|---|---|---|
| Writing secrets/logs to stdout | MCP handshake fails, plugin tagged errored | All diagnostics → stderr |
| `Required: nil` in inputSchema | Tool silently dropped from tools/list | `Required: []string{...}` always |
| Forgetting `notifications/initialized` handling | Plugin replies to a notification → handshake desync | Return `nil` for any method starting with `notifications/` |
| Long-running tool with no context cancellation | SIGKILL after timeout, audit log not flushed | Honor `ctx` from `context.WithTimeout` |
| Returning ADF / HTML / rich format | Claude renders raw markup | Always plaintext Markdown |
| Per-call audit log opening | File lock contention, slow | Open at boot, hold for process lifetime |
| Skipping rate limit | 429s, eventual IP block | Token bucket + backoff on 429 |
| Storing secrets in `~/.neo/.env` | Plain text, visible to anyone with file read | Use `neo login` (keystore + opt-in encryption) |

---

## Examples

| Repository file | Demonstrates |
|---|---|
| [`cmd/plugin-echo/main.go`](../cmd/plugin-echo/main.go) | Minimal MCP handshake, no external dependencies |
| [`cmd/plugin-jira/main.go`](../cmd/plugin-jira/main.go) | Real plugin with REST client, vault env, audit log on mutations |
| [`pkg/jira/client.go`](../pkg/jira/client.go) | REST client pattern: rate limit, typed errors, ADF rendering |
| [`pkg/plugin/mcp.go`](../pkg/plugin/mcp.go) | The Client side (Nexus → plugin) — useful to read for protocol details |

---

## See also

- [ADR-005 — Plugin architecture](./adr/ADR-005-plugin-architecture.md)
- [ADR-006 — Jira auth flow](./adr/ADR-006-jira-auth-flow.md)
- [PILAR XXIII overview](./pilar-xxiii-plugin-architecture.md)
- `~/.neo/nexus.yaml.example` — `nexus.plugins.enabled` flag
- `~/.neo/plugins.yaml.example` — manifest schema
