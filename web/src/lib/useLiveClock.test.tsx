import { KeelApiError, type KeelClient } from '@keel/sdk';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { type ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { liveLadder } from './liveClock';
import { useLiveClock } from './useLiveClock';

function setup(overrides: Partial<KeelClient> = {}) {
  const client = {
    clock: vi.fn(() => Promise.resolve({ speed: 1, max_speed: 20 })),
    setClock: vi.fn((speed: number) => Promise.resolve({ speed, max_speed: 20 })),
    ...overrides,
  } as unknown as KeelClient;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  return { client, ...renderHook(() => useLiveClock(client), { wrapper }) };
}

describe('useLiveClock', () => {
  it("reads keeld's bounds and puts a pace", async () => {
    const { client, result } = setup();
    await waitFor(() => expect(result.current.maxSpeed).toBe(20));
    expect(result.current.fixed).toBeUndefined();
    act(() => result.current.set(20));
    await waitFor(() => expect(client.setClock).toHaveBeenCalledWith(20));
    await waitFor(() => expect(result.current.pending).toBeUndefined());
    expect(result.current.error).toBeNull();
  });

  it("says why the pace stays real time, and keeps keeld's refusal", async () => {
    const refusal = new KeelApiError(409, { title: 'Conflict', status: 409, detail: 'UGV-01 is not simulated' }, 'Conflict');
    const { result } = setup({
      clock: vi.fn(() => Promise.resolve({ speed: 1, max_speed: 20, fixed: 'UGV-01 is not simulated' })),
      setClock: vi.fn(() => Promise.reject(refusal)),
    });
    await waitFor(() => expect(result.current.fixed).toBe('UGV-01 is not simulated'));
    act(() => result.current.set(5));
    await waitFor(() => expect(result.current.error).toBe(refusal));
  });
});

describe('liveLadder', () => {
  it('offers the rungs keeld accepts', () => {
    expect(liveLadder(20)).toEqual([1, 2, 5, 10, 20]);
    expect(liveLadder(5)).toEqual([1, 2, 5]);
    expect(liveLadder(1)).toEqual([1]);
  });
});
