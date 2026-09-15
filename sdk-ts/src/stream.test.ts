import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { openStream, type StreamStatus, type TypedFrame } from './stream';

/** A WebSocket the test drives: every instance is kept, messages are pushed by hand. */
class FakeSocket {
  static all: FakeSocket[] = [];
  onmessage: ((msg: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;
  closed: { code?: number } | undefined;

  constructor(public url: string) {
    FakeSocket.all.push(this);
  }

  close(code?: number) {
    this.closed = { code };
  }

  push(frame: object | string) {
    this.onmessage?.({ data: typeof frame === 'string' ? frame : JSON.stringify(frame) });
  }

  drop() {
    this.onclose?.();
  }
}

const last = () => FakeSocket.all[FakeSocket.all.length - 1]!;
const frame = (seq: number, type: string, data: unknown = {}) => ({ data, seq, t: seq * 100, type });

function open() {
  const frames: TypedFrame[] = [];
  const statuses: StreamStatus[] = [];
  const stream = openStream(
    { onFrame: (f) => frames.push(f), onStatus: (s) => statuses.push(s) },
    { url: 'ws://keeld/api/v1/stream', WebSocket: FakeSocket as unknown as typeof WebSocket, random: () => 1 },
  );
  return { stream, frames, statuses };
}

beforeEach(() => {
  FakeSocket.all = [];
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('openStream', () => {
  it('delivers the snapshot and every frame that follows it', () => {
    const { frames, statuses } = open();
    last().push(frame(1, 'mission'));
    last().push(frame(2, 'telemetry', []));
    last().push(frame(3, 'tick'));
    expect(frames.map((f) => [f.seq, f.type])).toEqual([
      [1, 'mission'],
      [2, 'telemetry'],
      [3, 'tick'],
    ]);
    expect(statuses).toEqual(['connecting', 'live']);
  });

  it('resynchronises on a gap: nothing after it is delivered, a new connection starts from a snapshot', () => {
    const { frames, statuses } = open();
    const first = last();
    first.push(frame(1, 'mission'));
    first.push(frame(3, 'tick'));
    expect(first.closed?.code).toBe(4000);
    expect(FakeSocket.all).toHaveLength(2);
    first.push(frame(4, 'tick'));
    expect(frames.map((f) => f.seq)).toEqual([1]);

    last().push(frame(1, 'mission'));
    expect(frames.map((f) => f.seq)).toEqual([1, 1]);
    expect(statuses).toEqual(['connecting', 'live', 'resyncing', 'live']);
  });

  it('refuses a connection whose first frame is not the snapshot, and redials through the backoff', () => {
    open();
    last().push(frame(1, 'tick'));
    expect(FakeSocket.all).toHaveLength(1);
    vi.advanceTimersByTime(250);
    expect(FakeSocket.all).toHaveLength(2);
  });

  it('treats a message that is not a frame as a lost frame', () => {
    const { frames } = open();
    last().push(frame(1, 'mission'));
    last().push('not json');
    expect(FakeSocket.all).toHaveLength(2);
    last().push(frame(1, 'mission'));
    last().push(frame(2, 'nonsense'));
    expect(FakeSocket.all).toHaveLength(3);
    expect(frames).toHaveLength(2);
  });

  it('redials a dropped connection after a doubling delay, reset by a snapshot', () => {
    const { statuses } = open();
    last().drop();
    expect(statuses.at(-1)).toBe('connecting');
    vi.advanceTimersByTime(249);
    expect(FakeSocket.all).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeSocket.all).toHaveLength(2);

    last().drop();
    vi.advanceTimersByTime(499);
    expect(FakeSocket.all).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeSocket.all).toHaveLength(3);

    last().push(frame(1, 'mission'));
    last().drop();
    vi.advanceTimersByTime(250);
    expect(FakeSocket.all).toHaveLength(4);
  });

  it('stops for good on close: no redial, no frame', () => {
    const { stream, frames, statuses } = open();
    const ws = last();
    stream.close();
    expect(ws.closed?.code).toBe(1000);
    ws.push(frame(1, 'mission'));
    ws.drop();
    vi.advanceTimersByTime(10_000);
    expect(FakeSocket.all).toHaveLength(1);
    expect(frames).toHaveLength(0);
    expect(statuses.at(-1)).toBe('closed');
  });
});
