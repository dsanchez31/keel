import { type Decision, type MissionView, type ReplayWindow } from '@keel/sdk';
import { describe, expect, it, vi } from 'vitest';

import { createReplayStore, windowSamples } from './replayStore';

const view = (tickMs: number) =>
  ({
    tick: tickMs / 100,
    tick_ms: tickMs,
    head: `h${tickMs}`,
    mission: { id: 'MSN-042', state: 'running', area: { grid: { explored: [false, false] } } },
    lanes: [],
    vectors: [],
    stations: [],
    explored: 0,
    total: 2,
  }) as unknown as MissionView;

const tick = (seq: number, ms: number) => ({ seq, t: ms, type: 'tick', data: { tick: ms / 100, head: `h${ms}`, state: 'running', explored: 1, total: 2, cursors: [] } });

// A window from 1000 ms: the snapshot after the tick at 900, then ticks
// sampled at 1000 and 2000, a coverage delta between them.
const window = {
  from_ms: 1000,
  to_ms: 2000,
  verified_ms: 2000,
  frames: [
    { seq: 1, t: 900, type: 'mission', data: view(900) },
    tick(2, 1000),
    { seq: 3, t: 1500, type: 'coverage', data: { cells: [0] } },
    tick(4, 2000),
  ],
} as unknown as ReplayWindow;

const decision = (ms: number, rationale: string) => ({ tick_ms: ms, kind: 'assignment', rationale }) as Decision;

describe('windowSamples', () => {
  it('collects each vector position once, in time order', () => {
    const at = (lat: number) => ({ lat, lon: 5, alt_m: 120 });
    const state = (id: string, ms: number, lat: number) => ({ id, position: at(lat), last_seen_ms: ms });
    const w = {
      frames: [
        { seq: 1, t: 1000, type: 'telemetry', data: [state('A', 1000, 45), state('B', 900, 46)] },
        { seq: 2, t: 2000, type: 'telemetry', data: [state('A', 2000, 45.1), state('B', 900, 46)] },
      ],
    } as unknown as ReplayWindow;
    const samples = windowSamples(w);
    expect(samples.get('A')).toEqual([
      { ms: 1000, position: at(45) },
      { ms: 2000, position: at(45.1) },
    ]);
    expect(samples.get('B')).toEqual([{ ms: 900, position: at(46) }]);
  });
});

describe('createReplayStore', () => {
  it('folds the window up to the playhead, publishing at its tick boundaries', () => {
    const store = createReplayStore();
    const listener = vi.fn();
    store.subscribe(listener);
    store.seek(1200);
    store.load(window);
    expect(store.get()?.tick_ms).toBe(1000);

    store.seek(1700);
    expect(store.get()?.tick_ms).toBe(1000);
    store.seek(2000);
    expect(store.get()?.head).toBe('h2000');
    expect(store.get()?.mission.area.grid.explored).toEqual([true, false]);
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it('folds again from the snapshot when the playhead goes back', () => {
    const store = createReplayStore();
    store.load(window);
    store.seek(2000);
    store.seek(1100);
    expect(store.get()?.tick_ms).toBe(1000);
    expect(store.get()?.mission.area.grid.explored).toEqual([false, false]);
  });

  it('has no view before the snapshot of its window', () => {
    const store = createReplayStore();
    store.load(window);
    store.seek(2000);
    store.seek(500);
    expect(store.get()).toBeUndefined();
  });

  it("traces the recording's decisions up to the tick the view is at, keeping each entry", () => {
    const store = createReplayStore();
    store.setDecisions([decision(100, 'launch'), decision(1000, 'a'), decision(1800, 'b')]);
    store.load(window);
    store.seek(1500);
    expect(store.trace().map((e) => ('decision' in e ? e.decision.rationale : ''))).toEqual(['launch', 'a']);
    const first = store.trace()[0];
    store.seek(2000);
    expect(store.trace()).toHaveLength(3);
    expect(store.trace()[0]).toBe(first);
  });
});
