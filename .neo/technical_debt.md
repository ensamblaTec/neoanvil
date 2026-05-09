# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de Neo-Go.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---

## Active deferred items

### [ds-audit-pending] Pattern D Docker stack — DeepSeek pro audit

**Status:** 2026-05-09 — Nexus down during the planned DS pro audit
(operator stopped native to test docker-up). Manual pen-and-paper
audit performed instead, covering 8 threat surfaces (UID mismatch,
bind-mount escape via symlinks, concurrent BoltDB, volume lifecycle,
docker-seed race, GPU sharing, project name collision, backward
compat). Findings 1, 2, 5, 7 applied; 3, 4, 6, 8 documented or
already covered.

**Re-run when Nexus is available:**
```bash
mcp__neoanvil__deepseek_call \
  action: red_team_audit \
  model: deepseek-v4-pro \
  reasoning_effort: high \
  files: ["Dockerfile", "docker-compose.yaml", "scripts/docker-entrypoint.sh", "docs/onboarding/docker-architecture.md"]
```

The pro+max audit may surface CVEs in the cgo toolchain (apk add gcc
musl-dev pulls a compiler chain into stage 3) or scheduler-level
issues with GPU sharing under sustained load that the pen-and-paper
trace can't reach.

---
