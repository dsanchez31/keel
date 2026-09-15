import { Link } from '@tanstack/react-router';

export function NotFound() {
  return (
    <main className="grid h-full place-items-center">
      <div className="space-y-2 text-center font-mono text-sm">
        <p className="text-muted-foreground">No such screen.</p>
        <Link to="/" className="underline underline-offset-4">
          Back to the mission
        </Link>
      </div>
    </main>
  );
}
