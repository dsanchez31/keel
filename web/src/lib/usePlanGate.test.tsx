import { KeelApiError, type KeelClient, type Outcome } from '@keel/sdk';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { type ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { usePlanGate } from './usePlanGate';

const planned = { backend: 'fake', intent: 'sweep the east', plan: { hash: 'abc' }, attempts: [] } as unknown as Outcome;
const refused = { backend: 'fake', intent: 'sweep', attempts: [{ n: 1, reply: '{}' }] } as unknown as Outcome;

function setup(overrides: Partial<KeelClient> = {}) {
  const client = {
    compile: vi.fn(() => Promise.resolve(planned)),
    approve: vi.fn(() => Promise.resolve({ mission: 'MSN-001', plan: 'abc' })),
    discard: vi.fn(() => Promise.resolve()),
    ...overrides,
  } as unknown as KeelClient;
  const queryClient = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  const hook = renderHook(() => usePlanGate(client), { wrapper });
  return { client, ...hook };
}

describe('usePlanGate', () => {
  it('holds the outcome until its plan is approved, then the approval', async () => {
    const { client, result } = setup();
    act(() => result.current.compile('sweep the east'));
    await waitFor(() => expect(result.current.outcome).toBe(planned));
    expect(client.compile).toHaveBeenCalledWith({ intent: 'sweep the east' });

    act(() => result.current.approve());
    await waitFor(() => expect(result.current.approval).toEqual({ mission: 'MSN-001', plan: 'abc' }));
    expect(client.approve).toHaveBeenCalledWith('abc');
    expect(result.current.outcome).toBeUndefined();
  });

  it('clears the outcome on discard', async () => {
    const { client, result } = setup();
    act(() => result.current.compile('sweep the east'));
    await waitFor(() => expect(result.current.outcome).toBe(planned));
    act(() => result.current.discard());
    await waitFor(() => expect(result.current.outcome).toBeUndefined());
    expect(client.discard).toHaveBeenCalledWith('abc');
  });

  it('keeps a refused compilation as an outcome with nothing to approve', async () => {
    const { client, result } = setup({ compile: vi.fn(() => Promise.resolve(refused)) });
    act(() => result.current.compile('sweep'));
    await waitFor(() => expect(result.current.outcome).toBe(refused));
    act(() => result.current.approve());
    expect(client.approve).not.toHaveBeenCalled();
  });

  it('shows the attempts a backend failure carries, with its error', async () => {
    const failure = new KeelApiError(502, { title: 'Bad Gateway', status: 502, detail: 'backend down', outcome: refused }, 'Bad Gateway');
    const { result } = setup({ compile: vi.fn(() => Promise.reject(failure)) });
    act(() => result.current.compile('sweep'));
    await waitFor(() => expect(result.current.error).toBe(failure));
    expect(result.current.outcome).toBe(refused);
  });

  it('keeps the plan when approval is refused', async () => {
    const conflict = new KeelApiError(409, { title: 'Conflict', status: 409, detail: 'a mission is running' }, 'Conflict');
    const { result } = setup({ approve: vi.fn(() => Promise.reject(conflict)) });
    act(() => result.current.compile('sweep the east'));
    await waitFor(() => expect(result.current.outcome).toBe(planned));
    act(() => result.current.approve());
    await waitFor(() => expect(result.current.error).toBe(conflict));
    expect(result.current.outcome).toBe(planned);
  });
});
