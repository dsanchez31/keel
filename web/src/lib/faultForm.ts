import { type Fault, type FaultKind, TickIntervalMs, type VectorID } from '@keel/sdk';

/** What each kind of fault needs, and what it does to the simulated vehicle (spec section 15.2). */
export interface FaultKindSpec {
  kind: FaultKind;
  label: string;
  effect: string;
  /** The magnitude's unit when the kind takes one; it must then be positive. */
  magnitude?: 'percent' | 'm/s';
  /** Whether the kind lasts for a duration, 0 meaning the rest of the run. */
  duration: boolean;
}

export const FAULT_KINDS: readonly FaultKindSpec[] = [
  { kind: 'kill', label: 'Kill', effect: 'No frame leaves the vehicle and no command reaches it.', duration: false },
  { kind: 'link_loss', label: 'Link loss', effect: 'The link is cut both ways.', duration: true },
  { kind: 'battery_drain', label: 'Battery drain', effect: 'The charge drops at once by the magnitude.', magnitude: 'percent', duration: false },
  { kind: 'gps_drift', label: 'GPS drift', effect: 'The reported position walks away, and recovers when the fault ends.', magnitude: 'm/s', duration: true },
  { kind: 'stale_telemetry', label: 'Stale telemetry', effect: 'The vehicle repeats its last frame: a stuck sensor.', duration: true },
];

export const faultKind = (kind: FaultKind): FaultKindSpec => FAULT_KINDS.find((k) => k.kind === kind) ?? FAULT_KINDS[0]!;

/** The form as typed: the magnitude in its unit, the duration in seconds, both as text. */
export interface FaultInput {
  vector: VectorID;
  kind: FaultKind;
  magnitude: string;
  durationS: string;
}

/**
 * The fault the form describes, or what is wrong with it, held to the rule
 * keeld applies to every fault (spec section 8.1): a positive magnitude for a
 * battery drain or a GPS drift, a duration of zero or whole ticks. Seconds
 * are rounded to the nearest tick, so 2.34 s is 2.3 s. keeld checks the rule
 * again; this only spares a round trip.
 */
export function faultRequest(input: FaultInput): { fault: Fault } | { error: string } {
  const spec = faultKind(input.kind);
  let magnitude: number | undefined;
  if (spec.magnitude) {
    magnitude = Number(input.magnitude);
    if (input.magnitude.trim() === '' || !Number.isFinite(magnitude) || magnitude <= 0) {
      return { error: `${spec.label} needs a positive magnitude in ${spec.magnitude}.` };
    }
  }
  let durationMs: number | undefined;
  if (spec.duration) {
    const seconds = input.durationS.trim() === '' ? 0 : Number(input.durationS);
    if (!Number.isFinite(seconds) || seconds < 0) return { error: 'The duration is a number of seconds, 0 for the rest of the run.' };
    durationMs = Math.round((seconds * 1000) / TickIntervalMs) * TickIntervalMs;
  }
  return {
    fault: {
      vector: input.vector,
      kind: input.kind,
      ...(magnitude !== undefined && { magnitude }),
      ...(durationMs !== undefined && { duration_ms: durationMs }),
    },
  };
}
