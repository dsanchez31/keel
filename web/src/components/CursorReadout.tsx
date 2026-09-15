import { type GroundPoint, latLon, mgrs } from '@/lib/geoFormat';
import { type Signal, useSignal } from '@/lib/signal';

/**
 * The ground position under the mouse, lat/lon and MGRS side by side
 * (design section 8.3), in the time strip. Blank off the globe.
 */
export function CursorReadout({ cursor }: { cursor: Signal<GroundPoint | undefined> }) {
  const at = useSignal(cursor);
  if (!at) return <span className="w-72 text-muted-foreground">cursor off the globe</span>;
  return (
    <span className="flex w-72 gap-2 tabular-nums" aria-live="off">
      <span>{mgrs(at.lat, at.lon) ?? 'no MGRS'}</span>
      <span className="text-muted-foreground">{latLon(at.lat, at.lon)}</span>
    </span>
  );
}
