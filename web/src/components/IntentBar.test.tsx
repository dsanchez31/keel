import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { createRef } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { IntentBar, type IntentBarHandle } from './IntentBar';

// Vitest runs without globals, so Testing Library cannot register its own cleanup.
afterEach(cleanup);

const setup = (compiling = false) => {
  const onCompile = vi.fn();
  render(<IntentBar onCompile={onCompile} compiling={compiling} />);
  return { onCompile, input: screen.getByLabelText('Intent'), button: screen.getByRole('button', { name: 'Compile intent' }) };
};

describe('IntentBar', () => {
  it('compiles the trimmed intent on Enter, and keeps it', () => {
    const { onCompile, input } = setup();
    fireEvent.change(input, { target: { value: '  sweep the east  ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onCompile).toHaveBeenCalledWith('sweep the east');
    expect(input).toHaveProperty('value', '  sweep the east  ');
  });

  it('breaks the line on Shift+Enter instead', () => {
    const { onCompile, input } = setup();
    fireEvent.change(input, { target: { value: 'sweep' } });
    fireEvent.keyDown(input, { key: 'Enter', shiftKey: true });
    expect(onCompile).not.toHaveBeenCalled();
  });

  it('refuses a blank intent', () => {
    const { onCompile, input, button } = setup();
    fireEvent.change(input, { target: { value: '   ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onCompile).not.toHaveBeenCalled();
    expect(button).toHaveProperty('disabled', true);
  });

  it('waits while a compilation runs', () => {
    const { onCompile, input } = setup(true);
    fireEvent.change(input, { target: { value: 'sweep' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onCompile).not.toHaveBeenCalled();
  });

  it('inserts a picked name at the caret, spaced from the words around it', () => {
    const bar = createRef<IntentBarHandle>();
    render(<IntentBar ref={bar} onCompile={vi.fn()} compiling={false} />);
    const input = screen.getByLabelText('Intent') as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: 'Sweep' } });
    act(() => bar.current?.insert('fog_of_war_east'));
    expect(input.value).toBe('Sweep fog_of_war_east');
    expect(input.selectionStart).toBe(input.value.length);

    fireEvent.change(input, { target: { value: 'Sweep with drones' } });
    input.setSelectionRange(5, 5);
    act(() => bar.current?.insert('fog_of_war_east'));
    expect(input.value).toBe('Sweep fog_of_war_east with drones');
    expect(input.selectionStart).toBe('Sweep fog_of_war_east'.length);
  });
});
