# NeoAnvil Synced Directives — History

Archived obsolete entries from neo-synced-directives.md. Read-only — for audit and lineage.
[SRE-115.A]

This file previously contained 105+ deprecated directive entries (V6.2 through V10.x).
All were struck-through (`~~...~~`) indicating obsolescence. The full history is
preserved in git and can be recovered via `git log -p -- .claude/rules/neo-synced-directives-history.md`.

Only the most recent entries are kept below as reference for the deprecation pattern.

---

## Active (not deprecated)

42. [SRE-DIRECTIVE-CRUD] neo_learn_directive acepta campo `action`: add (default), update, delete. Para update/delete se requiere `directive_id` (entero 1-based). ADD con `supersedes: [1, 2]` auto-depreca las directivas indicadas. DELETE es soft-delete: marca como `~~OBSOLETO~~`, no borra fisicamente. Dual-layer sync refleja el estado real en .claude/rules/neo-synced-directives.md.

103. [SRE-DIRECTIVE-CRUD] neo_learn_directive acepta campo `action`: add (default), update, delete. Para update/delete se requiere `directive_id` (entero 1-based). ADD con `supersedes: [1, 2]` auto-depreca las directivas indicadas. DELETE es soft-delete: marca como `~~OBSOLETO~~`, no borra fisicamente.

## Last deprecated entries (examples)

104. ~~[SRE-BLAST-FALLBACK] Cuando BLAST_RADIUS retorna `graph_status: not_indexed`, ejecuta automaticamente grep fallback. (deprecated_by: 160)~~

105. ~~[SRE-AST-TOPOGRAPHY] COMPILE_AUDIT con target especifico devuelve symbol_map JSON. (deprecated_by: 159)~~

228. ~~PROBE-EXIST: Test if directive 227 exists by collision detection — this should fail or trigger sync~~

---

Full history available via `git log -p` and `git blame` on this file.
