import {
  type LaneID,
  type LaneView,
  type MissionView,
  type Polygon,
  type Position,
  type Station,
  type VectorID,
  type VectorView,
} from '@keel/sdk';
import {
  ArcType,
  CallbackProperty,
  type Cartesian3,
  Color,
  ColorMaterialProperty,
  ConstantProperty,
  CustomDataSource,
  type Entity,
  ExtrapolationType,
  HeadingPitchRange,
  type JulianDate,
  Math as CesiumMath,
  SampledPositionProperty,
  type Viewer,
} from 'cesium';

import type { Sample } from '@/lib/replayStore';

import { flyOver } from './camera';
import { cartesian, cartesians, label, MARKER_PX, material } from './entityStyle';
import { toJulian } from './missionClock';
import {
  commsStroke,
  laneLegs,
  laneStrokes,
  nearestStation,
  nextWaypoint,
  PALETTE,
  type Stroke,
  vectorMarker,
} from './missionLayers';

/** How much of its trajectory a vector trails, in seconds of mission time. */
const TRAIL_S = 120;
/** How far the camera stands from a selected vector, in metres. */
const SELECTED_RANGE_M = 1500;

// Markers are white and tinted by the billboard's colour, one image per shape.
const svg = (body: string) =>
  `data:image/svg+xml,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 16 16">${body}</svg>`)}`;
const SYMBOLS = {
  diamond: svg('<path d="M8 1.5 14.5 8 8 14.5 1.5 8Z" fill="white" fill-opacity="0.35" stroke="white" stroke-width="1.5"/>'),
  square: svg('<rect x="2.5" y="2.5" width="11" height="11" fill="white" fill-opacity="0.35" stroke="white" stroke-width="1.5"/>'),
  station: svg('<rect x="3.5" y="3.5" width="9" height="9" fill="none" stroke="white" stroke-width="1.5"/><path d="M8 5.5v5M5.5 8h5" stroke="white" stroke-width="1.5"/>'),
};

interface Track {
  marker: Entity;
  comms: Entity;
  task: Entity;
  samples: SampledPositionProperty;
  /** Mission time of the last sample, the `last_seen_ms` it was taken at. */
  lastMs: number;
  /** The station the radio reaches for and the waypoint the vector heads for, if any. */
  station: Cartesian3 | undefined;
  target: Cartesian3 | undefined;
  style: string;
}

interface DrawnLane {
  lane: LaneView;
  walked: Entity;
  ahead: Entity;
}

/**
 * The mission view on the globe, as entities of one data source: the AO
 * boundary, the stations, every lane split at its cursor, and every vector
 * with its trajectory, a line to the waypoint it heads for (its lane
 * assignment) and a line to the station its radio reaches for, styled by the
 * link's state.
 *
 * A vector's position is a SampledPositionProperty: each telemetry the engine
 * accepted is a sample at its `last_seen_ms`, Cesium interpolates between
 * them, and the live clock runs a delay behind the latest tick so there is
 * always a sample on each side (liveFollow.ts). A vector heard from no more
 * holds its last position. The samples of a mission are kept whole.
 *
 * `update` reconciles by id and redraws only what changed: a lane when its
 * cursor or holder moves, the AO and stations when a mission frame brings new
 * ones. A view of another mission, or live with an earlier mission time (a
 * new engine, spec section 16.3), starts from a clean scene.
 *
 * In replay the playhead goes back as often as forward, so an earlier time
 * clears nothing, and a vector's samples come from the replay window all at
 * once (`preload`) rather than from the view as it advances: Cesium then
 * interpolates between two real samples whichever way the clock runs.
 */
export class MissionScene {
  private readonly viewer: Viewer;
  private readonly replay: boolean;
  private readonly source = new CustomDataSource('mission');
  private readonly preloaded = new Map<VectorID, readonly Sample[]>();
  private missionId: string | undefined;
  private lastTickMs = -Infinity;
  private polygon: Polygon | undefined;
  private ao: Entity | undefined;
  private stations: readonly Station[] | undefined;
  private readonly stationEntities: Entity[] = [];
  private readonly stationAt = new Map<string, Cartesian3>();
  private readonly lanes = new Map<LaneID, DrawnLane>();
  private readonly tracks = new Map<VectorID, Track>();
  private selected: VectorID | undefined;
  private framed: string | undefined;

  constructor(viewer: Viewer, opts: { replay?: boolean } = {}) {
    this.viewer = viewer;
    this.replay = opts.replay ?? false;
    void viewer.dataSources.add(this.source);
  }

  /**
   * Adds a replay window's samples to its vectors, those not on the globe yet
   * when they appear. A sample at a time already held replaces it.
   */
  preload(samples: ReadonlyMap<VectorID, readonly Sample[]>): void {
    for (const [id, list] of samples) {
      this.preloaded.set(id, list);
      const track = this.tracks.get(id);
      if (track) addSamples(track, list);
    }
    this.viewer.scene.requestRender();
  }

  update(view: MissionView): void {
    const otherMission = this.missionId !== undefined && view.mission.id !== this.missionId;
    if (otherMission || (!this.replay && view.tick_ms < this.lastTickMs)) {
      this.clear();
    }
    this.missionId = view.mission.id;
    this.lastTickMs = view.tick_ms;

    const { entities } = this.source;
    entities.suspendEvents();
    try {
      this.drawArea(view.mission.area.polygon);
      this.drawStations(view.stations);
      this.drawLanes(view.lanes);
      this.drawVectors(view);
    } finally {
      entities.resumeEvents();
    }
    this.viewer.scene.requestRender();
  }

  /**
   * Marks one vector, its marker drawn larger, and flies the camera to it,
   * looking north 40 degrees down; undefined clears the mark. A vector not on
   * the globe yet is marked when it appears.
   */
  select(id: VectorID | undefined): void {
    this.selected = id;
    for (const [vid, track] of this.tracks) this.mark(vid, track);
    const track = id === undefined ? undefined : this.tracks.get(id);
    if (track) {
      void this.viewer.flyTo(track.marker, {
        duration: 1,
        offset: new HeadingPitchRange(0, CesiumMath.toRadians(-40), SELECTED_RANGE_M),
      });
    }
    this.viewer.scene.requestRender();
  }

  /** Removes everything the scene drew; the viewer lives on. */
  destroy(): void {
    this.viewer.dataSources.remove(this.source, true);
  }

  private mark(id: VectorID, track: Track): void {
    if (track.marker.billboard) track.marker.billboard.scale = new ConstantProperty(id === this.selected ? 1.6 : 1);
  }

  private clear(): void {
    this.source.entities.removeAll();
    this.preloaded.clear();
    this.polygon = undefined;
    this.ao = undefined;
    this.stations = undefined;
    this.stationEntities.length = 0;
    this.stationAt.clear();
    this.lanes.clear();
    this.tracks.clear();
  }

  // The dashed magenta boundary, clamped to the ground. The zero mission of
  // an idle engine has an empty ring, and no boundary.
  private drawArea(polygon: Polygon): void {
    if (polygon === this.polygon) return;
    this.polygon = polygon;
    if (this.ao) this.source.entities.remove(this.ao);
    this.ao = undefined;
    const { ring } = polygon;
    if (ring.length < 3 || !ring[0]) return;
    // A mission's AO appearing brings the camera over it, once: the page
    // opened mid-mission, or a mission just started. Later mission frames
    // leave the camera where the operator put it.
    if (this.framed !== this.missionId) {
      this.framed = this.missionId;
      flyOver(this.viewer, ring);
    }
    this.ao = this.source.entities.add({
      id: 'ao',
      polyline: {
        positions: cartesians([...ring, ring[0]]),
        clampToGround: true,
        width: 2,
        material: material({ color: PALETTE.magenta, alpha: 0.9, width: 2, dashed: true }),
      },
    });
  }

  private drawStations(stations: readonly Station[]): void {
    if (stations === this.stations) return;
    this.stations = stations;
    for (const e of this.stationEntities) this.source.entities.remove(e);
    this.stationEntities.length = 0;
    this.stationAt.clear();
    for (const station of stations) {
      const at = cartesian(station.position);
      this.stationAt.set(station.name, at);
      this.stationEntities.push(
        this.source.entities.add({
          id: `station:${station.name}`,
          position: at,
          billboard: { image: SYMBOLS.station, width: MARKER_PX, height: MARKER_PX, color: Color.fromCssColorString(PALETTE.pale) },
          label: label(station.name),
        }),
      );
    }
  }

  private drawLanes(lanes: readonly LaneView[]): void {
    const seen = new Set<LaneID>();
    for (const lane of lanes) {
      seen.add(lane.id);
      const drawn = this.lanes.get(lane.id);
      // A lane object is replaced when its cursor or holder changes, and kept
      // otherwise (applyFrame), so identity says whether to redraw.
      if (drawn?.lane === lane) continue;
      const walked = drawn?.walked ?? this.line(`lane:${lane.id}:walked`);
      const ahead = drawn?.ahead ?? this.line(`lane:${lane.id}:ahead`);
      const legs = laneLegs(lane);
      const strokes = laneStrokes(lane);
      setLine(walked, legs.walked, strokes.walked);
      setLine(ahead, legs.ahead, strokes.ahead);
      this.lanes.set(lane.id, { lane, walked, ahead });
    }
    for (const [id, drawn] of this.lanes) {
      if (seen.has(id)) continue;
      this.source.entities.remove(drawn.walked);
      this.source.entities.remove(drawn.ahead);
      this.lanes.delete(id);
    }
  }

  private drawVectors(view: MissionView): void {
    const seen = new Set<VectorID>();
    for (const v of view.vectors) {
      const { id, position, last_seen_ms: seenMs, mode } = v.state;
      seen.add(id);
      const track = this.tracks.get(id) ?? this.addTrack(v);

      // Samples only move forward: a rejoin carrying an older time than the
      // last sample waits for the next telemetry.
      if (seenMs > track.lastMs) {
        track.samples.addSample(toJulian(seenMs), cartesian(position));
        track.lastMs = seenMs;
      }

      const station = nearestStation(view.stations, position);
      track.station = mode === 'down' || !station ? undefined : this.stationAt.get(station.name);
      const target = mode === 'down' ? undefined : nextWaypoint(view.lanes, id);
      track.target = target ? cartesian(target) : undefined;
      track.comms.show = track.station !== undefined;
      track.task.show = track.target !== undefined;

      const marker = vectorMarker(v);
      const style = `${marker.symbol}|${marker.color}|${marker.alpha}|${v.state.link}`;
      if (style !== track.style) {
        track.style = style;
        if (track.marker.billboard) {
          track.marker.billboard.image = new ConstantProperty(SYMBOLS[marker.symbol]);
          track.marker.billboard.color = new ConstantProperty(Color.fromCssColorString(marker.color).withAlpha(marker.alpha));
        }
        const comms = commsStroke(v.state.link);
        if (track.comms.polyline) track.comms.polyline.material = material(comms);
      }
    }
    for (const [id, track] of this.tracks) {
      if (seen.has(id)) continue;
      this.source.entities.remove(track.marker);
      this.source.entities.remove(track.comms);
      this.source.entities.remove(track.task);
      this.tracks.delete(id);
    }
  }

  private addTrack(v: VectorView): Track {
    const id = v.caps.id;
    const samples = new SampledPositionProperty();
    samples.forwardExtrapolationType = ExtrapolationType.HOLD;
    samples.backwardExtrapolationType = ExtrapolationType.HOLD;
    const here = (time?: JulianDate) => (time ? samples.getValue(time) : undefined);

    const marker = this.source.entities.add({
      id: `vector:${id}`,
      position: samples,
      billboard: { image: SYMBOLS.diamond, width: MARKER_PX, height: MARKER_PX },
      label: label(id),
      path: {
        leadTime: 0,
        trailTime: TRAIL_S,
        resolution: 1,
        width: 1.5,
        material: new ColorMaterialProperty(Color.fromCssColorString(PALETTE.cyan).withAlpha(0.8)),
      },
    });
    const track: Track = {
      marker,
      comms: this.line(`comms:${id}`),
      task: this.line(`task:${id}`),
      samples,
      lastMs: -Infinity,
      station: undefined,
      target: undefined,
      style: '',
    };
    if (track.comms.polyline) {
      track.comms.polyline.positions = new CallbackProperty((time) => endpoints(here(time), track.station), false);
    }
    if (track.task.polyline) {
      track.task.polyline.positions = new CallbackProperty((time) => endpoints(here(time), track.target), false);
      track.task.polyline.material = material({ color: PALETTE.magenta, alpha: 0.5, width: 1, dashed: true });
    }
    this.tracks.set(id, track);
    this.mark(id, track);
    const preloaded = this.preloaded.get(id);
    if (preloaded) addSamples(track, preloaded);
    return track;
  }

  // A straight line at the height of its points: lanes, links and tasks are
  // drawn where the vectors fly, above the clamped AO and basemap.
  private line(id: string): Entity {
    return this.source.entities.add({ id, polyline: { arcType: ArcType.NONE, width: 1 } });
  }
}

function addSamples(track: Track, samples: readonly Sample[]): void {
  for (const s of samples) {
    track.samples.addSample(toJulian(s.ms), cartesian(s.position));
    track.lastMs = Math.max(track.lastMs, s.ms);
  }
}

function endpoints(from: Cartesian3 | undefined, to: Cartesian3 | undefined): Cartesian3[] {
  return from && to ? [from, to] : [];
}

function setLine(entity: Entity, points: readonly Position[], stroke: Stroke): void {
  entity.show = points.length >= 2;
  if (!entity.polyline) return;
  entity.polyline.positions = new ConstantProperty(cartesians(points));
  entity.polyline.width = new ConstantProperty(stroke.width);
  entity.polyline.material = material(stroke);
}
