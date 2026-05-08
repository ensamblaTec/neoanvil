// [PILAR-XXVII/245.D] React hook that drives the two-tier polling
// loop: /status every 5s, /metrics every 2s — but only while the tab
// is visible. AbortController ensures unmount cancels pending fetches.

import { useEffect, useRef } from 'react';
import { useWorkspaceStore } from './useWorkspaceStore';

const STATUS_INTERVAL = 5_000;
const METRICS_INTERVAL = 2_000;

export function useMetricsPolling(): void {
  const currentWsID = useWorkspaceStore((s) => s.currentWsID);
  const refreshStatus = useWorkspaceStore((s) => s.refreshStatus);
  const refreshMetrics = useWorkspaceStore((s) => s.refreshMetrics);

  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current = new AbortController();
    const ctrl = abortRef.current;

    // Immediate fetch on mount / workspace change.
    refreshStatus(ctrl.signal);
    refreshMetrics(ctrl.signal);

    const statusTimer = setInterval(() => {
      if (document.visibilityState === 'visible') {
        refreshStatus(ctrl.signal);
      }
    }, STATUS_INTERVAL);

    const metricsTimer = setInterval(() => {
      if (document.visibilityState === 'visible') {
        refreshMetrics(ctrl.signal);
      }
    }, METRICS_INTERVAL);

    return () => {
      clearInterval(statusTimer);
      clearInterval(metricsTimer);
      ctrl.abort();
    };
  }, [currentWsID, refreshStatus, refreshMetrics]);
}
