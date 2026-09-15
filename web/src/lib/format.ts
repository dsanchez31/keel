/**
 * Mission time as the panels print it: `T+` then minutes, seconds and the
 * tenth a tick is, hours only past the first. Mission time is milliseconds
 * since the engine's first tick (spec section 3), never a time of day.
 */
export function missionTime(ms: number, opts: { tenths?: boolean } = {}): string {
  const tenths = Math.floor(Math.max(0, ms) / 100);
  const s = Math.floor(tenths / 10);
  const h = Math.floor(s / 3600);
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, '0');
  const ss = String(s % 60).padStart(2, '0');
  return `T+${h > 0 ? `${h}:` : ''}${mm}:${ss}${opts.tenths === false ? '' : `.${tenths % 10}`}`;
}

/** Explored cells over the AO's cells, and the share to one decimal. */
export function coverageText(explored: number, total: number): { percent: string; cells: string } {
  const share = total > 0 ? (explored * 100) / total : 0;
  return { percent: `${share.toFixed(1)} %`, cells: `${explored.toLocaleString('en')} / ${total.toLocaleString('en')}` };
}
