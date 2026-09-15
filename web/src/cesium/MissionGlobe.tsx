import { type VectorID } from '@keel/sdk';
import { type Viewer } from 'cesium';
import { useEffect, useRef } from 'react';

import { type MissionSource } from '@/lib/missionStore';
import type { Sample } from '@/lib/replayStore';
import { useFontReady } from '@/lib/useFontReady';

import { LABEL_FONT } from './entityStyle';
import { followLive } from './missionClock';
import { MissionScene } from './missionScene';

export interface MissionGlobeProps {
  viewer: Viewer;
  store: MissionSource;
  /** The vector the roster or the trace selected: marked, and flown to. */
  selected: VectorID | undefined;
  /**
   * Live (the default) keeps the clock trailing the stream; a replay leaves
   * the clock to its scrubber and draws its vectors from `samples`.
   */
  mode?: 'live' | 'replay';
  /** A replay window's samples (windowSamples), preloaded into the scene. */
  samples?: ReadonlyMap<VectorID, readonly Sample[]>;
}

/**
 * Draws the mission into the globe (missionScene.ts), live or replayed, and
 * in live keeps the clock trailing the stream (followLive). Renders nothing
 * of its own: the store is read imperatively at every tick boundary, so ten
 * ticks a second redraw the scene without rendering a component.
 *
 * The scene waits for the label font, since Cesium bakes a label's glyphs when
 * the entity is created (useFontReady). Unmounting removes what it drew and
 * stops the clock; the viewer itself is retained (viewerManager.ts).
 */
export function MissionGlobe({ viewer, store, selected, mode = 'live', samples }: MissionGlobeProps) {
  const fontReady = useFontReady(LABEL_FONT);
  const sceneRef = useRef<MissionScene | undefined>(undefined);

  useEffect(() => {
    if (!fontReady) return;
    const scene = new MissionScene(viewer, { replay: mode === 'replay' });
    sceneRef.current = scene;
    const draw = () => {
      const view = store.get();
      if (view) scene.update(view);
    };
    draw();
    const unsubscribe = store.subscribe(draw);
    const stopFollowing =
      mode === 'live'
        ? followLive(
            viewer,
            () => store.get()?.tick_ms,
            () => store.get()?.speed ?? 1,
          )
        : undefined;
    return () => {
      sceneRef.current = undefined;
      stopFollowing?.();
      unsubscribe();
      scene.destroy();
    };
  }, [viewer, store, fontReady, mode]);

  // The effects below run after the one above in the same commit, so a
  // scene it created is marked and sampled at once.
  useEffect(() => {
    if (samples) sceneRef.current?.preload(samples);
  }, [samples, viewer, store, fontReady, mode]);

  useEffect(() => {
    sceneRef.current?.select(selected);
  }, [selected, viewer, store, fontReady, mode]);

  return null;
}
