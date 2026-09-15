import { EarthRadiusM, type LaneView, type LinkState, type Position, type Station, type VectorID, type VectorView } from '@keel/sdk';

/**
 * What the globe draws of a mission view, decided apart from Cesium: which
 * lane a vector works, how far along it is, which station its radio reaches
 * for, and how each is styled. missionScene.ts turns it into entities.
 *
 * The globe draws the orchestrator's view and nothing else: what keeld knows.
 * A blackout zone is simulator physics the engine never sees (spec section
 * 15.2), so the operator sees a link lost, not the zone that lost it; KEEL
 * models no detection.
 */

/** Colours as CSS strings, the tactical palette of design section 8.3. */
export const PALETTE = {
  /** Air tracks and the AO boundary. */
  magenta: '#ff2bd6',
  /** Trajectories. */
  cyan: '#22d3ee',
  /** Lanes, stations, labels. */
  pale: '#e5e7eb',
  /** Returning to base, a degraded link, a lane waiting for a vector. */
  amber: '#f59e0b',
  /** A lost link. */
  red: '#f87171',
  /** A healthy link. */
  green: '#34d399',
  /** A vector down. */
  grey: '#6b7280',
} as const;

export interface Stroke {
  color: string;
  alpha: number;
  width: number;
  dashed: boolean;
}

/**
 * The lane a vector drives: the lowest-index lane it holds with a waypoint
 * left (spec section 4.4). Its other lanes are queued behind it.
 */
export function activeLane(lanes: readonly LaneView[], id: VectorID): LaneView | undefined {
  let active: LaneView | undefined;
  for (const lane of lanes) {
    if (lane.assigned_to !== id || lane.cursor >= lane.waypoints.length) continue;
    if (!active || lane.index < active.index) active = lane;
  }
  return active;
}

/** The waypoint a vector is heading for on its active lane. */
export function nextWaypoint(lanes: readonly LaneView[], id: VectorID): Position | undefined {
  const lane = activeLane(lanes, id);
  return lane?.waypoints[lane.cursor];
}

/**
 * A lane split at its cursor: the path walked, up to the last waypoint
 * reached, and the path ahead, from that waypoint on so the two meet. A leg
 * with fewer than two points draws nothing.
 */
export function laneLegs(lane: LaneView): { walked: readonly Position[]; ahead: readonly Position[] } {
  const reached = Math.min(Math.max(lane.cursor, 0), lane.waypoints.length);
  return {
    walked: lane.waypoints.slice(0, reached),
    ahead: lane.waypoints.slice(Math.max(reached - 1, 0)),
  };
}

/** Great-circle distance in metres, haversine on the engine's Earth radius. */
export function distanceM(a: Position, b: Position): number {
  const rad = Math.PI / 180;
  const dLat = (b.lat - a.lat) * rad;
  const dLon = (b.lon - a.lon) * rad;
  const h = Math.sin(dLat / 2) ** 2 + Math.cos(a.lat * rad) * Math.cos(b.lat * rad) * Math.sin(dLon / 2) ** 2;
  return 2 * EarthRadiusM * Math.asin(Math.min(1, Math.sqrt(h)));
}

/**
 * The station a vector's radio reaches for: the nearest, as the simulator's
 * link model judges range (spec section 15.2), ties to the first by name.
 */
export function nearestStation(stations: readonly Station[], at: Position): Station | undefined {
  let best: Station | undefined;
  let bestM = Infinity;
  for (const station of stations) {
    const m = distanceM(station.position, at);
    if (m < bestM || (m === bestM && best && station.name < best.name)) {
      best = station;
      bestM = m;
    }
  }
  return best;
}

/**
 * A vector's marker: a diamond for an air track, a square for a ground one;
 * magenta at work, amber returning to base, grey down; dimmed while its link
 * is lost, since the position shown is the last one heard.
 */
export function vectorMarker(v: VectorView): { symbol: 'diamond' | 'square'; color: string; alpha: number } {
  const { mode, link } = v.state;
  const color = mode === 'down' ? PALETTE.grey : mode === 'rtb' ? PALETTE.amber : PALETTE.magenta;
  return { symbol: v.caps.domain === 'aerial' ? 'diamond' : 'square', color, alpha: link === 'lost' ? 0.45 : 1 };
}

/** The line from a vector to its station, styled by the link's state. */
export function commsStroke(link: LinkState): Stroke {
  switch (link) {
    case 'ok':
      return { color: PALETTE.green, alpha: 0.35, width: 1, dashed: false };
    case 'degraded':
      return { color: PALETTE.amber, alpha: 0.6, width: 1, dashed: true };
    case 'lost':
      return { color: PALETTE.red, alpha: 0.6, width: 1, dashed: true };
  }
}

/** A lane's two legs: the path ahead bright, the path walked faint, a lane nobody holds dashed amber. */
export function laneStrokes(lane: LaneView): { walked: Stroke; ahead: Stroke } {
  if (!lane.assigned_to) {
    const pending: Stroke = { color: PALETTE.amber, alpha: 0.7, width: 1.5, dashed: true };
    return { walked: { ...pending, alpha: 0.2 }, ahead: pending };
  }
  return {
    walked: { color: PALETTE.pale, alpha: 0.15, width: 1, dashed: false },
    ahead: { color: PALETTE.pale, alpha: 0.55, width: 1.5, dashed: false },
  };
}
