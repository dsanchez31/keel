import { type Viewer } from 'cesium';
import { useEffect } from 'react';

import { type MissionSource } from '@/lib/missionStore';

import { FogLayer } from './fogLayer';

export interface FogOfWarProps {
  viewer: Viewer;
  store: MissionSource;
}

/**
 * Drapes the fog of war over the AO (fogLayer.ts), redrawn when a tick
 * explored a cell: the store's grid changes then, and only then. Renders
 * nothing of its own; unmounting removes the fog.
 */
export function FogOfWar({ viewer, store }: FogOfWarProps) {
  useEffect(() => {
    const layer = new FogLayer(viewer);
    const draw = () => {
      const view = store.get();
      if (view) layer.update(view.mission.area.grid);
    };
    draw();
    const unsubscribe = store.subscribe(draw);
    return () => {
      unsubscribe();
      layer.destroy();
    };
  }, [viewer, store]);

  return null;
}
