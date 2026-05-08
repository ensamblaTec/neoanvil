import { useSREStore } from '../store/useSREStore';
import { Clock } from 'lucide-react';

export default function TimeMachineSlider() {
  const history = useSREStore(s => s.historyQueue);
  const timePointer = useSREStore(s => s.timePointer);
  const setTimePointer = useSREStore(s => s.setTimePointer);
  
  const total = history.length;
  // Si -1, equivale al último, pero mostramos el valor real indexado
  const currentVal = timePointer === -1 ? total - 1 : timePointer;

  return (
    <div style={{ position: 'absolute', bottom: 20, left: 20, right: 350, zIndex: 10, background: 'rgba(0,0,0,0.8)', border: '1px solid #0ff', padding: '15px', color: '#0ff', fontFamily: 'monospace' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
        <Clock size={20} />
        <span style={{ fontSize: '18px', fontWeight: 'bold' }}>TIME-MACHINE BUFFER</span>
        <span style={{ marginLeft: 'auto' }}>
          TICK: {currentVal >= 0 ? currentVal : 0} / {total > 0 ? total - 1 : 0}
        </span>
      </div>
      <input 
        type="range" 
        min={0} 
        max={total > 0 ? total - 1 : 0} 
        value={currentVal >= 0 ? currentVal : 0} 
        onChange={(e) => {
            const num = parseInt(e.target.value);
            setTimePointer(num === total - 1 ? -1 : num);
        }}
        style={{ width: '100%', marginTop: '10px', accentColor: '#0ff' }}
      />
    </div>
  );
}