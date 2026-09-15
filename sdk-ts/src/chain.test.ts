import { describe, expect, it } from 'vitest';

import { headAt, lines, splitLine, verifyChain } from './chain';
// Written by the Go encoder, pinned by internal/eventlog/sdk_golden_test.go.
import golden from './testdata/chain.ndjson?raw';

const HEAD = 'cf8333f4cba8ad304967e56faf59f54594f024c36d7ca04a0ed02919c7c0dbe8';
const goldenLines = golden.split('\n');

describe('verifyChain', () => {
  it('recomputes the chain the Go encoder wrote, digest for digest', async () => {
    const r = await verifyChain(goldenLines);
    expect(r.broken).toBeUndefined();
    expect(r.records).toBe(5);
    expect(r.head).toBe(HEAD);
    expect(r.ticks).toEqual([
      { tickMs: 100, head: 'a7ab3b65918ad89792be34a4d91b5dc548c300773a3662b20eb9a89a752d0cd6' },
      { tickMs: 200, head: HEAD },
    ]);
  });

  it('hands every linked record over in order', async () => {
    const kinds: string[] = [];
    await verifyChain(goldenLines, { onRecord: (r) => kinds.push(r.kind) });
    expect(kinds).toEqual(['header', 'event', 'decision', 'tick', 'tick']);
  });

  it('finds a payload changed by one byte, at its own record', async () => {
    const tampered = [...goldenLines];
    tampered[1] = tampered[1]!.replace('1.5', '1.6');
    const r = await verifyChain(tampered);
    expect(r.broken?.seq).toBe(2);
    expect(r.broken?.reason).toMatch(/content implies/);
    expect(r.records).toBe(1);
  });

  it('finds a record dropped from the chain', async () => {
    const r = await verifyChain([goldenLines[0]!, goldenLines[2]!]);
    expect(r.broken).toEqual({ seq: 2, reason: 'the record carries seq 3' });
  });

  it('refuses a line that is not a record', async () => {
    expect((await verifyChain(['{"hash":"x"}'])).broken?.reason).toBe('the line is not a record');
  });
});

describe('splitLine', () => {
  it('takes the payload as the exact text between the fixed fields', () => {
    const r = splitLine(goldenLines[0]!);
    expect(r?.kind).toBe('header');
    expect(r?.payload).toBe('{"name":"golden","note":"é ✓ 🛰 \\"quoted\\" back\\\\slash\\nline\\ttab\\u0001"}');
    expect(r?.seq).toBe('1');
    expect(r?.tickMs).toBe('0');
  });
});

describe('lines', () => {
  it('splits a byte stream on newlines, whatever the chunks cut through', async () => {
    const bytes = new TextEncoder().encode(golden);
    const stream = new ReadableStream<Uint8Array<ArrayBuffer>>({
      start(c) {
        // Chunks of 7 bytes cut the multi-byte characters of the header.
        for (let i = 0; i < bytes.length; i += 7) c.enqueue(bytes.slice(i, i + 7));
        c.close();
      },
    });
    const got: string[] = [];
    for await (const l of lines(stream)) got.push(l);
    expect(got).toEqual(goldenLines.filter((l) => l !== ''));
  });
});

describe('headAt', () => {
  const ticks = [
    { tickMs: 100, head: 'a' },
    { tickMs: 200, head: 'b' },
    { tickMs: 300, head: 'c' },
  ];
  it('is the head after the tick at or before a time', () => {
    expect(headAt(ticks, 250)?.head).toBe('b');
    expect(headAt(ticks, 300)?.head).toBe('c');
    expect(headAt(ticks, 50)).toBeUndefined();
  });
});
