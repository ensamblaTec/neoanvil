// [PILAR-XXVII/245.C] Zustand store: current workspace + cached metrics
// + fleet list. Polling lives in useMetricsPolling.ts — the store itself
// is framework-agnostic.

import { create } from 'zustand';
import type { Snapshot, WorkspaceStatus } from '../types/metrics';
import { fetchMetrics, fetchStatus } from '../api/metrics';

interface WorkspaceState {
  currentWsID: string | null;
  workspaces: WorkspaceStatus[];
  metrics: Snapshot | null;
  lastFetch: number;
  loadingMetrics: boolean;
  loadingStatus: boolean;
  error: string | null;

  setCurrent: (id: string) => void;
  refreshStatus: (signal?: AbortSignal) => Promise<void>;
  refreshMetrics: (signal?: AbortSignal) => Promise<void>;
}

export const useWorkspaceStore = create<WorkspaceState>((set, get) => ({
  currentWsID: null,
  workspaces: [],
  metrics: null,
  lastFetch: 0,
  loadingMetrics: false,
  loadingStatus: false,
  error: null,

  setCurrent: (id) => {
    if (get().currentWsID === id) return;
    set({ currentWsID: id, metrics: null });
  },

  refreshStatus: async (signal) => {
    if (get().loadingStatus) return;
    set({ loadingStatus: true });
    try {
      const items = await fetchStatus(signal);
      set({ workspaces: items, error: null });
      // Auto-pick first running workspace if none selected yet.
      if (!get().currentWsID) {
        const running = items.find((w) => w.status === 'running');
        if (running) {
          set({ currentWsID: running.id });
        }
      }
    } catch (err) {
      if ((err as DOMException).name !== 'AbortError') {
        set({ error: (err as Error).message });
      }
    } finally {
      set({ loadingStatus: false });
    }
  },

  refreshMetrics: async (signal) => {
    const wsID = get().currentWsID;
    if (!wsID || get().loadingMetrics) return;
    set({ loadingMetrics: true });
    try {
      const snap = await fetchMetrics(wsID, signal);
      set({ metrics: snap, lastFetch: Date.now(), error: null });
    } catch (err) {
      if ((err as DOMException).name !== 'AbortError') {
        set({ error: (err as Error).message });
      }
    } finally {
      set({ loadingMetrics: false });
    }
  },
}));
