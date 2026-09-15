import { type VectorID } from '@keel/sdk';
import { type Viewer } from 'cesium';
import { useEffect, useRef, useState, useSyncExternalStore } from 'react';

import { CesiumViewer } from '@/cesium/CesiumViewer';
import { CursorTracker } from '@/cesium/CursorTracker';
import { FogOfWar } from '@/cesium/FogOfWar';
import { MissionGlobe } from '@/cesium/MissionGlobe';
import { driveReplay, toJulian } from '@/cesium/missionClock';
import { DEFAULT_SPEED, needsRoll, playbackWindow } from '@/cesium/playbackWindow';
import { getViewer, setClockPlaying, setClockSpeed, setClockTime } from '@/cesium/viewerManager';
import { FleetColumn } from '@/components/FleetColumn';
import { ReplayScrubber } from '@/components/ReplayScrubber';
import { type ReplayVerdict, ReplayVerification } from '@/components/ReplayVerification';
import { Readouts, type ReplayTransport, TimeControl } from '@/components/TimeControl';
import { TIMELINE_DEFAULT_SPAN_MS } from '@/components/timelineScale';
import { api } from '@/lib/api';
import { type GroundPoint } from '@/lib/geoFormat';
import { createPlayhead, type Playhead, usePlayhead } from '@/lib/playhead';
import { createReplayStore, type ReplayStore, type Sample, windowSamples } from '@/lib/replayStore';
import { createSignal, type Signal } from '@/lib/signal';
import { useChainCheck } from '@/lib/useChainCheck';

const NO_VERDICT: ReplayVerdict = { verifiedMs: undefined, divergence: undefined, end: undefined };

/**
 * A finished mission, replayed: the live screen's three columns fed by a
 * replay store instead of the stream, the time strip carrying a transport and
 * the scrubber, and on the right the proof that the replay reproduces the
 * recording.
 *
 * keeld serves the mission as windows of frames (GET /api/v1/replays/{id}/
 * frames), replayed and verified by the same Step that ran it; which window
 * follows from the playhead (playbackWindow.ts). The page checks the log's
 * chain itself meanwhile (useChainCheck), which also gives the trace the
 * recording's own decisions. The clock is the live view's, driven here by the
 * transport and the scrubber: one rendering path, two clock modes, as the
 * engine is one Step fed live or recorded input.
 */
