#!/usr/bin/env bash
# neo-restore.sh — Restaura un neo-bundle en la máquina destino.
#
# Scope: neoanvil workspace + nexus-home (shared/knowledge).
# NO toca: workspaces.json, nexus.yaml, otras workspaces (strategos, frontend, vision-link).
# NO sobreescribe archivos que estén en git (neo.yaml, .claude/rules/, docs/).
#
# Uso:
#   ./scripts/neo-restore.sh <bundle-dir>
#   ./scripts/neo-restore.sh .neo/docs/doc-prev-note/neo-bundle-20260424-160000
#
# Variables de entorno opcionales:
#   NEO_NEXUS_URL   URL del dispatcher (default: http://127.0.0.1:9000)
#   DRY_RUN=1       Solo muestra qué haría, sin ejecutar

set -euo pipefail

BUNDLE="${1:-}"
NEO_NEXUS_URL="${NEO_NEXUS_URL:-http://127.0.0.1:9000}"
DRY_RUN="${DRY_RUN:-0}"
NEO_HOME="${HOME}/.neo"
WORKSPACE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# ── helpers ──────────────────────────────────────────────────────────────────

log()  { echo "  $*"; }
ok()   { echo "✓ $*"; }
warn() { echo "⚠ $*"; }
err()  { echo "✗ $*" >&2; }
run()  { [[ "$DRY_RUN" == "1" ]] && echo "  [dry] $*" || eval "$@"; }

# ── validaciones ─────────────────────────────────────────────────────────────

if [[ -z "$BUNDLE" ]]; then
    err "Uso: $0 <bundle-dir>"
    exit 1
fi

BUNDLE="$(realpath "$BUNDLE")"
if [[ ! -d "$BUNDLE" ]]; then
    err "Bundle no encontrado: $BUNDLE"
    exit 1
fi

echo "neo-restore.sh"
echo "  Bundle:    $BUNDLE"
echo "  Workspace: $WORKSPACE_ROOT"
echo "  Nexus:     $NEO_NEXUS_URL"
[[ "$DRY_RUN" == "1" ]] && echo "  Modo:      DRY RUN (sin cambios)"
echo ""

# ── 1. Verificar qué hay en el bundle ────────────────────────────────────────

echo "[ 1/3 ] Analizando bundle..."

HAS_NEO_GO=0
HAS_NEXUS_HOME=0
[[ -d "$BUNDLE/neoanvil" ]] && HAS_NEO_GO=1
[[ -d "$BUNDLE/nexus-home" ]] && HAS_NEXUS_HOME=1

[[ $HAS_NEO_GO == 1 ]]    && log "neoanvil/ encontrado" || warn "neoanvil/ no encontrado"
[[ $HAS_NEXUS_HOME == 1 ]] && log "nexus-home/ encontrado" || warn "nexus-home/ no encontrado"

# ── 2. neoanvil workspace (solo archivos NO en git) ─────────────────────────────

if [[ $HAS_NEO_GO == 1 ]]; then
    echo ""
    echo "[ 2/3 ] Restaurando neoanvil workspace (archivos no-git)..."

    SRC_NEO="$BUNDLE/neoanvil/.neo"
    DST_NEO="$WORKSPACE_ROOT/.neo"

    # incidents/ — añadir los que falten (no reemplazar)
    if [[ -d "$SRC_NEO/incidents" ]]; then
        added=0
        for f in "$SRC_NEO/incidents/"*.md; do
            [[ -f "$f" ]] || continue
            fname="$(basename "$f")"
            if [[ ! -f "$DST_NEO/incidents/$fname" ]]; then
                run "cp '$f' '$DST_NEO/incidents/$fname'"
                log "incidents: +$fname"
                ((added++)) || true
            fi
        done
        [[ $added -gt 0 ]] && ok "incidents: $added archivos nuevos" || log "incidents: ya en sync"
    fi

    # master_plan / master_done — solo si el bundle es más reciente Y tiene más tareas cerradas
    for plan_file in master_plan.md master_done.md; do
        src="$SRC_NEO/$plan_file"
        dst="$DST_NEO/$plan_file"
        if [[ -f "$src" && -f "$dst" ]]; then
            src_closed=$(grep -c '^\- \[x\]' "$src" 2>/dev/null || echo 0)
            dst_closed=$(grep -c '^\- \[x\]' "$dst" 2>/dev/null || echo 0)
            if [[ $src_closed -gt $dst_closed ]]; then
                warn "$plan_file: bundle tiene más épicas cerradas ($src_closed vs $dst_closed) — revisar manualmente"
                warn "  cp '$src' '$dst'  # ejecutar si quieres el de Mac"
            else
                log "$plan_file: Linux en sync o más avanzado ($dst_closed >= $src_closed ✓)"
            fi
        fi
    done

    # technical_debt.md — informativo
    src="$SRC_NEO/technical_debt.md"
    dst="$DST_NEO/technical_debt.md"
    if [[ -f "$src" && -f "$dst" ]]; then
        src_lines=$(wc -l < "$src")
        dst_lines=$(wc -l < "$dst")
        if [[ $src_lines -ne $dst_lines ]]; then
            warn "technical_debt.md: bundle=$src_lines líneas vs Linux=$dst_lines — revisar manualmente"
        else
            log "technical_debt.md: en sync"
        fi
    fi
