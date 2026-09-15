import { type Outcome } from '@keel/sdk';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { type PlanGate } from '@/lib/usePlanGate';

import { AssistantPanel } from './AssistantPanel';

afterEach(cleanup);

const at = { lat: 45, lon: 5, alt_m: 120 };
const plan = {
  hash: 'a'.repeat(64),
  mission: '',
  intent: 'sweep the east',
  area: { name: 'fog_of_war_east', polygon: { ring: [] }, grid: { in_ao: [true], explored: [false] } },
  doctrine: { name: 'recon-standard', version: '2.1.0' },
  doctrine_hash: 'b'.repeat(64),
  lanes: [{ id: 'L0', index: 0, cells: [0], waypoints: [at, at] }],
  requires: ['camera'],
  policy: 'lowest_cost',
  gcs: [{ name: 'gcs-west', position: at }],
  rationale: 'Two lanes east to west follow the long axis.',
  swath_m: 162,
  scan_alt_m: 120,
};
const ir = { apiVersion: 'keel.plan/v1', intent: 'sweep the east', tactic: { pattern: 'parallel_lanes', orientation: 'long_axis', lanes: 1 } };
const refusedAttempt = { n: 1, reply: '{}', diagnostics: [{ gate: 'resolution', code: 'unknown_area', message: 'no area named east', pointer: '/mission/area', alternatives: ['fog_of_war_east'] }] };

const gate = (overrides: Partial<PlanGate> = {}): PlanGate => ({
  outcome: undefined,
  approval: undefined,
  error: null,
  busy: undefined,
  compile: vi.fn(),
  approve: vi.fn(),
  discard: vi.fn(),
  ...overrides,
});

describe('AssistantPanel', () => {
  it('shows the compiled plan, its rationale and lanes, and launches it on approval', () => {
    const outcome = { backend: 'fake', intent: 'sweep the east', ir, plan, attempts: [{ n: 1, reply: '{}' }], assignments: [{ lane: 'L0', winner: 'DRONE-01', candidates: [], rationale: 'nearest' }] } as unknown as Outcome;
    const g = gate({ outcome });
    render(<AssistantPanel gate={g} />);

    expect(screen.getByText('fog_of_war_east')).toBeTruthy();
    expect(screen.getByText('Two lanes east to west follow the long axis.')).toBeTruthy();
    expect(screen.getByText('DRONE-01')).toBeTruthy();
    expect(screen.getByText('aaaaaaaaaaaa')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: /Approve and launch/ }));
    expect(g.approve).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole('button', { name: /Discard/ }));
    expect(g.discard).toHaveBeenCalledTimes(1);
  });

  it('shows every diagnostic of a refused compilation and offers nothing to approve', () => {
    const outcome = { backend: 'fake', intent: 'sweep east', attempts: [refusedAttempt] } as unknown as Outcome;
    render(<AssistantPanel gate={gate({ outcome })} />);

    expect(screen.getByText(/the validator refused every attempt/)).toBeTruthy();
    expect(screen.getByText('resolution/unknown_area')).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Approve and launch/ })).toBeNull();
  });

  it('shows why an intent was declined, and neither attempts nor anything to approve', () => {
    const triage = { reply: '{}', mission: false, reason: 'A request for the fleet status, not a reconnaissance.' };
    const outcome = { backend: 'fake', intent: 'check the fleet', triage, attempts: [] } as unknown as Outcome;
    render(<AssistantPanel gate={gate({ outcome })} />);

    expect(screen.getByText(/Not a mission/)).toBeTruthy();
    expect(screen.getByText('A request for the fleet status, not a reconnaissance.')).toBeTruthy();
    expect(screen.queryByText(/refused/)).toBeNull();
    expect(screen.queryByRole('button', { name: /Approve and launch/ })).toBeNull();
  });

  it('reports a failed request and an accepted approval', () => {
    render(<AssistantPanel gate={gate({ error: new Error('a mission is running'), approval: { mission: 'MSN-004', plan: plan.hash } })} />);
    expect(screen.getByRole('alert').textContent).toContain('a mission is running');
    expect(screen.getByText('MSN-004')).toBeTruthy();
  });
});
