import { type LaneView, type Position, type VectorView } from '@keel/sdk';
import { describe, expect, it } from 'vitest';

import {
  activeLane,
  commsStroke,
  distanceM,
  laneLegs,
  laneStrokes,
  nearestStation,
  nextWaypoint,
  PALETTE,
  vectorMarker,
} from './missionLayers';

const at = (lat: number, lon = 5): Position => ({ lat, lon, alt_m: 120 });

const lane = (id: string, index: number, cursor: number, assigned_to?: string): LaneView => ({
  id,
  index,
  cells: [],
  waypoints: [at(45), at(45.001), at(45.002)],
  assigned_to,
  cursor,
});

const vector = (overrides: Partial<VectorView['state']> = {}, domain: 'aerial' | 'ground' = 'aerial') =>
  ({
    caps: { id: 'DRONE-01', domain },
    state: { id: 'DRONE-01', mode: 'scanning', link: 'ok', ...overrides },
  }) as unknown as VectorView;

describe('activeLane', () => {
  it('is the lowest-index lane the vector holds with a waypoint left', () => {
    const lanes = [lane('L2', 2, 0, 'A'), lane('L1', 1, 3, 'A'), lane('L3', 3, 1, 'A'), lane('L0', 0, 0, 'B')];
    expect(activeLane(lanes, 'A')?.id).toBe('L2');
    expect(nextWaypoint(lanes, 'A')).toEqual(at(45));
    expect(activeLane(lanes, 'C')).toBeUndefined();
    expect(nextWaypoint(lanes, 'C')).toBeUndefined();
  });
});

describe('laneLegs', () => {
  it('splits at the cursor, the legs meeting on the last waypoint reached', () => {
    expect(laneLegs(lane('L0', 0, 0))).toEqual({ walked: [], ahead: [at(45), at(45.001), at(45.002)] });
    expect(laneLegs(lane('L0', 0, 2))).toEqual({ walked: [at(45), at(45.001)], ahead: [at(45.001), at(45.002)] });
    expect(laneLegs(lane('L0', 0, 3))).toEqual({ walked: [at(45), at(45.001), at(45.002)], ahead: [at(45.002)] });
  });
});

describe('distanceM and nearestStation', () => {
  it('measures a degree of latitude as the engine does', () => {
    expect(distanceM(at(45), at(46))).toBeCloseTo(111_195, 0);
  });

  it('picks the nearest station, ties to the first by name', () => {
    const stations = [
      { name: 'gcs-west', position: at(45, 4.99) },
      { name: 'gcs-east', position: at(45, 5.01) },
      { name: 'gcs-far', position: at(46) },
    ];
    expect(nearestStation(stations, at(45, 5.002))?.name).toBe('gcs-east');
    expect(nearestStation(stations, at(45))?.name).toBe('gcs-east');
    expect(nearestStation([], at(45))).toBeUndefined();
  });
});

describe('styles', () => {
  it('marks a vector by domain, mode and link', () => {
    expect(vectorMarker(vector())).toEqual({ symbol: 'diamond', color: PALETTE.magenta, alpha: 1 });
    expect(vectorMarker(vector({ mode: 'rtb', link: 'lost' }, 'ground'))).toEqual({ symbol: 'square', color: PALETTE.amber, alpha: 0.45 });
    expect(vectorMarker(vector({ mode: 'down' })).color).toBe(PALETTE.grey);
  });

  it('dashes every link but a healthy one', () => {
    expect(commsStroke('ok').dashed).toBe(false);
    expect(commsStroke('degraded')).toMatchObject({ color: PALETTE.amber, dashed: true });
    expect(commsStroke('lost')).toMatchObject({ color: PALETTE.red, dashed: true });
  });

  it('dashes a lane nobody holds', () => {
    expect(laneStrokes(lane('L0', 0, 0)).ahead).toMatchObject({ color: PALETTE.amber, dashed: true });
    expect(laneStrokes(lane('L0', 0, 0, 'A')).ahead).toMatchObject({ color: PALETTE.pale, dashed: false });
  });
});
