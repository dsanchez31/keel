// Extracts the offline basemap of the reference area from OpenStreetMap.
//
//   pnpm --filter @keel/web basemap
//
// One Overpass API query over the area of examples/worlds/reference.yaml,
// written to public/basemap/reference.geojson: roads, tracks, railways,
// waterways and water bodies, as thin vector features the globe draws in the
// tactical style. The globe needs no tile server and no Cesium ion account.
//
// Why vectors and not raster tiles: the OSM tile usage policy forbids bulk
// download and offline use of tile.openstreetmap.org. OSM data itself is under
// the Open Database License, which allows extracting it, redistributing it and
// using it offline, provided it is attributed and stays under the ODbL: the
// file carries both, and the globe shows "© OpenStreetMap contributors".
//
// The output is deterministic for a given state of OSM: features sorted by
// class then way id, coordinates rounded to 6 decimals (about 0.1 m), only
// the class kept of each way's tags. Re-running it shows what changed on the
// ground as a readable diff. Multipolygon relations (large lakes, forests)
// are not fetched: the Bievre plain has none the mission needs. Buildings are
// not either: the area holds some 30,000 (France's cadastre import), which
// made the file 9.5 MB and 35,000 entities for the globe, against about one
// megabyte without them. The road network already locates every settlement.

import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

// The AO fog_of_war_east (lat 45.012 to 45.047, lon 5.024 to 5.097) and the
// station gcs-west (lon 5.005) of examples/worlds/reference.yaml, with about
// 1.5 km of margin around both.
const BBOX = { south: 45.0, west: 4.985, north: 45.06, east: 5.115 };

const ENDPOINT = 'https://overpass-api.de/api/interpreter';
const USER_AGENT = 'keel-basemap/0.1 (+https://github.com/dsanchez31/keel)';
const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', 'public', 'basemap', 'reference.geojson');

const query = `[out:json][timeout:120][bbox:${BBOX.south},${BBOX.west},${BBOX.north},${BBOX.east}];
(
  way["highway"~"^(motorway|trunk|primary|secondary|tertiary|unclassified|residential|living_street|service|track)$"];
  way["railway"="rail"];
  way["waterway"~"^(river|stream|canal|ditch|drain)$"];
  way["natural"="water"];
);
out geom;`;

// classOf maps a way's tags to the one property the globe styles on.
function classOf(tags) {
  const hw = tags.highway;
  if (hw) {
    if (['motorway', 'trunk', 'primary', 'secondary'].includes(hw)) return 'road-major';
    if (hw === 'track') return 'track';
    return 'road-minor';
  }
  if (tags.railway) return 'rail';
  if (tags.waterway) return 'waterway';
  if (tags.natural === 'water') return 'water';
  return undefined;
}

const AREAS = new Set(['water']);

const round = (x) => Math.round(x * 1e6) / 1e6;

function feature(way) {
  const cls = classOf(way.tags ?? {});
  if (!cls || !Array.isArray(way.geometry) || way.geometry.length < 2) return undefined;
  const coords = way.geometry.map((p) => [round(p.lon), round(p.lat)]);
  const first = coords[0];
  const last = coords[coords.length - 1];
  const closed = first[0] === last[0] && first[1] === last[1];
  const geometry =
    AREAS.has(cls) && closed && coords.length >= 4
      ? { type: 'Polygon', coordinates: [coords] }
      : { type: 'LineString', coordinates: coords };
  return { type: 'Feature', id: way.id, properties: { class: cls }, geometry };
}

const res = await fetch(ENDPOINT, {
  method: 'POST',
  headers: { 'User-Agent': USER_AGENT, 'Content-Type': 'application/x-www-form-urlencoded' },
  body: new URLSearchParams({ data: query }),
});
if (!res.ok) {
  throw new Error(`Overpass answered ${res.status} ${res.statusText}: ${await res.text()}`);
}
const osm = await res.json();

const features = osm.elements
  .filter((e) => e.type === 'way')
  .map(feature)
  .filter((f) => f !== undefined)
  .sort((a, b) => (a.properties.class < b.properties.class ? -1 : a.properties.class > b.properties.class ? 1 : a.id - b.id));

const collection = {
  type: 'FeatureCollection',
  // Foreign members (RFC 7946 section 6.1): the terms travel with the data.
  attribution: '© OpenStreetMap contributors',
  license: 'ODbL-1.0, https://opendatacommons.org/licenses/odbl/1-0/',
  source: `Overpass API, OSM data as of ${osm.osm3s?.timestamp_osm_base ?? 'unknown'}`,
  bbox: [BBOX.west, BBOX.south, BBOX.east, BBOX.north],
  features,
};

await mkdir(dirname(OUT), { recursive: true });
await writeFile(OUT, JSON.stringify(collection) + '\n');

const counts = Object.entries(
  features.reduce((acc, f) => ({ ...acc, [f.properties.class]: (acc[f.properties.class] ?? 0) + 1 }), {}),
)
  .map(([k, v]) => `${k} ${v}`)
  .join(', ');
console.log(`${features.length} features (${counts}) written to ${OUT}`);
