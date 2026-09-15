import { createSignal, type Signal, useSignal } from './signal';

/**
 * A replay's playhead in mission milliseconds, set on every frame Cesium
 * draws (driveReplay): read by the scrubber and the time strip only.
 */
export type Playhead = Signal<number>;

export const createPlayhead = (): Playhead => createSignal(0);

/** The playhead for a component, re-rendering it as it moves. */
export const usePlayhead = (playhead: Playhead): number => useSignal(playhead);
