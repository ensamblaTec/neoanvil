// [PILAR-XXVII/245] Multi-workspace observability HUD.
//
// Layout: sidebar (fleet) + main pane (per-workspace tabs).
// The legacy 3-panel SRE visualisation is preserved under a ?legacy=1
// query-string switch for operators still using it.

import { useEffect, useState } from 'react';
import { useSREStore } from './store/useSREStore';
import { useMetricsPolling } from './store/useMetricsPolling';
import { WorkspaceSidebar } from './components/WorkspaceSidebar';
import { WorkspaceTabs } from './components/WorkspaceTabs';
import Panopticon3D from './components/Panopticon3D';
import LayerSelector from './components/LayerSelector';
import TimeMachineSlider from './components/TimeMachineSlider';
import FirehoseConsole from './components/FirehoseConsole';
import './App.css';

function App() {
  const legacy = new URLSearchParams(window.location.search).get('legacy') === '1';
  if (legacy) return <LegacyApp />;
  return <ObservabilityApp />;
}

function ObservabilityApp() {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  useMetricsPolling();
  return (
    <div className="app-shell">
      <button
        type="button"
        className="ws-sidebar-toggle"
        aria-label="Toggle workspace sidebar"
        onClick={() => setSidebarOpen((v) => !v)}
      >
        {sidebarOpen ? '✕' : '☰'}
      </button>
      {sidebarOpen && (
        <div className="ws-sidebar-backdrop" onClick={() => setSidebarOpen(false)} />
      )}
      <WorkspaceSidebar
        mobileOpen={sidebarOpen}
        onItemPick={() => setSidebarOpen(false)}
      />
      <WorkspaceTabs />
    </div>
  );
}

function LegacyApp() {
  const initializeWS = useSREStore((s) => s.initializeWS);
  useEffect(() => {
    initializeWS();
  }, [initializeWS]);
  return (
    <div className="legacy-shell">
      <h1 style={{ color: 'white', position: 'absolute', zIndex: 9999, margin: 12 }}>
        SRE HUD (legacy)
      </h1>
      <Panopticon3D />
      <LayerSelector />
      <TimeMachineSlider />
      <FirehoseConsole />
    </div>
  );
}

export default App;
