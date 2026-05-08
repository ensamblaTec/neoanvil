# Network domain checklist

Append this to the audit prompt when files involve HTTP servers/clients,
SSE, MCP transport, or any external network exposure.

## Invariants to verify

- **SSRF guard**: outbound HTTP must use sre.SafeHTTPClient (rejects
  private/loopback/CGNAT/IPv6-mapped destinations) or
  sre.SafeInternalHTTPClient (loopback-only). Plain http.Client lets
  attacker-controlled URLs reach internal services.
- **Workspace ACL**: any handler that dispatches to a child workspace
  must enforce caller→target authority. The /workspaces/<id>/mcp/sse
  path id is operator-asserted; verify ACL against caller identity.
- **CORS**: `Access-Control-Allow-Origin: *` on a localhost server is
  a DNS-rebinding gateway. Either bind to 127.0.0.1 ONLY (not 0.0.0.0)
  AND validate Host header, or scope CORS to a fixed origin.
- **Host validation**: even on loopback bind, an attacker via DNS
  rebinding can reach the server with a Host header pointing at an
  external name. Reject Host headers that don't resolve to 127.0.0.1
  (`loopbackHostGuard` middleware pattern).
- **Rate limit ALL paths**: not just authenticated paths. Pre-auth
  rate limits prevent enumeration and password spraying.
- **TLS verify**: never `InsecureSkipVerify: true` outside test
  fixtures. If self-signed certs needed, pin via `RootCAs` not by
  disabling verify.
- **Body size limit**: every JSON-decode must use http.MaxBytesReader
  to bound RAM. Without it, attacker streams MB of garbage to OOM.
- **Timeout envelope**: http.Server with no ReadTimeout/WriteTimeout/
  IdleTimeout is a slowloris target. Set all three.
- **MCP tool name validation**: tool names from plugin handshake must
  match `^[a-zA-Z0-9_-]+$` (MCP spec). Slashes/spaces break clients
  silently.

## Severity floor

For findings in this domain, severity ≥ 6. Network exposure failures
are often pre-auth, no escalation needed.

## Common patterns

- ACL bypass via body field: workspaceID from request args fallback
  when header missing → caller controls authority.
- DNS rebinding on loopback: localhost binds aren't safe from browser
  attackers without Host validation.
- Host header injection: Host: evil.com may flow into log lines or
  cache keys, enabling cache poisoning.
