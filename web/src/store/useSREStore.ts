import { create } from 'zustand';

export interface GraphNode {
  id: string;
  name?: string;
  type: string;
  heat?: number;
  errorCount?: number;
  val?: number;
  duration?: number;
}

export interface GraphEdge {
  source: string;
  target: string;
  weight?: number;
  errorCount?: number;
}

export interface GraphData {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface SysStats {
  goroutines: number;
  cpu_usage_percent: number;
  alloc_mb: number;
  sys_mb: number;
  active_tasks: number;
  gc_cycles: number;
}

export interface ServerPayload {
  type: string;
  mcts?: GraphData;
  hnsw?: GraphData;
  heatmap?: GraphData;
  tools?: GraphData;
  stats?: SysStats;
  log?: string;
}

interface SREState {
  mctsData: GraphData;
  hnswData: GraphData;
  heatmapData: GraphData;
  toolsData: GraphData;
  codeFlowData: GraphData;
  stats: SysStats;
  firehose: string[];
  
  activeLayer: 'MCTS' | 'HNSW' | 'HEATMAP' | 'TOOLS' | 'CODE_FLOW';
  setActiveLayer: (layer: 'MCTS' | 'HNSW' | 'HEATMAP' | 'TOOLS' | 'CODE_FLOW') => void;

  historyQueue: ServerPayload[];
  timePointer: number;
  maxHistory: number;
  
  setTimePointer: (pt: number) => void;
  initializeWS: () => void;
  fetchTopology: () => Promise<void>;
  fetchCodeFlow: () => Promise<void>;
}

export const useSREStore = create<SREState>((set, get) => ({
  mctsData: { nodes: [], edges: [] },
  hnswData: { nodes: [], edges: [] },
  heatmapData: { nodes: [], edges: [] },
  toolsData: { nodes: [], edges: [] },
  codeFlowData: { nodes: [], edges: [] },
  stats: { goroutines: 0, cpu_usage_percent: 0, alloc_mb: 0, sys_mb: 0, active_tasks: 0, gc_cycles: 0 },
  firehose: [],
  
  activeLayer: 'MCTS',
  setActiveLayer: (l) => {
    set({ activeLayer: l });
    if (l === 'HNSW') {
      get().fetchTopology();
    }
    if (l === 'CODE_FLOW') {
      get().fetchCodeFlow();
    }
  },
  
  historyQueue: [],
  timePointer: -1,
  maxHistory: 60, // Búfer circular estricto de 60
  setTimePointer: (pt) => set({ timePointer: pt }),
  
  fetchTopology: async () => {
    try {
      const res = await fetch('http://' + window.location.host + '/api/v1/sre/topology');
      if (res.ok) {
        const data = await res.json();
        set({ hnswData: data });
      }
    } catch (e) {
      console.error('[SRE] Fetch topology failed:', e);
    }
  },

  fetchCodeFlow: async () => {
    try {
      const res = await fetch('http://' + window.location.host + '/api/v1/sre/topology');
      if (res.ok) {
        const data = await res.json();
        set({ codeFlowData: data });
      }
    } catch (e) {
      console.error('[SRE] Fetch code flow failed:', e);
    }
  },

  initializeWS: () => {
    // [SRE-DEFENSE] Conexión al proxy (puente) del propio HUD
    const ws = new WebSocket('ws://' + window.location.host + '/api/v1/sre/ws');
    
    ws.onopen = () => {
      console.log('[SRE] HUD WebSocket conectado.');
    };

    ws.onmessage = (event) => {
      try {
        const data: ServerPayload = JSON.parse(event.data);
        const { historyQueue, maxHistory, timePointer } = get();
        
        // Time machine logic
        const isLive = timePointer === -1 || timePointer === historyQueue.length - 1;
        
        const newHistory = [...historyQueue, data];
        if (newHistory.length > maxHistory) {
          newHistory.shift();
        }

        if (isLive) {
          const updates: Partial<SREState> = { historyQueue: newHistory };
          
          if (data.mcts) updates.mctsData = data.mcts;
          if (data.hnsw) updates.hnswData = data.hnsw;
          if (data.heatmap) updates.heatmapData = data.heatmap;
          if (data.tools) updates.toolsData = data.tools;
          if (data.stats) updates.stats = data.stats;
          
          if (data.log) {
            updates.firehose = [...get().firehose, data.log].slice(-100);
          }
          
          // Aguja temporal se mantiene en vivo
          updates.timePointer = newHistory.length - 1;
          
          set(updates);
        } else {
          // Si estamos en el pasado, solo grabamos el historial sin refrescar la vista UI
          set({ historyQueue: newHistory });
        }

      } catch (e) {
        console.error('[SRE] WS Payload Parsing Error:', e);
      }
    };
    
    ws.onclose = () => {
      console.warn('[SRE] Conexión perdida. Intentando reconectar...');
      setTimeout(get().initializeWS, 2000);
    };
  }
}));
