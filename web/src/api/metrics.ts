// [PILAR-XXVII/245.B] Typed fetch wrappers for the Nexus observability
// surface. Every caller passes an AbortSignal so polls don't leak when a
// component unmounts mid-request.

import type { Snapshot, SummaryResponse, WorkspaceStatus } from '../types/metrics';

const BASE = '';

function url(path: string): string {
  return `${BASE}${path}`;
}

export async function fetchMetrics(
  workspaceID: string,
  signal?: AbortSignal,
): Promise<Snapshot> {
  if (!workspaceID) throw new Error('workspaceID required');
  const res = await fetch(url(`/api/v1/workspaces/${encodeURIComponent(workspaceID)}/metrics`), { signal });
  if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
  return res.json() as Promise<Snapshot>;
}

export async function fetchSummary(signal?: AbortSignal): Promise<SummaryResponse> {
  const res = await fetch(url('/api/v1/metrics/summary'), { signal });
  if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
  return res.json() as Promise<SummaryResponse>;
}

export async function fetchStatus(signal?: AbortSignal): Promise<WorkspaceStatus[]> {
  const res = await fetch(url('/status'), { signal });
  if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
  return res.json() as Promise<WorkspaceStatus[]>;
}
