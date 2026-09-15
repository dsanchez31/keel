import { describe, expect, it, vi } from 'vitest';

import { type Outcome } from './generated/wire';
import { createClient, KeelApiError } from './rest';

function reply(status: number, body?: unknown, contentType = 'application/json'): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: body === undefined ? {} : { 'Content-Type': contentType },
  });
}

describe('createClient', () => {
  it('posts a compilation as JSON and returns the outcome', async () => {
    const outcome: Outcome = { backend: 'ollama', intent: 'sweep', attempts: [] };
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(200, outcome));
    const client = createClient({ baseUrl: 'http://keeld/api/v1/', fetch });

    await expect(client.compile({ intent: 'sweep' })).resolves.toEqual(outcome);
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('http://keeld/api/v1/plans');
    expect(init.method).toBe('POST');
    expect(init.body).toBe('{"intent":"sweep"}');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
  });

  it('escapes the path segments it is given', async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(204));
    await createClient({ fetch }).discard('a/b');
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/plans/a%2Fb');
  });

  it('sends a hot swap as {name, version} and reads no body from the 202', async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(202));
    await createClient({ fetch }).swapDoctrine('MSN-001', { name: 'recon-standard', version: '2.2.0' });
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/v1/missions/MSN-001/doctrine');
    expect(init.body).toBe('{"name":"recon-standard","version":"2.2.0"}');
  });

  it('asks for a replay window by its bounds in mission time', async () => {
    const window = { from_ms: 0, to_ms: 60000, frames: [], verified_ms: 60000 };
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(200, window));
    await expect(createClient({ fetch }).replayFrames('MSN-042', 0, 60000)).resolves.toEqual(window);
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/replays/MSN-042/frames?from=0&to=60000');
  });

  it('reads the world', async () => {
    const world = { name: 'reference', areas: [{ name: 'fog_of_war_east', polygon: { ring: [] }, cell_m: 50, scan_alt_m: 120, area_km2: 14.3 }], stations: [] };
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(200, world));
    await expect(createClient({ fetch }).world()).resolves.toEqual(world);
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/world');
  });

  it('reads the fleet', async () => {
    const fleet = [
      { id: 'DRONE-01', health: 'lost', detail: 'no frame yet; system 1 has no absolute position estimate' },
      { id: 'DRONE-02', health: 'ok' },
    ];
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(200, fleet));
    await expect(createClient({ fetch }).fleet()).resolves.toEqual(fleet);
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/fleet');
  });

  it('reads the pace and puts a new one as {speed}', async () => {
    const clock = { speed: 20, max_speed: 20 };
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(200, clock));
    const client = createClient({ fetch });
    await expect(client.clock()).resolves.toEqual(clock);
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/clock');
    await expect(client.setClock(20)).resolves.toEqual(clock);
    const [url, init] = fetch.mock.calls[1] as unknown as [string, RequestInit];
    expect(url).toBe('/api/v1/clock');
    expect(init.method).toBe('PUT');
    expect(init.body).toBe('{"speed":20}');
  });

  it('turns a problem into a KeelApiError carrying it, the outcome of a 502 included', async () => {
    const problem = { title: 'Bad Gateway', status: 502, detail: 'ollama did not answer', outcome: { backend: 'ollama', intent: 'x', attempts: [] } };
    const fetch = vi.fn<typeof globalThis.fetch>(async () => reply(502, problem, 'application/problem+json'));
    const err = await createClient({ fetch })
      .compile({ intent: 'x' })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(KeelApiError);
    expect((err as KeelApiError).status).toBe(502);
    expect((err as KeelApiError).message).toBe('ollama did not answer');
    expect((err as KeelApiError).problem?.outcome?.backend).toBe('ollama');
  });

  it('keeps the status of an error answer that is not a problem', async () => {
    const fetch = vi.fn<typeof globalThis.fetch>(async () => new Response('gateway down', { status: 503, statusText: 'Service Unavailable' }));
    const err = (await createClient({ fetch })
      .doctrines()
      .catch((e: unknown) => e)) as KeelApiError;
    expect(err.status).toBe(503);
    expect(err.problem).toBeUndefined();
    expect(err.message).toBe('Service Unavailable');
  });
});
