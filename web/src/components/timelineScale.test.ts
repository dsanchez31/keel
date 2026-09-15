import { describe, expect, it } from 'vitest';

import { ticks, tickStep, TIMELINE_MAX_SPAN_MS, TIMELINE_MIN_SPAN_MS, zoomSpan } from './timelineScale';

describe('tickStep', () => {
  it('takes the smallest round step keeping the labels under the target', () => {
    expect(tickStep(5 * 60_000)).toBe(60_000);
    expect(tickStep(30_000)).toBe(5000);
    expect(tickStep(60 * 60_000, 4)).toBe(15 * 60_000);
  });
});

describe('ticks', () => {
  it('lands on round mission times, not on the playhead', () => {
    expect(ticks(150_500, 5 * 60_000)).toEqual([60_000, 120_000, 180_000, 240_000, 300_000]);
  });

  it('puts nothing before the mission starts', () => {
    expect(ticks(20_000, 5 * 60_000)).toEqual([0, 60_000, 120_000]);
  });
});

describe('zoomSpan', () => {
  it('zooms out on a wheel down, in on a wheel up, within the bounds', () => {
    expect(zoomSpan(300_000, 100)).toBeGreaterThan(300_000);
    expect(zoomSpan(300_000, -100)).toBeLessThan(300_000);
    expect(zoomSpan(TIMELINE_MIN_SPAN_MS, -1000)).toBe(TIMELINE_MIN_SPAN_MS);
    expect(zoomSpan(TIMELINE_MAX_SPAN_MS, 1000)).toBe(TIMELINE_MAX_SPAN_MS);
  });
});
