// [PILAR-XXVII/245.F] Tab bar + body.

import { useState } from 'react';
import { useWorkspaceStore } from '../store/useWorkspaceStore';
import { OverviewTab } from './tabs/OverviewTab';
import { ToolsTab } from './tabs/ToolsTab';
import { TokensTab } from './tabs/TokensTab';
import { MutationsTab } from './tabs/MutationsTab';
import { MemoryTab } from './tabs/MemoryTab';
import { SystemTab } from './tabs/SystemTab';
import { RulesTab } from './tabs/RulesTab';
import { IncidentsTab } from './tabs/IncidentsTab';
import { ProjectsTab } from './tabs/ProjectsTab'; // [288]

const TABS = [
  'Overview', 'Tools', 'Tokens', 'Mutations',
  'Memory', 'System', 'Rules', 'Incidents', 'Projects',
];

const FLEET_TABS = new Set([8]); // tabs that work at fleet level (no workspace metrics needed)

export function WorkspaceTabs() {
  const [active, setActive] = useState(0);
  const metrics = useWorkspaceStore((s) => s.metrics);
  const error = useWorkspaceStore((s) => s.error);
  const currentWsID = useWorkspaceStore((s) => s.currentWsID);
  const loading = useWorkspaceStore((s) => s.loadingMetrics);

  // Fleet-level tabs render without requiring a selected workspace.
  if (FLEET_TABS.has(active)) {
    return (
      <section className="ws-main">
        <nav className="ws-tabs-bar">
          {TABS.map((name, i) => (
            <div key={name} className={`ws-tab${i === active ? ' active' : ''}`} onClick={() => setActive(i)}>
              {i + 1}. {name}
            </div>
          ))}
        </nav>
        <div className="ws-body">
          {active === 8 && <ProjectsTab />}
        </div>
      </section>
    );
  }

  let content;
  if (!currentWsID) {
    content = <div style={{ opacity: 0.7 }}>Select a workspace from the sidebar.</div>;
  } else if (error && !metrics) {
    content = <div style={{ color: '#ef4444' }}>Error loading metrics: {error}</div>;
  } else if (!metrics) {
    content = <div style={{ opacity: 0.7 }}>Loading metrics{loading ? ' …' : ''}</div>;
  } else {
    switch (active) {
      case 0: content = <OverviewTab snap={metrics} />; break;
      case 1: content = <ToolsTab snap={metrics} />; break;
      case 2: content = <TokensTab snap={metrics} />; break;
      case 3: content = <MutationsTab snap={metrics} />; break;
      case 4: content = <MemoryTab snap={metrics} />; break;
      case 5: content = <SystemTab snap={metrics} />; break;
      case 6: content = <RulesTab snap={metrics} />; break;
      case 7: content = <IncidentsTab snap={metrics} />; break;
    }
  }

  return (
    <section className="ws-main">
      <nav className="ws-tabs-bar">
        {TABS.map((name, i) => (
          <div
            key={name}
            className={`ws-tab${i === active ? ' active' : ''}`}
            onClick={() => setActive(i)}
          >
            {i + 1}. {name}
          </div>
        ))}
      </nav>
      <div className="ws-body">{content}</div>
    </section>
  );
}
