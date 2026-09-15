import { type MissionView, type TypedFrame } from '@keel/sdk';
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { createMissionStore, TRACE_LIMIT, useMissionView } from './missionStore';

const view = {
  tick: 1,
  tick_ms: 100,
  head: 'h1',
  mission: { id: 'MSN-001', state: 'running', area: { grid: { explored: [false, false] } } },
  lanes: [],
  vectors: [],
  stations: [],
  explored: 0,
  total: 2,
} as unknown as MissionView;

const frame = (seq: number, type: string, data: unknown, t = 200) => ({ seq, type, data, t }) as unknown as TypedFrame;
const tick = (seq: number, n: number) => frame(seq, 'tick', { tick: n, head: `h${n}`, state: 'running', explored: 1, total: 2, cursors: [] }, n * 100);

describe('createMissionStore', () => {
  it('publishes the snapshot, then once per tick, never between the frames of one', () => {
    const store = createMissionStore();
    const listener = vi.fn();
    store.subscribe(listener);

    store.apply(frame(1, 'mission', view, 100));
    expect(store.get()).toBe(view);
    expect(listener).toHaveBeenCalledTimes(1);

    store.apply(frame(2, 'coverage', { cells: [0] }));
    expect(store.get()).toBe(view);
    expect(listener).toHaveBeenCalledTimes(1);

    store.apply(tick(3, 2));
    expect(store.get()?.head).toBe('h2');
    expect(store.get()?.mission.area.grid.explored).toEqual([true, false]);
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it('holds a mid-tick mission frame until the tick closes', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    store.apply(frame(2, 'mission', { ...view, head: 'reshaped' }));
    expect(store.get()?.head).toBe('h1');
    store.apply(tick(3, 2));
    expect(store.get()?.head).toBe('h2');
  });

  it('stops calling a listener once unsubscribed', () => {
    const store = createMissionStore();
    const listener = vi.fn();
    store.subscribe(listener)();
    store.apply(frame(1, 'mission', view, 100));
    expect(listener).not.toHaveBeenCalled();
  });
});

describe('the trace', () => {
  const decision = (seq: number, rationale: string, t = 200) => frame(seq, 'decision', { tick_ms: t, kind: 'assignment', rationale }, t);
  const lines = (store: ReturnType<typeof createMissionStore>) =>
    store.trace().map((e) => ('decision' in e ? e.decision.rationale : `${e.mark}@${e.t}`));

  it('starts with the connection, and publishes decisions when their tick closes', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    store.apply(decision(2, 'a'));
    expect(lines(store)).toEqual(['connected@100']);
    store.apply(tick(3, 2));
    expect(lines(store)).toEqual(['connected@100', 'a']);
  });

  it('marks a reconnection, where decisions may be missing', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    store.apply(decision(2, 'a'));
    store.apply(tick(3, 2));
    store.apply(frame(1, 'mission', { ...view, tick: 9, tick_ms: 900 }, 900));
    expect(lines(store)).toEqual(['connected@100', 'a', 'resynced@900']);
  });

  it('starts over for another mission, keeping the decisions of the tick that started it', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    store.apply(decision(2, 'old'));
    store.apply(tick(3, 2));
    store.apply(decision(4, 'launch', 300));
    store.apply(frame(5, 'mission', { ...view, mission: { ...view.mission, id: 'MSN-002' } }, 300));
    store.apply(tick(6, 3));
    expect(lines(store)).toEqual(['launch']);
  });

  it('keeps a finished mission\'s trace through the idle engine that follows it', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    store.apply(decision(2, 'complete'));
    store.apply(tick(3, 2));
    store.apply(frame(4, 'mission', { ...view, mission: { ...view.mission, id: '' } }, 0));
    store.apply(tick(5, 1));
    expect(lines(store)).toEqual(['connected@100', 'complete']);
  });

  it('keeps the latest decisions up to its limit', () => {
    const store = createMissionStore();
    store.apply(frame(1, 'mission', view, 100));
    for (let i = 0; i <= TRACE_LIMIT; i++) store.apply(decision(i + 2, `d${i}`));
    store.apply(tick(TRACE_LIMIT + 3, 2));
    const all = lines(store);
    expect(all).toHaveLength(TRACE_LIMIT);
    expect(all.at(-1)).toBe(`d${TRACE_LIMIT}`);
    expect(all[0]).toBe('d1');
  });
});

describe('ended', () => {
  it('remembers a finished mission through the idle engine, until another starts', () => {
    const store = createMissionStore();
    const with_ = (id: string, state: string) => ({ ...view, mission: { ...view.mission, id, state } });
    const closing = (seq: number, state: string) => frame(seq, 'tick', { tick: seq, head: `h${seq}`, state, explored: 0, total: 2, cursors: [] });
    store.apply(frame(1, 'mission', view, 100));
    expect(store.ended()).toBeUndefined();
    store.apply(frame(2, 'mission', with_('MSN-001', 'complete')));
    store.apply(closing(3, 'complete'));
    expect(store.ended()).toBe('MSN-001');
    store.apply(frame(4, 'mission', with_('', 'planning'), 0));
    store.apply(closing(5, 'planning'));
    expect(store.ended()).toBe('MSN-001');
    store.apply(frame(6, 'mission', with_('MSN-002', 'running')));
    store.apply(closing(7, 'running'));
    expect(store.ended()).toBeUndefined();
  });
});

describe('useMissionView', () => {
  it('renders the published view and follows it', () => {
    const store = createMissionStore();
    const { result } = renderHook(() => useMissionView(store));
    expect(result.current).toBeUndefined();
    act(() => store.apply(frame(1, 'mission', view, 100)));
    expect(result.current).toBe(view);
    act(() => store.apply(tick(2, 2)));
    expect(result.current?.tick).toBe(2);
  });
});
