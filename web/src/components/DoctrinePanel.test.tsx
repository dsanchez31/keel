import { type DoctrineInfo, type KeelClient, type MissionView, type TypedFrame } from '@keel/sdk';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMissionStore } from '@/lib/missionStore';

import { DoctrinePanel } from './DoctrinePanel';

afterEach(cleanup);

const pack = (version: string, hash: string, priority: number): DoctrineInfo => ({
  ref: `recon-standard@${version}`,
  name: 'recon-standard',
  version,
  hash,
  document: {
    apiVersion: 'keel.doctrine/v1',
    name: 'recon-standard',
    version,
    params: { battery_reserve_pct: 20 },
    rules: [{ id: 'rtb-low-battery', when: 'agent.battery < 20', then: 'rtb()', priority }],
  },
});
const packs = [pack('2.1.0', 'a'.repeat(64), 90), pack('2.2.0', 'b'.repeat(64), 95)];

const view = (state: string) =>
  ({
    tick: 1,
    tick_ms: 100,
    head: 'h',
    mission: { id: 'MSN-001', state, doctrine: { name: 'recon-standard', version: '2.1.0' }, area: { grid: { explored: [] } } },
    doctrine_hash: 'a'.repeat(64),
    lanes: [],
    vectors: [],
    stations: [],
    explored: 0,
    total: 0,
  }) as unknown as MissionView;

function setup(state = 'running') {
  const store = createMissionStore();
  act(() => store.apply({ seq: 1, t: 100, type: 'mission', data: view(state) } as unknown as TypedFrame));
  const client = { doctrines: vi.fn(() => Promise.resolve(packs)), swapDoctrine: vi.fn(() => Promise.resolve()) } as unknown as KeelClient;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <DoctrinePanel store={store} client={client} />
    </QueryClientProvider>,
  );
  return { client };
}

describe('DoctrinePanel', () => {
  it('shows the active pack, diffs a target against it, and swaps to it', async () => {
    const { client } = setup();
    expect(screen.getByText('a'.repeat(64))).toBeTruthy();
    fireEvent.click(await screen.findByRole('button', { name: /recon-standard@2\.2\.0/ }));
    expect(screen.getByText('- when agent.battery < 20 then rtb() (priority 90)')).toBeTruthy();
    expect(screen.getByText('+ when agent.battery < 20 then rtb() (priority 95)')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: /Swap at the next tick/ }));
    await waitFor(() => expect(client.swapDoctrine).toHaveBeenCalledWith('MSN-001', { name: 'recon-standard', version: '2.2.0' }));
    expect(await screen.findByText(/Accepted/)).toBeTruthy();
  });

  it('swaps nothing unless a mission runs', async () => {
    setup('complete');
    fireEvent.click(await screen.findByRole('button', { name: /recon-standard@2\.2\.0/ }));
    expect(screen.getByRole('button', { name: /Swap at the next tick/ })).toHaveProperty('disabled', true);
  });
});
