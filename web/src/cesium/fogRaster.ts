import { EarthRadiusM, type Grid } from '@keel/sdk';

/**
 * The fog of war as pixels, apart from Cesium: one block of pixels per cell
 * of the AO raster, veiled while unexplored, faintly lit once explored,
 * transparent outside the AO. fogLayer.ts drapes it over the ground.
 */

export type Rgba = readonly [number, number, number, number];

/** Near-black veil over what no sensor has seen; a faint cyan wash over what one has. */
export const FOG_COLOURS: { hidden: Rgba; seen: Rgba; outside: Rgba } = {
  hidden: [5, 7, 10, 150],
  seen: [34, 211, 238, 20],
  outside: [0, 0, 0, 0],
};

/**
 * Texture size cap. Every WebGL implementation takes 2048 (WebGL 2 guarantees
 * it); a raster that large draws one pixel per cell.
 */
export const MAX_FOG_PX = 2048;

export interface FogImage {
  width: number;
  height: number;
  /** Pixels per cell edge. */
  scale: number;
  /** RGBA, row 0 the northern edge, as an image is uploaded. */
  data: Uint8ClampedArray<ArrayBuffer>;
}

/**
 * The raster's extent in degrees. The grid is square cells of `cell_m` in
 * the equirectangular projection at `ref_lat` (internal/domain/grid.go), so
 * it covers a latitude and longitude rectangle exactly.
 */
export function gridExtent(grid: Grid): { west: number; south: number; east: number; north: number } {
  const mLat = (EarthRadiusM * Math.PI) / 180;
  const mLon = mLat * Math.cos((grid.ref_lat * Math.PI) / 180);
  return {
    west: grid.origin.lon,
    south: grid.origin.lat,
    east: grid.origin.lon + (grid.cols * grid.cell_m) / mLon,
    north: grid.origin.lat + (grid.rows * grid.cell_m) / mLat,
  };
}

/**
 * Pixels per cell edge: four, so a texture sampled with linear filtering
 * still shows crisp cell edges at mission scale, fewer when the raster would
 * exceed MAX_FOG_PX.
 */
export function fogScale(grid: Grid): number {
  return Math.max(1, Math.min(4, Math.floor(MAX_FOG_PX / Math.max(grid.cols, grid.rows, 1))));
}

/** The whole raster painted. */
export function createFogImage(grid: Grid): FogImage {
  const scale = fogScale(grid);
  const width = grid.cols * scale;
  const height = grid.rows * scale;
  const image = { width, height, scale, data: new Uint8ClampedArray(width * height * 4) };
  for (let cell = 0; cell < grid.cols * grid.rows; cell++) paintCell(image, grid, cell);
  return image;
}

/**
 * Repaints the cells explored in `grid` and not in `before`, the raster being
 * the same. Coverage only grows (I2), so these are the cells of the coverage
 * frames since. Returns whether any was.
 */
export function updateFogImage(image: FogImage, grid: Grid, before: readonly boolean[]): boolean {
  let changed = false;
  for (let cell = 0; cell < grid.explored.length; cell++) {
    if (grid.explored[cell] === before[cell]) continue;
    paintCell(image, grid, cell);
    changed = true;
  }
  return changed;
}

/** Whether two grids are one raster: same place, size, cells and AO. */
export function sameRaster(a: Grid, b: Grid): boolean {
  if (a.cols !== b.cols || a.rows !== b.rows || a.cell_m !== b.cell_m || a.ref_lat !== b.ref_lat) return false;
  if (a.origin.lat !== b.origin.lat || a.origin.lon !== b.origin.lon) return false;
  return a.in_ao.length === b.in_ao.length && a.in_ao.every((inside, i) => inside === b.in_ao[i]);
}

function paintCell(image: FogImage, grid: Grid, cell: number): void {
  const colour = !grid.in_ao[cell] ? FOG_COLOURS.outside : grid.explored[cell] ? FOG_COLOURS.seen : FOG_COLOURS.hidden;
  const row = Math.floor(cell / grid.cols);
  const col = cell % grid.cols;
  // Row 0 of the grid is its southern edge, row 0 of an image its top.
  const top = (grid.rows - 1 - row) * image.scale;
  const left = col * image.scale;
  for (let y = top; y < top + image.scale; y++) {
    for (let x = left; x < left + image.scale; x++) {
      image.data.set(colour, (y * image.width + x) * 4);
    }
  }
}
