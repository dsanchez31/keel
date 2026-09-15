/**
 * The paces the live strip offers (spec section 16.5): ×1 is real time, the
 * others a simulated fleet running faster, keeld and every simulator together.
 * The reference mission, about 38 minutes of real time, lasts about 2 minutes
 * at the top, which is what a demonstration needs; a replay has its own
 * ladder (playbackWindow.ts).
 */
export const LIVE_SPEED_LADDER = [1, 2, 5, 10, 20] as const;

/** The rungs keeld accepts: the ladder up to its fastest pace. */
export const liveLadder = (maxSpeed: number): readonly number[] => LIVE_SPEED_LADDER.filter((s) => s <= maxSpeed);
