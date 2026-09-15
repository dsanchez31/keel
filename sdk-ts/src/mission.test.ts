import { describe, expect, it } from 'vitest';

import type { Capabilities, MissionView, VectorState } from './generated/wire';
import { applyFrame } from './mission';
import type { TypedFrame } from './stream';

const caps = (id: string): Capabilities => ({
  id,
  domain: 'aerial',
  tags: ['camera'],
  cruise_speed: 15,
  max_range_m: 20000,
  sensor_radius_m: 40,
});

const state = (id: string, lat = 45): VectorState => ({
  id,
  position: { lat, lon: 5, alt_m: 120 },
  heading: 0,
  speed: 15,
  battery_pct: 90,
  link: 'ok',
  mode: 'transit',
  last_seen_ms: 100,
  ack_seq: 0,
});

const snapshot: MissionView = {
  tick: 1,
  tick_ms: 100,
  head: 'h1',
  mission: {
    id: 'MSN-001',
    intent: 'sweep',
    area: {
      name: 'fog_of_war_east',
      polygon: { ring: [] },
      grid: { origin: { lat: 45, lon: 5, alt_m: 0 }, ref_lat: 45, cell_m: 20, cols: 2, rows: 2, in_ao: [true, true, true, true], explored: [false, false, false, false] },
    },
    doctrine: { name: 'recon-standard', version: '2.1.0' },
    plan: 'p',
    state: 'running',
    started_ms: 100,
  },
  doctrine_hash: 'd1',
  lanes: [
    { id: 'L0', index: 0, cells: [0, 1], waypoints: [], assigned_to: 'DRONE-01', cursor: 0 },
    { id: 'L1', index: 1, cells: [2, 3], waypoints: [], assigned_to: 'DRONE-02', cursor: 0 },
  ],
  vectors: [
    { caps: caps('DRONE-01'), state: state('DRONE-01') },
    { caps: caps('DRONE-02'), state: state('DRONE-02') },
  ],
  stations: [],
  explored: 0,
  total: 4,
};

const f = <K extends TypedFrame['type']>(type: K, data: Extract<TypedFrame, { type: K }>['data'], t = 200, seq = 2) =>
  ({ type, data, t, seq }) as unknown as TypedFrame;

describe('applyFrame', () => {
  it('has no view before the snapshot, and the snapshot replaces whatever came before', () => {
    expect(applyFrame(undefined, f('tick', { tick: 2, head: 'h2', state: 'running', explored: 0, total: 4, cursors: [] }))).toBeUndefined();
    const other = { ...snapshot, head: 'other' };
    expect(applyFrame(other, f('mission', snapshot, 100, 1))).toBe(snapshot);
  });

  it('takes the vectors a telemetry frame lists, keeping their capabilities', () => {
    const next = applyFrame(snapshot, f('telemetry', [state('DRONE-02', 46)]))!;
    expect(next.vectors).toEqual([{ caps: caps('DRONE-02'), state: state('DRONE-02', 46) }]);
    expect(snapshot.vectors).toHaveLength(2);
  });

  it('admits a joined vector in id order, and drops one the telemetry of its tick leaves out', () => {
    const joined = applyFrame(snapshot, f('event', { kind: 'vector_joined', caps: caps('DRONE-00'), telemetry: state('DRONE-00') }))!;
    expect(joined.vectors.map((v) => v.caps.id)).toEqual(['DRONE-00', 'DRONE-01', 'DRONE-02']);

    const refused = applyFrame(joined, f('telemetry', [state('DRONE-01'), state('DRONE-02')]))!;
    expect(refused.vectors.map((v) => v.caps.id)).toEqual(['DRONE-01', 'DRONE-02']);
  });

  it('re-admits a known vector with the capabilities it joins with', () => {
    const faster = { ...caps('DRONE-01'), cruise_speed: 20 };
    const next = applyFrame(snapshot, f('event', { kind: 'vector_joined', caps: faster, telemetry: state('DRONE-01') }))!;
    expect(next.vectors.map((v) => v.caps.cruise_speed)).toEqual([20, 15]);
  });

  it('marks coverage cells explored without touching the view it was given', () => {
    const next = applyFrame(snapshot, f('coverage', { cells: [1, 3, 9] }))!;
    expect(next.mission.area.grid.explored).toEqual([false, true, false, true]);
    expect(snapshot.mission.area.grid.explored).toEqual([false, false, false, false]);
  });

  it('closes a tick: time, head, state, coverage count and cursors', () => {
    const next = applyFrame(snapshot, f('tick', { tick: 2, head: 'h2', state: 'complete', explored: 2, total: 4, cursors: [{ lane: 'L1', cursor: 3 }] }))!;
    expect(next).toMatchObject({ tick: 2, tick_ms: 200, head: 'h2', explored: 2, total: 4 });
    expect(next.mission.state).toBe('complete');
    expect(next.lanes.map((l) => l.cursor)).toEqual([0, 3]);
    expect(next.lanes[0]).toBe(snapshot.lanes[0]);
  });

  it('takes the pace a live tick carries, and none from a replay tick', () => {
    const tick = { tick: 2, head: 'h2', state: 'running', explored: 0, total: 4, cursors: [] } as const;
    const fast = applyFrame(snapshot, f('tick', { ...tick, speed: 20 }))!;
    expect(fast.speed).toBe(20);
    expect(applyFrame(fast, f('tick', { ...tick, speed: 20 }))?.speed).toBe(20);
    const replayed = applyFrame(fast, f('tick', tick))!;
    expect(replayed.speed).toBeUndefined();
    expect('speed' in replayed).toBe(false);
  });

  it('returns the view itself for a frame that changes nothing', () => {
    expect(applyFrame(snapshot, f('decision', { tick_ms: 200, kind: 'assignment', rationale: 'r' }))).toBe(snapshot);
    expect(applyFrame(snapshot, f('event', { kind: 'link_changed', vector: 'DRONE-01', link: 'lost' }))).toBe(snapshot);
    const pack = { ref: 'recon-standard@2.1.0', name: 'recon-standard', version: '2.1.0', hash: 'd1', document: { apiVersion: 'v1', name: 'recon-standard', version: '2.1.0', params: { battery_reserve_pct: 20 } } };
    expect(applyFrame(snapshot, f('doctrine', pack))).toBe(snapshot);
    expect(applyFrame(snapshot, f('doctrine', { ...pack, hash: 'd2' }))?.doctrine_hash).toBe('d2');
  });
});
