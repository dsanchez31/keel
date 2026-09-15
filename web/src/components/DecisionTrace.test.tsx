import { type MissionView, type TypedFrame } from '@keel/sdk';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMissionStore } from '@/lib/missionStore';

import { DecisionTrace } from './DecisionTrace';

afterEach(cleanup);

const view = {
  tick: 1,
  tick_ms: 100,
  head: 'h',
  mission: { id: 'MSN-001', state: 'running', area: { grid: { explored: [] } } },
  lanes: [],
  vectors: [{ caps: { id: 'DRONE-01' } }, { caps: { id: 'DRONE-02' } }],
  stations: [],
  explored: 0,
  total: 0,
} as unknown as MissionView;

const frames = [
  { seq: 1, t: 100, type: 'mission', data: view },
  {
    seq: 2,
    t: 200,
    type: 'decision',
    data: {
      tick_ms: 200,
      kind: 'assignment',
      subject: 'L0',
      winner: 'DRONE-01',
      rationale: 'lowest cost',
      candidates: [
        { vector: 'DRONE-01', cost: 812.4 },
        { vector: 'DRONE-02', cost: 0, rejected: true, reason: 'insufficient range: needs 9000 m, has 4000 m above the 20% reserve' },
      ],
    },
  },
  { seq: 3, t: 300, type: 'decision', data: { tick_ms: 300, kind: 'doctrine_rule', subject: 'DRONE-02', rule_fired: 'rtb-low-battery', shadowed: [{ rule_id: 'hold-on-link-loss', shadowed_by: 'rtb-low-battery', priority: 50 }], rationale: 'battery below reserve' } },
  { seq: 4, t: 300, type: 'tick', data: { tick: 3, head: 'h3', state: 'running', explored: 0, total: 0, cursors: [] } },
] as unknown as TypedFrame[];

function setup(selected?: string) {
  const store = createMissionStore();
  act(() => frames.forEach((f) => store.apply(f)));
  const onSelect = vi.fn();
  render(<DecisionTrace store={store} selected={selected} onSelect={onSelect} />);
  return { onSelect };
}

describe('DecisionTrace', () => {
  it('shows decisions newest first, where its history starts, and every rejected candidate', () => {
    setup();
    const items = [...document.querySelectorAll('ol > li')].map((li) => li.textContent ?? '');
    expect(items[0]).toContain('battery below reserve');
    expect(items[0]).toContain('hold-on-link-loss (priority 50) shadowed by rtb-low-battery');
    expect(items[1]).toContain('insufficient range: needs 9000 m');
    expect(items[2]).toContain('connected: decisions before this were not seen');
  });

  it('keeps to the selected vector, and selects the vector a decision names', () => {
    const { onSelect } = setup('DRONE-02');
    expect(screen.queryByText('lowest cost')).toBeNull();
    expect(screen.getByText('battery below reserve')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'DRONE-02' }));
    expect(onSelect).toHaveBeenCalledWith('DRONE-02');
  });
});
