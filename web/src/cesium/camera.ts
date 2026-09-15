import { type Position } from '@keel/sdk';
import { BoundingSphere, HeadingPitchRange, Math as CesiumMath, type Viewer } from 'cesium';

import { cartesians } from './entityStyle';

/**
 * Brings the camera over an area, looking north and 40 degrees down: the
 * tilted perspective design section 8.3 takes from LE_VECTOR, where lanes
 * flown at altitude read as paths in the air above the ground they sweep.
 * Nothing happens for an empty ring.
 */
export function flyOver(viewer: Viewer, ring: readonly Position[]): void {
  const points = cartesians(ring);
  if (points.length === 0) return;
  const sphere = BoundingSphere.fromPoints(points);
  viewer.camera.flyToBoundingSphere(sphere, {
    offset: new HeadingPitchRange(0, CesiumMath.toRadians(-40), sphere.radius * 3),
    duration: 1.5,
  });
}
