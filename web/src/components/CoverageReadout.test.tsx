import { type MissionView, type TypedFrame } from '@keel/sdk';
import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { createMissionStore } from '@/lib/missionStore';

import { CoverageReadout } from './CoverageReadout';

afterEach(cleanup);

const view = (explored: number, total: number) =>
  ({ tick: 1, tick_ms: 100, head: 'h', mission: { id: '', state: 'planning', area: { grid: { explored: [] } } }, lanes: [], vectors: [], stations: [], explored, total }) as unknown as MissionView;

describe('CoverageReadout', () => {
  it('shows the explored share and the cell count', () => {
    const store = createMissionStore();
    act(() => store.apply({ seq: 1, t: 100, type: 'mission', data: view(3556, 5700) } as unknown as TypedFrame));
    render(<CoverageReadout store={store} />);
    expect(screen.getByText('62.4 %')).toBeTruthy();
    expect(screen.getByText('3,556 / 5,700')).toBeTruthy();
  });

  it('shows nothing without an AO', () => {
    const store = createMissionStore();
    act(() => store.apply({ seq: 1, t: 100, type: 'mission', data: view(0, 0) } as unknown as TypedFrame));
    const { container } = render(<CoverageReadout store={store} />);
    expect(container.textContent).toBe('');
  });
});
