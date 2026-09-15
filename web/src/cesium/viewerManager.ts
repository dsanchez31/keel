import 'cesium/Build/Cesium/Widgets/widgets.css';

import { ClockRange, ClockStep, Color, JulianDate, Viewer } from 'cesium';

import { loadBasemap } from './basemap';
import { offlineGlobe } from './imagery';

/**
 * Owns the Cesium WebGL contexts, outside of React.
 *
 * A Cesium Viewer is one WebGL context plus its worker pool and imagery
 * caches. Creating one per mount (the naive `useEffect(() => new
 * Viewer(ref))`) leaks it on every navigation: browsers cap the number of live
 * contexts and reclaim the oldest, but the JavaScript side keeps growing.
 *
 * So a viewer is created once per distinct view and reused for the life of the
 * tab. It renders into a detached container this module owns; mounting moves
 * the container into the React host node, unmounting moves it back out. The
 * component never constructs and never destroys: it only re-parents.
 */

/** One entry per distinct view. KEEL has one globe, live and replay alike. */
export type ViewerId = 'mission-globe';

interface ManagedViewer {
  viewer: Viewer;
  /** Owned by this module and reused across mounts; never removed from the map. */
  container: HTMLDivElement;
}

const viewers = new Map<ViewerId, ManagedViewer>();

const createManagedViewer = (): ManagedViewer => {
  const container = document.createElement('div');
  container.className = 'h-full w-full';

  const { baseLayer, terrainProvider } = offlineGlobe();
  const viewer = new Viewer(container, {
    // A transparent canvas over the host's own background, the app's tokens
    // rather than Cesium's sky.
    contextOptions: { webgl: { alpha: true } },
    baseLayer,
    terrainProvider,
    skyBox: false,
    skyAtmosphere: false,
    // The ODbL requires "© OpenStreetMap contributors" on screen, and it shows there.
    //
    // The clock is driven by KEEL's own time controls; Cesium's dial and
    // timeline carry their own labels and look.
    animation: false,
    timeline: false,
    // Every widget below would reach for something the globe does not have:
    // ion's asset browser, ion's geocoder, an iframe info box.
    baseLayerPicker: false,
    geocoder: false,
    homeButton: false,
    navigationHelpButton: false,
    fullscreenButton: false,
    infoBox: false,
    selectionIndicator: false,
    sceneModePicker: false,
    // A still scene costs nothing; any change of simulated time requests a
    // frame (maximumRenderTimeChange is 0), so playback renders continuously.
    requestRenderMode: true,
  });

  // Mission time, not the time of day: the clock runs at the rate keeld ticks
  // (speed 1) and is unbounded, the replay scrubber moving it where it needs.
  // It starts stopped; the live stream and the time controls start it.
  viewer.clock.shouldAnimate = false;
  viewer.clock.clockStep = ClockStep.SYSTEM_CLOCK_MULTIPLIER;
  viewer.clock.multiplier = 1;
  viewer.clock.clockRange = ClockRange.UNBOUNDED;

  const { scene } = viewer;
  scene.backgroundColor = Color.TRANSPARENT;
  // Where no imagery covers the globe, it is near-black rather than Cesium's
  // blue.
  scene.globe.baseColor = Color.BLACK;
  // No terminator and no atmosphere: the mission is read against its lanes and
  // its fog, and mission time says nothing of where the sun is.
  scene.globe.enableLighting = false;
  scene.globe.showGroundAtmosphere = false;
  if (scene.sun) scene.sun.show = false;
  if (scene.moon) scene.moon.show = false;
  // Render at the device's own pixel ratio: thin lanes and tracks stay crisp
  // on a HiDPI panel.
  viewer.useBrowserRecommendedResolution = false;

  // The map at mission scale, for the life of the viewer like the imagery. A
  // globe without it still flies the mission, so a failure is reported, not
  // thrown.
  viewer.dataSources.add(loadBasemap()).then(
    () => scene.requestRender(),
    (err: unknown) => console.error('the offline basemap did not load', err),
  );

  return { viewer, container };
};

/**
 * Moves the view's canvas into `host`, creating the viewer on first use.
 *
 * Safe to call repeatedly for the same id: under StrictMode React mounts,
 * unmounts and remounts every effect, which exercises exactly this path.
 */
export const attachViewer = (id: ViewerId, host: HTMLElement): Viewer => {
  let managed = viewers.get(id);

  if (!managed) {
    managed = createManagedViewer();
    viewers.set(id, managed);
  }

  host.appendChild(managed.container);
  // The container was detached, zero sized, while unmounted: Cesium's cached
  // canvas dimensions are stale until it measures the new host.
  managed.viewer.resize();

  return managed.viewer;
};

/**
 * Writing to the viewer's clock, every write, from anywhere (and in
 * missionClock.ts `followLive`, which steers it frame by frame, and
 * `driveReplay`, which holds it inside the mission).
 *
 * Functions here rather than statements in the components because React may
 * not mutate a Cesium viewer: the React Compiler treats props and state as
 * immutable (`react-hooks/immutability`), and a Cesium Clock is an external,
 * mutable object whose API is assignment. This module owns the viewer's
 * lifetime, so it owns its mutation. Each ends by asking for a frame, since a
 * paused scene in requestRenderMode redraws only when asked.
 */

/** Moves the playhead. */
export const setClockTime = (viewer: Viewer, at: JulianDate): void => {
  viewer.clock.currentTime = JulianDate.clone(at);
  viewer.scene.requestRender();
};

/** Starts or stops playback, leaving the speed alone. */
export const setClockPlaying = (viewer: Viewer, isPlaying: boolean): void => {
  viewer.clock.shouldAnimate = isPlaying;
  viewer.scene.requestRender();
};

/**
 * Sets the signed speed and starts playback with it: the sense of playback is
 * the sign of the multiplier, and choosing a speed for a stopped clock would
 * otherwise do nothing a reader could see.
 */
export const setClockSpeed = (viewer: Viewer, multiplier: number): void => {
  viewer.clock.multiplier = multiplier;
  viewer.clock.shouldAnimate = true;
  viewer.scene.requestRender();
};

/** The viewer behind `id`, or undefined before its first attachViewer. */
export const getViewer = (id: ViewerId): Viewer | undefined => viewers.get(id)?.viewer;

/**
 * Detaches the canvas from the DOM, keeping the viewer (its WebGL context,
 * imagery, basemap and entities) alive for the next mount. There is no
 * destroyViewer by design.
 */
export const detachViewer = (id: ViewerId): void => {
  viewers.get(id)?.container.remove();
};

/**
 * Test only. The module's point is that viewers outlive their consumers, so a
 * suite asserting on creation counts needs a way back to a clean slate.
 */
export const resetViewersForTest = (): void => {
  for (const { viewer, container } of viewers.values()) {
    viewer.destroy();
    container.remove();
  }
  viewers.clear();
};
