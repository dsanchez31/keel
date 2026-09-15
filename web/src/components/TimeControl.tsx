import { type StreamStatus } from '@keel/sdk';
import { Link } from '@tanstack/react-router';
import { Pause, Play, Rewind } from 'lucide-react';
import { type ReactNode, useSyncExternalStore } from 'react';

import { setSpeedMagnitude, SPEED_LADDER } from '@/cesium/playbackWindow';
import { Button } from '@/components/ui/button';
import { missionTime } from '@/lib/format';
import { type GroundPoint } from '@/lib/geoFormat';
import { liveLadder } from '@/lib/liveClock';
import { type MissionSource, type MissionStore } from '@/lib/missionStore';
import { type Signal } from '@/lib/signal';
import { type LiveClock } from '@/lib/useLiveClock';
import { cn } from '@/lib/utils';

import { CoverageReadout } from './CoverageReadout';
import { CursorReadout } from './CursorReadout';

/** The transport of a replay, driven by the replay scrubber, which owns its clock. */
export interface ReplayTransport {
  playing: boolean;
  /** Signed: the sense of playback is the sign (playbackWindow.ts). */
  speed: number;
  onPlaying: (playing: boolean) => void;
  /** Called with the new signed speed. */
  onSpeed: (speed: number) => void;
}

export interface TimeControlProps {
  /** Mission time of the latest tick, in milliseconds; undefined before the first snapshot. */
  tickMs: number | undefined;
  /** The log head after that tick, the hash chain's value the operator reads. */
  head: string | undefined;
  /** The live stream's state; absent in replay. */
  status?: StreamStatus;
  /** Present in replay only: live has nothing to pause or rewind. */
  replay?: ReplayTransport;
  /** Live, the pace controls beside the stream's state, never shrunk (LiveSpeed). */
  controls?: ReactNode;
  /**
   * keeld's pace on the live stream. Above 1 the strip turns amber and its
   * state reads "accelerated ×N" rather than "live", so nobody takes the
   * fleet for real time.
   */
  speed?: number;
  /** What sits along the strip: the replay scrubber, or the link to replay a mission just ended. */
  children?: ReactNode;
  /** Readouts beside the head: coverage, the position under the cursor. */
  readouts?: ReactNode;
}

const STATUS_LABEL: Record<StreamStatus, string> = {
  connecting: 'connecting to keeld',
  live: 'live',
  resyncing: 'resynchronising',
  closed: 'closed',
};

/**
 * The strip under the globe: mission time, the log head, and either the
 * live stream's state or a replay's transport.
 *
 * Live is the present. keeld cannot be paused, and the fog, lanes and roster
 * always show the present, so there is nothing to pause or rewind there:
 * rewinding the trajectories alone would put two instants on one screen. Its
 * pace is keeld's, which a simulated fleet lets the operator raise, keeld and
 * every simulator together (LiveSpeed). A replay plays every layer from the
 * same instant of the log, and gets play, pause, the direction and the speed
 * ladder, direction and magnitude on separate controls.
 */
export function TimeControl({ tickMs, head, status, replay, controls, speed = 1, children, readouts }: TimeControlProps) {
  const accelerated = status === 'live' && speed > 1;
  return (
    <div
      className={cn(
        'flex h-12 shrink-0 items-center gap-3 border-t bg-card px-4 font-mono text-xs text-card-foreground',
        accelerated && 'border-t-2 border-t-amber-400',
      )}
    >
      <span className="tabular-nums">{tickMs === undefined ? 'T+--:--.-' : missionTime(tickMs)}</span>
      {replay ? (
        <Transport replay={replay} />
      ) : (
        status && (
          <span
            className="flex shrink-0 items-center gap-2"
            role="status"
            title={accelerated ? `The fleet runs ${speed} times faster than real time` : undefined}
          >
            <span
              aria-hidden
              className={cn('size-2 rounded-full', status === 'live' && !accelerated ? 'animate-pulse bg-red-500' : 'bg-amber-400', accelerated && 'animate-pulse')}
            />
            {accelerated ? (
              <span className="font-semibold text-amber-400 uppercase">Accelerated ×{speed}</span>
            ) : (
              <span className={status === 'live' ? 'tracking-widest uppercase' : 'text-amber-400'}>{STATUS_LABEL[status]}</span>
            )}
          </span>
        )
      )}
      {controls && <div className="shrink-0">{controls}</div>}
      {/* On a narrow strip, what gives way is clipped rather than painted over its neighbours: this slot first, then the readouts. */}
      <div className="min-w-0 flex-1 overflow-hidden">{children}</div>
      {readouts && <div className="flex min-w-0 items-center gap-3 overflow-hidden whitespace-nowrap">{readouts}</div>}
      {head && (
        <span className="truncate text-muted-foreground" title={`Log head: ${head}`}>
          head {head.slice(0, 16)}
        </span>
      )}
    </div>
  );
}

