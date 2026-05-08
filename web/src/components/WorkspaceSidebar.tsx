// [PILAR-XXVII/245.E] Left sidebar — fleet overview. [284.B/C/D] Project grouping.

import { useWorkspaceStore } from '../store/useWorkspaceStore';
import type { WorkspaceStatus } from '../types/metrics';

interface Props {
  mobileOpen?: boolean;
  onItemPick?: () => void;
}

function statusDot(status: WorkspaceStatus['status']): { color: string; label: string } {
  switch (status) {
    case 'running':     return { color: '#22c55e', label: '●' };
    case 'starting':    return { color: '#eab308', label: '◐' };
    case 'unhealthy':   return { color: '#f97316', label: '○' };
    case 'quarantined': return { color: '#eab308', label: '⊘' };
    case 'error':       return { color: '#ef4444', label: '✕' };
    case 'stopped':     return { color: '#6b7280', label: '·' };
    default:            return { color: '#6b7280', label: '?' };
  }
}

// [Épica 248.D] Returns subtitle text and color based on MCP activity counters.
function activityInfo(ws: WorkspaceStatus): { label: string; color: string } {
  if (ws.status !== 'running') return { label: ws.status, color: '#6b7280' };
  const count = ws.tool_call_count ?? 0;
  const idle = ws.idle_seconds ?? 0;
  if (count === 0) return { label: 'running · never used', color: '#6b7280' };
  if (idle < 300)  return { label: `active · ${idle}s ago`, color: '#22c55e' };
  if (idle < 1800) return { label: `idle · ${Math.floor(idle / 60)}m`, color: '#eab308' };
  if (idle < 86400) return { label: `idle · ${Math.floor(idle / 3600)}h`, color: '#6b7280' };
  return { label: 'idle · >1d', color: '#6b7280' };
}

// [284.D] Lang accent color for project badge.
function langColor(lang?: string): string {
  switch (lang) {
    case 'go':         return '#22d3ee';
    case 'typescript': return '#60a5fa';
    case 'python':     return '#fbbf24';
    case 'rust':       return '#fb923c';
    default:           return '#9ca3af';
  }
}

// [284.C] Collapse state stored in localStorage.
function isGroupCollapsed(projectID: string): boolean {
  try {
    return localStorage.getItem(`neo_sidebar_collapsed_${projectID}`) === '1';
  } catch { return false; }
}
function toggleGroupCollapse(projectID: string): void {
  try {
    const key = `neo_sidebar_collapsed_${projectID}`;
    if (localStorage.getItem(key) === '1') {
      localStorage.removeItem(key);
    } else {
      localStorage.setItem(key, '1');
    }
  } catch { /* ignore */ }
}

function WorkspaceRow({
  ws,
  isActive,
  onPick,
}: {
  ws: WorkspaceStatus;
  isActive: boolean;
  onPick: (id: string) => void;
}) {
  const dot = statusDot(ws.status);
  const activity = activityInfo(ws);
  return (
    <div
      key={ws.id}
      onClick={() => onPick(ws.id)}
      title={`${ws.name} (${ws.status})\n${activity.label}${ws.tool_call_count ? ` · ${ws.tool_call_count} calls` : ''}\n${ws.path ?? ''}`}
      style={{
        padding: '8px 14px',
        cursor: 'pointer',
        background: isActive ? '#1f3a4d' : 'transparent',
        borderLeft: isActive ? '3px solid #22d3ee' : '3px solid transparent',
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        minWidth: 0,
      }}
    >
      <span style={{ color: dot.color, minWidth: 14, textAlign: 'center' }}>{dot.label}</span>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          fontSize: 14,
          fontWeight: isActive ? 600 : 400,
          whiteSpace: 'nowrap',
          overflow: 'hidden',
          textOverflow: 'ellipsis',
        }}>{ws.name}</div>
        <div style={{ fontSize: 10, color: activity.color }}>
          {activity.label}{ws.port ? ` · :${ws.port}` : ''}
          {ws.project_name && (
            <span style={{
              marginLeft: 6,
              padding: '0 4px',
              borderRadius: 3,
              background: '#1e293b',
              color: langColor(undefined),
              fontSize: 9,
            }}>
              {ws.project_name}
            </span>
          )}
        </div>
      </div>
    </div>
  );
}

