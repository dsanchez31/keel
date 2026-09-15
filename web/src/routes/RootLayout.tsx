import { Outlet } from '@tanstack/react-router';

/** The shell every screen renders inside: the whole viewport, nothing else yet. */
export function RootLayout() {
  return (
    <div className="h-full bg-background text-foreground">
      <Outlet />
    </div>
  );
}
