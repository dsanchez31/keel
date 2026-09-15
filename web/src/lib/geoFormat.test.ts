import { toPoint } from 'mgrs';
import { describe, expect, it } from 'vitest';

import { latLon, mgrs } from './geoFormat';

describe('latLon', () => {
  it('prints five decimals with hemispheres', () => {
    expect(latLon(45.026834, 5.005061)).toBe('45.02683 N 5.00506 E');
    expect(latLon(-33.8688, -70.1)).toBe('33.86880 S 70.10000 W');
  });
});

describe('mgrs', () => {
  it('groups the 1 m reference, which points back to within a metre', () => {
    const ref = mgrs(45.02683, 5.00506);
    expect(ref).toMatch(/^31T [A-Z]{2} \d{5} \d{5}$/);
    const [lon, lat] = toPoint(ref!.replaceAll(' ', ''));
    expect(Math.abs(lat - 45.02683) * 111_195).toBeLessThan(1);
    expect(Math.abs(lon - 5.00506) * 111_195 * Math.cos((45 * Math.PI) / 180)).toBeLessThan(1);
  });

  it('has none past the polar limits', () => {
    expect(mgrs(85, 0)).toBeUndefined();
    expect(mgrs(-81, 0)).toBeUndefined();
  });
});
