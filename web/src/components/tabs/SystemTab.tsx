// [PILAR-XXVII/245.M]
import type { Snapshot } from '../../types/metrics';

interface Props { snap: Snapshot }

export function SystemTab({ snap }: Props) {
  return (
    <div>
      <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 14, marginBottom: 16 }}>
        <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>Runtime snapshot</div>
        <Row k="Workspace ID" v={snap.workspace_id} />
        <Row k="Workspace name" v={snap.workspace_name} />
        <Row k="Uptime (s)" v={String(snap.uptime_seconds)} />
        <Row k="Generated at" v={new Date(snap.generated_at).toLocaleString()} />
        <Row k="Heap MB" v={snap.memory.heap_mb.toFixed(1)} />
        <Row k="Goroutines" v={String(snap.memory.goroutines)} />
      </div>
      <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 14 }}>
        <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>Recent events ({snap.recent_events.length})</div>
        <div style={{ maxHeight: 300, overflowY: 'auto' }}>
          {snap.recent_events.length === 0 ? (
            <div style={{ color: '#777', fontSize: 12 }}>No events captured yet.</div>
          ) : (
            snap.recent_events.slice(0, 50).map((e, i) => (
              <div key={i} style={{ fontSize: 12, padding: '3px 0', borderBottom: '1px solid #1a1a1a', display: 'flex', gap: 10 }}>
                <span style={{ color: '#666', minWidth: 70 }}>{new Date(e.ts).toLocaleTimeString()}</span>
                <span style={{ color: severityColor(e.severity), minWidth: 80 }}>{e.severity ?? 'info'}</span>
                <span>{e.type}</span>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', padding: '3px 0', fontSize: 13 }}>
      <span style={{ color: '#aaa' }}>{k}</span>
      <span style={{ fontFamily: 'monospace' }}>{v}</span>
    </div>
  );
}

function severityColor(s?: string): string {
  switch (s) {
    case 'critical': return '#ef4444';
    case 'warning':  return '#eab308';
    default:         return '#888';
  }
}
