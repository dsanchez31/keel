import { describe, expect, it, vi } from 'vitest';

import { createSignal } from './signal';

describe('createSignal', () => {
  it('calls its listeners on a change only, until they unsubscribe', () => {
    const s = createSignal(0);
    const listener = vi.fn();
    const off = s.subscribe(listener);
    s.set(0);
    s.set(1);
    expect(s.get()).toBe(1);
    expect(listener).toHaveBeenCalledTimes(1);
    off();
    s.set(2);
    expect(listener).toHaveBeenCalledTimes(1);
  });
});
