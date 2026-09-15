import { type WorldView } from '@keel/sdk';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { WorldNames } from './WorldNames';

afterEach(cleanup);

const area = { name: 'fog_of_war_east', polygon: { ring: [{ lat: 45, lon: 5, alt_m: 0 }] }, cell_m: 50, scan_alt_m: 120, area_km2: 14.312 };
const view: WorldView = { name: 'reference', areas: [area], stations: [{ name: 'gcs-west', position: { lat: 45, lon: 5, alt_m: 0 } }] };

describe('WorldNames', () => {
  it('lists the areas with their surface and the stations', () => {
    render(<WorldNames world={{ view, error: null }} onPick={vi.fn()} />);
    expect(within(screen.getByRole('group', { name: 'Areas' })).getByRole('button').textContent).toBe('fog_of_war_east 14.3 km²');
    expect(within(screen.getByRole('group', { name: 'Stations' })).getByRole('button').textContent).toBe('gcs-west');
  });

  it('hands a picked name to the intent, and a picked area to the globe', () => {
    const onPick = vi.fn();
    const onArea = vi.fn();
    render(<WorldNames world={{ view, error: null }} onPick={onPick} onArea={onArea} />);
    fireEvent.click(screen.getByRole('button', { name: /fog_of_war_east/ }));
    expect(onPick).toHaveBeenLastCalledWith('fog_of_war_east');
    expect(onArea).toHaveBeenCalledWith(area);
    fireEvent.click(screen.getByRole('button', { name: 'gcs-west' }));
    expect(onPick).toHaveBeenLastCalledWith('gcs-west');
    expect(onArea).toHaveBeenCalledTimes(1);
  });

  it('says why the areas are missing, and shows nothing while they load', () => {
    const { rerender } = render(<WorldNames world={{ view: undefined, error: null }} onPick={vi.fn()} />);
    expect(screen.queryByRole('group')).toBeNull();
    rerender(<WorldNames world={{ view: undefined, error: new Error('keeld unreachable') }} onPick={vi.fn()} />);
    expect(screen.getByText(/Areas unavailable: keeld unreachable/)).toBeTruthy();
  });
});
