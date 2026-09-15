import {
  Cartographic,
  Math as CesiumMath,
  ScreenSpaceEventHandler,
  ScreenSpaceEventType,
  type Viewer,
} from 'cesium';
import { useEffect } from 'react';

import type { GroundPoint } from '@/lib/geoFormat';
import type { Signal } from '@/lib/signal';

/**
 * Writes the ground position under the mouse into `cursor`, undefined off
 * the globe: picked on the WGS84 ellipsoid, which is the terrain the globe
 * draws (design section 9.4), so the position is exact rather than
 * approximated. Renders nothing; unmounting stops listening.
 */
export function CursorTracker({ viewer, cursor }: { viewer: Viewer; cursor: Signal<GroundPoint | undefined> }) {
  useEffect(() => {
    const handler = new ScreenSpaceEventHandler(viewer.scene.canvas);
    handler.setInputAction((movement: ScreenSpaceEventHandler.MotionEvent) => {
      const at = viewer.camera.pickEllipsoid(movement.endPosition, viewer.scene.globe.ellipsoid);
      if (!at) {
        cursor.set(undefined);
        return;
      }
      const c = Cartographic.fromCartesian(at);
      cursor.set({ lat: CesiumMath.toDegrees(c.latitude), lon: CesiumMath.toDegrees(c.longitude) });
    }, ScreenSpaceEventType.MOUSE_MOVE);
    return () => {
      handler.destroy();
      cursor.set(undefined);
    };
  }, [viewer, cursor]);

  return null;
}
