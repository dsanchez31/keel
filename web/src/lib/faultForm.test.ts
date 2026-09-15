import { describe, expect, it } from 'vitest';

import { faultRequest } from './faultForm';

const input = { vector: 'DRONE-02', magnitude: '', durationS: '' };

describe('faultRequest', () => {
  it('sends a kill with nothing but its vector', () => {
    expect(faultRequest({ ...input, kind: 'kill', magnitude: '5', durationS: '3' })).toEqual({ fault: { vector: 'DRONE-02', kind: 'kill' } });
  });

  it('rounds a duration to whole ticks, blank meaning the rest of the run', () => {
    expect(faultRequest({ ...input, kind: 'link_loss', durationS: '2.34' })).toEqual({ fault: { vector: 'DRONE-02', kind: 'link_loss', duration_ms: 2300 } });
    expect(faultRequest({ ...input, kind: 'link_loss' })).toEqual({ fault: { vector: 'DRONE-02', kind: 'link_loss', duration_ms: 0 } });
  });

  it('needs a positive magnitude for a drain or a drift', () => {
    expect(faultRequest({ ...input, kind: 'battery_drain' })).toHaveProperty('error');
    expect(faultRequest({ ...input, kind: 'gps_drift', magnitude: '-1' })).toHaveProperty('error');
    expect(faultRequest({ ...input, kind: 'gps_drift', magnitude: '4', durationS: '10' })).toEqual({
      fault: { vector: 'DRONE-02', kind: 'gps_drift', magnitude: 4, duration_ms: 10_000 },
    });
  });

  it('refuses a negative or unreadable duration', () => {
    expect(faultRequest({ ...input, kind: 'stale_telemetry', durationS: '-2' })).toHaveProperty('error');
    expect(faultRequest({ ...input, kind: 'stale_telemetry', durationS: 'soon' })).toHaveProperty('error');
  });
});
