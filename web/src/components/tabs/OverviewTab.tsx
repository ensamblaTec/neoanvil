// [PILAR-XXVII/245.G]
import type { Snapshot } from '../../types/metrics';
import { Gauge } from '../Gauge';

interface Props { snap: Snapshot }

export function OverviewTab({ snap }: Props) {
  const mem = snap.memory;
  const tools = snap.tools;
  const heapLimit = mem.cpg_heap_limit_mb; // real ceiling from neo.yaml cpg.max_heap_mb
  return (
    <div>
      <h2 style={{ margin: 0, color: '#22d3ee', fontSize: 20, overflow: 'hidden', textOverflow: 'ellipsis' }}>
        {snap.workspace_name}{' '}
        <span style={{ fontSize: 12, color: '#888' }}>· {shortId(snap.workspace_id)}</span>
      </h2>
      <div style={{ fontSize: 12, color: '#888', marginBottom: 16 }}>
        Uptime: {humanDuration(snap.uptime_seconds)} · refreshed {new Date(snap.generated_at).toLocaleTimeString()}
      </div>

      <div className="grid-gauges">
        {/* Heap — scaled against the configured CPG heap limit (usually
            512 MB) since that's the real ceiling enforced by the
            orchestrator. CPG heap == process heap; we don't duplicate
            the gauge. [PILAR-XXVII/245.Q] */}
        <Gauge
          label="Heap / CPG limit"
          value={heapLimit > 0 ? (mem.heap_mb / heapLimit) * 100 : Math.min(100, (mem.heap_mb / 512) * 100)}
          display={`${mem.heap_mb.toFixed(0)}/${heapLimit || '—'} MB`}
          thresholds={{ yellow: 60, red: 85 }}
        />
        {/* Goroutines — scaled to a 1000-goroutine ceiling. 200 is
            healthy (green), 500 is elevated (yellow), 800+ is concerning
            (red). A neo-mcp at idle sits around 30-100. */}
        <Gauge
          label="Goroutines"
          value={Math.min(100, (mem.goroutines / 1000) * 100)}
          display={String(mem.goroutines)}
          thresholds={{ yellow: 50, red: 80 }}
        />
        {(() => {
          // [PILAR-XXVIII hotfix] When no cache has handled a request
          // yet, hit rate is legitimately 0% but painting it red is
          // misleading. Show gray "n/a" + fixed mid-dial until at
          // least one cache has real traffic.
          const hasData = mem.query_cache_hit_rate + mem.text_cache_hit_rate + mem.emb_cache_hit_rate > 0;
          return hasData ? (
            <Gauge
              label="Cache hit"
              value={avgCache(mem) * 100}
              thresholds={{ yellow: 40, red: 70 }}
              invert
            />
          ) : (
            <Gauge
              label="Cache hit"
              value={50}              // fill half the ring — visual placeholder
              display="n/a"
              color="#6b7280"         // neutral gray — no alarm
            />
          );
        })()}
      </div>

      <div className="grid-2" style={{ marginTop: 24 }}>
        <div className="card">
          <div className="card-title">Tools — 24h: {tools.total_calls_24h} calls</div>
          {tools.top_by_calls.slice(0, 5).map((t) => (
            <Row key={t.name} k={t.name} v={`${t.calls} calls · p99 ${t.p99_ms.toFixed(1)} ms`} />
          ))}
          {tools.top_by_calls.length === 0 && <EmptyHint>No tool calls yet.</EmptyHint>}
        </div>
        <div className="card">
          <div className="card-title">High-p99 tools</div>
          {tools.top_by_p99.slice(0, 5).map((t) => (
            <Row
              key={t.name}
              k={t.name}
              v={<span style={{ color: t.p99_ms > 100 ? '#ef4444' : '#bbb' }}>{t.p99_ms.toFixed(1)} ms</span>}
            />
          ))}
          {tools.top_by_p99.length === 0 && <EmptyHint>(no data)</EmptyHint>}
        </div>
      </div>
    </div>
  );
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="kv-row">
      <span className="k" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{k}</span>
      <span className="v">{v}</span>
    </div>
  );
}

function EmptyHint({ children }: { children: React.ReactNode }) {
  return <div style={{ fontSize: 12, color: '#777', padding: '6px 0' }}>{children}</div>;
}

function shortId(id: string) { return id.length > 10 ? `${id.slice(0, 10)}…` : id; }

function humanDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

function avgCache(mem: Snapshot['memory']): number {
  const v = [mem.query_cache_hit_rate, mem.text_cache_hit_rate, mem.emb_cache_hit_rate].filter((x) => x > 0);
  if (v.length === 0) return 0;
  return v.reduce((a, b) => a + b, 0) / v.length;
}
