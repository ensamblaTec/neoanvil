import { useRef, useEffect } from 'react';
import ForceGraph3D from 'react-force-graph-3d';
import * as THREE from 'three';
import { useSREStore } from '../store/useSREStore';
import type { GraphNode, GraphEdge, GraphData } from '../store/useSREStore';

export default function Panopticon3D() {
  const store = useSREStore();
  const active = store.activeLayer;
  let data: GraphData | undefined;
  switch (active) {
    case 'MCTS': data = store.mctsData; break;
    case 'HNSW': data = store.hnswData; break;
    case 'HEATMAP': data = store.heatmapData; break;
    case 'TOOLS': data = store.toolsData; break;
    case 'CODE_FLOW': data = store.codeFlowData; break;
  }
  const graphRef = useRef<{ nodes: any[]; links: any[] }>({ nodes: [], links: [] });

  useEffect(() => {
    if (!data) return;

    let preNodes = data.nodes.filter((n: GraphNode) => n.id != null);
    if (active === 'CODE_FLOW') {
        preNodes = preNodes.filter((n: GraphNode) => data!.edges.some((e: any) => 
            e.source === n.id || e.target === n.id || (e.source && e.source.id === n.id) || (e.target && e.target.id === n.id)
        ));
    }

    const nodeIds = new Set(preNodes.map((n: GraphNode) => n.id));

    // Obtener listas actuales del Ref
    const { nodes: oldNodes, links: oldLinks } = graphRef.current;
    
    // Reconciliación de Nodos (mutar o reusar viejos para mantener x,y,z)
    const newNodes = preNodes.map((n: GraphNode) => {
      const oldNode = oldNodes.find((old: any) => old.id === n.id);
      if (oldNode) {
        return Object.assign(oldNode, n); // Mantener referencia
      }
      return { ...n };
    });

    // Reconciliación de Enlaces
    const newLinks = data.edges
      .filter((e: GraphEdge) => {
        // e.source puede ser objeto si ya fue mutado
        const srcId = typeof e.source === 'object' ? (e.source as any).id : e.source;
        const tgtId = typeof e.target === 'object' ? (e.target as any).id : e.target;
        return srcId != null && tgtId != null && nodeIds.has(srcId) && nodeIds.has(tgtId);
      })
      .map((e: GraphEdge) => {
        const srcId = typeof e.source === 'object' ? (e.source as any).id : e.source;
        const tgtId = typeof e.target === 'object' ? (e.target as any).id : e.target;
        const oldLink = oldLinks.find((old: any) => {
          const oSrcId = typeof old.source === 'object' ? old.source.id : old.source;
          const oTgtId = typeof old.target === 'object' ? old.target.id : old.target;
          return oSrcId === srcId && oTgtId === tgtId;
        });
        if (oldLink) {
          return Object.assign(oldLink, e); // Mantener referencia para no romper resortes
        }
        return { ...e, source: srcId, target: tgtId };
      });

    graphRef.current = { nodes: newNodes, links: newLinks };
  }, [data]);

  return (
    <div style={{ position: 'absolute', top: 0, left: 0, width: '100vw', height: '100vh', zIndex: 0 }}>
      <ForceGraph3D
        graphData={graphRef.current}
        dagMode={active === 'CODE_FLOW' ? 'td' : undefined}
        dagLevelDistance={active === 'CODE_FLOW' ? 50 : undefined}
        nodeThreeObject={(node: any) => {
          let color = '#ffffff';
          if (active === 'HNSW') color = '#00ffff';
          if (active === 'MCTS') color = '#00ff00';
          if (active === 'HEATMAP') color = node.heat ? '#ff0000' : '#444444';
          if (active === 'TOOLS') color = node.type === 'root' ? '#ffffff' : '#ff00ff';
          if (active === 'CODE_FLOW') color = '#ff8c00';
          
          let size = 5;
          if (active === 'HEATMAP') size = node.heat ? 8 : 3;
          if (active === 'TOOLS') size = node.type === 'root' ? 24 : 5 + Math.min(10, (node.duration || 0));
          if (active === 'CODE_FLOW') size = (node.val && node.val > 5) ? 12 : 6;
          
          let geometry: THREE.BufferGeometry = new THREE.SphereGeometry(size);
          let material: THREE.Material = new THREE.MeshBasicMaterial({ color });

          if (active === 'HEATMAP' && node.heat) {
            geometry = new THREE.IcosahedronGeometry(size);
            material = new THREE.MeshPhongMaterial({ 
              color: '#ff0000', 
              emissive: '#ff0000',
              emissiveIntensity: 0.8,
              shininess: 100
            });
            const mesh = new THREE.Mesh(geometry, material);
            const haloGeo = new THREE.IcosahedronGeometry(size * 1.5);
            const haloMat = new THREE.MeshBasicMaterial({ color: '#ff0000', transparent: true, opacity: 0.3 });
            const halo = new THREE.Mesh(haloGeo, haloMat);
            mesh.add(halo);
            return mesh;
          }

          return new THREE.Mesh(geometry, material);
        }}
        linkDirectionalParticles={(link: any) => {
          if (active === 'CODE_FLOW') return 3;
          if (active === 'TOOLS') {
            if (link.errorCount > 0) return 6;
            const flow = (link.target && typeof link.target === 'object') ? link.target.val : 0;
            return flow > 0 ? Math.min(10, Math.max(2, Math.floor(flow / 2))) : 2;
          }
          return 2;
        }}
        linkDirectionalParticleWidth={(link: any) => {
          if (active === 'TOOLS') {
              if (link.errorCount > 0) return 3;
              const targetNode = (link.target && typeof link.target === 'object') ? link.target : null;
              if (targetNode && targetNode.duration) {
                  return 1.5 + Math.min(4, targetNode.duration * 5); // Grosor proporcional a latencia
              }
              return 1.5;
          }
          if (active === 'CODE_FLOW') return 2;
          return 1.5;
        }}
        linkDirectionalParticleColor={(link: any) => {
          if (active === 'TOOLS' && link.errorCount > 0) return '#ff0000';
          if (active === 'CODE_FLOW') return '#ffff00';
          return 'rgba(255,255,255,0.5)';
        }}
        linkDirectionalParticleSpeed={(link: any) => {
          if (active === 'TOOLS' && link.errorCount > 0) return 0.05;
          if (active === 'CODE_FLOW') return 0.015;
          return 0.01;
        }}
      />
    </div>
  );
}