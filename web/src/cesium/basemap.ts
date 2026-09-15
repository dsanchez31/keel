import { Color, ColorMaterialProperty, ConstantProperty, Credit, GeoJsonDataSource, JulianDate } from 'cesium';

import { BASEMAP_STYLE, isBasemapClass, OSM_ATTRIBUTION } from './basemapStyle';

/**
 * The offline basemap of the reference area, extracted from OpenStreetMap by
 * scripts/basemap.mjs and served from public/. Vector, not raster: the OSM
 * tile policy forbids offline use of its tiles, the ODbL allows offline use
 * of its data.
 */
export const BASEMAP_URL = '/basemap/reference.geojson';

// The features are static: any instant reads the same values.
const ANY_TIME = new JulianDate();

/**
 * Loads the basemap as a data source for the globe: every feature clamped to
 * the ground (ground primitives, which do not flicker against the ellipsoid
 * as lines drawn at height 0 would), styled by its class, and credited with
 * the attribution the ODbL requires, which the viewer's credit display shows.
 * A feature of a class the style table does not know is hidden rather than
 * drawn in Cesium's default yellow.
 */
export async function loadBasemap(url: string = BASEMAP_URL): Promise<GeoJsonDataSource> {
  const source = await GeoJsonDataSource.load(url, {
    clampToGround: true,
    credit: new Credit(OSM_ATTRIBUTION, true),
  });
  for (const entity of source.entities.values) {
    const cls: unknown = (entity.properties?.getValue(ANY_TIME) as { class?: unknown } | undefined)?.class;
    if (!isBasemapClass(cls)) {
      entity.show = false;
      continue;
    }
    const style = BASEMAP_STYLE[cls];
    const material = new ColorMaterialProperty(Color.fromCssColorString(style.color));
    if (entity.polyline) {
      entity.polyline.material = material;
      entity.polyline.width = new ConstantProperty(style.width);
    }
    if (entity.polygon) {
      entity.polygon.material = material;
      entity.polygon.outline = new ConstantProperty(false);
    }
  }
  return source;
}
