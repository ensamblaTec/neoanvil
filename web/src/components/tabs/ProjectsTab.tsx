// [284.B/288] Projects tab — fleet-level project federation overview.
// Reads from the workspace store (no snap prop — project data is fleet-level, not workspace-level).

import { useState, useEffect } from 'react';
import { useWorkspaceStore } from '../../store/useWorkspaceStore';
import type { WorkspaceStatus } from '../../types/metrics';

interface ProjectActivity {
  project_id: string;
  active_members: number;
  total_members: number;
  last_tool_call_unix: number;
  tool_call_count: number;
  min_idle_seconds: number;
  per_member: Array<{
    workspace_id: string;
    name: string;
    status: string;
    idle_seconds: number;
    tool_call_count: number;
  }>;
}

function statusColor(s: string): string {
  switch (s) {
    case 'running':     return '#22c55e';
    case 'starting':    return '#eab308';
    case 'unhealthy':   return '#f97316';
    case 'quarantined': return '#eab308';
    case 'error':       return '#ef4444';
    default:            return '#6b7280';
  }
}

function idleLabel(seconds: number): string {
  if (seconds === 0) return 'never used';
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return '>1d ago';
}

function ProjectCard({
  projectID,
  members,
}: {
  projectID: string;
  members: WorkspaceStatus[];
}) {
  const [open, setOpen] = useState(true);
  const [activity, setActivity] = useState<ProjectActivity | null>(null);

  const projectName = members[0]?.project_name ?? projectID;
  const runningCount = members.filter((m) => m.status === 'running').length;

  useEffect(() => {
    let cancelled = false;
    fetch(`/api/v1/projects/${projectID}/activity`)
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => { if (!cancelled && d) setActivity(d); })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [projectID]);

  return (
    <div style={{ border: '1px solid #1e293b', borderRadius: 6, marginBottom: 12 }}>
      {/* Header */}
      <div
        onClick={() => setOpen((v) => !v)}
        style={{
          display: 'flex', alignItems: 'center', gap: 10,
          padding: '8px 14px', cursor: 'pointer',
          background: '#0f1f2d', borderRadius: open ? '6px 6px 0 0' : 6,
        }}
      >
        <span style={{ color: '#64748b', fontSize: 10 }}>{open ? '▼' : '▶'}</span>
        <span style={{ fontWeight: 600, color: '#94a3b8', fontSize: 13, flex: 1 }}>{projectName}</span>
        <span style={{ fontSize: 11, color: runningCount > 0 ? '#22c55e' : '#6b7280' }}>
          {runningCount}/{members.length} running
        </span>
        {activity && activity.tool_call_count > 0 && (
          <span style={{ fontSize: 11, color: '#64748b' }}>
            {activity.tool_call_count} calls · idle {idleLabel(activity.min_idle_seconds)}
          </span>
        )}
      </div>

      {open && (
        <div style={{ padding: '6px 0' }}>
          {members.map((ws) => {
            const memberActivity = activity?.per_member.find((m) => m.workspace_id === ws.id);
            return (
              <div
                key={ws.id}
                style={{
                  display: 'flex', alignItems: 'center', gap: 10,
                  padding: '5px 14px 5px 28px', fontSize: 12,
                  borderTop: '1px solid #0f1f2d',
                }}
              >
                <span style={{ color: statusColor(ws.status), minWidth: 8 }}>●</span>
                <span style={{ flex: 1, color: '#cbd5e1', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {ws.name}
                </span>
                <span style={{ color: '#475569', fontSize: 11, minWidth: 70, textAlign: 'right' }}>
                  {ws.port ? `:${ws.port}` : ''}
                </span>
                {memberActivity ? (
                  <span style={{ color: '#64748b', fontSize: 11, minWidth: 90, textAlign: 'right' }}>
                    {memberActivity.tool_call_count > 0
                      ? `${memberActivity.tool_call_count} calls · ${idleLabel(memberActivity.idle_seconds)}`
                      : 'never used'}
                  </span>
                ) : (
                  <span style={{ color: '#475569', fontSize: 11, minWidth: 90, textAlign: 'right' }}>
                    {ws.idle_seconds && ws.idle_seconds > 0 ? idleLabel(ws.idle_seconds) : '—'}
                  </span>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

export function ProjectsTab() {
  const workspaces = useWorkspaceStore((s) => s.workspaces);

  const groups = new Map<string, WorkspaceStatus[]>();
  for (const ws of workspaces) {
    if (ws.project_id) {
      const list = groups.get(ws.project_id) ?? [];
      list.push(ws);
      groups.set(ws.project_id, list);
    }
  }

  const sorted = [...groups.entries()].sort(([, a], [, b]) => {
    return (a[0]?.project_name ?? '').localeCompare(b[0]?.project_name ?? '');
  });

  if (sorted.length === 0) {
    return (
      <div style={{ color: '#6b7280', fontSize: 12, padding: '20px 0' }}>
        No projects registered. Create a <code>.neo-project/neo.yaml</code> in
        the project root and restart Neo-Nexus.
      </div>
    );
  }

  return (
    <div>
      <div style={{ fontSize: 11, color: '#888', textTransform: 'uppercase', marginBottom: 12, letterSpacing: 0.5 }}>
        Projects ({sorted.length})
      </div>
      {sorted.map(([projectID, members]) => (
        <ProjectCard key={projectID} projectID={projectID} members={members} />
      ))}
    </div>
  );
}
