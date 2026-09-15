import type { CellID, LaneCursor, LaneView, MissionView, VectorState, VectorView } from './generated/wire';
import type { TypedFrame } from './stream';

/**
 * Folds one stream frame into the mission view (spec section 8.2): what
 * `GET /api/v1/missions/{id}` would answer after the same frames, held by a
 * client between two mission frames.
 *
 * - `mission` replaces the view. It starts every connection and comes again
 *   whenever a tick reshapes the mission, so nothing folded before it
 *   survives.
 * - `event`: a `vector_joined` adds the vector, or re-admits it, with the
 *   capabilities and initial state it carries. The telemetry frame of the
 *   same tick settles it: a join the engine refused is absent from it.
 * - `telemetry` holds every vector's state in id order: the vectors are the
 *   ones it lists, each keeping the capabilities it joined with.
 * - `coverage` marks its cells explored in the area's raster.
 * - `tick` carries the tick, its mission time, the log head, the mission
 *   state, the coverage count, every lane's cursor and, on the live stream,
 *   keeld's pace (a replay's frames carry none).
 * - `doctrine` carries the active pack's hash.
 * - `decision` changes nothing: a decision is a record, not state.
 *
 * A lane's `engaged` flag travels in mission frames only, so it is the one
 * field that can lag a tick behind; the assignee's mode, `scanning` once the
 * lane is engaged, is current in every telemetry frame.
 *
 * Pure: the view given is never mutated, and a frame that changes nothing
 * returns it unchanged. Before the first mission frame there is no view, and
 * any other frame leaves it undefined.
 */
export function applyFrame(view: MissionView | undefined, frame: TypedFrame): MissionView | undefined {
  if (frame.type === 'mission') return frame.data;
  if (!view) return undefined;
  switch (frame.type) {
    case 'event': {
      const { kind, caps, telemetry } = frame.data;
      return kind === 'vector_joined' && caps && telemetry ? withJoined(view, { caps, state: telemetry }) : view;
    }
    case 'telemetry':
      return withTelemetry(view, frame.data);
    case 'coverage':
      return withExplored(view, frame.data.cells);
    case 'tick': {
      const { tick, head, state, explored, total, cursors, speed } = frame.data;
      const mission = state === view.mission.state ? view.mission : { ...view.mission, state };
      return withSpeed({ ...view, tick, tick_ms: frame.t, head, mission, explored, total, lanes: withCursors(view.lanes, cursors) }, speed);
    }
    case 'doctrine':
      return frame.data.hash === view.doctrine_hash ? view : { ...view, doctrine_hash: frame.data.hash };
    case 'decision':
      return view;
  }
}

function withSpeed(view: MissionView, speed: number | undefined): MissionView {
  if (speed === view.speed) return view;
  if (speed === undefined) {
    const { speed: _dropped, ...rest } = view;
    return rest;
  }
  return { ...view, speed };
}

function withJoined(view: MissionView, joined: VectorView): MissionView {
  const id = joined.caps.id;
  const vectors = view.vectors.filter((v) => v.caps.id !== id);
  vectors.push(joined);
  vectors.sort((a, b) => (a.caps.id < b.caps.id ? -1 : a.caps.id > b.caps.id ? 1 : 0));
  return { ...view, vectors };
}

// A state of a vector the view never saw join is left out: a connection
// starts from a snapshot and every join after it arrives as an event before
// the telemetry of its tick, so there is no such state on a stream that keeps
// its contract, and no capabilities to give it.
function withTelemetry(view: MissionView, states: readonly VectorState[]): MissionView {
  const caps = new Map(view.vectors.map((v) => [v.caps.id, v.caps]));
  const vectors: VectorView[] = [];
  for (const state of states) {
    const c = caps.get(state.id);
    if (c) vectors.push({ caps: c, state });
  }
  return { ...view, vectors };
}

// Coverage is monotonic (I2): a cell is only ever marked. One outside the
// raster, which a stream keeping its contract never sends, is ignored.
function withExplored(view: MissionView, cells: readonly CellID[]): MissionView {
  const { area } = view.mission;
  const explored = area.grid.explored.slice();
  for (const cell of cells) {
    if (cell >= 0 && cell < explored.length) explored[cell] = true;
  }
  return { ...view, mission: { ...view.mission, area: { ...area, grid: { ...area.grid, explored } } } };
}

function withCursors(lanes: readonly LaneView[], cursors: readonly LaneCursor[]): readonly LaneView[] {
  const byLane = new Map(cursors.map((c) => [c.lane, c.cursor]));
  let changed = false;
  const next = lanes.map((lane) => {
    const cursor = byLane.get(lane.id);
    if (cursor === undefined || cursor === lane.cursor) return lane;
    changed = true;
    return { ...lane, cursor };
  });
  return changed ? next : lanes;
}
