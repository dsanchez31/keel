/**
 * The replay's sampled window and its playback speeds, in mission time.
 * Its window-as-cache
 * model, speed ladder and split of direction from magnitude, sized for a
 * mission rather than an orbit.
 *
 * A pure module (no React, no Cesium) because these numbers are what the
 * replay is sized against and must be readable, and testable, without a
 * WebGL context: every tick of the window is a batch of the log decoded
 * into samples for the globe, so widening it is never free, and the number
 * lives here alone.
 *
 * **The window is a cache, not a limit.** Playback runs over the whole
 * mission and the window follows the playhead, re-centred (`needsRoll`)
 * whenever it drifts near an edge. Nothing in the interface shows it, and
 * nothing stops at it but the mission's own start and end.
 *
 * **Mission time is bounded where an orbit is not.** It starts at 0 and a
 * finished log ends, so the window is clamped to the mission, and an edge
 * sitting on the mission's start or end is no reason to roll: there is
 * nothing beyond it to sample.
 */

/**
 * Half-width of the sampled window: ten minutes of mission either side of
 * the playhead. The reference run lasts about 38 minutes of mission time
 * (spec section 14), so it decodes in two or three rolls, and a mission of
 * hours in rolls of twenty minutes, never all at once.
 */
export const PLAYBACK_HALF_WINDOW_MS = 10 * 60_000;

/**
 * How close the playhead may come to an edge of the window before it is
 * re-centred: two minutes, a few seconds of real time at the top of the
 * ladder, time enough to decode the next span before the playhead needs it.
 */
export const PLAYBACK_ROLL_MARGIN_MS = 2 * 60_000;

/**
 * The playback speeds, as **magnitudes**. The sense of playback is not on
 * this ladder: it belongs to whichever transport button was pressed.
 *
 * Capped at ×60, the reference run in 38 seconds: past that ticks go by
 * faster than frames can show them, and the window rolls every few seconds.
 */
export const SPEED_LADDER = [1, 5, 10, 30, 60] as const;

/** Real time. Where the transport starts, and where the live view always runs. */
export const DEFAULT_SPEED = SPEED_LADDER[0];

/** A span of mission time, in milliseconds. */
export interface PlaybackWindow {
  startMs: number;
  stopMs: number;
}

/**
 * The window `originMs` sits at the centre of, clamped to the mission: from
 * 0, and to `endMs` when the mission has ended.
 */
export const playbackWindow = (originMs: number, endMs?: number): PlaybackWindow => ({
  startMs: Math.max(0, originMs - PLAYBACK_HALF_WINDOW_MS),
  stopMs: endMs === undefined ? originMs + PLAYBACK_HALF_WINDOW_MS : Math.min(endMs, originMs + PLAYBACK_HALF_WINDOW_MS),
});

/**
 * Whether the playhead has drifted close enough to an edge that the window
 * must be re-centred on it. An edge on the mission's start, or on its end
 * when it has ended, never asks for a roll.
 *
 * Checked on every clock tick. A playhead that left the window entirely
 * (a jump of the scrubber) always rolls.
 */
export const needsRoll = (window: PlaybackWindow, atMs: number, endMs?: number): boolean => {
  if (atMs < window.startMs || atMs > window.stopMs) return true;
  const nearStart = window.startMs > 0 && atMs - window.startMs < PLAYBACK_ROLL_MARGIN_MS;
  const nearStop = window.stopMs !== endMs && window.stopMs - atMs < PLAYBACK_ROLL_MARGIN_MS;
  return nearStart || nearStop;
};

/**
 * The speed that playing at `magnitude` in the current direction means.
 *
 * Direction and magnitude are two decisions and they get two controls: a row
 * of badges for this, a single toggle for the sense. Neither reaches into
 * the other's answer, so choosing ×10 while rewinding keeps rewinding.
 */
export const setSpeedMagnitude = (current: number, magnitude: number): number => (current < 0 ? -magnitude : magnitude);
