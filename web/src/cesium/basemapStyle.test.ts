import { describe, expect, it } from 'vitest';

import extract from '../../public/basemap/reference.geojson?raw';
import { BASEMAP_CLASSES, BASEMAP_STYLE, isBasemapClass, OSM_ATTRIBUTION } from './basemapStyle';

interface Collection {
  attribution?: string;
  license?: string;
  features: { properties: { class?: unknown } }[];
}

const collection = JSON.parse(extract) as Collection;

describe('the committed basemap extract', () => {
  it('carries the ODbL terms and the attribution the globe shows', () => {
    expect(collection.attribution).toBe(OSM_ATTRIBUTION);
    expect(collection.license).toMatch(/^ODbL-1\.0/);
  });

  it('holds only classes the style table draws', () => {
    const unknown = collection.features.map((f) => f.properties.class).filter((c) => !isBasemapClass(c));
    expect(unknown).toEqual([]);
  });
});

describe('the style table', () => {
  it('styles every class', () => {
    for (const cls of BASEMAP_CLASSES) {
      expect(BASEMAP_STYLE[cls].color).not.toBe('');
    }
  });

  it('refuses what is not a class', () => {
    expect(isBasemapClass('building')).toBe(false);
    expect(isBasemapClass(undefined)).toBe(false);
    expect(isBasemapClass('water')).toBe(true);
  });
});
