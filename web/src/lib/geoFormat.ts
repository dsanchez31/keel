import { forward } from 'mgrs';

/**
 * Positions as the operator reads them: decimal degrees with hemispheres,
 * and MGRS beside them (design section 8.3). MGRS is a display format only
 * (spec section 3): nothing stored or sent is ever in it.
 */

/** A ground position in decimal degrees. */
export interface GroundPoint {
  lat: number;
  lon: number;
}

/** `45.02683 N 5.00506 E`: five decimals, a metre or so. */
export function latLon(lat: number, lon: number): string {
  return `${Math.abs(lat).toFixed(5)} ${lat < 0 ? 'S' : 'N'} ${Math.abs(lon).toFixed(5)} ${lon < 0 ? 'W' : 'E'}`;
}

/**
 * The 1 m MGRS reference, grouped as it is read aloud: `31T GK 12345 67890`
 * (zone and band, the 100 km square, easting, northing). Undefined where
 * MGRS is not defined, beyond 84° N and 80° S.
 */
export function mgrs(lat: number, lon: number): string | undefined {
  if (lat > 84 || lat < -80) return undefined;
  const ref = forward([lon, lat], 5);
  const m = /^(\d{1,2}[A-Z])([A-Z]{2})(\d{5})(\d{5})$/.exec(ref);
  return m ? `${m[1]} ${m[2]} ${m[3]} ${m[4]}` : ref;
}
