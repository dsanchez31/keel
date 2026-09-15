import { describe, expect, it } from 'vitest';

import { coverageText, missionTime } from './format';

describe('missionTime', () => {
  it('prints minutes, seconds and tenths, hours past the first', () => {
    expect(missionTime(0)).toBe('T+00:00.0');
    expect(missionTime(252_300)).toBe('T+04:12.3');
    expect(missionTime(3_723_400)).toBe('T+1:02:03.4');
    expect(missionTime(252_300, { tenths: false })).toBe('T+04:12');
  });
});

describe('coverageText', () => {
  it('gives the share to one decimal and the cell count', () => {
    expect(coverageText(3556, 5700)).toEqual({ percent: '62.4 %', cells: '3,556 / 5,700' });
    expect(coverageText(0, 0)).toEqual({ percent: '0.0 %', cells: '0 / 0' });
  });
});
