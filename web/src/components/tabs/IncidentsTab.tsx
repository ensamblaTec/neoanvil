// [PILAR-XXVII/245.N]
import { useState } from 'react';
import type { Snapshot, EventEntry } from '../../types/metrics';

interface Props { snap: Snapshot }

export function IncidentsTab({ snap }: Props) {
  const [expanded, setExpanded] = useState<number | null>(null);
  return (
    <div>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 10 }}>
        Recent events / incidents ({snap.recent_events.length})
      </div>
      {snap.recent_events.length === 0 ? (
        <div style={{ color: '#777', fontSize: 12 }}>
          No events recorded. The SSE event bus feeds this list via the events_ring bucket.
        </div>
      ) : (
        snap.recent_events.map((e, i) => (
          <EventRow key={i} ev={e} expanded={expanded === i} onToggle={() => setExpanded(expanded === i ? null : i)} />
        ))
      )}
    </div>
  );
}

function EventRow({ ev, expanded, onToggle }: { ev: EventEntry; expanded: boolean; onToggle: () => void }) {
  return (
    <div style={{ borderBottom: '1px solid #1a1a1a', padding: '6px 0', cursor: 'pointer' }} onClick={onToggle}>
      <div style={{ display: 'flex', gap: 10, fontSize: 13, alignItems: 'center' }}>
        <span style={{ color: '#666', minWidth: 80 }}>{new Date(ev.ts).toLocaleTimeString()}</span>
        <span style={{ color: sev(ev.severity), minWidth: 80 }}>{ev.severity ?? 'info'}</span>
        <span>{ev.type}</span>
        <span style={{ marginLeft: 'auto', color: '#555' }}>{expanded ? '▾' : '▸'}</span>
      </div>
      {expanded && ev.payload && (
        <pre style={{ fontSize: 11, background: '#050505', padding: 8, margin: '6px 0 2px', borderRadius: 4, color: '#bbb', overflowX: 'auto' }}>
          {JSON.stringify(ev.payload, null, 2)}
        </pre>
      )}
    </div>
  );
}

function sev(s?: string): string {
  switch (s) {
    case 'critical': return '#ef4444';
    case 'warning':  return '#eab308';
    default:         return '#888';
  }
}
