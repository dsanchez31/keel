import { type ClockView, type KeelClient } from '@keel/sdk';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './api';

/**
 * keeld's pace as the live strip drives it. The pace in force is not here:
 * it travels on the stream, in every tick (spec section 8.2), so every page
 * shows the pace keeld runs at, whoever set it. This holds what the stream
 * does not carry: the fastest pace keeld accepts, why the pace stays real
 * time when it must, and the request in flight.
 */
export interface LiveClock {
  /** The fastest pace keeld accepts; 1 until it has answered. */
  maxSpeed: number;
  /** Why the pace stays real time: a vector no simulator runs, a loop not paced in real time. */
  fixed: string | undefined;
  /** The pace asked for and not answered yet. */
  pending: number | undefined;
  /** keeld's refusal of the last pace asked for, until the next. */
  error: Error | null;
  set(speed: number): void;
}

const CLOCK_KEY = ['clock'] as const;

export function useLiveClock(client: KeelClient = api): LiveClock {
  const queryClient = useQueryClient();
  const clock = useQuery({ queryKey: CLOCK_KEY, queryFn: ({ signal }) => client.clock({ signal }) });
  const set = useMutation({
    mutationFn: (speed: number) => client.setClock(speed),
    onSuccess: (view: ClockView) => queryClient.setQueryData(CLOCK_KEY, view),
  });
  return {
    maxSpeed: clock.data?.max_speed ?? 1,
    fixed: clock.data?.fixed,
    pending: set.isPending ? set.variables : undefined,
    error: set.error,
    set: (speed) => set.mutate(speed),
  };
}
