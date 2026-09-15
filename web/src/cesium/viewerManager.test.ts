import { afterEach, describe, expect, it, vi } from 'vitest';

/**
 * Cesium needs a WebGL context, which jsdom does not provide, so the module is
 * mocked down to the surface viewerManager touches. What is under test is the
 * retention policy (how many contexts are created, when they are destroyed,
 * where the canvas lives), not Cesium.
 */
const constructed = vi.fn();
const destroyed = vi.fn();
const basemaps = vi.fn();

vi.mock('cesium', () => {
  class Viewer {
    clock = { shouldAnimate: true, clockStep: -1, multiplier: 0, clockRange: -1, currentTime: {} };
    scene = {
      backgroundColor: null as unknown,
      globe: { baseColor: null as unknown, enableLighting: true, showGroundAtmosphere: true },
      sun: { show: true },
      moon: { show: true },
      requestRender: vi.fn(),
    };
    dataSources = { add: (p: Promise<unknown>) => p };
    useBrowserRecommendedResolution = true;
    resize = vi.fn();

    constructor(
      public container: HTMLElement,
      public options: Record<string, unknown>,
    ) {
      constructed(options);
    }

    destroy() {
      destroyed();
    }
  }
  return {
    Viewer,
    ClockRange: { UNBOUNDED: 2 },
    ClockStep: { SYSTEM_CLOCK_MULTIPLIER: 1 },
    Color: { TRANSPARENT: 'transparent', BLACK: 'black' },
    JulianDate: { clone: (d: unknown) => d },
  };
});
vi.mock('cesium/Build/Cesium/Widgets/widgets.css', () => ({}));
vi.mock('./imagery', () => ({ offlineGlobe: () => ({ baseLayer: 'natural-earth', terrainProvider: 'ellipsoid' }) }));
vi.mock('./basemap', () => ({
  loadBasemap: () => {
    basemaps();
    return Promise.resolve('basemap');
  },
}));

const { attachViewer, detachViewer, getViewer, resetViewersForTest } = await import('./viewerManager');

afterEach(() => {
  resetViewersForTest();
  vi.clearAllMocks();
});

describe('viewerManager', () => {
  it('creates one viewer per id, however often it is attached', () => {
    const a = document.createElement('div');
    const b = document.createElement('div');
    const first = attachViewer('mission-globe', a);
    detachViewer('mission-globe');
    const second = attachViewer('mission-globe', b);
    expect(second).toBe(first);
    expect(constructed).toHaveBeenCalledTimes(1);
    expect(basemaps).toHaveBeenCalledTimes(1);
    expect(b.childElementCount).toBe(1);
    expect(a.childElementCount).toBe(0);
  });

  it('builds the offline globe and keeps the credit display', () => {
    attachViewer('mission-globe', document.createElement('div'));
    const options = constructed.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(options.baseLayer).toBe('natural-earth');
    expect(options.terrainProvider).toBe('ellipsoid');
    expect(options).not.toHaveProperty('creditContainer');
    expect(options.geocoder).toBe(false);
    expect(options.baseLayerPicker).toBe(false);
  });

  it('detaches the canvas without destroying the viewer', () => {
    const host = document.createElement('div');
    attachViewer('mission-globe', host);
    detachViewer('mission-globe');
    expect(host.childElementCount).toBe(0);
    expect(destroyed).not.toHaveBeenCalled();
    expect(getViewer('mission-globe')).toBeDefined();
  });

  it('resizes on every attach, since the detached canvas measured nothing', () => {
    const viewer = attachViewer('mission-globe', document.createElement('div'));
    attachViewer('mission-globe', document.createElement('div'));
    expect(viewer.resize).toHaveBeenCalledTimes(2);
  });
});