export function ReplayScreen({ mission }: { mission: string }) {
  const [store] = useState(createReplayStore);
  const [playhead] = useState(createPlayhead);
  const check = useChainCheck(mission);
  const [selected, setSelected] = useState<VectorID | undefined>(undefined);
  const [cursor] = useState(() => createSignal<GroundPoint | undefined>(undefined));
  const [samples, setSamples] = useState<ReadonlyMap<VectorID, readonly Sample[]> | undefined>(undefined);
  const [verdict, setVerdict] = useState<ReplayVerdict>(NO_VERDICT);
  const [windowError, setWindowError] = useState<string | undefined>(undefined);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState<number>(DEFAULT_SPEED);

  const endMs = verdict.end?.tick_ms ?? (check.status === 'done' ? check.result.ticks.at(-1)?.tickMs : undefined);
  const endRef = useRef(endMs);
  useEffect(() => {
    endRef.current = endMs;
  }, [endMs]);

  useEffect(() => {
    if (check.status === 'done') store.setDecisions(check.result.decisions);
  }, [check, store]);

  // The store follows the playhead, and the window follows it too: a new
  // one is asked for when the playhead nears an edge of the one loaded, one
  // request at a time. A failed request is not retried behind the operator's
  // back: the panel says so.
  useEffect(() => {
    let inFlight: AbortController | undefined;
    let failed = false;
    const follow = () => {
      const ms = playhead.get();
      store.seek(ms);
      const loaded = store.window();
      if (inFlight || failed) return;
      if (loaded && !needsRoll({ startMs: loaded.from_ms, stopMs: loaded.to_ms }, ms, endRef.current)) return;
      const w = playbackWindow(ms, endRef.current);
      const request = new AbortController();
      inFlight = request;
      api.replayFrames(mission, w.startMs, w.stopMs, { signal: request.signal }).then(
        (win) => {
          inFlight = undefined;
          store.load(win);
          store.seek(playhead.get());
          setSamples(windowSamples(win));
          setVerdict((v) => ({
            verifiedMs: Math.max(v.verifiedMs ?? 0, win.verified_ms),
            divergence: v.divergence ?? win.divergence,
            end: v.end ?? win.end,
          }));
        },
        (err: unknown) => {
          if (request.signal.aborted) return;
          inFlight = undefined;
          failed = true;
          setWindowError(err instanceof Error ? err.message : String(err));
        },
      );
    };
    follow();
    const unsubscribe = playhead.subscribe(follow);
    return () => {
      unsubscribe();
      inFlight?.abort();
    };
  }, [mission, playhead, store]);

  const viewer = () => getViewer('mission-globe');
  const scrub = (ms: number) => {
    const v = viewer();
    if (v) setClockTime(v, toJulian(ms));
  };
  const transport: ReplayTransport = {
    playing,
    speed,
    onPlaying(next: boolean) {
      const v = viewer();
      // Playing forward from the end starts over.
      if (next && speed > 0 && endMs !== undefined && playhead.get() >= endMs) scrub(0);
      if (v) setClockPlaying(v, next);
      setPlaying(next);
    },
    onSpeed(next: number) {
      const v = viewer();
      if (v) setClockSpeed(v, next);
      setSpeed(next);
      setPlaying(true);
    },
  };

  return (
    <main className="grid h-full grid-cols-[22rem_minmax(0,1fr)_26rem]">
      <FleetColumn store={store} selected={selected} onSelect={setSelected} />
      <div className="flex min-h-0 flex-col">
        <div className="min-h-0 flex-1">
          <CesiumViewer id="mission-globe" label="Replay globe">
            {(v) => (
              <>
                <ReplayClock viewer={v} playhead={playhead} endRef={endRef} onBound={() => setPlaying(false)} />
                <CursorTracker viewer={v} cursor={cursor} />
                <FogOfWar viewer={v} store={store} />
                <MissionGlobe viewer={v} store={store} selected={selected} mode="replay" samples={samples} />
              </>
            )}
          </CesiumViewer>
        </div>
        <ReplayStrip store={store} playhead={playhead} cursor={cursor} endMs={endMs} verdict={verdict} transport={transport} onScrub={scrub} />
      </div>
      <ReplayVerification mission={mission} store={store} check={check} verdict={verdict} windowError={windowError} />
    </main>
  );
}

/**
 * Reports the clock's mission time into the playhead on every frame, held
 * inside the mission (driveReplay), the clock stopped at the playhead to
 * begin with. Renders nothing.
 */
function ReplayClock({
  viewer,
  playhead,
  endRef,
  onBound,
}: {
  viewer: Viewer;
  playhead: Playhead;
  endRef: { readonly current: number | undefined };
  onBound: () => void;
}) {
  const boundRef = useRef(onBound);
  useEffect(() => {
    boundRef.current = onBound;
  }, [onBound]);

  useEffect(() => {
    setClockPlaying(viewer, false);
    setClockTime(viewer, toJulian(playhead.get()));
    return driveReplay(viewer, { onTime: playhead.set, endMs: () => endRef.current, onBound: () => boundRef.current() });
  }, [viewer, playhead, endRef]);

  return null;
}

/** The time strip of a replay: re-rendered as the playhead moves, alone. */
function ReplayStrip({
  store,
  playhead,
  cursor,
  endMs,
  verdict,
  transport,
  onScrub,
}: {
  store: ReplayStore;
  playhead: Playhead;
  cursor: Signal<GroundPoint | undefined>;
  endMs: number | undefined;
  verdict: ReplayVerdict;
  transport: ReplayTransport;
  onScrub: (ms: number) => void;
}) {
  const ms = usePlayhead(playhead);
  const head = useSyncExternalStore(store.subscribe, () => store.get()?.head ?? '');
  const [span, setSpan] = useState(TIMELINE_DEFAULT_SPAN_MS);

  return (
    <TimeControl tickMs={ms} head={head || undefined} replay={transport} readouts={<Readouts store={store} cursor={cursor} />}>
      <ReplayScrubber
        currentMs={ms}
        spanMs={span}
        endMs={endMs}
        verifiedMs={verdict.verifiedMs}
        divergenceMs={verdict.divergence?.tick_ms}
        onScrub={onScrub}
        onSpanChange={setSpan}
      />
    </TimeControl>
  );
}
