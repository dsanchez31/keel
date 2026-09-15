import { type FleetVectorView, type KeelClient, type MissionView } from '@keel/sdk';
import { useQuery } from '@tanstack/react-query';

import { api } from './api';
import { type MissionSource } from './missionStore';

const FLEET_KEY = ['fleet'] as const;

/** How often the fleet is read again while a bound vector has not joined. */
export const FLEET_POLL_MS = 1000;

/**
 * Every bound vector's health (GET /api/v1/fleet), in id order: its adapter's
 * verdict and why, the vectors that have not joined yet included (an
 * autopilot waiting for its position estimate, a simulator not dialled yet).
 * The stream only knows a vector once it joins, so the route is read again
 * every second until keeld has answered and every vector it lists is in the
 * store's view, and no more once the whole fleet has joined. The view is read
 * when a poll answers, not subscribed to: the screen does not render again at
 * every tick for it.
 */
export function useFleet(store: MissionSource, client: KeelClient = api): readonly FleetVectorView[] {
  const fleet = useQuery({
    queryKey: FLEET_KEY,
    queryFn: ({ signal }) => client.fleet({ signal }),
    refetchInterval: (query) => {
      const data = query.state.data;
      return data === undefined || waiting(data, store.get()).length > 0 ? FLEET_POLL_MS : false;
    },
  });
  return fleet.data ?? [];
}

/** The vectors of the fleet the view does not hold yet. */
export function waiting(fleet: readonly FleetVectorView[], view: MissionView | undefined): readonly FleetVectorView[] {
  const joined = new Set(view?.vectors.map((v) => v.caps.id));
  return fleet.filter((v) => !joined.has(v.id));
}
