// [PILAR-XXVII/245.I]
import type { Snapshot, TokenBreakdown } from '../../types/metrics';

interface Props { snap: Snapshot }

export function TokensTab({ snap }: Props) {
  const { today_input_tokens, today_output_tokens, today_cost_usd, mcp_traffic, internal_inference, last_7_days } = snap.tokens;
  return (
    <div>
      <div className="grid-2" style={{ marginBottom: 20 }}>
        <Card title="Today total">
          <Row k="Input tokens" v={fmt(today_input_tokens)} />
          <Row k="Output tokens" v={fmt(today_output_tokens)} />
          <Row k="Cost USD" v={`$${today_cost_usd.toFixed(4)}`} />
        </Card>
        <Card title="Last 7 days cost">
          <div style={{ fontSize: 24, color: '#22d3ee', fontWeight: 600 }}>
            ${last_7_days.reduce((a, b) => a + b.cost_usd, 0).toFixed(4)}
          </div>
          <div style={{ fontSize: 11, color: '#888' }}>across {last_7_days.length} days with activity</div>
        </Card>
      </div>

      <div className="grid-2" style={{ marginBottom: 20 }}>
        <BreakdownCard title="MCP traffic (external agent)" breakdown={mcp_traffic} />
        <BreakdownCard title="Internal inference (pkg/inference)" breakdown={internal_inference} />
      </div>

      {last_7_days.length > 0 && (
        <Card title="Last 7 days">
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', minWidth: 480, fontSize: 13, borderCollapse: 'collapse' }}>
              <thead>
                <tr style={{ color: '#888' }}>
                  <th style={{ textAlign: 'left', padding: '4px 8px' }}>Day</th>
                  <th style={{ textAlign: 'right', padding: '4px 8px' }}>MCP in/out</th>
                  <th style={{ textAlign: 'right', padding: '4px 8px' }}>Internal in/out</th>
                  <th style={{ textAlign: 'right', padding: '4px 8px' }}>Cost</th>
                </tr>
              </thead>
              <tbody>
                {last_7_days.map((d) => (
                  <tr key={d.day} style={{ borderTop: '1px solid #1a1a1a' }}>
                    <td style={{ padding: '4px 8px' }}>{d.day}</td>
                    <td style={{ padding: '4px 8px', textAlign: 'right' }}>{fmt(d.mcp_input)}/{fmt(d.mcp_output)}</td>
                    <td style={{ padding: '4px 8px', textAlign: 'right' }}>{fmt(d.internal_input)}/{fmt(d.internal_output)}</td>
                    <td style={{ padding: '4px 8px', textAlign: 'right' }}>${d.cost_usd.toFixed(4)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}

function BreakdownCard({ title, breakdown }: { title: string; breakdown: TokenBreakdown }) {
  return (
    <Card title={title}>
      <Row k="Input" v={fmt(breakdown.input_tokens)} />
      <Row k="Output" v={fmt(breakdown.output_tokens)} />
      <Row k="Cost" v={`$${breakdown.cost_usd.toFixed(4)}`} />
      <KVSection title="By agent" m={breakdown.by_agent} />
      <KVSection title="By tool" m={breakdown.by_tool} />
      {breakdown.by_prompt_type && <KVSection title="By prompt type" m={breakdown.by_prompt_type} />}
    </Card>
  );
}

function KVSection({ title, m }: { title: string; m: Record<string, number> | undefined }) {
  if (!m || Object.keys(m).length === 0) return null;
  const sorted = Object.entries(m).sort((a, b) => b[1] - a[1]);
  return (
    <div style={{ marginTop: 10 }}>
      <div style={{ fontSize: 10, color: '#777', textTransform: 'uppercase', marginBottom: 4 }}>{title}</div>
      {sorted.map(([k, v]) => (
        <div key={k} style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, padding: '2px 0' }}>
          <span>{k}</span><span style={{ color: '#bbb' }}>{fmt(v)}</span>
        </div>
      ))}
    </div>
  );
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div style={{ background: '#0d0d0d', border: '1px solid #1f1f1f', borderRadius: 6, padding: 12 }}>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 8 }}>{title}</div>
      {children}
    </div>
  );
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 13, padding: '2px 0' }}>
      <span style={{ color: '#aaa' }}>{k}</span>
      <span>{v}</span>
    </div>
  );
}

function fmt(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toLocaleString();
}
