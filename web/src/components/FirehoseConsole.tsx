import { useSREStore } from '../store/useSREStore';
import { Terminal } from 'lucide-react';
import { useEffect, useRef } from 'react';

export default function FirehoseConsole() {
  const firehose = useSREStore(s => s.firehose);
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [firehose]);

  return (
    <div style={{ position: 'absolute', bottom: 20, right: 20, width: '300px', height: '300px', zIndex: 10, background: 'rgba(0,0,0,0.9)', border: '1px solid #f0f', padding: '10px', color: '#f0f', fontFamily: 'monospace', overflowY: 'auto' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px', borderBottom: '1px solid #f0f', paddingBottom: '5px', marginBottom: '10px' }}>
        <Terminal size={16} /> <span>SRE FIREHOSE</span>
      </div>
      <div style={{ fontSize: '12px', lineHeight: '1.4' }}>
        {firehose.map((log: string, i: number) => (
          <div key={i} style={{ marginBottom: '4px' }}>&gt; {log}</div>
        ))}
        <div ref={endRef} />
      </div>
    </div>
  );
}