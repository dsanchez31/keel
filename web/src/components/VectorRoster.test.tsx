import { type FleetVectorView, type MissionView, type TypedFrame } from '@keel/sdk';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMissionStore } from '@/lib/missionStore';

import { VectorRoster } from './VectorRoster';

afterEach(cleanup);

const at = { lat: 45, lon: 5, alt_m: 120 };
const vector = (id: string, overrides: object = {}) => ({
  caps: { id, domain: 'aerial', tags: [], cruise_speed: 18, max_range_m: 60000, sensor_radius_m: 90 },
  state: { id, position: at, heading: 0, speed: 18, battery_pct: 80, link: 'ok', mode: 'scanning', last_seen_ms: 5000, ack_seq: 1, ...overrides },
});
const view = {
  tick: 50,
  tick_ms: 5000,
  head: 'h',
  mission: { id: 'MSN-001', state: 'running', area: { grid: { explored: [] } } },
  lanes: [{ id: 'L0', index: 0, cells: [], waypoints: [at, at, at, at], assigned_to: 'DRONE-01', cursor: 2 }],
  vectors: [vector('DRONE-01'), vector('DRONE-02', { link: 'lost', mode: 'rtb', last_seen_ms: 1800 })],
  stations: [],
  explored: 0,
  total: 0,
} as unknown as MissionView;

function setup(selected?: string, fleet?: readonly FleetVectorView[]) {
  const store = createMissionStore();
  act(() => store.apply({ seq: 1, t: 5000, type: 'mission', data: view } as unknown as TypedFrame));
  const onSelect = vi.fn();
  render(<VectorRoster store={store} selected={selected} onSelect={onSelect} fleet={fleet} />);
  return { onSelect };
}

const ekf = 'no frame yet; system 1 has no absolute position estimate (EKF_STATUS_REPORT flags 0xa7)';

describe('VectorRoster', () => {
  it('lists every vector with its mode, link, battery, lane and silence', () => {
    setup();
    const [first, second] = screen.getAllByRole('button');
    expect(first?.textContent).toContain('DRONE-01');
    expect(first?.textContent).toContain('scanning');
    expect(first?.textContent).toContain('L0 2/4');
    expect(second?.textContent).toContain('lost');
    expect(second?.textContent).toContain('no lane');
    expect(second?.textContent).toContain('silent 3.2 s');
  });

  it('selects a vector, and clears the selection from the selected row', () => {
    const { onSelect } = setup('DRONE-02');
    const [first, second] = screen.getAllByRole('button');
    expect(second?.getAttribute('aria-pressed')).toBe('true');
    expect(second?.textContent).toMatch(/31T [A-Z]{2} \d{5} \d{5}/);
    expect(first?.textContent).not.toMatch(/31T/);
    fireEvent.click(first!);
    expect(onSelect).toHaveBeenLastCalledWith('DRONE-01');
    fireEvent.click(second!);
    expect(onSelect).toHaveBeenLastCalledWith(undefined);
  });

  it('lists a bound vector not joined yet in id order, with its reason, selecting nothing', () => {
    setup(undefined, [
      { id: 'DRONE-00', health: 'lost', detail: ekf },
      { id: 'DRONE-01', health: 'ok' },
      { id: 'DRONE-02', health: 'lost' },
    ]);
    const rows = screen.getAllByRole('listitem');
    expect(rows.map((r) => /DRONE-\d{2}/.exec(r.textContent ?? '')?.[0])).toEqual(['DRONE-00', 'DRONE-01', 'DRONE-02']);
    expect(rows[0]?.textContent).toContain('waiting');
    expect(rows[0]?.textContent).toContain(ekf);
    expect(rows[0]?.textContent).toContain('lost');
    expect(rows[1]?.textContent).not.toContain('waiting');
    expect(screen.getAllByRole('button')).toHaveLength(2);
    expect(screen.getByRole('heading', { name: /Vectors/ }).textContent).toContain('2');
  });
});
