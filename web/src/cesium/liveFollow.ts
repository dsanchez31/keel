/**
 * How the live clock trails the stream: entity interpolation, the technique
 * networked games draw remote players with (Valve's Source multiplayer
 * networking; Gambetta, "Entity Interpolation").
 *
 * A vector's samples stop at the last telemetry the engine accepted. A clock
 * on the latest tick would always ask for a position past them, and the
 * vector would hold still, then jump when the next arrived. Drawn a fixed
 * delay behind the latest tick instead, every vector sits between two samples
 * it has, and moves as smoothly at 4 Hz MAVLink as at the simulator's 10 Hz.
 * The globe shows the mission that much late; the head hash the operator reads
 * stays the latest tick's.
 *
 * keeld's pacer and the browser's frame clock drift apart, and frames arrive
 * in bursts, so the clock is steered rather than set: its rate rises when it
 * falls behind its target and drops when it gets ahead, never past the latest
 * tick, and it jumps only when it is too far off to catch up (a first
 * snapshot, a tab back from the background, a new mission's clock).
 *
 * Faster than real time the whole policy scales with keeld's pace: the fleet
 * covers speed times more mission time per second, the stream sends one
 * telemetry frame in speed ticks (spec section 8.2), so the delay, the horizon
 * and the jump are speed times longer in mission time, the same in wall
 * clock, and the rate is steered around the pace rather than around 1.
 *
 * Pure and free of Cesium, so the policy is tested apart from the globe.
 */

export interface LiveFollow {
  /** How far behind the latest tick the clock runs, in milliseconds. */
  delayMs: number;
  /** An error the rate needs this long to absorb, in milliseconds: the gain's inverse. */
  horizonMs: number;
  /** An error beyond which the clock jumps instead, in milliseconds. */
  snapMs: number;
}

/** Five ticks of delay, errors absorbed over 2 s, a jump beyond 2 s. */
export const LIVE_FOLLOW: LiveFollow = { delayMs: 500, horizonMs: 2000, snapMs: 2000 };

/** A new rate for the clock, and a time to jump to when it must. */
export interface LiveFollowStep {
  atMs?: number;
  multiplier: number;
}

/**
 * The clock's next rate, given its mission time, the latest tick's and
 * keeld's pace. The rate is the pace on target, proportional to the error
 * around it within [0, 2 × pace].
 */
export function liveFollowStep(currentMs: number, latestMs: number, follow: LiveFollow = LIVE_FOLLOW, speed = 1): LiveFollowStep {
  const target = latestMs - follow.delayMs * speed;
  const error = target - currentMs;
  if (Math.abs(error) > follow.snapMs * speed) return { atMs: target, multiplier: speed };
  if (currentMs > latestMs) return { atMs: latestMs, multiplier: 0 };
  return { multiplier: speed * Math.min(2, Math.max(0, 1 + error / (follow.horizonMs * speed))) };
}
