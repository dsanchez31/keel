import { type ApprovedPlan } from '@keel/sdk';
import { describe, expect, it } from 'vitest';

import { expandedSource, laneRows, shortHash } from './planView';

const at = { lat: 45, lon: 5, alt_m: 120 };
const plan = {
  hash: 'f'.repeat(64),
  area: { name: 'fog_of_war_east', grid: { cols: 2, rows: 1, in_ao: [true, false], explored: [false, false] } },
  lanes: [
    { id: 'L1', index: 1, cells: [1], waypoints: [at] },
    { id: 'L0', index: 0, cells: [0], waypoints: [at, at, at] },
  ],
} as unknown as ApprovedPlan;

describe('laneRows', () => {
  it('lists lanes in index order with their steps and projected vector', () => {
    const rows = laneRows(plan, [{ lane: 'L0', winner: 'DRONE-01', candidates: [], rationale: 'nearest' }]);
    expect(rows).toEqual([
      { id: 'L0', index: 0, steps: 3, cells: 1, vector: 'DRONE-01', rationale: 'nearest' },
      { id: 'L1', index: 1, steps: 1, cells: 1, vector: undefined, rationale: undefined },
    ]);
  });
});

describe('expandedSource', () => {
  it('elides the raster bitmaps and keeps everything else', () => {
    const source = expandedSource(plan);
    expect(source).toContain('"in_ao": "<2 cells, elided>"');
    expect(source).toContain('"explored": "<2 cells, elided>"');
    expect(source).toContain('"cols": 2');
    expect(JSON.parse(source)).toMatchObject({ hash: plan.hash, lanes: plan.lanes });
  });
});

describe('shortHash', () => {
  it('keeps twelve hex digits', () => {
    expect(shortHash('0123456789abcdef')).toBe('0123456789ab');
  });
});
