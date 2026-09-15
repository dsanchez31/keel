import { type LucideIcon } from 'lucide-react';
import { type ReactNode } from 'react';

export interface SectionHeaderProps {
  icon: LucideIcon;
  title: string;
  count: number;
  /** Controls at the header's end, such as a filter to clear. */
  children?: ReactNode;
}

/**
 * The title bar of a section of the left column: an icon, the name and its
 * count on a band of its own, so the roster and the trace read as two lists
 * rather than one.
 */
export function SectionHeader({ icon: Icon, title, count, children }: SectionHeaderProps) {
  return (
    <h2 className="flex shrink-0 items-center gap-2 border-b bg-muted/40 px-4 py-2 font-mono text-xs tracking-widest text-muted-foreground uppercase">
      <Icon aria-hidden className="size-3.5" />
      {title} <span className="text-foreground">{count}</span>
      {children}
    </h2>
  );
}