export function WorkspaceSidebar({ mobileOpen = false, onItemPick }: Props) {
  const workspaces = useWorkspaceStore((s) => s.workspaces);
  const currentWsID = useWorkspaceStore((s) => s.currentWsID);
  const setCurrent = useWorkspaceStore((s) => s.setCurrent);
  const loading = useWorkspaceStore((s) => s.loadingStatus);

  const handlePick = (id: string) => {
    setCurrent(id);
    onItemPick?.();
  };

  // [284.B] Partition into project groups and standalone.
  const groups = new Map<string, WorkspaceStatus[]>();
  const standalone: WorkspaceStatus[] = [];
  for (const ws of workspaces) {
    if (ws.project_id) {
      const list = groups.get(ws.project_id) ?? [];
      list.push(ws);
      groups.set(ws.project_id, list);
    } else {
      standalone.push(ws);
    }
  }

  // Sort groups by project name (first member's project_name).
  const sortedGroups = [...groups.entries()].sort(([, a], [, b]) => {
    const na = a[0]?.project_name ?? '';
    const nb = b[0]?.project_name ?? '';
    return na.localeCompare(nb);
  });

  return (
    <aside className={`ws-sidebar${mobileOpen ? ' open' : ''}`}>
      <div style={{ padding: '8px 14px', fontSize: 11, color: '#888', textTransform: 'uppercase', letterSpacing: 0.5 }}>
        Workspaces ({workspaces.length}){loading ? ' …' : ''}
      </div>
      {workspaces.length === 0 && !loading && (
        <div style={{ padding: 14, color: '#888', fontSize: 12 }}>
          No workspaces registered. Start one with neo-nexus.
        </div>
      )}

      {/* [284.B] Project groups */}
      {sortedGroups.map(([projectID, members]) => {
        const projectName = members[0]?.project_name ?? projectID;
        const bestActivity = members.reduce((best, ws) => {
          const idle = ws.idle_seconds ?? Infinity;
          return idle < (best.idle_seconds ?? Infinity) ? ws : best;
        }, members[0]);
        const headerActivity = activityInfo(bestActivity);
        const collapsed = isGroupCollapsed(projectID);

        return (
          <div key={projectID}>
            {/* Group header */}
            <div
              onClick={() => { toggleGroupCollapse(projectID); }}
              style={{
                padding: '6px 14px',
                cursor: 'pointer',
                background: '#0f1f2d',
                borderLeft: '3px solid #334155',
                display: 'flex',
                alignItems: 'center',
                gap: 6,
                userSelect: 'none',
              }}
            >
              <span style={{ color: '#64748b', fontSize: 10 }}>{collapsed ? '▶' : '▼'}</span>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: '#94a3b8', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {projectName}
                </div>
                <div style={{ fontSize: 10, color: headerActivity.color }}>
                  {headerActivity.label} · {members.length} members
                </div>
              </div>
            </div>
            {/* Members */}
            {!collapsed && members.map((ws) => (
              <div key={ws.id} style={{ paddingLeft: 10 }}>
                <WorkspaceRow ws={ws} isActive={ws.id === currentWsID} onPick={handlePick} />
              </div>
            ))}
          </div>
        );
      })}

      {/* Standalone workspaces */}
      {sortedGroups.length > 0 && standalone.length > 0 && (
        <div style={{ padding: '6px 14px', fontSize: 10, color: '#475569', textTransform: 'uppercase', letterSpacing: 0.4 }}>
          Standalone
        </div>
      )}
      {standalone.map((ws) => (
        <WorkspaceRow key={ws.id} ws={ws} isActive={ws.id === currentWsID} onPick={handlePick} />
      ))}
    </aside>
  );
}
