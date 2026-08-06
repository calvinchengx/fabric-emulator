import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

/** cn merges Tailwind classes, letting a caller override a component's defaults. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// The type helpers every shadcn-svelte component imports from here. They belong
// to the TypeScript variant's own utils module; this file was generated for the
// JavaScript variant, which has no use for them — so flipping components.json to
// `"typescript": true` left the regenerated components importing four names that
// did not exist yet.

/** A component's props plus the element its `ref` binds to. */
export type WithElementRef<T, U extends HTMLElement = HTMLElement> = T & {
  ref?: U | null;
};

/** Props without bits-ui's render-delegation `child` snippet. */
export type WithoutChild<T> = T extends { child?: unknown } ? Omit<T, 'child'> : T;

/** Props without `children`, for a component that renders its own content. */
export type WithoutChildren<T> = T extends { children?: unknown } ? Omit<T, 'children'> : T;

/** Neither, for a leaf that takes no slot at all. */
export type WithoutChildrenOrChild<T> = WithoutChildren<WithoutChild<T>>;
