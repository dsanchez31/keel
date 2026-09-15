/**
 * How the offline basemap draws each class of feature scripts/basemap.mjs
 * extracts: muted steel for the ground network, dark teal for water, so the
 * lanes, tracks and fog stay the brightest things on screen.
 *
 * Kept free of Cesium so the table and its coverage of the extract test
 * without a WebGL context.
 */
export const BASEMAP_CLASSES = ['road-major', 'road-minor', 'track', 'rail', 'waterway', 'water'] as const;

export type BasemapClass = (typeof BASEMAP_CLASSES)[number];

export interface BasemapStyle {
  /** CSS colour, alpha included. */
  color: string;
  /** Line width in pixels; ignored for areas. */
  width: number;
}

export const BASEMAP_STYLE: Record<BasemapClass, BasemapStyle> = {
  'road-major': { color: 'rgba(122, 138, 153, 0.9)', width: 2 },
  'road-minor': { color: 'rgba(84, 97, 110, 0.8)', width: 1 },
  track: { color: 'rgba(66, 76, 86, 0.7)', width: 1 },
  rail: { color: 'rgba(120, 104, 140, 0.8)', width: 1.5 },
  waterway: { color: 'rgba(40, 96, 128, 0.85)', width: 1.5 },
  water: { color: 'rgba(22, 60, 84, 0.8)', width: 0 },
};

export function isBasemapClass(value: unknown): value is BasemapClass {
  return typeof value === 'string' && (BASEMAP_CLASSES as readonly string[]).includes(value);
}

/** The attribution the ODbL requires wherever OSM data is shown. */
export const OSM_ATTRIBUTION = '© OpenStreetMap contributors';