fi

# ── 3. nexus-home shared/knowledge → global.db ───────────────────────────────

if [[ $HAS_NEXUS_HOME == 1 ]]; then
    echo ""
    echo "[ 3/3 ] Restaurando nexus-home shared/knowledge → global.db..."

    SRC_KNW="$BUNDLE/nexus-home/shared/knowledge"

    if [[ ! -d "$SRC_KNW" ]]; then
        warn "nexus-home/shared/knowledge/ no encontrado, saltando"
    else
        # Crear estructura de subdirectorios
        for sub_dir in debt lessons patterns decisions enums epics flows improvements \
                       inbox incidents operator rules shared types upgrades; do
            run "mkdir -p '$NEO_HOME/shared/knowledge/$sub_dir'"
        done

        # Verificar que Nexus responde
        if ! curl -sf "$NEO_NEXUS_URL/status" > /dev/null 2>&1; then
            warn "Nexus no responde en $NEO_NEXUS_URL — copiando .md sin importar a global.db"
            warn "Ejecuta make rebuild-restart y luego: $0 $BUNDLE"
            # Copiar de todas formas para boot-time import cuando Nexus reinicie
            run "cp -n '$SRC_KNW'/**/*.md '$NEO_HOME/shared/knowledge/'**/ 2>/dev/null || true"
        else
            # Importar via REST — más fiable que fsnotify para directorios nuevos
            imported=0
            skipped=0
            errors=0

            while IFS= read -r md_file; do
                [[ -f "$md_file" ]] || continue

                # Determinar namespace desde la ruta relativa
                rel="${md_file#$SRC_KNW/}"
                namespace="${rel%%/*}"
                fname="$(basename "$md_file" .md)"

                # Parsear frontmatter con python3
                result=$(python3 - "$md_file" "$NEO_NEXUS_URL" <<'PYEOF'
import sys, json, re, urllib.request

md_path, nexus_url = sys.argv[1], sys.argv[2]
text = open(md_path).read()
fm_match = re.match(r'^---\n(.*?)\n---\n(.*)', text, re.DOTALL)
if not fm_match:
    print("ERR:no_frontmatter")
    sys.exit(0)

fm_raw, content = fm_match.group(1), fm_match.group(2).strip()
key_m  = re.search(r'^key:\s*(.+)$', fm_raw, re.MULTILINE)
ns_m   = re.search(r'^namespace:\s*(.+)$', fm_raw, re.MULTILINE)
hot_m  = re.search(r'^hot:\s*(.+)$', fm_raw, re.MULTILINE)
tags_m = re.search(r'^tags:\s*\[(.+)\]$', fm_raw, re.MULTILINE)

if not key_m or not ns_m:
    print("ERR:missing_key_ns")
    sys.exit(0)

payload = json.dumps({
    "namespace": ns_m.group(1).strip(),
    "key":       key_m.group(1).strip(),
    "content":   content,
    "tags":      [t.strip() for t in tags_m.group(1).split(",")] if tags_m else [],
    "hot":       hot_m.group(1).strip().lower() == "true" if hot_m else False
}).encode()

try:
    req = urllib.request.Request(f"{nexus_url}/api/v1/shared/nexus/store",
        data=payload, headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=5) as r:
        print("OK:" + r.read().decode()[:80])
except Exception as e:
    print(f"ERR:{e}")
PYEOF
)
                if [[ "$result" == ERR:* ]]; then
                    warn "$(basename "$md_file"): $result"
                    ((errors++)) || true
                else
                    # También copiar el .md al disco para persistencia
                    tgt_dir="$NEO_HOME/shared/knowledge/$namespace"
                    run "cp '$md_file' '$tgt_dir/'"
                    log "  $namespace/$(basename "$md_file" .md)"
                    ((imported++)) || true
                fi
            done < <(find "$SRC_KNW" -name "*.md" | sort)

            ok "shared/knowledge: $imported importados, $errors errores"
        fi
    fi
fi

# ── Resumen final ─────────────────────────────────────────────────────────────

echo ""
echo "────────────────────────────────────"
echo "Restauración completada."
echo ""
echo "Pendiente (requiere Opción B — DB export/import tool):"
echo "  • coldstore.db (memex/directives de Mac)"
echo "  • knowledge.db workspace (knowledge store local)"
echo ""
echo "Para verificar global.db:"
echo "  curl -s -X POST $NEO_NEXUS_URL/api/v1/shared/nexus/list \\"
echo "    -H 'Content-Type: application/json' -d '{\"namespace\":\"*\"}' | python3 -m json.tool | grep '\"key\"'"
