// [PILAR-XXVII/245.L] Placeholder — a dedicated /api/v1/directives
// endpoint is pending. For now the tab explains where to look on disk.
import type { Snapshot } from '../../types/metrics';

interface Props { snap: Snapshot }

export function RulesTab(_: Props) {
  return (
    <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 16 }}>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>Rules tab — pending API</div>
      <p style={{ lineHeight: 1.6, fontSize: 13 }}>
        Directives aren't exposed through the metrics API yet. For the
        current active list, read <code>.claude/rules/neo-synced-directives.md</code>
        directly, or invoke <code>neo_memory(action: "learn")</code> to
        add/update entries — they sync both to BoltDB and the rules file.
      </p>
      <p style={{ fontSize: 12, color: '#888', marginTop: 14 }}>
        Planned: table of active vs deprecated directives with search + filter,
        backed by a new <code>/api/v1/directives</code> endpoint on neo-mcp.
      </p>
    </div>
  );
}
