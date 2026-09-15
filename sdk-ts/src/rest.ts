import type {
  Approval,
  ClockView,
  CompileRequest,
  DoctrineInfo,
  DoctrineRef,
  Fault,
  FleetVectorView,
  MissionID,
  MissionView,
  Outcome,
  PlanHash,
  Problem,
  ReplayWindow,
  WorldView,
} from './generated/wire';

/**
 * A request keeld answered with an error: its status and, when it sent one,
 * the RFC 9457 problem (spec section 8.1). A backend failure during
 * compilation (502) carries the attempts made before it in `problem.outcome`.
 */
export class KeelApiError extends Error {
  readonly status: number;
  readonly problem: Problem | undefined;

  constructor(status: number, problem: Problem | undefined, statusText: string) {
    super(problem?.detail ?? problem?.title ?? (statusText || `HTTP ${status}`));
    this.name = 'KeelApiError';
    this.status = status;
    this.problem = problem;
  }
}

export interface ClientOptions {
  /** Where keeld's API is, `/api/v1` of the page's own origin by default. */
  baseUrl?: string;
  /** The fetch to use, the global one by default. */
  fetch?: typeof fetch;
}

/** What a request may carry beside its body. */
export interface RequestOptions {
  signal?: AbortSignal;
}

/**
 * keeld's REST API (spec section 8.1), one method per route. Approval, hot
 * swap and fault injection are accepted, not applied: their result is a
 * decision on the stream. A mission's recorded log is read by the replay
 * client, not here.
 */
export interface KeelClient {
  /** Compiles intent. A compilation refused after the repair budget is an outcome, not an error. */
  compile(req: CompileRequest, opts?: RequestOptions): Promise<Outcome>;
  /** The human gate: the pending plan runs at the engine's next tick. */
  approve(plan: PlanHash, opts?: RequestOptions): Promise<Approval>;
  /** Drops a pending plan. */
  discard(plan: PlanHash, opts?: RequestOptions): Promise<void>;
  /** The mission view of a running or finished mission. */
  mission(id: MissionID, opts?: RequestOptions): Promise<MissionView>;
  /** Hot swaps the running mission's pack at the next tick boundary. */
  swapDoctrine(id: MissionID, ref: DoctrineRef, opts?: RequestOptions): Promise<void>;
  /** The registered packs, by name then version. */
  doctrines(opts?: RequestOptions): Promise<readonly DoctrineInfo[]>;
  /** The world keeld loaded: its areas and stations, the names an intent may use. */
  world(opts?: RequestOptions): Promise<WorldView>;
  /**
   * Every bound vector's health now, in id order: its adapter's verdict and
   * why, those that have not joined yet included. Not engine state: the
   * stream never carries it and the log never records it.
   */
  fleet(opts?: RequestOptions): Promise<readonly FleetVectorView[]>;
  /** Injects a fault into the simulator serving its vector. */
  injectFault(fault: Fault, opts?: RequestOptions): Promise<void>;
  /**
   * A window of a finished mission, replayed and verified by keeld: stream
   * frames from a snapshot at `fromMs` to the tick at `toMs`, whole
   * milliseconds of mission time at most 30 minutes apart.
   */
  replayFrames(id: MissionID, fromMs: number, toMs: number, opts?: RequestOptions): Promise<ReplayWindow>;
  /** keeld's pace: how many times faster than real time the fleet moves, and why it cannot change when it cannot. */
  clock(opts?: RequestOptions): Promise<ClockView>;
  /**
   * Sets the pace of keeld and of every simulator of its fleet, a whole
   * number in [1, max_speed], 1 being real time. Answered once in force.
   */
  setClock(speed: number, opts?: RequestOptions): Promise<ClockView>;
}

export function createClient({ baseUrl = '/api/v1', fetch: doFetch = globalThis.fetch.bind(globalThis) }: ClientOptions = {}): KeelClient {
  const base = baseUrl.replace(/\/+$/, '');

  async function send(method: string, path: string, body: unknown, opts: RequestOptions | undefined): Promise<Response> {
    const init: RequestInit = { method, credentials: 'same-origin' };
    if (opts?.signal) init.signal = opts.signal;
    if (body !== undefined) {
      init.headers = { 'Content-Type': 'application/json' };
      init.body = JSON.stringify(body);
    }
    const res = await doFetch(`${base}${path}`, init);
    if (!res.ok) throw new KeelApiError(res.status, await problemOf(res), res.statusText);
    return res;
  }

  const json = async <T>(method: string, path: string, body: unknown, opts: RequestOptions | undefined): Promise<T> =>
    (await (await send(method, path, body, opts)).json()) as T;

  const none = async (method: string, path: string, body: unknown, opts: RequestOptions | undefined): Promise<void> => {
    await send(method, path, body, opts);
  };

  const seg = encodeURIComponent;

  return {
    compile: (req, opts) => json('POST', '/plans', req, opts),
    approve: (plan, opts) => json('POST', `/plans/${seg(plan)}/approve`, undefined, opts),
    discard: (plan, opts) => none('DELETE', `/plans/${seg(plan)}`, undefined, opts),
    mission: (id, opts) => json('GET', `/missions/${seg(id)}`, undefined, opts),
    swapDoctrine: (id, ref, opts) => none('POST', `/missions/${seg(id)}/doctrine`, { name: ref.name, version: ref.version }, opts),
    doctrines: (opts) => json('GET', '/doctrine', undefined, opts),
    world: (opts) => json('GET', '/world', undefined, opts),
    fleet: (opts) => json('GET', '/fleet', undefined, opts),
    injectFault: (fault, opts) => none('POST', '/faults', fault, opts),
    replayFrames: (id, fromMs, toMs, opts) =>
      json('GET', `/replays/${seg(id)}/frames?${new URLSearchParams({ from: String(fromMs), to: String(toMs) })}`, undefined, opts),
    clock: (opts) => json('GET', '/clock', undefined, opts),
    setClock: (speed, opts) => json('PUT', '/clock', { speed } satisfies Pick<ClockView, 'speed'>, opts),
  };
}

/** The problem an error answer carries, if it is one. */
async function problemOf(res: Response): Promise<Problem | undefined> {
  if (!(res.headers.get('Content-Type') ?? '').startsWith('application/problem+json')) return undefined;
  try {
    return (await res.json()) as Problem;
  } catch {
    return undefined;
  }
}
