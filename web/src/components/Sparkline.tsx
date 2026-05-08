// [PILAR-XXVII/245.O] SVG sparkline — minimal line-chart primitive.

interface SparklineProps {
  data: number[];
  width?: number;
  height?: number;
  color?: string;
  fill?: string;
}

export function Sparkline({ data, width = 200, height = 40, color = '#22d3ee', fill = 'rgba(34, 211, 238, 0.15)' }: SparklineProps) {
  if (!data || data.length === 0) return null;
  const min = Math.min(...data);
  const max = Math.max(...data);
  const span = max - min || 1;
  const step = data.length > 1 ? width / (data.length - 1) : width;
  const points = data.map((v, i) => {
    const x = i * step;
    const y = height - ((v - min) / span) * height;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const path = `M0,${height} L${points.join(' L')} L${width},${height} Z`;
  const line = `M${points.join(' L')}`;
  return (
    <svg width={width} height={height} style={{ display: 'block' }}>
      <path d={path} fill={fill} stroke="none" />
      <path d={line} fill="none" stroke={color} strokeWidth={1.5} />
    </svg>
  );
}
