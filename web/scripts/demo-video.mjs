// Records the demo video through the real UI, against a running stack.
//
//   make docker-up        # in another terminal, and no mission running
//   make demo-video       # or: pnpm --filter @keel/web demo-video [--base URL] [--headed]
//
// It drives the live screen the way an operator does (the intent typed, the
// plan compiled by the local model, the launch gate, the fleet at ×20, a link
// loss on DRONE-02, coverage to completion, back to real time) then the
// replay, until the page, keeld's recording and keeld's replay agree on one
// head, and closes on the architecture card (demo-architecture.html).
//
// Capture: the Chrome DevTools screencast of the page, JPEG frames piped as
// they come into the system ffmpeg, which stamps them with the wall clock and
// writes a constant 30 fps intermediate. The long stretches of the ×20
// mission are then cut down in an edit list, under an on-screen time-lapse
// badge, so the video is about two minutes: nothing is sped up without
// saying so. The system Chrome (channel "chrome") and ffmpeg are used,
// nothing is downloaded.
//
// Outputs: run/demo/keel-demo.mp4 (not versioned, published elsewhere),
// run/demo/marks.json (the chapters on the raw recording), and
// docs/media/demo.gif, the README's loop: the fault and the fleet taking over,
// at ×20.

import { spawn } from 'node:child_process';
import { mkdir, readFile, stat, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, '..', '..');
const OUT_DIR = join(ROOT, 'run', 'demo');
const RAW = join(OUT_DIR, 'raw.mp4');
const VIDEO = join(OUT_DIR, 'keel-demo.mp4');
const MARKS = join(OUT_DIR, 'marks.json');
const GIF = join(ROOT, 'docs', 'media', 'demo.gif');
const CARD = join(HERE, 'demo-architecture.html');
const FONT = join(HERE, '..', 'node_modules', '@fontsource-variable', 'geist-mono', 'files', 'geist-mono-latin-wght-normal.woff2');

const WIDTH = 1920;
const HEIGHT = 1080;
/** The globe's centre on the page: between the 22rem and 26rem columns, above the strip. */
const GLOBE_X = 352 + (WIDTH - 352 - 416) / 2;
const GLOBE_Y = (HEIGHT - 48) / 2;
const FPS = 30;
/** The length the edit list aims for, in seconds. */
const TARGET_S = 120;
/** The README loop: under this, or it is cut smaller. */
const GIF_MAX_BYTES = 8 * 1024 * 1024;

const INTENT = 'Sweep fog_of_war_east with every camera drone, return to gcs-west';
const FLEET = 6;
const FAST = 20;
/** Coverage at which DRONE-02 loses its link, percent, and for how long. */
const FAULT_AT_PCT = 25;
/**
 * A minute: the fleet redistributes DRONE-02's lane, then takes it back. Lost
 * for the rest of the run this early, with DRONE-01 (a SITL on ArduPilot's own
 * battery model) home on low battery, the two drones left lack the range for
 * what remains, and the engine rightly fails the mission on its stall rule.
 */
const FAULT_DURATION_S = 60;

const { values: opts } = parseArgs({
  options: {
    base: { type: 'string', default: 'http://localhost:5173' },
    headed: { type: 'boolean', default: false },
  },
});
const base = opts.base.replace(/\/+$/, '');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

