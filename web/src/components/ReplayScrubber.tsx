import { type KeyboardEvent, type PointerEvent, useCallback, useEffect, useRef, useState } from 'react';

import { missionTime } from '@/lib/format';

import { ticks, tickStep, zoomSpan } from './timelineScale';

/** Geometry of the drawn axis, in SVG user units (CSS pixels here). */
const HEIGHT = 40;
const AXIS_Y = 16;
const TICK_LENGTH = 6;
const LABEL_Y = 32;

/** How many gradations one Page key moves, against one arrow key. */
const PAGE_TICKS = 10;

/** Horizontal room per label: the gradation follows the measured width. */
const LABEL_PIXEL_BUDGET = 120;

export interface ReplayScrubberProps {
  /** Mission time under the playhead. The axis is centred on it. */
  currentMs: number;
  /** How much mission time the axis shows, end to end. */
  spanMs: number;
  /** The recording's last tick, once known. */
  endMs: number | undefined;
  /** keeld verified the replay to here (ReplayWindow.verified_ms). */
  verifiedMs: number | undefined;
  /** The first record keeld did not rebuild, if any. */
  divergenceMs: number | undefined;
  onScrub: (ms: number) => void;
  onSpanChange: (spanMs: number) => void;
}

/**
 * The replay's time axis: gradations
 * in mission time, and a playhead pinned to the centre.
 *
 * **The playhead does not move; the axis does.** Playback scrolls the
 * gradations under it, and dragging the axis moves time, because nothing
 * else could move. The gradations are aligned on the mission's start
 * (`timelineScale.ticks`), so they stay still relative to each other.
 *
 * **A mission has ends, where an orbit had none.** Before T+0 and after the
 * last tick the axis is shaded and the scrub is held inside, and along it a
 * cyan band shows how far keeld's replay has been verified against the
 * recording, a red mark where it diverged.
 *
 * A slider by hand, its semantics written out: `aria-valuetext` carries the
 * mission time a screen reader announces.
 */
export function ReplayScrubber({ currentMs, spanMs, endMs, verifiedMs, divergenceMs, onScrub, onSpanChange }: ReplayScrubberProps) {
  const rootRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  // The drag in progress: a ref, not state, since only the time it scrubs to
  // is rendered, and that lives in the clock.
  const dragRef = useRef<{ pointerId: number; x: number; atMs: number } | null>(null);

  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    const observer = new ResizeObserver(([entry]) => {
      const box = entry?.contentRect;
      if (box) setWidth(box.width);
    });
    observer.observe(root);
    return () => observer.disconnect();
  }, []);

  // Attached by hand: React registers onWheel passively, and preventDefault
  // there would do nothing but warn while the page scrolled.
  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      onSpanChange(zoomSpan(spanMs, event.deltaY));
    };
    root.addEventListener('wheel', onWheel, { passive: false });
    return () => root.removeEventListener('wheel', onWheel);
  }, [spanMs, onSpanChange]);

  const scrubTo = useCallback(
    (ms: number) => onScrub(Math.max(0, endMs === undefined ? ms : Math.min(endMs, ms))),
    [onScrub, endMs],
  );

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture(event.pointerId);
    dragRef.current = { pointerId: event.pointerId, x: event.clientX, atMs: currentMs };
  };

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId || width === 0) return;
    // Dragging the axis left pulls later times under the playhead.
    scrubTo(drag.atMs - ((event.clientX - drag.x) * spanMs) / width);
  };

  const endDrag = (event: PointerEvent<HTMLDivElement>) => {
    if (dragRef.current?.pointerId === event.pointerId) dragRef.current = null;
  };

  const targetTicks = Math.max(3, Math.round(width / LABEL_PIXEL_BUDGET));

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const step = tickStep(spanMs, targetTicks);
    const moves: Record<string, number> = { ArrowLeft: -step, ArrowRight: step, PageDown: -step * PAGE_TICKS, PageUp: step * PAGE_TICKS };
    if (event.key === 'Home') scrubTo(0);
    else if (event.key === 'End' && endMs !== undefined) scrubTo(endMs);
    else if (event.key in moves) scrubTo(currentMs + moves[event.key]!);
    else return;
    event.preventDefault();
  };

  /** Mission time to x. The playhead is the origin, at the centre. */
  const xOf = (ms: number) => width / 2 + ((ms - currentMs) * width) / spanMs;
  const clampX = (x: number) => Math.min(width, Math.max(0, x));
  const centre = width / 2;
  const startX = clampX(xOf(0));
  const endX = endMs === undefined ? width : clampX(xOf(endMs));

  return (
    <div
      ref={rootRef}
      role="slider"
      tabIndex={0}
      aria-label="Replay time"
      aria-valuemin={0}
      aria-valuemax={endMs}
      aria-valuenow={Math.round(currentMs)}
      aria-valuetext={missionTime(currentMs)}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      onKeyDown={onKeyDown}
      className="w-full cursor-grab touch-none outline-none select-none focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:ring-inset active:cursor-grabbing"
    >
      {/* Drawn once measured: every x divides by the width. */}
      {width > 0 && (
        <svg width={width} height={HEIGHT} aria-hidden className="block">
          {/* Outside the mission. */}
          <rect x={0} y={0} width={startX} height={HEIGHT} className="fill-background/70" />
          <rect x={endX} y={0} width={width - endX} height={HEIGHT} className="fill-background/70" />

          <line x1={startX} y1={AXIS_Y} x2={endX} y2={AXIS_Y} className="stroke-border" />
          {verifiedMs !== undefined && (
            <line x1={startX} y1={AXIS_Y - 2} x2={clampX(xOf(verifiedMs))} y2={AXIS_Y - 2} strokeWidth={2} className="stroke-cyan-400/70" />
          )}
          {divergenceMs !== undefined && (
            <line x1={xOf(divergenceMs)} y1={2} x2={xOf(divergenceMs)} y2={AXIS_Y + TICK_LENGTH} strokeWidth={2} className="stroke-red-500" />
          )}

          {ticks(currentMs, spanMs, targetTicks)
            .filter((at) => endMs === undefined || at <= endMs)
            .map((at) => (
              <g key={at}>
                <line x1={xOf(at)} y1={AXIS_Y} x2={xOf(at)} y2={AXIS_Y + TICK_LENGTH} className="stroke-border" />
                <text x={xOf(at)} y={LABEL_Y} textAnchor="middle" className="fill-muted-foreground font-mono text-[10px]">
                  {missionTime(at, { tenths: false })}
                </text>
              </g>
            ))}

          <g className="stroke-primary">
            <line x1={centre} y1={0} x2={centre} y2={HEIGHT} strokeWidth={1} opacity={0.35} />
            <line x1={centre} y1={2} x2={centre} y2={AXIS_Y + TICK_LENGTH} strokeWidth={1.5} />
          </g>
        </svg>
      )}
    </div>
  );
}
