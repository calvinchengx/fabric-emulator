import { clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

/** cn merges Tailwind classes, letting a caller override a component's defaults. */
export function cn(...inputs) {
  return twMerge(clsx(inputs));
}