async function api(method, path, body) {
  const res = await fetch(`${base}/api/v1${path}`, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return text ? JSON.parse(text) : undefined;
}

/** The first frame of the stream: the mission view keeld holds now. */
function snapshot() {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${base.replace(/^http/, 'ws')}/api/v1/stream`);
    const timer = setTimeout(() => reject(new Error('no snapshot from the stream within 10 s')), 10_000);
    ws.onmessage = (e) => {
      clearTimeout(timer);
      ws.close();
      resolve(JSON.parse(e.data).data);
    };
    ws.onerror = () => reject(new Error(`the stream at ${base} is not reachable: is make docker-up running?`));
  });
}

/** The stack is up, idle, in real time, can change pace, and its fleet is fresh. */
async function preconditions() {
  const clock = await api('GET', '/clock').catch((e) => {
    throw new Error(`keeld is not reachable through ${base}: is make docker-up running? (${e.message})`);
  });
  if (clock.fixed) throw new Error(`keeld cannot change pace: ${clock.fixed}`);
  if (clock.max_speed < FAST) throw new Error(`keeld's fastest pace is ×${clock.max_speed}, the demo runs at ×${FAST}`);
  const view = await snapshot();
  if (view.mission?.state === 'running') throw new Error(`${view.mission.id} is running: let it end, or restart the stack (make docker-down, make docker-up)`);
  // The simulators keep their batteries from one mission to the next, and a
  // drained fleet gets no plan past the validator: nothing to film.
  const drained = view.vectors.filter((v) => v.state.battery_pct < 95).map((v) => `${v.caps.id} at ${v.state.battery_pct} %`);
  if (drained.length) throw new Error(`the fleet has flown already (${drained.join(', ')}): restart the stack (docker compose down, make docker-up)`);
  if (clock.speed !== 1) await api('PUT', '/clock', { speed: 1 });
}

function run(cmd, args, input) {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { stdio: [input ? 'pipe' : 'ignore', 'ignore', 'pipe'] });
    let err = '';
    p.stderr.on('data', (d) => (err = (err + d).slice(-4000)));
    p.on('error', reject);
    p.on('close', (code) => (code === 0 ? resolve() : reject(new Error(`${cmd} exited ${code}: ${err}`))));
    if (input) input(p.stdin);
  });
}

/**
 * The screencast piped into ffmpeg. Frames come when the page paints, so
 * ffmpeg stamps each with the wall clock it arrives at and fills the gaps to
 * a constant rate. `mark` returns seconds on that timeline.
 */
async function startCapture(page) {
  const cdp = await page.context().newCDPSession(page);
  const ffmpeg = spawn(
    'ffmpeg',
    ['-y', '-loglevel', 'error', '-use_wallclock_as_timestamps', '1', '-f', 'image2pipe', '-c:v', 'mjpeg', '-i', '-',
      '-vf', `fps=${FPS},scale=${WIDTH}:${HEIGHT}:flags=lanczos,format=yuv420p`, '-c:v', 'libx264', '-preset', 'veryfast', '-crf', '14', RAW],
    { stdio: ['pipe', 'ignore', 'inherit'] },
  );
  const done = new Promise((resolve, reject) => {
    ffmpeg.on('error', reject);
    ffmpeg.on('close', (code) => (code === 0 ? resolve() : reject(new Error(`ffmpeg exited ${code}`))));
  });
  let t0;
  let last;
  cdp.on('Page.screencastFrame', ({ data, sessionId }) => {
    t0 ??= Date.now();
    last = Buffer.from(data, 'base64');
    ffmpeg.stdin.write(last);
    cdp.send('Page.screencastFrameAck', { sessionId }).catch(() => {});
  });
  await cdp.send('Page.startScreencast', { format: 'jpeg', quality: 92, maxWidth: WIDTH, maxHeight: HEIGHT, everyNthFrame: 1 });
  while (t0 === undefined) await sleep(20);
  return {
    mark: () => (Date.now() - t0) / 1000,
    async stop() {
      await cdp.send('Page.stopScreencast');
      // A page that no longer paints sends no frame (the closing card): the
      // last one again, stamped now, holds it until the end instead of the
      // recording ending on its first frame.
      if (last) ffmpeg.stdin.write(last);
      ffmpeg.stdin.end();
      await done;
    },
  };
}

