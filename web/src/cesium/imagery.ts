import { buildModuleUrl, EllipsoidTerrainProvider, ImageryLayer, Ion, TileMapServiceImageryProvider } from 'cesium';

/**
 * The globe is offline: no tile server, no Cesium ion, nothing fetched from
 * another origin.
 *
 * - Imagery: NaturalEarthII, bundled with Cesium and served from `/cesium`
 *   with its other assets. It is coarse (level 2, about 10 km a pixel): global
 *   context when zoomed out, dimmed so the vector basemap (basemap.ts) and the
 *   mission read over it at mission scale.
 * - Terrain: the WGS84 ellipsoid, which is what the world file and the engine
 *   measure altitudes against (spec section 3). The Bievre plain is flat to a
 *   few tens of metres.
 *
 * Left to itself, Viewer builds ion's world imagery and would reach Cesium's
 * servers with the demo token Cesium ships. Passing these to the Viewer keeps
 * it from building them; clearing the default token makes anything else that
 * reaches for ion (a geocoder, a stray asset id) fail loudly rather than
 * quietly go online.
 */
export interface OfflineGlobe {
  baseLayer: ImageryLayer;
  terrainProvider: EllipsoidTerrainProvider;
}

export function offlineGlobe(): OfflineGlobe {
  Ion.defaultAccessToken = '';
  return {
    baseLayer: ImageryLayer.fromProviderAsync(
      TileMapServiceImageryProvider.fromUrl(buildModuleUrl('Assets/Textures/NaturalEarthII')),
      { brightness: 0.35, saturation: 0.25 },
    ),
    terrainProvider: new EllipsoidTerrainProvider(),
  };
}
