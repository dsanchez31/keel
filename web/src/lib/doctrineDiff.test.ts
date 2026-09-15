import { type Document } from '@keel/sdk';
import { describe, expect, it } from 'vitest';

import { diffPacks, isEmpty } from './doctrineDiff';

const base: Document = {
  apiVersion: 'keel.doctrine/v1',
  name: 'recon-standard',
  version: '2.1.0',
  params: { battery_reserve_pct: 20 },
  roles: [{ id: 'scout', requires: ['camera'] }],
  constraints: [{ id: 'geofence', rule: 'agent.in_ao or agent.mode != "scanning"' }],
  rules: [
    { id: 'hold-on-link-loss', when: 'agent.link == "lost" for 5s', then: 'hold()', priority: 50 },
    { id: 'rtb-low-battery', when: 'agent.battery < doctrine.battery_reserve_pct', then: 'rtb()', priority: 90 },
  ],
};

describe('diffPacks', () => {
  it('finds nothing between a pack and itself', () => {
    expect(isEmpty(diffPacks(base, base))).toBe(true);
  });

  it('reports what a swap adds, removes and changes, by id', () => {
    const next: Document = {
      ...base,
      version: '2.2.0',
      params: { battery_reserve_pct: 25 },
      roles: [{ id: 'scout', requires: ['camera', 'gps'] }],
      constraints: [],
      rules: [
        { id: 'redecompose-on-loss', when: 'agent.mode == "down"', then: 'redecompose()', priority: 70 },
        { id: 'rtb-low-battery', when: 'agent.battery < doctrine.battery_reserve_pct', then: 'rtb()', priority: 95 },
      ],
    };
    const d = diffPacks(base, next);
    expect(d.params).toEqual([{ id: 'battery_reserve_pct', change: 'changed', from: 20, to: 25 }]);
    expect(d.roles.map((c) => c.change)).toEqual(['changed']);
    expect(d.constraints).toEqual([{ id: 'geofence', change: 'removed', from: base.constraints![0] }]);
    expect(d.rules.map((c) => [c.id, c.change])).toEqual([
      ['hold-on-link-loss', 'removed'],
      ['redecompose-on-loss', 'added'],
      ['rtb-low-battery', 'changed'],
    ]);
  });
});
