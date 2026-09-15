import { describe, expect, it } from 'vitest';

import { needsRoll, PLAYBACK_HALF_WINDOW_MS, PLAYBACK_ROLL_MARGIN_MS, playbackWindow, setSpeedMagnitude } from './playbackWindow';

const origin = 30 * 60_000;
const window = playbackWindow(origin);

describe('playbackWindow', () => {
  it('centres the window on the origin', () => {
    expect(origin - window.startMs).toBe(PLAYBACK_HALF_WINDOW_MS);
    expect(window.stopMs - origin).toBe(PLAYBACK_HALF_WINDOW_MS);
  });

  it('clamps it to the mission, from 0 to its end', () => {
    expect(playbackWindow(60_000, 5 * 60_000)).toEqual({ startMs: 0, stopMs: 5 * 60_000 });
  });
});

describe('needsRoll', () => {
  it('leaves a window alone while the playhead is well inside it', () => {
    expect(needsRoll(window, origin)).toBe(false);
  });

  /**
   * The margin is the whole point: rolling only once the playhead is at an
   * edge would mean decoding while it sits on the last sample it has.
   */
  it('rolls before either edge is reached, not at it', () => {
    expect(needsRoll(window, window.stopMs - PLAYBACK_ROLL_MARGIN_MS + 1)).toBe(true);
    expect(needsRoll(window, window.startMs + PLAYBACK_ROLL_MARGIN_MS - 1)).toBe(true);
  });

  it('never rolls against the mission start or its end', () => {
    const ended = playbackWindow(60_000, 5 * 60_000);
    expect(needsRoll(ended, 1000, 5 * 60_000)).toBe(false);
    expect(needsRoll(ended, 5 * 60_000 - 1000, 5 * 60_000)).toBe(false);
  });

  it('rolls for a playhead that has left the window entirely', () => {
    expect(needsRoll(window, window.stopMs + 60_000)).toBe(true);
    expect(needsRoll(window, window.startMs - 60_000)).toBe(true);
  });
});

describe('setSpeedMagnitude', () => {
  it('applies the magnitude in the direction already being played, never changing it', () => {
    expect(setSpeedMagnitude(1, 60)).toBe(60);
    expect(setSpeedMagnitude(-10, 60)).toBe(-60);
    expect(setSpeedMagnitude(-60, 1)).toBe(-1);
  });
});