/**
 * The strip for the live mission, reading the latest tick from the store as
 * a string compared by value: it renders once a tick, and nothing around it
 * does. It carries the pace, and once a mission ends offers its replay, which
 * stays on offer after the idle engine has taken the stream over.
 */
export function LiveTimeControl({
  store,
  status,
  cursor,
  clock,
}: {
  store: MissionStore;
  status: StreamStatus;
  cursor: Signal<GroundPoint | undefined>;
  clock: LiveClock;
}) {
  const tick = useSyncExternalStore(store.subscribe, () => {
    const view = store.get();
    return view ? `${view.tick_ms} ${view.head} ${view.speed ?? 1}` : '';
  });
  const ended = useSyncExternalStore(store.subscribe, store.ended);
  const [ms, head, pace] = tick.split(' ');
  const speed = pace ? Number(pace) : 1;
  return (
    <TimeControl
      tickMs={ms ? Number(ms) : undefined}
      head={head}
      status={status}
      speed={speed}
      controls={<LiveSpeed speed={speed} clock={clock} />}
      readouts={<Readouts store={store} cursor={cursor} />}
    >
      {ended && (
        <Link to="/replays/$id" params={{ id: ended }} className="whitespace-nowrap text-cyan-400 underline-offset-4 hover:underline">
          Replay {ended}
        </Link>
      )}
    </TimeControl>
  );
}

/**
 * The live pace: a ladder whose first rung is real time, pressed on the pace
 * the stream reports, keeld's rather than the last click. The strip's state
 * says when the fleet runs faster (TimeControl). keeld's reason stands on the
 * ladder when the pace must stay real time, its refusal beside it when one
 * comes.
 */
export function LiveSpeed({ speed, clock }: { speed: number; clock: LiveClock }) {
  const busy = clock.pending !== undefined;
  const locked = clock.fixed !== undefined || busy;
  return (
    <span className="flex min-w-0 items-center gap-2">
      <span role="group" aria-label="Live pace" aria-busy={busy} title={clock.fixed} className="flex shrink-0 items-center gap-1">
        {liveLadder(Math.max(clock.maxSpeed, speed)).map((m) => (
          <Button
            key={m}
            size="xs"
            variant="ghost"
            aria-pressed={m === speed}
            disabled={locked}
            className={cn('tabular-nums', m === speed && (m > 1 ? 'bg-amber-400/15 text-amber-400' : 'bg-muted text-foreground'))}
            onClick={() => clock.set(m)}
          >
            {m === 1 ? 'Real time' : `×${m}`}
          </Button>
        ))}
      </span>
      {clock.error && (
        <span role="alert" className="truncate text-destructive" title={clock.error.message}>
          {clock.error.message}
        </span>
      )}
    </span>
  );
}

/** The strip's readouts, live and replayed: coverage, and the position under the cursor. */
export function Readouts({ store, cursor }: { store: MissionSource; cursor: Signal<GroundPoint | undefined> }) {
  return (
    <>
      <CoverageReadout store={store} />
      <CursorReadout cursor={cursor} />
    </>
  );
}

function Transport({ replay }: { replay: ReplayTransport }) {
  const { playing, speed, onPlaying, onSpeed } = replay;
  const magnitude = Math.abs(speed);
  const backward = speed < 0;

  return (
    <span className="flex items-center gap-1">
      <Button size="icon-sm" variant="ghost" aria-label={playing ? 'Pause' : 'Play'} onClick={() => onPlaying(!playing)}>
        {playing ? <Pause /> : <Play />}
      </Button>
      <Button
        size="icon-sm"
        variant="ghost"
        aria-label="Play backward"
        aria-pressed={backward}
        className={cn(backward && 'bg-muted text-foreground')}
        onClick={() => onSpeed(-speed)}
      >
        <Rewind />
      </Button>
      {SPEED_LADDER.map((m) => (
        <Button
          key={m}
          size="xs"
          variant="ghost"
          aria-pressed={m === magnitude}
          className={cn('tabular-nums', m === magnitude && 'bg-muted text-foreground')}
          onClick={() => onSpeed(setSpeedMagnitude(speed, m))}
        >
          ×{m}
        </Button>
      ))}
    </span>
  );
}
