import { type ApprovedPlan, type Assignment } from '@keel/sdk';
import { ArcType, Color, CustomDataSource, type Viewer } from 'cesium';

import { flyOver } from './camera';
import { cartesian, cartesians, label, MARKER_PX, material } from './entityStyle';
import { PALETTE, type Stroke } from './missionLayers';

const PROPOSED_AO: Stroke = { color: PALETTE.cyan, alpha: 0.9, width: 2, dashed: true };
const PROPOSED_LANE: Stroke = { color: PALETTE.cyan, alpha: 0.7, width: 1.5, dashed: true };
const UNASSIGNED_LANE: Stroke = { color: PALETTE.amber, alpha: 0.8, width: 1.5, dashed: true };

/**
 * The compiled plan awaiting the human gate, drawn for inspection before
 * approval (spec section 8.1: the gate shows the resolved AO). Its own data
 * source, apart from the mission's, in a proposed style: everything dashed
 * cyan, the AO, every lane's full path at scan altitude, each lane labelled
 * with the vector the allocator projects onto it, a lane nobody can take in
 * amber. Showing a plan brings the camera over its AO, tilted; approval or
 * discard destroys the preview, the mission scene then drawing what runs.
 */
export class PlanPreviewScene {
  private readonly viewer: Viewer;
  private readonly source = new CustomDataSource('plan-preview');

  constructor(viewer: Viewer) {
    this.viewer = viewer;
    void viewer.dataSources.add(this.source);
  }

  /** Draws the plan in place of whatever was drawn, and brings the camera over it. */
  show(plan: ApprovedPlan, assignments: readonly Assignment[] = []): void {
    const { entities } = this.source;
    const winners = new Map(assignments.map((a) => [a.lane, a.winner]));
    entities.suspendEvents();
    entities.removeAll();
    const { ring } = plan.area.polygon;
    if (ring.length >= 3 && ring[0]) {
      entities.add({
        id: 'plan:ao',
        polyline: { positions: cartesians([...ring, ring[0]]), clampToGround: true, width: 2, material: material(PROPOSED_AO) },
      });
    }
    for (const lane of plan.lanes) {
      const first = lane.waypoints[0];
      if (lane.waypoints.length < 2 || !first) continue;
      const winner = winners.get(lane.id);
      const stroke = winner ? PROPOSED_LANE : UNASSIGNED_LANE;
      entities.add({
        id: `plan:lane:${lane.id}`,
        position: cartesian(first),
        polyline: { positions: cartesians(lane.waypoints), arcType: ArcType.NONE, width: stroke.width, material: material(stroke) },
        label: label(`${lane.id} ${winner ?? 'unassigned'}`),
      });
    }
    for (const station of plan.gcs ?? []) {
      entities.add({
        id: `plan:gcs:${station.name}`,
        position: cartesian(station.position),
        point: { pixelSize: MARKER_PX / 2, color: Color.fromCssColorString(PALETTE.pale) },
        label: label(station.name),
      });
    }
    entities.resumeEvents();
    flyOver(this.viewer, plan.area.polygon.ring);
    this.viewer.scene.requestRender();
  }

  destroy(): void {
    this.viewer.dataSources.remove(this.source, true);
  }
}
