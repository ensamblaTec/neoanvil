---
name: jira-create-pilar
description: Create a Jira Epic + child Stories from a neoanvil master_plan PILAR block. Use when the user asks to "create epic in jira", "convertir las épicas a stories", or wants to mirror a PILAR's progress in the project board. Reads .neo/master_plan.md, picks the named PILAR block, and walks the operator through Epic → child Stories creation with the workflow doctrine.
disable-model-invocation: true
argument-hint: <PILAR-name-or-block-prefix>
---

# Create Jira Epic + Stories from PILAR

You are about to mirror a chunk of `.neo/master_plan.md` into the
`MCPI` project board. Walk the operator through the steps end-to-end
and verify each side before continuing.

## Step 1 — Read the PILAR block

The user passed the argument: $ARGUMENTS

If empty, ask which PILAR or épica-prefix (e.g. "PILAR XXIII",
"123.x", "124.4"). Then:

1. Read `.neo/master_plan.md`
2. Extract the section matching the argument:
   - Header `## PILAR XXIII` → that whole section
   - Prefix `123.x` → all checked/unchecked items starting with `123.`
   - Single épica `124.4` → just that line
3. Show the operator the matched block and confirm before creating
   anything.

## Step 2 — Create the Epic

Apply rules from skills/jira-workflow/SKILL.md. Format:

```
jira_jira(
  action: "create_issue",
  issue_type: "Epic",
  summary: "[<label>] <PILAR title>",  // label = architecture|feature|bug|...
  description: "<contenido del bloque master_plan formato Epic>",
  labels: ["<label>", "neoanvil"],
  story_points: 13,    // Epic siempre 13
  start_date: "<inicio del bloque>",
  due_date: "<fin del bloque>",
  assignee_email: "user@example.com"
)
```

Capture the returned `MCPI-N` key.

## Step 3 — Create child Stories

For each épica del bloque (cerrada `[x]` o abierta `[ ]`):

1. Determine story_points por complejidad:
   - 1 — trivial (flag, .yaml.example update)
   - 2 — pequeño (helper, schema field)
   - 3 — estándar (single tool action, loader)
   - 5 — mediano (cross-package feature, REST client)
   - 8 — grande (full pipeline)
2. Format del summary: `[<label>] Épica N.M — <descripción corta>`
3. Description: contenido completo del bullet del master_plan + commit
   refs cuando estén disponibles
4. Set `parent_key` al Epic creado en step 2

```
jira_jira(
  action: "create_issue",
  issue_type: "Story",
  summary: "[<label>] Épica <N.M> — <text>",
  description: "<full content from master_plan>",
  parent_key: "<EPIC_KEY_FROM_STEP_2>",
  labels: ["<label>", "neoanvil"],
  story_points: <int>,
  start_date: "<YYYY-MM-DD>",
  due_date: "<YYYY-MM-DD>",
  assignee_email: "user@example.com"
)
```

NO incluir `reporter_email` — el screen scheme del project no lo
acepta.

## Step 4 — Apply transitions for closed épicas

Si la épica está marcada `[x]` en master_plan, walk it through:

```
Backlog → Selected for Development → In Progress → REVIEW → READY TO DEPLOY → Done
```

Each transition needs a `resolution_comment` con señal real (commit
hash, test count, files touched).

For open épicas `[ ]`: leave en Backlog.

## Step 5 — Move the Epic when applicable

- Si CUALQUIER child entró a "In Progress" o más allá → move Epic to
  In Progress
- Si TODAS las child stories están Done → move Epic to Done

## Step 6 — Optional documentation pack

Si el operador quiere atachar docs:

```
mkdir -p ~/.neo/jira-docs/<EPIC_KEY>/{code,images,design}
# operator drops files
jira_jira(action: "attach_artifact", ticket_id: "<EPIC_KEY>")
```

## Output al operador

Al terminar reporta:

```
✅ Epic MCPI-N created with M child stories
   Stories closed (Done): X
   Stories open (Backlog): Y
   Epic state: <In Progress | Done | Backlog>
```

Y el path Atlassian del Epic:
`https://your-org.atlassian.net/browse/<EPIC_KEY>`

## Check final invariantes

Antes de declarar listo:
- Cada Story tiene parent_key
- Summaries siguen `[<label>] ...`
- Story points son del set Asana {1,2,3,5,8,13}
- Epic en estado consistente con sus child stories
