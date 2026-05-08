---
name: neo-doc-pack
description: Generate a documentation pack (README + code-snap PNGs) for a Jira ticket and attach it. Use when the user asks to "document MCPI-N", "generate doc pack", or "attach screenshots to <ticket>". Renders code snippets via chroma + headless Chrome to PNGs and zips them up for Jira upload.
disable-model-invocation: true
argument-hint: <TICKET_KEY> [file1.go file2.go ...]
allowed-tools: Bash Read Write
---

# Generate documentation pack for a Jira ticket

The user passed: $ARGUMENTS

The first argument is the ticket key (e.g. `MCPI-3`). Subsequent
arguments are paths to source files to render as code-snap PNGs.

## Step 1 — Validate inputs

If `$ARGUMENTS` is empty or first token doesn't match `^[A-Z]+-\d+$`,
abort and ask the operator for the ticket key.

If no file paths follow, ask "Which files should I include?". The
operator can also paste a `git diff --name-only HEAD~5..HEAD` listing
recent changes to pick from.

## Step 2 — Build the folder

```
mkdir -p ~/.neo/jira-docs/<TICKET_KEY>/{code,images,design}
```

For each source file requested, derive a **short snake_case descriptor**
from the function/concept (NOT from the original path):

| If file is... | Rename to... |
|---|---|
| `pkg/plugin/manifest.go` (the LoadManifest + perms check) | `manifest_permissions_check.go` |
| `pkg/nexus/plugin_pool.go` (full lifecycle) | `plugin_pool_lifecycle.go` |
| `cmd/plugin-jira/main.go` (transition handler) | `jira_transition_handler.go` |

```
# Strip path, give it a meaningful name
cp <src> ~/.neo/jira-docs/<TICKET_KEY>/code/<descriptor>.<ext>
```

**Naming rules** (canonical, see jira-workflow skill):
- 2-4 words snake_case, lowercase
- Reflects the QUÉ (concept/function), not the path
- Coherent across code/, images/, design/ — same basename, different extension

## Step 3 — Generate the README

Write `~/.neo/jira-docs/<TICKET_KEY>/README.md` with this template:

```markdown
# <TICKET_KEY> — <ticket summary fetched via jira/get_context>

## Resumen

<1-paragraph describing what changed and why, derived from the ticket
description and git log of the related commits>

## Commits

<bullet list of commit hashes touching these files,
extracted from `git log --oneline -- <files>`>

## Tests

<test count, coverage, AST_AUDIT status — verifiable by reading
the relevant *_test.go and running `go test -short -cover ./pkg/...`>

## Files included

<bullet list with one-line descriptions of each file>

## Snapshots

<list of .png files in images/, named after their source>
```

## Step 4 — Render code-snap PNGs

For each source file:

```bash
# Use the ${CLAUDE_SKILL_DIR}/scripts/codesnap.go OR call the plugin
# action when wired (TODO Épica 127.D):
go run ./internal/codesnap-cli \
   -src ~/.neo/jira-docs/<TICKET_KEY>/code/<basename> \
   -out ~/.neo/jira-docs/<TICKET_KEY>/images/<basename>.png \
   -title "<descriptive title>"
```

Each invocation produces both an HTML preview and a PNG. Keep both.

If the operator's machine has no Chrome/Chromium binary, the renderer
falls back to HTML-only (still useful — they can open in a browser
to verify output before zipping).

## Step 5 — Attach to Jira

```
jira_jira(action: "attach_artifact", ticket_id: "<TICKET_KEY>")
```

This zips the whole folder and uploads it to the Jira issue. Audit
log entry is automatic.

## Step 6 — Verify

```
jira_jira(action: "get_context", ticket_id: "<TICKET_KEY>")
```

Inspect the response for the new attachment in the issue's metadata,
or `curl -s -u "$EMAIL:$TOKEN" "https://<DOMAIN>/rest/api/3/issue/<KEY>?fields=attachment"`.

## Output

Print to operator:

```
📦 Pack ready for <TICKET_KEY>
  Location: ~/.neo/jira-docs/<TICKET_KEY>/
  Files:    N source snippets, M PNG snapshots
  Zip:      <TICKET_KEY>-artifacts.zip (X KB)
  Uploaded: ✓ visible at https://your-org.atlassian.net/browse/<TICKET_KEY>
```

## Anti-patterns

- Do NOT include `.env` files, credentials, secrets in the pack
- Do NOT pack files larger than ~5 MB total (Jira attachment cap is
  10 MB per file)
- Do NOT skip the README — the snapshots without context are noise

## See also

- `.claude/skills/jira-workflow/SKILL.md` — overall Jira doctrine
- `pkg/jira/codesnap.go` — the chroma + chromedp renderer
- `pkg/jira/attachments.go` — the ZIP + upload pipeline
