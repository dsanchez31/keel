---
status: accepted
date: 2026-09-12
---

# Offline cartography, without Cesium ion or any tile server

## Context and Problem Statement

The frontend draws the mission on a CesiumJS globe. Cesium's defaults pull imagery and terrain from Cesium ion, which needs an account token and the internet. Where should the globe's imagery, map and terrain come from?

## Decision Drivers

* The repository must render from a clean clone with no credentials and no sign-up.
* An orchestration layer for this domain must run where there is no internet, the argument that already makes the local planner the default.
* Licences must allow redistribution and offline use.
* Enough map at mission scale for an operator to read the ground: roads, tracks, waterways.

## Considered Options

* Offline: NaturalEarthII imagery bundled with Cesium, an OpenStreetMap vector extract of the reference area, the WGS84 ellipsoid as terrain
* Cesium ion (world terrain and imagery) with an access token
* OpenStreetMap raster tiles
* A self-hosted vector tile set (an OpenMapTiles or Protomaps extract)

## Decision Outcome

Chosen option: "offline cartography", because it is the only option that renders with no account and no network while staying within every licence. The globe fetches nothing from another origin.

* **Imagery**: NaturalEarthII, bundled with Cesium and copied with its assets, coarse and dimmed, context when zoomed out.
* **Map at mission scale**: an OSM vector extract of the reference area (`web/public/basemap/reference.geojson`, roads, tracks, rail, waterways, water) drawn ground-clamped. The ODbL allows extracting, redistributing and using the data offline, attributed and kept under the ODbL.
* **Terrain**: the WGS84 ellipsoid, what the world file and the engine measure altitudes against.
* `Ion.defaultAccessToken` is cleared, so anything reaching for ion by accident fails loudly instead of going online on Cesium's demo token.

### Consequences

* Good, because the globe renders on the first run, offline, with nothing to configure.
* Good, because `web/scripts/basemap.mjs` reproduces the extract from one Overpass query, deterministically, so a refresh is a readable diff.
* Bad, because the extract covers the reference area only: an AO elsewhere shows the ellipsoid and NaturalEarthII, with no road on it, and there is no real imagery or relief anywhere (`design.md` section 10, item 22). The durable fix is an extract per world, derived from its areas and stations, or a self-hosted vector tile set.
* Bad, because buildings are left out (some 30,000 in the area, 9.5 MB), the road network giving the context.
* Neutral, because Cesium's credit display stays visible, where "© OpenStreetMap contributors" shows as the ODbL requires.

### Confirmation

* `Ion.defaultAccessToken = ''` in `web/src/cesium/imagery.ts`.
* `pnpm --filter @keel/web basemap` regenerates the extract byte for byte from the same query.

## Pros and Cons of the Options

### Offline cartography

* Good, because no credential, no network, no third-party availability in the demo path.
* Bad, because limited to one area and no imagery or relief at mission scale.

### Cesium ion with a token

* Good, because global terrain and imagery with no work.
* Bad, because every reader must create an account before the globe renders, and it does not run disconnected.

### OpenStreetMap raster tiles

* Good, because a familiar, detailed map.
* Bad, because the OSM tile usage policy forbids bulk download and offline use of `tile.openstreetmap.org`, and online use means a third-party dependency in the demo path.

### A self-hosted vector tile set

* Good, because any region, served from the app's own origin, offline.
* Bad, because a tile pipeline and a styling layer to build and maintain for a demo that flies one area. It is the path when missions run elsewhere.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken during the frontend phase and holds in the code as built. Reasoning: `design.md` section 9.4. Real imagery offline would mean an orthophoto pyramid built with GDAL from an open source (IGN BD ORTHO in France, under Licence Ouverte 2.0).