/** The caption overlay, over the globe's top edge, and the time-lapse badge. */
async function installOverlay(page) {
  await page.addStyleTag({
    content: `
      #demo-caption { position: fixed; top: 20px; left: calc(22rem + (100vw - 48rem) / 2); transform: translateX(-50%);
        max-width: min(60rem, calc(100vw - 50rem)); z-index: 9999; pointer-events: none;
        font: 500 22px/1.35 var(--font-mono, monospace); color: #e6f7ff; text-align: center;
        background: rgba(4, 8, 12, 0.86); border: 1px solid rgba(34, 211, 238, 0.55); border-radius: 6px;
        padding: 12px 20px; box-shadow: 0 8px 30px rgba(0, 0, 0, 0.5); transition: opacity 250ms; }
      #demo-caption small { display: block; margin-top: 4px; font-size: 16px; color: #8fb3c2; }
      #demo-caption:empty { opacity: 0; }
      #demo-lapse { position: fixed; bottom: 68px; right: calc(26rem + 20px); z-index: 9999; pointer-events: none;
        font: 600 15px/1 var(--font-mono, monospace); letter-spacing: 0.12em; text-transform: uppercase;
        color: #0b0f14; background: #fbbf24; border-radius: 4px; padding: 8px 12px; }
    `,
  });
  await page.evaluate(() => {
    for (const id of ['demo-caption', 'demo-lapse']) {
      const el = document.createElement('div');
      el.id = id;
      if (id === 'demo-lapse') el.hidden = true;
      document.body.append(el);
    }
  });
}

const caption = (page, text, sub) =>
  page.evaluate(
    ([t, s]) => {
      const el = document.getElementById('demo-caption');
      el.textContent = t;
      if (s) {
        const small = document.createElement('small');
        small.textContent = s;
        el.append(small);
      }
    },
    [text, sub ?? ''],
  );

const lapse = (page, on) =>
  page.evaluate((show) => {
    const el = document.getElementById('demo-lapse');
    el.textContent = 'Time-lapse: video sped up';
    el.hidden = !show;
  }, on);

/** Coverage from the time strip's readout, percent. */
const coverage = (page) =>
  page.evaluate(() => {
    const el = document.querySelector('[title$="cells of the AO explored"]');
    const m = el?.textContent?.match(/([\d.]+) %/);
    return m ? Number(m[1]) : 0;
  });

async function until(what, cond, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (!(await cond())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(250);
  }
}

