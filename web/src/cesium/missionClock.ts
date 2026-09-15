import { JulianDate, type Viewer } from 'cesium';

import { LIVE_FOLLOW, liveFollowStep } from './liveFollow';

/**
 * Mission time on the Cesium clock.
 *
 * Mission time is milliseconds since an engine's first tick and carries no
 * date (spec section 3). Cesium's clock counts JulianDates, so mission time 0
 * is pinned to one fixed instant. Nothing reads it as a date: the globe draws
 * no sun, and the time controls print mission time.
 */
export const MISSION_EPOCH = JulianDate.fromIso8601('2000-01-01T00:00:00Z');

/** The clock instant of a mission time in milliseconds. */
export const toJulian = (ms: number, result = new JulianDate()): JulianDate =>
  JulianDate.addSeconds(MISSION_EPOCH, ms / 1000, result);

/** The mission time in milliseconds of a clock instant. */
export const toMissionMs = (at: JulianDate): number => JulianDate.secondsDifference(at, MISSION_EPOCH) * 1000;

/**
 * Reports a replay's playhead: `onTime` is called with the clock's mission
 * time on every frame Cesium draws, the clock held within the mission, from
 * 0 to `endMs()` once the end is known. A clock that runs into either bound
 * stops there and `onBound` is called, so the transport shows it stopped.
 * Returns the function that stops reporting, leaving the clock stopped.
 *
 * The transport and the scrubber move the clock through viewerManager's
 * setters; this only reads it, and holds it inside the mission.
 */
export const driveReplay = (
  viewer: Viewer,
  opts: { onTime: (ms: number) => void; endMs: () => number | undefined; onBound: () => void },
): (() => void) => {
  const { clock } = viewer;
  const remove = clock.onTick.addEventListener(() => {
    const ms = toMissionMs(clock.currentTime);
    const end = opts.endMs();
    const held = ms < 0 ? 0 : end !== undefined && ms > end ? end : undefined;
    if (held !== undefined) {
      clock.currentTime = toJulian(held);
      if (clock.shouldAnimate) {
        clock.shouldAnimate = false;
        opts.onBound();
      }
    }
    opts.onTime(held ?? ms);
  });
  return () => {
    remove();
    clock.shouldAnimate = false;
    clock.multiplier = 1;
    viewer.scene.requestRender();
  };
};

/**
 * Keeps the viewer's clock trailing the live stream (liveFollow.ts):
 * `latestMs` is read on every frame Cesium draws, the mission time of the
 * latest tick, undefined before the first, and `speed` keeld's pace. Returns
 * the function that stops following, leaving the clock stopped where it is at
 * speed 1.
 *
 * A clock write, which viewerManager.ts says only functions make: this one
 * owns its writes for as long as it follows. Live has no other clock control:
 * its pace is keeld's, which the live strip sets (TimeControl.tsx), and the
 * clock follows it; a replay does not follow, and moves the clock through its
 * transport and scrubber (driveReplay).
 */
export const followLive = (viewer: Viewer, latestMs: () => number | undefined, speed: () => number = () => 1): (() => void) => {
  const { clock } = viewer;
  clock.shouldAnimate = true;
  // Clock.tick runs on every animation frame, whether requestRenderMode then
  // draws or not, so the rate follows each tick without asking for a frame.
  const remove = clock.onTick.addEventListener(() => {
    const latest = latestMs();
    if (latest === undefined) {
      clock.multiplier = 0;
      return;
    }
    const step = liveFollowStep(toMissionMs(clock.currentTime), latest, LIVE_FOLLOW, speed());
    if (step.atMs !== undefined) clock.currentTime = toJulian(step.atMs);
    clock.multiplier = step.multiplier;
  });
  return () => {
    remove();
    clock.shouldAnimate = false;
    clock.multiplier = 1;
    viewer.scene.requestRender();
  };
};
