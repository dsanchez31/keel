import { type MissionView, type Position, type WorldView } from '@keel/sdk';
import { Color, CustomDataSource, type Entity, type Viewer } from 'cesium';

import { flyOver } from './camera';
import { cartesian, cartesians, label, MARKER_PX, material } from './entityStyle';
import { PALETTE, type Stroke } from './missionLayers';

const AREA: Stroke = { color: PALETTE.pale, alpha: 0.45, width: 1.5, dashed: false };

/**
 * The world's named areas and stations, what an intent may name, drawn
 * before any mission so the operator sees what the names in the assistant
 * mean: every area outlined thin and pale, clamped to the ground, labelled
 * at its middle; every station as the plan preview marks one. Its own data
 * source, under the mission's: the running mission's AO is left to the
 * mission scene (dashed magenta), and so are the stations once the mission
 * carries them, so nothing is drawn twice.
 */
export class WorldScene {
  private readonly viewer: Viewer;
  private readonly source = new CustomDataSource('world');
  private readonly areas = new Map<string, Entity>();
  private readonly stations: Entity[] = [];
  private points: Position[] = [];
  private hiddenArea: string | undefined;
  private stationsShown = true;

  constructor(viewer: Viewer) {
    this.viewer = viewer;
    void viewer.dataSources.add(this.source);
  }

  show(world: WorldView): void {
    const { entities } = this.source;
    entities.suspendEvents();
    for (const area of world.areas) {
      const { ring } = area.polygon;
      if (ring.length < 3 || !ring[0]) continue;
      this.areas.set(
        area.name,
        entities.add({
          id: `world:area:${area.name}`,
          position: cartesian(middle(ring)),
          polyline: { positions: cartesians([...ring, ring[0]]), clampToGround: true, width: AREA.width, material: material(AREA) },
          label: label(area.name),
        }),
      );
      this.points.push(...ring);
    }
    for (const station of world.stations) {
      this.stations.push(
        entities.add({
          id: `world:station:${station.name}`,
          position: cartesian(station.position),
          point: { pixelSize: MARKER_PX / 2, color: Color.fromCssColorString(PALETTE.pale) },
          label: label(station.name),
        }),
      );
      this.points.push(station.position);
    }
    entities.resumeEvents();
    this.viewer.scene.requestRender();
  }

  /** Leaves the mission's AO and stations to the mission scene; called every tick, it changes something only when they do. */
  update(view: MissionView | undefined): void {
    const area = view && view.mission.area.polygon.ring.length > 0 ? view.mission.area.name : undefined;
    const stationsShown = !view || view.stations.length === 0;
    if (area === this.hiddenArea && stationsShown === this.stationsShown) return;
    this.hiddenArea = area;
    this.stationsShown = stationsShown;
    for (const [name, entity] of this.areas) entity.show = name !== area;
    for (const entity of this.stations) entity.show = stationsShown;
    this.viewer.scene.requestRender();
  }

  /** Brings the camera over every area and station. */
  frame(): void {
    flyOver(this.viewer, this.points);
  }

  destroy(): void {
    this.viewer.dataSources.remove(this.source, true);
  }
}

// Where an area's name goes: the mean of its vertices, inside any convex
// area and near the middle of the others.
function middle(ring: readonly Position[]): Position {
  const lat = ring.reduce((sum, p) => sum + p.lat, 0) / ring.length;
  const lon = ring.reduce((sum, p) => sum + p.lon, 0) / ring.length;
  return { lat, lon, alt_m: 0 };
}