async function record() {
  const browser = await chromium.launch({
    channel: 'chrome',
    headless: !opts.headed,
    // Hardware WebGL through ANGLE on Vulkan: SwiftShader, the fallback,
    // draws the globe at a few frames a second.
    args: ['--enable-gpu', '--ignore-gpu-blocklist', '--use-angle=vulkan', '--enable-features=Vulkan', `--window-size=${WIDTH},${HEIGHT}`],
  });
  const context = await browser.newContext({ viewport: { width: WIDTH, height: HEIGHT }, deviceScaleFactor: 1, colorScheme: 'dark' });
  const page = await context.newPage();
  const marks = {};
  let capture;
  const mark = (name) => {
    marks[name] = capture.mark();
    log(`${name} at ${marks[name].toFixed(1)} s`);
  };
  try {
    const renderer = await page.evaluate(() => {
      const gl = document.createElement('canvas').getContext('webgl2');
      const ext = gl?.getExtension('WEBGL_debug_renderer_info');
      return gl ? (ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER)) : 'none';
    });
    log(`WebGL renderer: ${renderer}`);

    await page.goto(base);
    const roster = page.getByRole('region', { name: 'Vectors' }).getByRole('button');
    log(`waiting for the fleet of ${FLEET}`);
    await until('the whole fleet in the roster', async () => (await roster.count()) >= FLEET, 5 * 60_000);
    await installOverlay(page);
    // The page brings the camera over the world's areas and stations once
    // it has read them (WorldAreas): the fleet on its pad by gcs-west.
    await page.getByRole('group', { name: 'Areas' }).waitFor({ timeout: 30_000 });
    await sleep(2500);

    capture = await startCapture(page);
    mark('start');
    await caption(page, 'Six vehicles live: four simulated by keelsim, two ArduPilot SITL autopilots', 'KEEL, the orchestrator, sees them through one contract');
    await sleep(5000);

    const intent = page.getByPlaceholder(INTENT);
    await caption(page, 'The operator states intent in plain language', 'Area and station by name, never a coordinate');
    await intent.click();
    await intent.pressSequentially(INTENT, { delay: 35 });
    await sleep(600);
    mark('typed');
    await intent.press('Enter');
    await caption(page, 'A local model (qwen3:14b on Ollama) compiles it into a plan', 'It picks the tactic; the validator checks schema, names, feasibility and doctrine');
    const approve = page.getByRole('button', { name: 'Approve and launch' });
    await approve.waitFor({ timeout: 5 * 60_000 });
    mark('compiled');
    await caption(page, 'The plan: its rationale, lanes computed by code, the geometry drawn for review', 'Nothing moves before a human approves what they see');
    await sleep(8000);

    await approve.click();
    mark('approved');
    await caption(page, 'Approved. Below the fence the engine is a pure, seeded function', 'Same events, same decisions, every one hash-chained');
    await sleep(6000);

    await page.getByRole('button', { name: `×${FAST}` }).click();
    await page.getByText(`Accelerated ×${FAST}`).waitFor({ timeout: 15_000 });
    mark('fast');
    await caption(page, `×${FAST}: keeld, keelsim and both SITL change pace together`, 'The pace decides when a tick runs, never what it decides');
    await sleep(7000);

    await caption(page, 'The fog of war lifts as the lanes are scanned');
    await lapse(page, true);
    mark('lapse1On');
    await until(`${FAULT_AT_PCT} % coverage`, async () => (await coverage(page)) >= FAULT_AT_PCT, 10 * 60_000);
    await lapse(page, false);
    mark('lapse1Off');

    await roster.filter({ hasText: 'DRONE-02' }).click();
    await caption(page, `DRONE-02 loses its link for ${FAULT_DURATION_S} s`, 'A fault injected into the simulator; the engine only sees its consequences');
    // Faults are an action: the right column's third tab, on the vector the roster selected.
    await page.getByRole('tab', { name: 'Faults' }).click();
    const faults = page.getByRole('form', { name: 'Faults for DRONE-02' });
    await faults.getByRole('radio', { name: 'Link loss' }).click();
    await faults.getByLabel(/Duration/).fill(String(FAULT_DURATION_S));
    await sleep(1200);
    await faults.getByRole('button', { name: /Inject link loss/ }).click();
    mark('fault');
    await page.getByText('Accepted: injected at the next tick.').waitFor({ timeout: 15_000 });
    await sleep(3000);
    await caption(page, 'The engine redistributes its lane among the vectors left', 'Every candidate and every rejection is in the decision trace');
    await sleep(9000);
    // Selecting DRONE-02 brought the camera down to it: the selection
    // cleared, the wheel takes it back up over the whole area.
    await roster.filter({ hasText: 'DRONE-02' }).click();
    await page.mouse.move(GLOBE_X, GLOBE_Y);
    for (let i = 0; i < 3; i++) {
      await page.mouse.wheel(0, 400);
      await sleep(120);
    }

    await caption(page, 'Coverage continues; DRONE-02 rejoins once its link is back');
    await lapse(page, true);
    mark('lapse2On');
    const replayLink = page.getByRole('link', { name: /^Replay MSN-/ });
    await until('coverage near completion', async () => (await coverage(page)) >= 98.5 || (await replayLink.count()) > 0, 15 * 60_000);
    await lapse(page, false);
    mark('lapse2Off');
    await replayLink.waitFor({ timeout: 10 * 60_000 });
    const mission = (await replayLink.textContent()).replace('Replay ', '').trim();
    const ended = await api('GET', `/missions/${mission}`);
    if (ended.mission.state !== 'complete') {
      throw new Error(`${mission} ended ${ended.mission.state} at ${((100 * ended.explored) / ended.total).toFixed(1)} %: not the demo to film`);
    }
    mark('complete');
    await caption(page, 'Coverage complete', 'The mission log is closed: every batch, decision and command on the chain');
    await sleep(4000);

    await page.getByRole('button', { name: 'Real time' }).click();
    await page.getByText(`Accelerated ×${FAST}`).waitFor({ state: 'detached', timeout: 15_000 });
    mark('realtime');
    await caption(page, 'Back to real time, one click');
    await sleep(3000);

    await replayLink.click();
    mark('replay');
    await caption(page, 'The replay: keeld runs the log through the same Step again', 'while this page re-hashes every record of it from its bytes');
    await page.getByText(/records linked/).waitFor({ timeout: 5 * 60_000 });
    await sleep(1500);
    await page.getByRole('slider', { name: 'Replay time' }).focus();
    await page.keyboard.press('End');
    await page.getByText('The replay reproduces the recording').waitFor({ timeout: 5 * 60_000 });
    mark('reproduced');
    await caption(page, 'Three heads, one value: the replay reproduces the recording', 'Recorded by keeld, rebuilt by keeld, recomputed by the browser');
    await sleep(7000);

    const font = (await readFile(FONT)).toString('base64');
    await page.setContent(await readFile(CARD, 'utf8'));
    await page.addStyleTag({ content: `@font-face { font-family: 'Geist Mono'; font-weight: 100 900; src: url(data:font/woff2;base64,${font}) format('woff2'); }` });
    await page.evaluate(() => document.fonts.ready);
    mark('card');
    await sleep(10_000);
    mark('end');
  } finally {
    await capture?.stop();
    await browser.close();
    // A pace left fast would outlive the recording.
    await api('PUT', '/clock', { speed: 1 }).catch(() => {});
  }
  return marks;
}

