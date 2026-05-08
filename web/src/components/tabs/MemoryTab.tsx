// [PILAR-XXVII/245.K]
import type { Snapshot } from '../../types/metrics';

interface Props { snap: Snapshot }

export function MemoryTab({ snap }: Props) {
  const m = snap.memory;
  const rows: { k: string; v: string; warn?: boolean }[] = [
    { k: 'Heap', v: `${m.heap_mb.toFixed(1)} MB` },
    { k: 'Stack', v: `${m.stack_mb.toFixed(1)} MB` },
    { k: 'Goroutines', v: String(m.goroutines) },
    { k: 'GC runs', v: String(m.gc_runs) },
    { k: 'GC pause last', v: `${m.gc_pause_last_ms.toFixed(2)} ms` },
    ...(m.cpg_heap_limit_mb > 0
      ? [{ k: 'CPG', v: `${m.cpg_heap_mb}/${m.cpg_heap_limit_mb} MB (${m.cpg_heap_pct}%)`, warn: m.cpg_heap_pct > 85 }]
      : []),
    { k: 'NumCPU', v: String(m.num_cpu) },
  ];
  const caches: { k: string; v: number }[] = [
    { k: 'QueryCache', v: m.query_cache_hit_rate },
    { k: 'TextCache', v: m.text_cache_hit_rate },
    { k: 'EmbCache', v: m.emb_cache_hit_rate },
  ];
  return (
    <div className="grid-2">
      <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 14 }}>
        <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>Runtime</div>
        {rows.map((r) => (
          <div key={r.k} style={{ display: 'flex', justifyContent: 'space-between', padding: '4px 0', fontSize: 13 }}>
            <span style={{ color: '#aaa' }}>{r.k}</span>
            <span style={{ color: r.warn ? '#eab308' : undefined }}>{r.v}</span>
          </div>
        ))}
      </div>
      <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 14 }}>
        <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 8 }}>
          Cache hit-rate (5-min window)
        </div>
        {caches.map((c) => (
          <div key={c.k} style={{ marginBottom: 8 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 13 }}>
              <span style={{ color: '#aaa' }}>{c.k}</span>
              <span>{(c.v * 100).toFixed(1)}%</span>
            </div>
            <div style={{ background: '#222', height: 4, borderRadius: 2, marginTop: 2 }}>
              <div style={{ width: `${c.v * 100}%`, background: '#22d3ee', height: '100%', borderRadius: 2 }} />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
