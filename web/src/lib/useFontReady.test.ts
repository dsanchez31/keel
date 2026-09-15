import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { FONT_MONO } from './fonts';
import { useFontReady } from './useFontReady';

const FONT = `12px ${FONT_MONO}`;

function stubFonts(load: (font: string) => Promise<unknown>) {
  Object.defineProperty(document, 'fonts', { value: { load }, configurable: true });
}

afterEach(() => {
  // jsdom has no FontFaceSet: remove the stub so the next test sees none.
  Reflect.deleteProperty(document, 'fonts');
});

describe('useFontReady', () => {
  it('is ready at once without FontFaceSet, rather than waiting forever', () => {
    const { result } = renderHook(() => useFontReady(FONT));
    expect(result.current).toBe(true);
  });

  it('waits for the face, then is ready', async () => {
    let resolve: () => void = () => {};
    const load = vi.fn(() => new Promise<unknown>((r) => (resolve = () => r([]))));
    stubFonts(load);

    const { result } = renderHook(() => useFontReady(FONT));
    expect(result.current).toBe(false);
    expect(load).toHaveBeenCalledWith(FONT);

    await act(async () => resolve());
    expect(result.current).toBe(true);
  });

  it('is ready when the load fails: draw with what there is', async () => {
    stubFonts(() => Promise.reject(new SyntaxError('malformed')));
    const { result } = renderHook(() => useFontReady(FONT));
    await act(async () => {});
    expect(result.current).toBe(true);
  });
});
