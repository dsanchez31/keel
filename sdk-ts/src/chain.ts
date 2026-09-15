/**
 * The hash chain of a mission log (spec section 10.1), checked in the
 * browser from the bytes keeld serves (`GET /api/v1/replays/{id}`), so the
 * page proves the chain links (I6) without taking keeld's word for it.
 *
 * A record's digest is SHA-256 of the canonical encoding of five fields
 * (internal/eventlog/hash.go): `kind`, the payload's bytes, `prev_hash`,
 * `seq`, `tick_ms`, keys sorted, byte strings as standard base64. That is one
 * fixed shape, written here, not a second canonical encoder: every value in
 * it is a string of a known alphabet or an integer taken verbatim from the
 * line, and the payload is hashed as the exact bytes the line carries, never
 * decoded and re-encoded. A golden test pins it against records the Go
 * encoder wrote (testdata/chain.ndjson).
 */

/** One line of a log, split: its fields, and its payload as the exact text it carries. */
export interface LogLine {
  hash: string;
  kind: string;
  payload: string;
  prevHash: string;
  /** Integers as their digits on the line, so the preimage carries them unchanged. */
  seq: string;
  tickMs: string;
}

// A line is the canonical encoding of {hash, kind, payload, prev_hash, seq,
// tick_ms} (internal/eventlog/log.go), keys sorted, the payload spliced in
// raw: it sits between a fixed prefix and a fixed suffix, whatever it holds.
const PREFIX = /^\{"hash":"([0-9a-f]{64})","kind":"([a-z_]+)","payload":/;
const SUFFIX = /,"prev_hash":"([0-9a-f]{64})","seq":(0|[1-9]\d*),"tick_ms":(-?(?:0|[1-9]\d*))\}$/;

/** The line's fields, or undefined when it is not a record line. */
export function splitLine(line: string): LogLine | undefined {
  const head = PREFIX.exec(line);
  const tail = SUFFIX.exec(line);
  if (!head || !tail || head[0].length > tail.index) return undefined;
  return {
    hash: head[1]!,
    kind: head[2]!,
    payload: line.slice(head[0].length, tail.index),
    prevHash: tail[1]!,
    seq: tail[2]!,
    tickMs: tail[3]!,
  };
}

const utf8 = new TextEncoder();

/** The bytes whose SHA-256 is the record's digest. */
export function preimage(r: LogLine): Uint8Array<ArrayBuffer> {
  const payload = base64(utf8.encode(r.payload));
  const prev = base64(hexBytes(r.prevHash));
  return utf8.encode(`{"kind":"${r.kind}","payload":"${payload}","prev_hash":"${prev}","seq":${r.seq},"tick_ms":${r.tickMs}}`);
}

/** The record's digest as lowercase hex. */
export async function digest(r: LogLine): Promise<string> {
  return hex(new Uint8Array(await crypto.subtle.digest('SHA-256', preimage(r))));
}

/** Where a chain stops holding. */
export interface ChainBreak {
  /** The record's position in the log, 1 for the first line. */
  seq: number;
  reason: string;
}

/** The head after one tick: the digest of its `tick` record. */
export interface TickHead {
  tickMs: number;
  head: string;
}

export interface ChainResult {
  records: number;
  /** The digest of the last record, the recording's head. */
  head: string;
  /** Every tick's head, in order: what the page compares a replay against, tick by tick. */
  ticks: TickHead[];
  /** Set when the chain breaks; nothing after it is checked. */
  broken?: ChainBreak;
}

export interface ChainProgress {
  records: number;
  tickMs: number;
}

const ZERO = '0'.repeat(64);

export interface VerifyOptions {
  /** Called every `every` records (5000 by default). */
  onProgress?: (p: ChainProgress) => void;
  every?: number;
  /** Called with every record once it is found to link, in order. */
  onRecord?: (r: LogLine) => void;
}

/**
 * Checks a log line by line: every record carries the next `seq`, links to
 * the previous digest (the first to the zero digest), and carries the
 * digest its content implies. Stops at the first that does not.
 */
export async function verifyChain(lines: AsyncIterable<string> | Iterable<string>, opts: VerifyOptions = {}): Promise<ChainResult> {
  const { onProgress, every = 5000, onRecord } = opts;
  const result: ChainResult = { records: 0, head: ZERO, ticks: [] };
  for await (const line of lines) {
    if (line === '') continue;
    const n = result.records + 1;
    const r = splitLine(line);
    if (!r) return { ...result, broken: { seq: n, reason: 'the line is not a record' } };
    if (r.seq !== String(n)) return { ...result, broken: { seq: n, reason: `the record carries seq ${r.seq}` } };
    if (r.prevHash !== result.head) return { ...result, broken: { seq: n, reason: `it links to ${r.prevHash}, the previous digest is ${result.head}` } };
    const d = await digest(r);
    if (d !== r.hash) return { ...result, broken: { seq: n, reason: `it carries digest ${r.hash}, its content implies ${d}` } };
    result.records = n;
    result.head = d;
    onRecord?.(r);
    if (r.kind === 'tick') result.ticks.push({ tickMs: Number(r.tickMs), head: d });
    if (onProgress && n % every === 0) onProgress({ records: n, tickMs: Number(r.tickMs) });
  }
  return result;
}

/**
 * The lines of a byte stream, split on newlines, decoded as UTF-8. The bytes
 * are ArrayBuffer backed, as a fetch body's are: TextDecoderStream takes no
 * SharedArrayBuffer.
 */
export async function* lines(body: ReadableStream<Uint8Array<ArrayBuffer>>): AsyncGenerator<string> {
  const reader = body.pipeThrough(new TextDecoderStream()).getReader();
  let rest = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    rest += value;
    let nl: number;
    while ((nl = rest.indexOf('\n')) >= 0) {
      yield rest.slice(0, nl);
      rest = rest.slice(nl + 1);
    }
  }
  if (rest !== '') yield rest;
}

/** The head after the tick at or before `tickMs`, by binary search over `ticks`. */
export function headAt(ticks: readonly TickHead[], tickMs: number): TickHead | undefined {
  let lo = 0;
  let hi = ticks.length - 1;
  let found: TickHead | undefined;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const t = ticks[mid]!;
    if (t.tickMs <= tickMs) {
      found = t;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}

function hexBytes(h: string): Uint8Array {
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(h.slice(i * 2, i * 2 + 2), 16);
  return out;
}

function hex(b: Uint8Array): string {
  let s = '';
  for (const x of b) s += x.toString(16).padStart(2, '0');
  return s;
}

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

/** Standard base64 with padding, Go's base64.StdEncoding. */
function base64(b: Uint8Array): string {
  let s = '';
  let i = 0;
  for (; i + 2 < b.length; i += 3) {
    const n = (b[i]! << 16) | (b[i + 1]! << 8) | b[i + 2]!;
    s += B64[n >> 18]! + B64[(n >> 12) & 63]! + B64[(n >> 6) & 63]! + B64[n & 63]!;
  }
  if (i < b.length) {
    const n = (b[i]! << 16) | ((b[i + 1] ?? 0) << 8);
    s += B64[n >> 18]! + B64[(n >> 12) & 63]! + (i + 1 < b.length ? B64[(n >> 6) & 63]! : '=') + '=';
  }
  return s;
}
