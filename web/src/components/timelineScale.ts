/**
 * The arithmetic behind the replay scrubber's axis: how wide it is, where
 * its gradations fall, and how the wheel changes its scale. Gradations of seconds and minutes aligned
 * on the mission's start, spans from half a minute to an hour.
 *
 * Pure (no React, no Cesium, no DOM) so the tick maths can be read and tested
 * on its own. `ReplayScrubber.tsx` is then only pointers, SVG and ARIA.
 *
 * **This is the displayed window, not `playbackWindow`'s sampled one.** The
 * sampled window is twenty minutes of decoded frames and moves in jumps; this
 * one costs nothing and slides continuously under a fixed playhead.
 */

const SECOND_MS = 1000;
const MINUTE_MS = 60_000;

/** Five minutes wide, ±2.5 around the playhead: a lane's worth of sweep. */
export const TIMELINE_DEFAULT_SPAN_MS = 5 * MINUTE_MS;

/** Half a minute. Below this the labels collide. */
export const TIMELINE_MIN_SPAN_MS = 30 * SECOND_MS;

/** An hour: past it the reference run fits twice, and the gradations say little. */
export const TIMELINE_MAX_SPAN_MS = 60 * MINUTE_MS;

/**
 * The only spacings a gradation takes, ascending: round numbers a reader
 * already thinks in, never the 2.5-minute steps a `10^floor(log10)` rule
 * would produce.
 */
const TICK_STEPS_MS: readonly number[] = [
  5 * SECOND_MS,
  10 * SECOND_MS,
  15 * SECOND_MS,
  30 * SECOND_MS,
  MINUTE_MS,
  2 * MINUTE_MS,
  5 * MINUTE_MS,
  10 * MINUTE_MS,
  15 * MINUTE_MS,
  30 * MINUTE_MS,
];
const COARSEST_TICK_STEP_MS = 30 * MINUTE_MS;

/** How many labels the axis aims for when the caller has not measured itself. */
const TARGET_TICK_COUNT = 6;

/** The smallest rung keeping the label count at or under `targetCount`. */
export const tickStep = (spanMs: number, targetCount: number = TARGET_TICK_COUNT): number =>
  TICK_STEPS_MS.find((step) => spanMs / step <= targetCount) ?? COARSEST_TICK_STEP_MS;

/**
 * The mission times a gradation falls on across the window centred on
 * `centreMs`, aligned on the mission's start so a mark lands on T+04:00 and
 * stays still while the axis slides. None before the start: mission time
 * begins at 0.
 */
export const ticks = (centreMs: number, spanMs: number, targetCount: number = TARGET_TICK_COUNT): number[] => {
  const step = tickStep(spanMs, targetCount);
  const from = Math.max(0, centreMs - spanMs / 2);
  const to = centreMs + spanMs / 2;
  const marks: number[] = [];
  for (let at = Math.ceil(from / step) * step; at <= to; at += step) marks.push(at);
  return marks;
};

/** About 16% of span per wheel notch of 100 units, gentle on a trackpad. */
const ZOOM_PER_DELTA = 1.0015;

/** The span a wheel gesture selects: down zooms out, clamped to the bounds. */
export const zoomSpan = (spanMs: number, deltaY: number, minMs = TIMELINE_MIN_SPAN_MS, maxMs = TIMELINE_MAX_SPAN_MS): number =>
  Math.min(maxMs, Math.max(minMs, spanMs * ZOOM_PER_DELTA ** deltaY));
