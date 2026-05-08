// [PILAR-XXVII/245.J]
import type { Snapshot } from '../../types/metrics';

interface Props { snap: Snapshot }

export function MutationsTab({ snap }: Props) {
  const m = snap.mutations;
  const max = m.top_hotspots.reduce((acc, h) => Math.max(acc, h.count), 0) || 1;
  return (
    <div>
      <div style={{ display: 'flex', gap: 20, marginBottom: 20 }}>
        <Stat label="Certified (24h)" value={m.certified_24h} color="#22c55e" />
        <Stat label="Bypassed (24h)" value={m.bypassed_24h} color={m.bypassed_24h > 0 ? '#eab308' : '#444'} />
      </div>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>Top hotspots</div>
      {m.top_hotspots.length === 0 ? (
        <div style={{ fontSize: 12, color: '#777' }}>
          No certified mutations recorded yet. Edit + certify files to populate the heatmap.
        </div>
      ) : (
        m.top_hotspots.map((h) => {
          const pct = (h.count / max) * 100;
          return (
            <div key={h.path} style={{ marginBottom: 6 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12 }}>
                <span style={{ fontFamily: 'monospace' }}>{h.path}</span>
                <span style={{ color: '#bbb' }}>{h.count}</span>
              </div>
              <div style={{ background: '#222', height: 6, borderRadius: 3, marginTop: 2 }}>
                <div style={{ width: `${pct}%`, background: '#22d3ee', height: '100%', borderRadius: 3 }} />
              </div>
            </div>
          );
        })
      )}
    </div>
  );
}

function Stat({ label, value, color }: { label: string; value: number; color: string }) {
  return (
    <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: '12px 18px', minWidth: 180 }}>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
      <div style={{ fontSize: 28, color, fontWeight: 600 }}>{value}</div>
    </div>
  );
}
