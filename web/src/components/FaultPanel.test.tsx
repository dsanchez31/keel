import { KeelApiError, type KeelClient, type MissionView, type TypedFrame } from '@keel/sdk';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMissionStore } from '@/lib/missionStore';

import { FaultPanel } from './FaultPanel';

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

// selected null: no vector selected (undefined would take the default).
function setup(injectFault = vi.fn(() => Promise.resolve()), selected: string | null = 'DRONE-02') {
  const client = { injectFault } as unknown as KeelClient;
  const store = createMissionStore();
  act(() => store.apply({ seq: 1, t: 100, type: 'mission', data: view } as unknown as TypedFrame));
  const onSelect = vi.fn();
  const queryClient = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <FaultPanel store={store} selected={selected ?? undefined} onSelect={onSelect} client={client} />
    </QueryClientProvider>,
  );
  return { injectFault, onSelect };
}

describe('FaultPanel', () => {
  it('picks the vector among the fleet, the pick being the screen selection', () => {
    const { onSelect } = setup(undefined, null);
    expect(screen.queryByRole('form')).toBeNull();
    expect(screen.getByText(/Pick the vector to fault/)).toBeTruthy();
    const picker = screen.getByRole('radiogroup', { name: 'Vector' });
    fireEvent.click(within(picker).getByRole('radio', { name: 'DRONE-01' }));
    expect(onSelect).toHaveBeenCalledWith('DRONE-01');
  });

  it('marks the selected vector and offers its form', () => {
    setup();
    expect(screen.getByRole('radio', { name: 'DRONE-02' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('form', { name: 'Faults for DRONE-02' })).toBeTruthy();
  });

  it('kills the selected vector', async () => {
    const { injectFault } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Inject kill/ }));
    await waitFor(() => expect(injectFault).toHaveBeenCalledWith({ vector: 'DRONE-02', kind: 'kill' }));
    expect(await screen.findByText(/Accepted/)).toBeTruthy();
  });

  it('asks a drift for its magnitude and duration, and refuses it without one', async () => {
    const { injectFault } = setup();
    fireEvent.click(screen.getByRole('radio', { name: 'GPS drift' }));
    fireEvent.click(screen.getByRole('button', { name: /Inject gps drift/ }));
    expect(screen.getByText(/needs a positive magnitude/)).toBeTruthy();
    expect(injectFault).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText(/Magnitude/), { target: { value: '4' } });
    fireEvent.change(screen.getByLabelText(/Duration/), { target: { value: '10' } });
    fireEvent.click(screen.getByRole('button', { name: /Inject gps drift/ }));
    await waitFor(() => expect(injectFault).toHaveBeenCalledWith({ vector: 'DRONE-02', kind: 'gps_drift', magnitude: 4, duration_ms: 10_000 }));
  });

  it("shows keeld's reason for a refused fault", async () => {
    const refusal = new KeelApiError(422, { title: 'Unprocessable Entity', status: 422, detail: 'DRONE-02 is bound to no simulator' }, '');
    setup(vi.fn(() => Promise.reject(refusal)));
    fireEvent.click(screen.getByRole('button', { name: /Inject kill/ }));
    expect((await screen.findByRole('alert')).textContent).toContain('bound to no simulator');
  });
});
