import { useSREStore } from '../store/useSREStore';
import { Layers } from 'lucide-react';

export default function LayerSelector() {
  const activeLayer = useSREStore(s => s.activeLayer);
  const setActiveLayer = useSREStore(s => s.setActiveLayer);
  const layers: Array<'MCTS'|'HNSW'|'HEATMAP'|'TOOLS'|'CODE_FLOW'> = ['MCTS', 'HNSW', 'HEATMAP', 'TOOLS', 'CODE_FLOW'];

  return (
    <div style={{ position: 'absolute', top: 20, right: 20, zIndex: 10, background: 'rgba(0,0,0,0.8)', border: '1px solid #0f0', padding: '10px', color: '#0f0', fontFamily: 'monospace' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '10px' }}>
        <Layers size={18} /> <span>DIMENSION SRE</span>
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: '5px' }}>
        {layers.map(l => (
          <button 
            key={l} 
            onClick={() => setActiveLayer(l)}
            style={{ 
              background: activeLayer === l ? '#0f0' : 'transparent',
              color: activeLayer === l ? '#000' : '#0f0',
              border: '1px solid #0f0',
              padding: '5px 10px',
              cursor: 'pointer',
              fontFamily: 'monospace',
              fontWeight: 'bold'
            }}
          >
            {l}
          </button>
        ))}
      </div>
    </div>
  );
}