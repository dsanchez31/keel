import { type StreamHandlers, type TypedFrame } from '@keel/sdk';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

const opened: StreamHandlers[] = [];
const close = vi.fn();

vi.mock('@keel/sdk', () => ({
  openStream: (handlers: StreamHandlers) => {
    opened.push(handlers);
    return { close };
  },
}));

const { useMissionStream } = await import('./useMissionStream');

const snapshot = { data: {}, seq: 1, t: 0, type: 'mission' } as unknown as TypedFrame;

afterEach(() => {
  opened.length = 0;
  vi.clearAllMocks();
});

describe('useMissionStream', () => {
  it('opens one stream, reports its status, and closes it on unmount', () => {
    const { result, unmount } = renderHook(() => useMissionStream(() => {}));
    expect(opened).toHaveLength(1);
    expect(result.current).toBe('connecting');

    act(() => opened[0]?.onStatus?.('live'));
    expect(result.current).toBe('live');

    unmount();
    expect(close).toHaveBeenCalledTimes(1);
  });

  it('hands frames to the latest handler without reopening the stream', () => {
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook(({ onFrame }) => useMissionStream(onFrame), { initialProps: { onFrame: first } });
    rerender({ onFrame: second });

    act(() => opened[0]?.onFrame(snapshot));
    expect(opened).toHaveLength(1);
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledWith(snapshot);
  });
});
