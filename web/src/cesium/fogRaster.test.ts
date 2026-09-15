import { type Grid } from '@keel/sdk';
import { describe, expect, it } from 'vitest';

import { createFogImage, FOG_COLOURS, fogScale, gridExtent, sameRaster, updateFogImage } from './fogRaster';

// Two columns, two rows: cell 0 south-west, 1 south-east, 2 north-west, 3
// north-east. Cell 3 lies outside the AO.
const grid = (explored = [false, false, false, false]): Grid => ({
  origin: { lat: 45, lon: 5, alt_m: 0 },
  ref_lat: 45,
  cell_m: 50,
  cols: 2,
  rows: 2,
  in_ao: [true, true, true, false],
  explored,
});

// The colour of a cell's top-left pixel, image row 0 being the north.
const pixel = (image: ReturnType<typeof createFogImage>, row: number, col: number) => {
  const i = ((1 - row) * image.scale * image.width + col * image.scale) * 4;
  return [...image.data.slice(i, i + 4)];
};

describe('gridExtent', () => {
  it('spans the cells in the projection the engine built them in', () => {
    const e = gridExtent(grid());
    expect(e.west).toBe(5);
    expect(e.south).toBe(45);
    expect((e.north - e.south) * 111_195.08).toBeCloseTo(100, 2);
    expect((e.east - e.west) * 111_195.08 * Math.cos(Math.PI / 4)).toBeCloseTo(100, 2);
  });
});

describe('fogScale', () => {
  it('draws four pixels a cell, fewer for a raster that would pass the cap', () => {
    expect(fogScale(grid())).toBe(4);
    expect(fogScale({ ...grid(), cols: 1000 })).toBe(2);
    expect(fogScale({ ...grid(), cols: 5000 })).toBe(1);
  });
});

describe('createFogImage and updateFogImage', () => {
  it('veils the unexplored AO, washes the explored, leaves the rest clear, north up', () => {
    const image = createFogImage(grid([true, false, false, false]));
    expect(image.width).toBe(8);
    expect(pixel(image, 0, 0)).toEqual([...FOG_COLOURS.seen]);
    expect(pixel(image, 0, 1)).toEqual([...FOG_COLOURS.hidden]);
    expect(pixel(image, 1, 1)).toEqual([...FOG_COLOURS.outside]);
    expect([...image.data.slice(0, 4)]).toEqual([...FOG_COLOURS.hidden]);
  });

  it('repaints only the cells explored since, and says whether there were any', () => {
    const before = grid();
    const image = createFogImage(before);
    const after = grid([false, false, true, false]);
    expect(updateFogImage(image, after, before.explored)).toBe(true);
    expect(pixel(image, 1, 0)).toEqual([...FOG_COLOURS.seen]);
    expect(updateFogImage(image, after, after.explored)).toBe(false);
  });
});

describe('sameRaster', () => {
  it('is the same raster whatever was explored, another one if its AO differs', () => {
    expect(sameRaster(grid(), grid([true, true, true, false]))).toBe(true);
    expect(sameRaster(grid(), { ...grid(), in_ao: [true, true, true, true] })).toBe(false);
    expect(sameRaster(grid(), { ...grid(), cell_m: 25 })).toBe(false);
  });
});
