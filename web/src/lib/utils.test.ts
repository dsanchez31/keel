import { describe, expect, it } from 'vitest';

import { cn } from './utils';

// Pins the behaviour the components rely on against a 0.x dependency.
describe('cn', () => {
  it('lets the last utility of a conflicting group win', () => {
    expect(cn('px-2 text-sm', 'px-4')).toBe('text-sm px-4');
  });

  it('drops falsy entries', () => {
    expect(cn('font-mono', false, undefined, null, 'text-cyan-400')).toBe('font-mono text-cyan-400');
  });
});
