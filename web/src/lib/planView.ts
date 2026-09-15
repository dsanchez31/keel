import { type ApprovedPlan, type Assignment, type LaneID, type Plan, type VectorID } from '@keel/sdk';

/** A hash as the panels print it: its first 12 hex digits. */
export const shortHash = (hash: string): string => hash.slice(0, 12);

/** One row of the per-lane breakdown the operator approves. */
export interface LaneRow {
  id: LaneID;
  index: number;
  /** Waypoints of its boustrophedon path. */
  steps: number;
  cells: number;
  /** The vector the allocator projects onto it, none when no vector could take it. */
  vector: VectorID | undefined;
  rationale: string | undefined;
}

/** The expanded plan's lanes in index order, each with its projected assignment. */
export function laneRows(plan: ApprovedPlan, assignments: readonly Assignment[] = []): LaneRow[] {
  const byLane = new Map(assignments.map((a) => [a.lane, a]));
  return [...plan.lanes]
    .sort((a, b) => a.index - b.index)
    .map((lane) => {
      const a = byLane.get(lane.id);
      return { id: lane.id, index: lane.index, steps: lane.waypoints.length, cells: lane.cells.length, vector: a?.winner, rationale: a?.rationale };
    });
}

/** The Plan IR the model wrote, as the source view shows it. */
export const irSource = (ir: Plan): string => JSON.stringify(ir, null, 2);

const RASTERS = new Set(['in_ao', 'explored']);

/**
 * The expanded plan the hash covers, as the source view shows it: every field
 * as keeld sent it, but the AO raster's two bitmaps, a boolean per cell,
 * replaced by their length. The hash still covers them.
 */
export function expandedSource(plan: ApprovedPlan): string {
  return JSON.stringify(
    plan,
    (key, value: unknown) => (RASTERS.has(key) && Array.isArray(value) ? `<${value.length} cells, elided>` : value),
    2,
  );
}
