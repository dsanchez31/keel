import { type KeelClient, type WorldView } from '@keel/sdk';
import { useQuery } from '@tanstack/react-query';

import { api } from './api';

/** The world keeld loaded, or why it could not be read. */
export interface World {
  view: WorldView | undefined;
  error: Error | null;
}

const WORLD_KEY = ['world'] as const;

/**
 * The world's areas and stations (GET /api/v1/world), the names an intent
 * may use. keeld loads its world file once, at startup, so it is read once
 * per page and never refetched.
 */
export function useWorld(client: KeelClient = api): World {
  const world = useQuery({ queryKey: WORLD_KEY, queryFn: ({ signal }) => client.world({ signal }), staleTime: Infinity });
  return { view: world.data, error: world.error };
}

/** The example intent the assistant shows: the reference one, or one naming this world's first area and station. */
export function exampleIntent(view: WorldView | undefined): string {
  const area = view?.areas[0]?.name ?? 'fog_of_war_east';
  const station = view?.stations[0]?.name ?? 'gcs-west';
  return `Sweep ${area} with every camera drone, return to ${station}`;
}
