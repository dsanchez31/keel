import { LoaderCircle, SendHorizontal } from 'lucide-react';
import { type FormEvent, type KeyboardEvent, type Ref, useId, useImperativeHandle, useLayoutEffect, useRef, useState } from 'react';

import { Button } from '@/components/ui/button';
import { Textarea } from '@/components/ui/textarea';
import { exampleIntent } from '@/lib/useWorld';

export interface IntentBarProps {
  /** Called with the intent, trimmed and never blank. */
  onCompile: (intent: string) => void;
  /** A compilation is running: the bar waits for it. */
  compiling: boolean;
  /** The example shown while the bar is empty. */
  placeholder?: string;
  ref?: Ref<IntentBarHandle>;
}

/** What the assistant does with the bar from outside: a name picked in the list above it. */
export interface IntentBarHandle {
  /** Inserts text at the caret, spaced from the words around it, and leaves the caret after it. */
  insert(text: string): void;
}

/**
 * Where the operator states intent in natural language, the only input the
 * planner gets (spec section 5): the area is named, never drawn. Enter
 * compiles, Shift+Enter breaks the line. The text stays after compiling, so a
 * refused intent is refined rather than retyped. A name picked above the bar
 * lands at the caret (IntentBarHandle).
 */
export function IntentBar({ onCompile, compiling, placeholder, ref }: IntentBarProps) {
  const [intent, setIntent] = useState('');
  const id = useId();
  const area = useRef<HTMLTextAreaElement>(null);
  // Where the caret goes once an insertion is rendered.
  const caret = useRef<number | undefined>(undefined);

  useImperativeHandle(
    ref,
    () => ({
      insert(text) {
        const start = area.current?.selectionStart ?? intent.length;
        const end = area.current?.selectionEnd ?? intent.length;
        const before = intent.slice(0, start);
        const after = intent.slice(end);
        const head = before + (before && !/\s$/.test(before) ? ' ' : '') + text;
        caret.current = head.length;
        setIntent(head + (after && !/^\s/.test(after) ? ' ' : '') + after);
      },
    }),
    [intent],
  );

  useLayoutEffect(() => {
    const at = caret.current;
    if (at === undefined || !area.current) return;
    caret.current = undefined;
    area.current.focus();
    area.current.setSelectionRange(at, at);
  }, [intent]);

  const trimmed = intent.trim();
  const ready = trimmed !== '' && !compiling;

  const submit = (e?: FormEvent) => {
    e?.preventDefault();
    if (ready) onCompile(trimmed);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) submit(e);
  };

  return (
    <form onSubmit={submit} className="flex items-end gap-2">
      <label htmlFor={id} className="sr-only">
        Intent
      </label>
      <Textarea
        ref={area}
        id={id}
        value={intent}
        onChange={(e) => setIntent(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder={placeholder ?? exampleIntent(undefined)}
        rows={2}
        className="max-h-40 resize-none font-mono text-xs"
      />
      <Button type="submit" size="icon" disabled={!ready} aria-label="Compile intent">
        {compiling ? <LoaderCircle className="animate-spin" /> : <SendHorizontal />}
      </Button>
    </form>
  );
}
