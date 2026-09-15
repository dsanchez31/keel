/**
 * Joins class names, then lets the last Tailwind utility of a conflicting
 * group win: plain joining would emit `px-2 px-4` and leave the winner to the
 * stylesheet's order, not to the order the caller wrote.
 *
 * One engine, the `cn` package shadcn's registry imports its components
 * against (shadcn-ui/cn, the successor of clsx and tailwind-merge), so a
 * component added by `shadcn add` and one written here merge classes the
 * same way. Re-exported under the `@/lib/utils` alias components.json
 * declares, which older registry items still import.
 */
export { cn } from 'cn';
