import {
  applyFrame,
  type Decision,
  type MissionView,
  type Position,
  type ReplayWindow,
  type TypedFrame,
  typedFrame,
  type VectorID,
} from '@keel/sdk';

import { type MissionSource, type TraceEntry } from './missionStore';

/**
 * A finished mission at a playhead: what the screen's parts read in replay,
 * as they read the live store (MissionSource).
 *
 * The view is keeld's replay window (GET /api/v1/replays/{id}/frames) folded
 * from its snapshot up to the playhead, with the same applyFrame as live, and
 * published at the tick boundaries the window carries (one a second, its
 * tick frames being thinned). Moving the playhead forward folds on; moving it
 * back folds again from the snapshot, which is what a window starting from
 * one is for.
 *
 * The trace is the recording's own decisions (chainWorker.ts), complete from
 * the first tick, up to the tick the view is at: a window's decisions start
 * at its snapshot.
 */
export interface ReplayStore extends MissionSource {
  /** Replaces the window, folding it again up to the playhead. */
  load(window: ReplayWindow): void;
  /** The window loaded, if any. */
  window(): ReplayWindow | undefined;
  /** Moves the playhead, in milliseconds of mission time. */
  seek(ms: number): void;
  /** The recording's decisions, in order. */
  setDecisions(decisions: readonly Decision[]): void;
}

/** One position of a vector at a mission time. */
export interface Sample {
  ms: number;
  position: Position;
}

/**
 * Every position a window's telemetry frames carry, per vector, in time
 * order, each once: what the globe samples a replayed vector from all at
 * once, so it interpolates whichever way the playhead moves.
 */
export function windowSamples(w: ReplayWindow): ReadonlyMap<VectorID, readonly Sample[]> {
  const out = new Map<VectorID, Sample[]>();
  for (const raw of w.frames) {
    const frame = typedFrame(raw);
    if (frame?.type !== 'telemetry') continue;
    for (const state of frame.data) {
      const list = out.get(state.id) ?? [];
      if (list.length === 0 || list[list.length - 1]!.ms < state.last_seen_ms) list.push({ ms: state.last_seen_ms, position: state.position });
      out.set(state.id, list);
    }
  }
  return out;
}

export function createReplayStore(): ReplayStore {
  let win: ReplayWindow | undefined;
  let frames: TypedFrame[] = [];
  let next = 0;
  let playhead = 0;
  let pending: MissionView | undefined;
  let published: MissionView | undefined;
  // Every recorded decision as a trace entry, built once so an entry keeps
  // its identity whatever the playhead does (DecisionTrace memoises on it).
  let entries: readonly { key: number; decision: Decision }[] = [];
  let trace: readonly TraceEntry[] = [];
  const listeners = new Set<() => void>();

  const notify = () => {
    for (const listener of [...listeners]) listener();
  };

  // The decisions up to the view's tick, the same array while that count
  // does not change, so readers compare by reference.
  const retrace = (): boolean => {
    const upTo = published?.tick_ms ?? -1;
    let n = 0;
    while (n < entries.length && entries[n]!.decision.tick_ms <= upTo) n++;
    if (n === trace.length) return false;
    trace = entries.slice(0, n);
    return true;
  };

  const fold = (changedBefore = false) => {
    let changed = changedBefore;
    while (next < frames.length && frames[next]!.t <= playhead) {
      const frame = frames[next]!;
      pending = applyFrame(pending, frame);
      if ((frame.type === 'tick' || next === 0) && pending !== published) {
        published = pending;
        changed = true;
      }
      next++;
    }
    if (retrace()) changed = true;
    if (changed) notify();
  };

  // Back to before the window's snapshot. Says whether readers held a view.
  const restart = (): boolean => {
    const had = published !== undefined;
    next = 0;
    pending = undefined;
    published = undefined;
    return had;
  };

  return {
    get: () => published,
    trace: () => trace,
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    window: () => win,
    load(w) {
      win = w;
      frames = w.frames.flatMap((f) => typedFrame(f) ?? []);
      fold(restart());
    },
    seek(ms) {
      const back = ms < playhead && restart();
      playhead = ms;
      fold(back);
    },
    setDecisions(d) {
      entries = d.map((decision, key) => ({ key, decision }));
      trace = [];
      retrace();
      notify();
    },
  };
}
