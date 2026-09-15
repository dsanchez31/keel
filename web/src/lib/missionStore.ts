import { applyFrame, type Decision, type MissionView, type TypedFrame } from '@keel/sdk';
import { useSyncExternalStore } from 'react';

/** Most decisions the trace holds; older ones are dropped first. */
export const TRACE_LIMIT = 2000;

/**
 * One line of the decision trace: a decision as the stream delivered it, or a
 * mark saying where the stream's history starts or has a hole.
 *
 * - `connected`: the first snapshot. Decisions before it were never seen.
 * - `resynced`: a later snapshot, after a lost frame or a dropped
 *   connection. Decisions in between may be missing.
 */
export type TraceEntry =
  | { readonly key: number; readonly decision: Decision }
  | { readonly key: number; readonly mark: 'connected' | 'resynced'; readonly t: number };

/**
 * The mission view the stream builds, and the trace of its decisions, held
 * outside React: the globe reads them imperatively, so ten ticks a second
 * redraw the scene without rendering a component, and the panels read them
 * through useMissionView and useTrace.
 *
 * It publishes at tick boundaries only. A tick is several frames (decisions,
 * telemetry, coverage, then the tick frame that closes it), and a reader
 * shown the view between two of them would see positions of one tick beside
 * cursors of the last. So frames fold into pending state, and what readers
 * get moves on when a tick frame closes a tick or a snapshot starts a
 * connection: one notification per tick, every one of them a state the
 * engine was in.
 *
 * The trace is what this page saw, not the mission's whole record: a
 * connection starts from a snapshot without past decisions, and a resync
 * loses the decisions of the frames it skipped, so both are marked. It keeps
 * one mission's decisions: a mission frame naming another mission starts a
 * new trace, keeping the decisions of that tick, which the engine made
 * before the frame that names it. The idle engine's decisions between two
 * missions stay with the mission before them. The mission's log is the
 * complete trace, read by the replay scrubber once it is finished.
 */
export interface MissionStore extends MissionSource {
  /** Folds one frame, publishing when it closes a tick or is a snapshot. */
  apply(frame: TypedFrame): void;
  /**
   * The last mission seen complete or failed, until another one starts: the
   * one to offer a replay of, after the idle engine has taken the stream
   * over (spec section 16.3).
   */
  ended(): string | undefined;
}

/**
 * What the screen's parts read, live or replayed (replayStore.ts): a view
 * and a trace published at tick boundaries.
 */
export interface MissionSource {
  /** The view as of the last tick boundary, undefined before the first. */
  get(): MissionView | undefined;
  /** The trace as of the last tick boundary, oldest first. */
  trace(): readonly TraceEntry[];
  /** Calls `listener` at every tick boundary that changed the view or the trace. Returns the unsubscribe. */
  subscribe(listener: () => void): () => void;
}

export function createMissionStore(): MissionStore {
  let pending: MissionView | undefined;
  let published: MissionView | undefined;
  let entries: TraceEntry[] = [];
  let publishedTrace: readonly TraceEntry[] = [];
  let traceChanged = false;
  // Where the entries of the tick being folded start.
  let tickStart = 0;
  let key = 0;
  let missionId = '';
  let endedId: string | undefined;
  let connected = false;
  const listeners = new Set<() => void>();

  const push = (entry: TraceEntry) => {
    entries.push(entry);
    if (entries.length > TRACE_LIMIT) {
      const over = entries.length - TRACE_LIMIT;
      entries = entries.slice(over);
      tickStart = Math.max(0, tickStart - over);
    }
    traceChanged = true;
  };

  const trace = (frame: TypedFrame) => {
    if (frame.type === 'decision') {
      push({ key: key++, decision: frame.data });
      return;
    }
    if (frame.type !== 'mission') return;
    const id = frame.data.mission.id;
    if (id && missionId && id !== missionId) {
      entries = entries.slice(tickStart);
      tickStart = 0;
      traceChanged = true;
    }
    if (id) missionId = id;
    if (frame.seq === 1) {
      push({ key: key++, mark: connected ? 'resynced' : 'connected', t: frame.t });
      connected = true;
    }
  };

  return {
    get: () => published,
    trace: () => publishedTrace,
    ended: () => endedId,
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    apply(frame) {
      pending = applyFrame(pending, frame);
      trace(frame);
      const boundary = frame.type === 'tick' || (frame.type === 'mission' && frame.seq === 1);
      if (!boundary) return;
      tickStart = entries.length;
      if (pending === published && !traceChanged) return;
      published = pending;
      const m = published?.mission;
      if (m?.id && (m.state === 'complete' || m.state === 'failed')) endedId = m.id;
      else if (m?.id && m.id !== endedId) endedId = undefined;
      if (traceChanged) {
        publishedTrace = entries.slice();
        traceChanged = false;
      }
      for (const listener of [...listeners]) listener();
    },
  };
}

/** The store's view for a component, re-rendering it once per tick. */
export function useMissionView(store: MissionSource): MissionView | undefined {
  return useSyncExternalStore(store.subscribe, store.get);
}

/** The store's decision trace for a component, re-rendering it when a tick added to it. */
export function useTrace(store: MissionSource): readonly TraceEntry[] {
  return useSyncExternalStore(store.subscribe, store.trace);
}
