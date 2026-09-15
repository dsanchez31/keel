import { type FleetVectorView, type LaneView, type LinkState, type VectorID, type VectorMode, type VectorView } from '@keel/sdk';
import { Radar } from 'lucide-react';

import { activeLane, PALETTE, vectorMarker } from '@/cesium/missionLayers';
import { latLon, mgrs } from '@/lib/geoFormat';
import { type MissionSource, useMissionView } from '@/lib/missionStore';
import { waiting } from '@/lib/useFleet';
import { cn } from '@/lib/utils';

import { SectionHeader } from './SectionHeader';

export interface VectorRosterProps {
  store: MissionSource;
  selected: VectorID | undefined;
  /** Selects a vector, or clears the selection with undefined. */
  onSelect: (id: VectorID | undefined) => void;
  /**
   * Every bound vector with its adapter's health, live only: those the view
   * does not hold yet are listed as waiting, with why. A replay has no live
   * fleet and passes none.
   */
  fleet?: readonly FleetVectorView[];
}

/** One line of the roster: a vector the view holds, or a bound one not joined yet. */
type Row = { kind: 'joined'; id: VectorID; vector: VectorView } | { kind: 'waiting'; id: VectorID; entry: FleetVectorView };

/** A frame older than this, in mission time, is called out: the vector is drawn where it was last heard. */
const SILENT_MS = 1000;

const MODE_COLOUR: Record<VectorMode, string> = {
  idle: PALETTE.grey,
  transit: PALETTE.pale,
  scanning: PALETTE.cyan,
  rtb: PALETTE.amber,
  down: PALETTE.red,
};

const LINK_COLOUR: Record<LinkState, string> = {
  ok: PALETTE.green,
  degraded: PALETTE.amber,
  lost: PALETTE.red,
};

/**
 * Every vector the engine holds, as of the last tick, in id order: its mode
 * (orchestration modes included), link, battery, the lane it works with its
 * progress, and how long it has been silent when it has. A row selects its
 * vector: the globe flies to it and the decision trace keeps to it. A bound
 * vector that has not joined yet takes its place in the order as waiting,
 * with its adapter's reason (an autopilot's position estimate, about 40 s
 * after an ArduPilot boots), and selects nothing: there is nowhere to fly to.
 * The count is the vectors joined.
 */
export function VectorRoster({ store, selected, onSelect, fleet = [] }: VectorRosterProps) {
  const view = useMissionView(store);
  const vectors = view?.vectors ?? [];
  const rows: Row[] = [
    ...vectors.map((v): Row => ({ kind: 'joined', id: v.caps.id, vector: v })),
    ...waiting(fleet, view).map((entry): Row => ({ kind: 'waiting', id: entry.id, entry })),
  ];
  rows.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

  return (
    <section aria-label="Vectors" className="flex h-full min-h-0 flex-col">
      <SectionHeader icon={Radar} title="Vectors" count={vectors.length} />
      {rows.length === 0 ? (
        <p className="px-4 py-3 text-sm text-muted-foreground">No vector has joined.</p>
      ) : (
        <ul className="min-h-0 overflow-y-auto">
          {rows.map((row) => (
            <li key={row.id}>
              {row.kind === 'joined' ? (
                <VectorRow
                  vector={row.vector}
                  lane={laneOf(view?.lanes ?? [], row.id)}
                  silentMs={(view?.tick_ms ?? 0) - row.vector.state.last_seen_ms}
                  selected={row.id === selected}
                  onSelect={() => onSelect(row.id === selected ? undefined : row.id)}
                />
              ) : (
                <WaitingRow entry={row.entry} />
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** A bound vector not joined yet: no position, no state, only its adapter's verdict and reason. */
function WaitingRow({ entry }: { entry: FleetVectorView }) {
  return (
    <div className="grid w-full grid-cols-[1rem_1fr_auto] items-start gap-x-2 gap-y-1 border-b border-border/50 px-4 py-2 font-mono text-xs text-muted-foreground">
      <span aria-hidden>○</span>
      <span className="truncate">{entry.id}</span>
      <span>waiting</span>

      <span />
      <span className="line-clamp-2 break-words" title={entry.detail}>
        {entry.detail ?? 'not joined yet'}
      </span>
      <span style={{ color: LINK_COLOUR[entry.health] }}>{entry.health}</span>
    </div>
  );
}

function laneOf(lanes: readonly LaneView[], id: VectorID): string | undefined {
  const lane = activeLane(lanes, id);
  return lane && `${lane.id} ${lane.cursor}/${lane.waypoints.length}`;
}

interface VectorRowProps {
  vector: VectorView;
  lane: string | undefined;
  silentMs: number;
  selected: boolean;
  onSelect: () => void;
}

function VectorRow({ vector, lane, silentMs, selected, onSelect }: VectorRowProps) {
  const { state, caps } = vector;
  const marker = vectorMarker(vector);
  const battery = Math.min(100, Math.max(0, state.battery_pct));

  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onSelect}
      className={cn(
        'grid w-full grid-cols-[1rem_1fr_auto] items-center gap-x-2 gap-y-1 border-b border-border/50 px-4 py-2 text-left font-mono text-xs hover:bg-muted/50',
        selected && 'bg-muted',
      )}
    >
      <span aria-hidden style={{ color: marker.color, opacity: marker.alpha }}>
        {marker.symbol === 'diamond' ? '◆' : '■'}
      </span>
      <span className="truncate">{caps.id}</span>
      <span style={{ color: MODE_COLOUR[state.mode] }}>{state.mode}</span>

      <span />
      <span className="flex items-center gap-2 text-muted-foreground">
        <span className="h-1 w-12 overflow-hidden rounded-full bg-muted" aria-hidden>
          <span
            className="block h-full"
            style={{ width: `${battery}%`, backgroundColor: battery > 30 ? PALETTE.green : battery > 15 ? PALETTE.amber : PALETTE.red }}
          />
        </span>
        {battery}%<span>{lane ?? 'no lane'}</span>
      </span>
      <span style={{ color: LINK_COLOUR[state.link] }}>{state.link}</span>

      {selected && (
        <>
          <span />
          <span className="col-span-2 text-muted-foreground" title={latLon(state.position.lat, state.position.lon)}>
            {mgrs(state.position.lat, state.position.lon) ?? latLon(state.position.lat, state.position.lon)}
          </span>
        </>
      )}
      {silentMs > SILENT_MS && state.mode !== 'down' && (
        <>
          <span />
          <span className="col-span-2" style={{ color: PALETTE.amber }}>
            silent {(silentMs / 1000).toFixed(1)} s
          </span>
        </>
      )}
    </button>
  );
}
