// [PILAR-XXVII/245.H]
import { useMemo, useState } from 'react';
import type { Snapshot, ToolStats } from '../../types/metrics';

interface Props { snap: Snapshot }

type SortKey = 'name' | 'calls' | 'errors' | 'error_rate' | 'p50_ms' | 'p95_ms' | 'p99_ms';

export function ToolsTab({ snap }: Props) {
  const [sortKey, setSortKey] = useState<SortKey>('calls');
  const [search, setSearch] = useState('');

  const rows = useMemo(() => {
    const all = unique(snap.tools.top_by_calls, snap.tools.top_by_errors, snap.tools.top_by_p99);
    const filtered = search
      ? all.filter((t) => t.name.toLowerCase().includes(search.toLowerCase()))
      : all;
    return [...filtered].sort((a, b) => {
      if (sortKey === 'name') return a.name.localeCompare(b.name);
      return (b[sortKey] as number) - (a[sortKey] as number);
    });
  }, [snap, sortKey, search]);

  const headers: { key: SortKey; label: string; align?: 'right' }[] = [
    { key: 'name', label: 'Tool' },
    { key: 'calls', label: 'Calls', align: 'right' },
    { key: 'errors', label: 'Errors', align: 'right' },
    { key: 'error_rate', label: 'Err %', align: 'right' },
    { key: 'p50_ms', label: 'p50 ms', align: 'right' },
    { key: 'p95_ms', label: 'p95 ms', align: 'right' },
    { key: 'p99_ms', label: 'p99 ms', align: 'right' },
  ];

  return (
    <div>
      <div style={{ marginBottom: 12, display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'center' }}>
        <input
          type="text"
          className="ws-search"
          placeholder="search tool name…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <div style={{ fontSize: 12, color: '#888' }}>Total 24h: {snap.tools.total_calls_24h} calls</div>
      </div>

      <div className="tools-table-wrap">
        <table className="tools-table">
          <thead>
            <tr>
              {headers.map((h) => (
                <th
                  key={h.key}
                  className={`${h.align === 'right' ? 'right' : ''}${sortKey === h.key ? ' active' : ''}`}
                  onClick={() => setSortKey(h.key)}
                >
                  {h.label}{sortKey === h.key ? ' ▾' : ''}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.name}>
                <td>{r.name}</td>
                <td className="right">{r.calls}</td>
                <td className="right" style={{ color: r.errors > 0 ? '#f97316' : undefined }}>{r.errors}</td>
                <td className="right">{(r.error_rate * 100).toFixed(1)}%</td>
                <td className="right">{r.p50_ms.toFixed(2)}</td>
                <td className="right">{r.p95_ms.toFixed(2)}</td>
                <td className="right" style={{ color: r.p99_ms > 100 ? '#ef4444' : undefined }}>
                  {r.p99_ms.toFixed(2)}
                </td>
              </tr>
            ))}
            {rows.length === 0 && (
              <tr><td colSpan={7} style={{ padding: 14, color: '#777', textAlign: 'center' }}>(no tool activity yet)</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function unique(...lists: ToolStats[][]): ToolStats[] {
  const seen = new Map<string, ToolStats>();
  for (const list of lists) {
    for (const t of list) {
      const existing = seen.get(t.name);
      if (!existing || t.calls > existing.calls) seen.set(t.name, t);
    }
  }
  return Array.from(seen.values());
}
