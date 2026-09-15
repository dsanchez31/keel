import { describe, expect, it } from 'vitest';

import { LIVE_FOLLOW, liveFollowStep } from './liveFollow';

describe('liveFollowStep', () => {
  it('runs at real time on its target, the delay behind the latest tick', () => {
    expect(liveFollowStep(9500, 10_000)).toEqual({ multiplier: 1 });
  });

  it('speeds up behind its target and slows down ahead of it', () => {
    expect(liveFollowStep(9000, 10_000).multiplier).toBeCloseTo(1.25);
    expect(liveFollowStep(9800, 10_000).multiplier).toBeCloseTo(0.85);
  });

  it('never runs past the latest tick', () => {
    expect(liveFollowStep(10_300, 10_000)).toEqual({ atMs: 10_000, multiplier: 0 });
  });

  it('jumps when it is too far off to catch up, either way', () => {
    expect(liveFollowStep(0, 10_000)).toEqual({ atMs: 9500, multiplier: 1 });
    expect(liveFollowStep(60_000, 10_000)).toEqual({ atMs: 9500, multiplier: 1 });
  });

  it('scales with the pace: delay, horizon and jump in mission time, rate around the pace', () => {
    expect(liveFollowStep(0, 10_000, LIVE_FOLLOW, 20)).toEqual({ multiplier: 20 });
    expect(liveFollowStep(-10_000, 10_000, LIVE_FOLLOW, 20).multiplier).toBeCloseTo(25);
    expect(liveFollowStep(-40_000, 10_000, LIVE_FOLLOW, 20).multiplier).toBeCloseTo(40);
    expect(liveFollowStep(-40_001, 10_000, LIVE_FOLLOW, 20)).toEqual({ atMs: 0, multiplier: 20 });
    expect(liveFollowStep(10_100, 10_000, LIVE_FOLLOW, 20)).toEqual({ atMs: 10_000, multiplier: 0 });
  });

  it('takes its bounds from the policy it is given', () => {
    expect(liveFollowStep(9000, 10_000, { ...LIVE_FOLLOW, delayMs: 1000 })).toEqual({ multiplier: 1 });
  });
});