/**
 * The edit list: every chapter at its own pace, the two stretches under the
 * time-lapse badge compressed by one factor so the whole lasts TARGET_S.
 */
function editList(m) {
  const cuts = [
    [m.start, m.lapse1On, 1],
    [m.lapse1On, m.lapse1Off, 'lapse'],
    [m.lapse1Off, m.lapse2On, 1],
    [m.lapse2On, m.lapse2Off, 'lapse'],
    [m.lapse2Off, m.replay, 1],
    // The replay's chain check is a wait: it keeps its pace, under captions
    // that say what runs.
    [m.replay, m.end, 1],
  ];
  const fixed = cuts.filter(([, , f]) => f !== 'lapse').reduce((s, [a, b]) => s + (b - a), 0);
  const lapsed = cuts.filter(([, , f]) => f === 'lapse').reduce((s, [a, b]) => s + (b - a), 0);
  const factor = Math.max(1, lapsed / Math.max(10, TARGET_S - fixed));
  return { factor, cuts: cuts.map(([a, b, f]) => [a, b, f === 'lapse' ? factor : f]) };
}

async function cut(marks) {
  const { factor, cuts } = editList(marks);
  const parts = cuts.map(([a, b, f], i) => `[0:v]trim=start=${a.toFixed(3)}:end=${b.toFixed(3)},setpts=(PTS-STARTPTS)/${f.toFixed(4)}[v${i}]`);
  const filter = `${parts.join(';')};${cuts.map((_, i) => `[v${i}]`).join('')}concat=n=${cuts.length}:v=1:a=0,fps=${FPS}[out]`;
  log(`editing: time-lapse ×${factor.toFixed(1)}`);
  await run('ffmpeg', ['-y', '-loglevel', 'error', '-i', RAW, '-filter_complex', filter, '-map', '[out]',
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '20', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', VIDEO]);

  // The loop: from just before the fault to the fleet taking over, at ×20.
  const from = marks.fault - 3;
  const length = 12;
  await mkdir(dirname(GIF), { recursive: true });
  for (const [width, fps] of [[960, 12], [800, 10], [720, 8]]) {
    const graph = `fps=${fps},scale=${width}:-1:flags=lanczos,split[a][b];[a]palettegen=stats_mode=diff:max_colors=128[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle`;
    await run('ffmpeg', ['-y', '-loglevel', 'error', '-ss', from.toFixed(3), '-t', String(length), '-i', RAW, '-filter_complex', graph, '-loop', '0', GIF]);
    const { size } = await stat(GIF);
    log(`GIF ${width} px at ${fps} fps: ${(size / 1024 / 1024).toFixed(1)} MB`);
    if (size <= GIF_MAX_BYTES) return factor;
  }
  throw new Error(`the GIF stays above ${GIF_MAX_BYTES} bytes even at 720 px`);
}

await mkdir(OUT_DIR, { recursive: true });
await preconditions();
const marks = await record();
await writeFile(MARKS, `${JSON.stringify(marks, null, 2)}\n`);
const factor = await cut(marks);
log(`done: ${VIDEO}, ${GIF} (time-lapse ×${factor.toFixed(1)}), chapters in ${MARKS}`);
