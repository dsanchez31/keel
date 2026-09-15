import type {
  CoverageDelta,
  Decision,
  DoctrineInfo,
  Event,
  Frame,
  FrameType,
  MissionView,
  TickSummary,
  VectorState,
} from './generated/wire';

/**
 * The data each frame type carries (spec section 8.2). Keyed by the generated
 * FrameType: a frame type added in Go without an entry here fails to compile
 * (see `_exhaustive` below), so the table cannot fall behind the server.
 */
export interface FrameData {
  mission: MissionView;
  event: Event;
  decision: Decision;
  doctrine: DoctrineInfo;
  /** Every vector's state, in vector id order. */
  telemetry: readonly VectorState[];
  coverage: CoverageDelta;
  tick: TickSummary;
}

const _exhaustive: Record<FrameType, keyof FrameData> = {
  mission: 'mission',
  event: 'event',
  decision: 'decision',
  doctrine: 'doctrine',
  telemetry: 'telemetry',
  coverage: 'coverage',
  tick: 'tick',
};
const FRAME_TYPES = new Set<string>(Object.keys(_exhaustive));

/** One frame, its data typed by its type. */
export type TypedFrame = {
  [K in FrameType]: Omit<Frame, 'type' | 'data'> & { readonly type: K; readonly data: FrameData[K] };
}[FrameType];

/**
 * Where the stream is:
 *
 * - `connecting`: dialling, or waiting to redial after a drop.
 * - `live`: the snapshot arrived and every frame since followed it.
 * - `resyncing`: a frame was lost (a gap in `seq`) or the stream broke its
 *   contract; the connection is being replaced by a fresh one, which starts
 *   from a snapshot. Frames are withheld until it does.
 * - `closed`: `close()` was called.
 */
export type StreamStatus = 'connecting' | 'live' | 'resyncing' | 'closed';

export interface StreamHandlers {
  /**
   * Every frame of the current connection, in order. A `mission` frame with
   * `seq` 1 starts a connection: whatever was built from a previous one is
   * superseded by it.
   */
  onFrame: (frame: TypedFrame) => void;
  onStatus?: (status: StreamStatus) => void;
}

export interface StreamOptions {
  /** The stream's URL, `/api/v1/stream` of the page's origin by default. */
  url?: string;
  /** The WebSocket to use, the global one by default. */
  WebSocket?: typeof WebSocket;
  /** Redial delay bounds in milliseconds, 250 and 5000 by default. */
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /** A number in [0, 1), Math.random by default: the redial jitter. */
  random?: () => number;
}

export interface Stream {
  /** Closes the connection for good: no redial. */
  close(): void;
}

/** Status 4000 to 4999 are the application's: this one says "resynchronising". */
const RESYNC_CLOSE = 4000;

/**
 * Opens keeld's stream (spec section 8.2) and keeps it open.
 *
 * A connection's first frame is the mission snapshot (`seq` 1), and every
 * frame after it is numbered one more than the last. A frame the server
 * dropped for a slow client still consumed its `seq`, so a gap is a frame
 * lost, and the spec's rule is to resynchronise rather than interpolate: the
 * connection is closed and a new one opened at once, which starts from a
 * fresh snapshot. Nothing after the gap is delivered. A frame that is not
 * JSON, of an unknown type, or a connection whose first frame is not the
 * snapshot, is handled the same way.
 *
 * A connection that drops (keeld restarting, the network) is redialled after
 * a delay doubling from minBackoffMs to maxBackoffMs, drawn uniformly between
 * half and all of it so a fleet of tabs does not redial in step; a snapshot
 * resets the delay.
 */
export function openStream(handlers: StreamHandlers, options: StreamOptions = {}): Stream {
  const {
    url = defaultUrl(),
    WebSocket: WS = globalThis.WebSocket,
    minBackoffMs = 250,
    maxBackoffMs = 5000,
    random = Math.random,
  } = options;

  let socket: WebSocket | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let backoff = minBackoffMs;
  let lastSeq = 0;
  let stopped = false;
  let status: StreamStatus | undefined;

  const setStatus = (next: StreamStatus) => {
    if (next === status) return;
    status = next;
    handlers.onStatus?.(next);
  };

  const dial = () => {
    timer = undefined;
    if (stopped) return;
    lastSeq = 0;
    const ws = new WS(url);
    socket = ws;
    ws.onmessage = (msg) => {
      if (ws !== socket) return;
      const frame = parse(msg.data);
      const expected = lastSeq + 1;
      if (!frame || frame.seq !== expected || (expected === 1 && frame.type !== 'mission')) {
        resync(ws);
        return;
      }
      lastSeq = frame.seq;
      if (frame.type === 'mission' && frame.seq === 1) {
        backoff = minBackoffMs;
        setStatus('live');
      }
      handlers.onFrame(frame);
    };
    ws.onclose = () => {
      if (ws !== socket || stopped) return;
      socket = undefined;
      setStatus('connecting');
      schedule();
    };
  };

  // A lost frame: this connection is done, and a fresh one starts from a
  // snapshot, at once when this one had delivered its own. One that broke the
  // contract before its snapshot redials through the backoff instead, so a
  // server that keeps doing it is not hammered.
  const resync = (ws: WebSocket) => {
    const hadSnapshot = lastSeq > 0;
    setStatus('resyncing');
    socket = undefined;
    ws.onmessage = null;
    ws.onclose = null;
    ws.close(RESYNC_CLOSE, 'resync');
    if (hadSnapshot) dial();
    else schedule();
  };

  const schedule = () => {
    const delay = backoff * (0.5 + random() / 2);
    backoff = Math.min(backoff * 2, maxBackoffMs);
    timer = setTimeout(dial, delay);
  };

  setStatus('connecting');
  dial();

  return {
    close() {
      if (stopped) return;
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
      const ws = socket;
      socket = undefined;
      if (ws) {
        ws.onmessage = null;
        ws.onclose = null;
        ws.close(1000, 'closed');
      }
      setStatus('closed');
    },
  };
}

/** A frame, or undefined when the message is not one. */
function parse(data: unknown): TypedFrame | undefined {
  if (typeof data !== 'string') return undefined;
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    return undefined;
  }
  return typedFrame(value);
}

/**
 * A decoded frame, from the stream or a replay window's `frames`, typed by
 * its type; undefined when the value is not a frame of a known type. Its
 * data is taken as the type says: keeld encodes it from that very Go type.
 */
export function typedFrame(value: unknown): TypedFrame | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const f = value as Partial<Frame>;
  if (typeof f.seq !== 'number' || typeof f.t !== 'number' || typeof f.type !== 'string' || !FRAME_TYPES.has(f.type) || !('data' in f)) {
    return undefined;
  }
  return f as TypedFrame;
}

function defaultUrl(): string {
  const { protocol, host } = globalThis.location;
  return `${protocol === 'https:' ? 'wss' : 'ws'}://${host}/api/v1/stream`;
}
