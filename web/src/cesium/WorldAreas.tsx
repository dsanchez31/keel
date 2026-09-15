import { type WorldView } from '@keel/sdk';
import { type Viewer } from 'cesium';
import { useEffect } from 'react';

import { type MissionSource } from '@/lib/missionStore';
import { useFontReady } from '@/lib/useFontReady';

import { LABEL_FONT } from './entityStyle';
import { WorldScene } from './worldScene';

export interface WorldAreasProps {
  viewer: Viewer;
  store: MissionSource;
  /** The world keeld loaded, undefined until it is read. */
  world: WorldView | undefined;
}

/**
 * Draws the world's areas and stations on the globe (worldScene.ts), leaving
 * the running mission's own to the mission scene. With no mission AO on
 * screen when the world arrives, the camera is brought over the world, as it
 * is over an AO when a mission appears. Waits for the label font, as every
 * scene with labels does. Renders nothing of its own.
 */
export function WorldAreas({ viewer, store, world }: WorldAreasProps) {
  const fontReady = useFontReady(LABEL_FONT);

  useEffect(() => {
    if (!fontReady || !world) return;
    const scene = new WorldScene(viewer);
    scene.show(world);
    const update = () => scene.update(store.get());
    update();
    if (!store.get()?.mission.area.polygon.ring.length) scene.frame();
    const unsubscribe = store.subscribe(update);
    return () => {
      unsubscribe();
      scene.destroy();
    };
  }, [viewer, store, world, fontReady]);

  return null;
}
