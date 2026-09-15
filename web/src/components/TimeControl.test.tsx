import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { type LiveClock } from '@/lib/useLiveClock';

import { LiveSpeed, TimeControl } from './TimeControl';

afterEach(cleanup);

describe('TimeControl', () => {
  it('shows mission time, the stream state and the head in live, and nothing to pause', () => {
    render(<TimeControl tickMs={252_300} head={'c'.repeat(64)} status="live" />);
    expect(screen.getByText('T+04:12.3')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toContain('live');
    expect(screen.getByText(`head ${'c'.repeat(16)}`)).toBeTruthy();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('drives a replay: play, the direction, and a magnitude that keeps the direction', () => {
    const onPlaying = vi.fn();
    const onSpeed = vi.fn();
    render(<TimeControl tickMs={0} head={undefined} replay={{ playing: false, speed: -10, onPlaying, onSpeed }} />);

    fireEvent.click(screen.getByRole('button', { name: 'Play' }));
    expect(onPlaying).toHaveBeenCalledWith(true);
    fireEvent.click(screen.getByRole('button', { name: '×30' }));
    expect(onSpeed).toHaveBeenLastCalledWith(-30);
    fireEvent.click(screen.getByRole('button', { name: 'Play backward' }));
    expect(onSpeed).toHaveBeenLastCalledWith(10);
    expect(screen.getByRole('button', { name: '×10' }).getAttribute('aria-pressed')).toBe('true');
  });
});

const clock = (over: Partial<LiveClock> = {}): LiveClock => ({ maxSpeed: 20, fixed: undefined, pending: undefined, error: null, set: vi.fn(), ...over });

describe('LiveSpeed', () => {
  it("presses the stream's pace, real time the first rung", () => {
    const c = clock();
    render(<LiveSpeed speed={1} clock={c} />);
    expect(screen.getByRole('button', { name: 'Real time' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.queryByRole('button', { name: '×1' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '×20' }));
    expect(c.set).toHaveBeenCalledWith(20);
  });

  it('goes back to real time from a faster pace', () => {
    const c = clock();
    render(<LiveSpeed speed={20} clock={c} />);
    expect(screen.getByRole('button', { name: '×20' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(screen.getByRole('button', { name: 'Real time' }));
    expect(c.set).toHaveBeenCalledWith(1);
  });

  it("stays in real time with keeld's reason, and shows a refusal", () => {
    render(<LiveSpeed speed={1} clock={clock({ fixed: 'UGV-01 is not simulated', error: new Error('SITL DRONE-01 did not confirm') })} />);
    const group = screen.getByRole('group', { name: 'Live pace' });
    expect(group.getAttribute('title')).toBe('UGV-01 is not simulated');
    expect((screen.getByRole('button', { name: '×5' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole('alert').textContent).toBe('SITL DRONE-01 did not confirm');
  });

  it('offers no pace keeld would refuse', () => {
    render(<LiveSpeed speed={1} clock={clock({ maxSpeed: 5 })} />);
    expect(screen.queryByRole('button', { name: '×10' })).toBeNull();
    expect(screen.getByRole('button', { name: '×5' })).toBeTruthy();
  });
});

describe('TimeControl, faster than real time', () => {
  it('says so in place of live', () => {
    render(<TimeControl tickMs={0} head={undefined} status="live" speed={20} />);
    expect(screen.getByRole('status').textContent).toBe('Accelerated ×20');
    cleanup();
    render(<TimeControl tickMs={0} head={undefined} status="live" speed={1} />);
    expect(screen.getByRole('status').textContent).toBe('live');
  });
});
