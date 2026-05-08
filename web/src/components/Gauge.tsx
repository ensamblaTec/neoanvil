// [PILAR-XXVII/245.O] SVG gauge primitive. Zero dependencies beyond React.

interface GaugeProps {
  label: string;
  value: number;       // 0-100
  unit?: string;       // e.g. "%", "MB"
  display?: string;    // overrides auto-formatted text inside the dial
  size?: number;       // px — gauges scale naturally in a flexbox
  color?: string;      // override fill — default bucketed by value
  // [PILAR-XXVII/245.Q] Custom band thresholds over the pct axis.
  // value < yellow → green; value < red → yellow; ≥ red → red.
  // Defaults (50 / 80) preserve previous behaviour for callers that
  // don't supply explicit thresholds.
  thresholds?: { yellow: number; red: number };
  // When true, colour semantics flip (higher = better). Used for cache
  // hit ratios: 80% is healthy, 20% is concerning.
  invert?: boolean;
}

export function Gauge({
  label,
  value,
  unit = '%',
  display,
  size = 120,
  color,
  thresholds,
  invert,
}: GaugeProps) {
  const pct = Math.max(0, Math.min(100, value));
  const r = size / 2 - 10;
  const cx = size / 2;
  const cy = size / 2;
  const circ = 2 * Math.PI * r;
  const filled = (pct / 100) * circ;
  const yellow = thresholds?.yellow ?? 50;
  const red = thresholds?.red ?? 80;
  // Standard: low = good. Inverted: high = good (cache hit rates).
  const auto = invert
    ? (pct >= red    ? '#22c55e'
       : pct >= yellow ? '#eab308'
       :                 '#ef4444')
    : (pct < yellow ? '#22c55e'
       : pct < red  ? '#eab308'
       :              '#ef4444');
  const stroke = color ?? auto;

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        padding: 8,
        color: '#ddd',
        fontFamily: 'monospace',
        flexShrink: 0,
      }}
    >
      <div style={{ position: 'relative', width: size, height: size }}>
        <svg width={size} height={size} style={{ transform: 'rotate(-90deg)' }}>
          <circle cx={cx} cy={cy} r={r} fill="none" stroke="#333" strokeWidth={10} />
          <circle
            cx={cx}
            cy={cy}
            r={r}
            fill="none"
            stroke={stroke}
            strokeWidth={10}
            strokeDasharray={`${filled} ${circ}`}
            strokeLinecap="round"
          />
        </svg>
        <div
          style={{
            position: 'absolute',
            inset: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            fontSize: size / 5.5,
            fontWeight: 'bold',
            textAlign: 'center',
            pointerEvents: 'none',
          }}
        >
          {display ?? `${pct.toFixed(0)}${unit}`}
        </div>
      </div>
      <div style={{ marginTop: 4, fontSize: 12, opacity: 0.8 }}>{label}</div>
    </div>
  );
}
