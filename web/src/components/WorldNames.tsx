import { type AreaView } from '@keel/sdk';
import { LandPlot, MapPinned } from 'lucide-react';
import { type ReactNode } from 'react';

import { type World } from '@/lib/useWorld';

export interface WorldNamesProps {
  world: World;
  /** A name was picked: the intent bar takes it at its caret. */
  onPick: (name: string) => void;
  /** An area was picked: the globe shows it. */
  onArea?: (area: AreaView) => void;
}

/**
 * The names an intent may use, above the intent bar: the world's areas, with
 * their surface, and its ground stations. The planner resolves nothing else
 * (spec section 5.1), so the operator reads them here rather than knowing
 * them beforehand. A name picked goes into the intent; an area picked is also
 * shown on the globe, where every area is outlined.
 */
export function WorldNames({ world, onPick, onArea }: WorldNamesProps) {
  if (world.error) {
    return <p className="text-xs text-destructive">Areas unavailable: {world.error.message}</p>;
  }
  const view = world.view;
  if (!view) return null;

  return (
    <div className="space-y-1.5 text-xs">
      <Names label="Areas" icon={<LandPlot aria-hidden className="size-3.5" />}>
        {view.areas.map((a) => (
          <Chip
            key={a.name}
            title={`${a.name}: ${a.area_km2.toFixed(1)} km², cells of ${a.cell_m} m, scanned at ${a.scan_alt_m} m`}
            onClick={() => {
              onPick(a.name);
              onArea?.(a);
            }}
          >
            {a.name} <span className="text-muted-foreground">{a.area_km2.toFixed(1)} km²</span>
          </Chip>
        ))}
      </Names>
      <Names label="Stations" icon={<MapPinned aria-hidden className="size-3.5" />}>
        {view.stations.map((s) => (
          <Chip key={s.name} title={s.name} onClick={() => onPick(s.name)}>
            {s.name}
          </Chip>
        ))}
      </Names>
    </div>
  );
}

function Names({ label, icon, children }: { label: string; icon: ReactNode; children: ReactNode }) {
  return (
    <div role="group" aria-label={label} className="flex flex-wrap items-center gap-1">
      <span className="flex items-center gap-1 pr-1 font-mono tracking-widest text-muted-foreground uppercase">
        {icon}
        {label}
      </span>
      {children}
    </div>
  );
}

function Chip({ title, onClick, children }: { title: string; onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" title={title} onClick={onClick} className="rounded-md border px-2 py-0.5 font-mono hover:bg-muted/50">
      {children}
    </button>
  );
}
